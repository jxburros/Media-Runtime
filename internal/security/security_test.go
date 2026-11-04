package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPoliciesRejectAmbiguousGrants(t *testing.T) {
	valid := `{"name":"studio","namespace":"studio","permissions":["jobs.submit","jobs.read"],"models":["sdxl"],"cidrs":["127.0.0.0/8"],"allowed_namespaces":["studio:*"],"concurrency_limit":2,"queue_limit":4,"rate_limit":10}`
	p, e := DecodePolicy([]byte(valid))
	if e != nil {
		t.Fatal(e)
	}
	if !p.Allows("jobs.read") || p.Allows("admin") || !p.AllowsModel("sdxl") || p.AllowsModel("other") || !p.AllowsNamespace("studio:personal") || p.AllowsNamespace("other") {
		t.Fatal("policy broadens a grant")
	}
	for _, bad := range []string{strings.Replace(valid, `"jobs.read"`, `"admin","admin"`, 1), strings.Replace(valid, `"127.0.0.0/8"`, `"anything"`, 1), strings.Replace(valid, `"rate_limit":10`, `"rate_limit":-1`, 1), strings.TrimSuffix(valid, "}") + `,"unknown":true}`, valid + "{}"} {
		if _, err := DecodePolicy([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid policy %s", bad)
		}
	}
}
func TestSlidingLimiterAndBoundedIdentities(t *testing.T) {
	l := NewLimiter(2)
	if ok, _ := l.Allow("a", 2); !ok {
		t.Fatal("first request denied")
	}
	l.Allow("a", 2)
	if ok, retry := l.Allow("a", 2); ok || retry < 1 || retry > 61 {
		t.Fatal("window not enforced")
	}
	l.Allow("b", 2)
	if ok, _ := l.Allow("c", 2); ok {
		t.Fatal("identity cap bypassed")
	}
	l.entries["a"] = []time.Time{time.Now().Add(-2 * time.Minute)}
	if ok, _ := l.Allow("c", 2); !ok {
		t.Fatal("expired identities not pruned")
	}
}
func TestAuditHasBoundedGrowth(t *testing.T) {
	a := &Audit{path: filepath.Join(t.TempDir(), "audit.jsonl")}
	for i := 0; i < 200; i++ {
		a.Record("authentication.refused", "", "")
	}
	b, e := os.ReadFile(a.path)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(string(b), "\n") != 100 {
		t.Fatal("event cap not enforced")
	}
	st, _ := os.Stat(a.path)
	if st.Mode().Perm() != 0o600 {
		t.Fatal("audit permissions")
	}
	os.WriteFile(a.path, make([]byte, 1024*1024), 0o600)
	a.window = time.Now().Add(-2 * time.Minute)
	a.Record("key.revoked", "key_id", "")
	if _, e = os.Stat(a.path + ".1"); e != nil {
		t.Fatal("rotation missing")
	}
}
