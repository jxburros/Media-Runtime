// Package security owns operator-created credentials. Tokens have 256 random bits;
// only their SHA-256 hashes are persisted. Policies are read for each request, so
// revocation takes effect without a process restart.
package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var Permissions = []string{"jobs.submit", "jobs.read", "models.read", "models.register", "admin"}
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,200}$`)

type Policy struct {
	Name              string   `json:"name"`
	Namespace         string   `json:"namespace"`
	Permissions       []string `json:"permissions"`
	Models            []string `json:"models"`
	ExpiresAt         string   `json:"expires_at,omitempty"`
	CIDRs             []string `json:"cidrs"`
	AllowedNamespaces []string `json:"allowed_namespaces"`
	AllowFallbacks    bool     `json:"allow_fallbacks"`
	ConcurrencyLimit  int      `json:"concurrency_limit"`
	QueueLimit        int      `json:"queue_limit"`
	RateLimit         int      `json:"rate_limit"`
}

type Key struct {
	ID        string `json:"key_id"`
	CreatedAt string `json:"created_at"`
	Revoked   bool   `json:"revoked"`
	Policy
}

func DecodePolicy(data []byte) (Policy, error) {
	var p Policy
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, errors.New("invalid key policy")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return p, errors.New("expected one key policy")
	}
	if len(p.Name) == 0 || len(p.Name) > 100 || !namePattern.MatchString(p.Namespace) {
		return p, errors.New("name and a valid namespace are required")
	}
	if len(p.Permissions) == 0 || len(p.Permissions) > len(Permissions) {
		return p, errors.New("permissions must be explicit")
	}
	seen := map[string]bool{}
	for _, v := range p.Permissions {
		if !contains(Permissions, v) || seen[v] {
			return p, errors.New("invalid or duplicate permission")
		}
		seen[v] = true
	}
	if len(p.Models) > 256 || len(p.CIDRs) > 64 || len(p.AllowedNamespaces) > 64 {
		return p, errors.New("policy list is too large")
	}
	for _, v := range p.Models {
		if !namePattern.MatchString(v) {
			return p, errors.New("invalid model allow-list")
		}
	}
	for _, v := range p.CIDRs {
		if _, _, err := net.ParseCIDR(v); err != nil {
			return p, errors.New("invalid CIDR restriction")
		}
	}
	for _, v := range p.AllowedNamespaces {
		if !namePattern.MatchString(strings.TrimSuffix(v, "*")) {
			return p, errors.New("invalid allowed namespace")
		}
	}
	if p.ExpiresAt != "" {
		v, err := time.Parse(time.RFC3339, p.ExpiresAt)
		if err != nil || !v.After(time.Now()) {
			return p, errors.New("expires_at must be a future RFC3339 timestamp")
		}
	}
	if p.ConcurrencyLimit == 0 {
		p.ConcurrencyLimit = 1
	}
	if p.QueueLimit == 0 {
		p.QueueLimit = 4
	}
	if p.RateLimit == 0 {
		p.RateLimit = 1200
	}
	if p.ConcurrencyLimit < 1 || p.ConcurrencyLimit > 1024 || p.QueueLimit < 1 || p.QueueLimit > 10000 || p.RateLimit < 1 || p.RateLimit > 10000 {
		return p, errors.New("limits are outside supported ranges")
	}
	return p, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func (p Policy) Allows(permission string) bool {
	return contains(p.Permissions, "admin") || contains(p.Permissions, permission)
}
func (p Policy) AllowsModel(id string) bool { return len(p.Models) == 0 || contains(p.Models, id) }
func (p Policy) AllowsIP(ip net.IP) bool {
	if len(p.CIDRs) == 0 {
		return true
	}
	for _, s := range p.CIDRs {
		_, n, err := net.ParseCIDR(s)
		if err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
func (p Policy) AllowsNamespace(ns string) bool {
	if ns == p.Namespace {
		return true
	}
	for _, s := range p.AllowedNamespaces {
		if ns == s || (strings.HasSuffix(s, "*") && strings.HasPrefix(ns, strings.TrimSuffix(s, "*"))) {
			return true
		}
	}
	return false
}

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }

type Manager struct {
	db    *sql.DB
	Audit *Audit
	IP    *Limiter
	Keys  *Limiter
}

func New(db *sql.DB, directory string) (*Manager, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS managed_keys (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS pairing_codes (code_hash TEXT PRIMARY KEY, expires_at INTEGER NOT NULL, policy TEXT NOT NULL);`)
	if err != nil {
		return nil, err
	}
	return &Manager{db: db, Audit: &Audit{path: filepath.Join(directory, "audit.jsonl")}, IP: NewLimiter(4096), Keys: NewLimiter(4096)}, nil
}
func (m *Manager) Create(p Policy) (Key, string, error) {
	id, err := random(16)
	if err != nil {
		return Key{}, "", err
	}
	secret, err := random(32)
	if err != nil {
		return Key{}, "", err
	}
	k := Key{ID: "key_" + id, CreatedAt: time.Now().UTC().Format(time.RFC3339), Policy: p}
	token := "mr_" + id + "_" + secret
	data, _ := json.Marshal(k)
	_, err = m.db.Exec("INSERT INTO managed_keys VALUES (?,?,?)", k.ID, digest(token), string(data))
	if err == nil {
		m.Audit.Record("key.created", k.ID, "")
	}
	return k, token, err
}
func (m *Manager) List() ([]Key, error) {
	rows, err := m.db.Query("SELECT data FROM managed_keys ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var data string
		var k Key
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(data), &k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
func (m *Manager) Revoke(id string) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var data string
	if err = tx.QueryRow("SELECT data FROM managed_keys WHERE id=?", id).Scan(&data); err != nil {
		return err
	}
	var k Key
	if err = json.Unmarshal([]byte(data), &k); err != nil {
		return err
	}
	k.Revoked = true
	b, _ := json.Marshal(k)
	if _, err = tx.Exec("UPDATE managed_keys SET data=? WHERE id=?", string(b), id); err != nil {
		return err
	}
	if err = tx.Commit(); err == nil {
		m.Audit.Record("key.revoked", id, "")
	}
	return err
}
func (m *Manager) Authenticate(token string, ip net.IP) (*Key, error) {
	if len(token) != 100 || !strings.HasPrefix(token, "mr_") {
		return nil, nil
	}
	var stored, data string
	err := m.db.QueryRow("SELECT token_hash,data FROM managed_keys WHERE id=?", "key_"+token[3:35]).Scan(&stored, &data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	actual := digest(token)
	if subtle.ConstantTimeCompare([]byte(stored), []byte(actual)) != 1 {
		return nil, nil
	}
	var k Key
	if err = json.Unmarshal([]byte(data), &k); err != nil {
		return nil, err
	}
	if k.Revoked || !k.AllowsIP(ip) {
		return nil, nil
	}
	if k.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339, k.ExpiresAt)
		if err != nil || !expiry.After(time.Now()) {
			return nil, nil
		}
	}
	return &k, nil
}
func (m *Manager) Pair(p Policy) (string, error) {
	code, err := random(16)
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(p)
	_, err = m.db.Exec("DELETE FROM pairing_codes WHERE expires_at<=?", time.Now().Unix())
	if err != nil {
		return "", err
	}
	var n int
	if err = m.db.QueryRow("SELECT count(*) FROM pairing_codes").Scan(&n); err != nil {
		return "", err
	}
	if n >= 64 {
		return "", errors.New("too many pending pairing codes")
	}
	_, err = m.db.Exec("INSERT INTO pairing_codes VALUES (?,?,?)", digest(code), time.Now().Add(5*time.Minute).Unix(), string(data))
	return code, err
}
func (m *Manager) Redeem(code string) (Key, string, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return Key{}, "", err
	}
	defer tx.Rollback()
	var policy string
	if err = tx.QueryRow("SELECT policy FROM pairing_codes WHERE code_hash=? AND expires_at>?", digest(code), time.Now().Unix()).Scan(&policy); err != nil {
		return Key{}, "", err
	}
	var p Policy
	if err = json.Unmarshal([]byte(policy), &p); err != nil {
		return Key{}, "", err
	}
	id, err := random(16)
	if err != nil {
		return Key{}, "", err
	}
	secret, err := random(32)
	if err != nil {
		return Key{}, "", err
	}
	k := Key{ID: "key_" + id, CreatedAt: time.Now().UTC().Format(time.RFC3339), Policy: p}
	token := "mr_" + id + "_" + secret
	data, _ := json.Marshal(k)
	if _, err = tx.Exec("INSERT INTO managed_keys VALUES (?,?,?)", k.ID, digest(token), string(data)); err != nil {
		return Key{}, "", err
	}
	if _, err = tx.Exec("DELETE FROM pairing_codes WHERE code_hash=?", digest(code)); err != nil {
		return Key{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return Key{}, "", err
	}
	m.Audit.Record("pairing.redeemed", k.ID, "")
	return k, token, nil
}

// Limiter uses a sliding minute, with a hard cap on tracked identities. It does
// not trust X-Forwarded-For: only the socket peer is eligible for IP accounting.
type Limiter struct {
	mu      sync.Mutex
	entries map[string][]time.Time
	max     int
}

func NewLimiter(max int) *Limiter { return &Limiter{entries: map[string][]time.Time{}, max: max} }
func (l *Limiter) Allow(id string, limit int) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	times := l.entries[id]
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	times = times[i:]
	if len(times) >= limit {
		return false, int(time.Until(times[0].Add(time.Minute)).Seconds()) + 1
	}
	if _, ok := l.entries[id]; !ok && len(l.entries) >= l.max {
		for k, v := range l.entries {
			if len(v) == 0 || !v[len(v)-1].After(cutoff) {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= l.max {
			return false, 60
		}
	}
	l.entries[id] = append(times, now)
	return true, 0
}

// Audit contains identifiers and event categories only, never payloads/tokens.
// Four one-MiB files and a global event budget bound unauthenticated log growth.
type Audit struct {
	mu     sync.Mutex
	path   string
	window time.Time
	count  int
}

func (a *Audit) Record(event, key, resource string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if now.Sub(a.window) >= time.Minute {
		a.window = now
		a.count = 0
	}
	if event == "authentication.refused" {
		if a.count >= 100 {
			return
		}
		a.count++
	}
	if st, err := os.Stat(a.path); err == nil && st.Size() >= 1024*1024 {
		os.Remove(a.path + ".3")
		for i := 2; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1))
		}
		os.Rename(a.path, a.path+".1")
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]string{"at": now.UTC().Format(time.RFC3339Nano), "event": event, "key_id": key, "resource_id": resource})
	f.Write(append(b, '\n'))
	f.Sync()
}
