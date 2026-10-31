// Package config reads and validates runtime.config.json.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

type Credential struct {
	TokenEnv          string
	Namespace         string
	Operator          bool
	AllowedNamespaces []string
	AllowFallbacks    bool
}

type Token struct {
	Token             string
	Namespace         string
	Operator          bool
	AllowedNamespaces []string
	// AllowFallbacks lets this credential forward authorized_fallbacks to the worker.
	AllowFallbacks bool
}

// OutputMIMEs are the output types the runtime can verify, with their default size limits.
var OutputMIMEs = []string{"image/png", "application/zip", "video/mp4", "audio/wav"}

var defaultOutputBytes = map[string]int{
	"image/png":       32 * 1024 * 1024,
	"application/zip": 256 * 1024 * 1024,
	"video/mp4":       256 * 1024 * 1024,
	"audio/wav":       64 * 1024 * 1024,
}

// Worker is one configured Engine process on one device.
type Worker struct {
	// WorkerID names the worker in jobs, manifests and the API; a legacy single worker is "default".
	WorkerID  string
	URL       string
	TokenEnv  string
	Command   []string
	BackendID string
	// Device is the device the operator expects; informational until the probe confirms it.
	Device string
	// MemoryBudgetMB is the operator's per-worker budget (0 when unset); informational.
	MemoryBudgetMB int
	// Remote is an https:// worker: spoken to over mutual TLS and never owned.
	Remote bool
	// TLS is the remote worker's client certificate, CA and optional pin; nil for loopback HTTP.
	TLS *WorkerTLS
	// RemoteTimeoutMS is a remote worker's request timeout (0 uses worker_timeout_ms).
	RemoteTimeoutMS int
}

// WorkerTLS is the material for one remote worker: the CA its server certificate must chain to,
// the runtime's client certificate and key, and an optional SHA-256 of the server certificate's
// DER (lowercase hex). Without a pin the fingerprint seen on first contact is recorded and
// required on every later connection.
type WorkerTLS struct {
	CAFile       string
	CertFile     string
	KeyFile      string
	PinnedSHA256 string
}

// ServerTLS is the runtime API's own certificate and key, required on a non-loopback bind.
type ServerTLS struct {
	CertFile string
	KeyFile  string
}

// DefaultWorkerID is the ID a legacy single `worker` entry receives.
const DefaultWorkerID = "default"

type Config struct {
	Port int
	// Bind is the API's listen address, an IP address (default 127.0.0.1).
	Bind string
	// TLS serves the API over HTTPS; Parse requires it when Bind is not loopback.
	TLS *ServerTLS
	// AllowedOrigins are the browser origins the API accepts; empty refuses every Origin.
	AllowedOrigins      []string
	Directory           string
	QueueCapacity       int
	CancellationGraceMS int
	PollIntervalMS      int
	WorkerTimeoutMS     int
	// MaxOutputBytes is the per-MIME limit for one committed output.
	MaxOutputBytes map[string]int
	MaxInputBytes  int
	// OutputRetentionDays is nil for unlimited retention.
	OutputRetentionDays *float64
	Credentials         []Credential
	// ArtifactCatalog maps opaque handles to absolute directories, in file order.
	ArtifactCatalog *jsonv.Object
	// Worker is the legacy single worker; Parse sets it to the first pool entry. A configuration
	// built in process with only Worker is a pool of one (see Pool).
	Worker  Worker
	Workers []Worker
	// StrictDevices refuses to start when two workers report one device identity; Parse
	// defaults it to true. Otherwise both workers are marked unavailable.
	StrictDevices bool
	// AllowReordering lets a later queued job run on a free worker the FIFO head cannot use.
	AllowReordering bool
	Tokens          []Token
}

// Pool is the configured workers in order: Workers, or the single legacy Worker as "default".
func (cfg *Config) Pool() []Worker {
	if len(cfg.Workers) > 0 {
		return cfg.Workers
	}
	w := cfg.Worker
	if w.WorkerID == "" {
		w.WorkerID = DefaultWorkerID
	}
	return []Worker{w}
}

// DefaultOutputLimits is a fresh copy of the default per-MIME output limits.
func DefaultOutputLimits() map[string]int {
	limits := map[string]int{}
	for mime, limit := range defaultOutputBytes {
		limits[mime] = limit
	}
	return limits
}

// OutputLimit is the size limit for one output of a MIME type (0 for an unknown type).
func (cfg *Config) OutputLimit(mime string) int { return cfg.MaxOutputBytes[mime] }

// Catalog returns the configured directory for a handle, or "".
func (cfg *Config) Catalog(handle string) string { return cfg.ArtifactCatalog.Str(handle) }

var positive = c.Num(c.NumOpts{Int: true, GreaterThan: c.F(0)})

func def(v any) func() any { return func() any { return v } }

// nonEmpty is a non-empty string, such as a file path.
var nonEmpty = c.Str(c.StrOpts{Min: 1})

var schema = c.StrictObject(
	c.Field{Name: "port", Check: c.Num(c.NumOpts{Int: true, Min: c.F(0), Max: c.F(65535)}), Default: def(7810.0)},
	c.Field{Name: "bind", Check: c.Str(c.StrOpts{Refine: func(s string) bool { return net.ParseIP(s) != nil }}), Default: def(DefaultBind)},
	c.Field{Name: "tls", Check: c.StrictObject(
		c.Field{Name: "cert_file", Check: nonEmpty},
		c.Field{Name: "key_file", Check: nonEmpty},
	), Optional: true},
	c.Field{Name: "allowed_origins", Check: c.Array(c.Str(c.StrOpts{Refine: func(s string) bool { _, ok := NormalizeOrigin(s); return ok }}), c.ArrOpts{}), Default: func() any { return []any{} }},
	c.Field{Name: "directory", Check: c.Str(c.StrOpts{}), Default: def(".runtime")},
	c.Field{Name: "queue_capacity", Check: positive, Default: def(32.0)},
	c.Field{Name: "cancellation_grace_ms", Check: positive, Default: def(10000.0)},
	c.Field{Name: "poll_interval_ms", Check: positive, Default: def(250.0)},
	c.Field{Name: "worker_timeout_ms", Check: positive, Default: def(30000.0)},
	c.Field{Name: "max_output_bytes", Check: outputLimits, Optional: true},
	c.Field{Name: "max_input_bytes", Check: positive, Default: def(float64(32 * 1024 * 1024))},
	c.Field{Name: "output_retention_days", Check: c.Nullable(c.Num(c.NumOpts{GreaterThan: c.F(0)})), Default: def(nil)},
	c.Field{Name: "credentials", Check: c.Array(c.StrictObject(
		c.Field{Name: "token_env", Check: c.Str(c.StrOpts{})},
		c.Field{Name: "namespace", Check: c.ID},
		c.Field{Name: "operator", Check: c.Bool(), Default: def(false)},
		c.Field{Name: "allowed_namespaces", Check: c.Array(c.Str(c.StrOpts{Min: 1}), c.ArrOpts{}), Default: func() any { return []any{} }},
		c.Field{Name: "allow_fallbacks", Check: c.Bool(), Default: def(false)},
	), c.ArrOpts{Min: 1})},
	c.Field{Name: "artifact_catalog", Check: c.Record(c.Str(c.StrOpts{Refine: filepath.IsAbs}))},
	c.Field{Name: "worker", Check: workerEntry(false), Optional: true},
	c.Field{Name: "workers", Check: c.Array(workerEntry(true), c.ArrOpts{Min: 1}), Optional: true},
	c.Field{Name: "strict_devices", Check: c.Bool(), Default: def(true)},
	c.Field{Name: "allow_reordering", Check: c.Bool(), Default: def(false)},
)

// workerEntry is one worker's fields; a pool entry must name its worker_id, the legacy single
// worker may not (it is "default").
func workerEntry(pool bool) c.Check {
	fields := []c.Field{}
	if pool {
		fields = append(fields, c.Field{Name: "worker_id", Check: c.ID})
	}
	return c.StrictObject(append(fields,
		c.Field{Name: "url", Check: c.Str(c.StrOpts{Refine: func(s string) bool { u, err := url.Parse(s); return err == nil && u.Scheme != "" && u.Host != "" }}), Default: def("http://127.0.0.1:7811")},
		c.Field{Name: "token_env", Check: c.Str(c.StrOpts{}), Default: def("MEDIA_ENGINE_TOKEN")},
		c.Field{Name: "command", Check: c.Array(c.Str(c.StrOpts{}), c.ArrOpts{Min: 1}), Optional: true},
		c.Field{Name: "backend_id", Check: c.Enum(c.BACKENDS...), Optional: true},
		c.Field{Name: "device", Check: c.Str(c.StrOpts{Min: 1, Max: 80}), Optional: true},
		c.Field{Name: "memory_budget_mb", Check: positive, Optional: true},
		c.Field{Name: "tls", Check: c.StrictObject(
			c.Field{Name: "ca_file", Check: nonEmpty},
			c.Field{Name: "cert_file", Check: nonEmpty},
			c.Field{Name: "key_file", Check: nonEmpty},
			c.Field{Name: "pinned_sha256", Check: c.Str(c.StrOpts{Pattern: fingerprintPattern}), Optional: true},
		), Optional: true},
		c.Field{Name: "remote_timeout_ms", Check: positive, Optional: true},
	)...)
}

// DefaultBind is the API's default listen address.
const DefaultBind = "127.0.0.1"

var fingerprintPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// IsLoopback reports whether an address (an IP, or a URL host name) stays on this host.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NormalizeOrigin reads a browser origin (scheme://host[:port], http or https, nothing else) in
// the lowercase form browsers send.
func NormalizeOrigin(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), true
}

// absolute resolves a configured file path against the working directory.
func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// outputLimits accepts the 0.4.0 number (the PNG limit) or a map of the known MIME types.
var outputLimits c.Check = func(v any, path []string) (any, error) {
	if _, ok := v.(float64); ok {
		return positive(v, path)
	}
	fields := []c.Field{}
	for _, mime := range OutputMIMEs {
		fields = append(fields, c.Field{Name: mime, Check: positive, Optional: true})
	}
	return c.StrictObject(fields...)(v, path)
}

// workers reads the pool: `workers`, or the legacy single `worker` as a pool of one named
// "default". Worker IDs, configured devices and external URLs must each be distinct.
func workers(parsed *jsonv.Object, cfg *Config) error {
	var entries []any
	switch {
	case parsed.Has("worker") && parsed.Has("workers"):
		return errors.New("Configure either worker or workers, not both")
	case parsed.Has("workers"):
		entries = parsed.Arr("workers")
	case parsed.Has("worker"):
		entries = []any{parsed.Obj("worker").Clone().Set("worker_id", DefaultWorkerID)}
	default:
		return errors.New("Invalid configuration field workers")
	}
	ids, devices, urls := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, raw := range entries {
		o := raw.(*jsonv.Object)
		w := Worker{WorkerID: o.Str("worker_id"), URL: o.Str("url"), TokenEnv: o.Str("token_env"), BackendID: o.Str("backend_id"),
			Device: o.Str("device"), MemoryBudgetMB: num(o, "memory_budget_mb")}
		for _, part := range o.Arr("command") {
			w.Command = append(w.Command, part.(string))
		}
		u, _ := url.Parse(w.URL)
		host := strings.ToLower(u.Hostname())
		if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "/" && u.Path != "") ||
			(u.Scheme != "https" && (u.Scheme != "http" || (host != "127.0.0.1" && host != "localhost" && host != "::1"))) {
			return errors.New("Worker must use an authenticated loopback HTTP URL or an https:// URL with tls")
		}
		w.Remote = u.Scheme == "https"
		label := "Worker " + w.WorkerID
		if !w.Remote {
			if o.Has("tls") || o.Has("remote_timeout_ms") {
				return fmt.Errorf("%s: tls and remote_timeout_ms need an https:// worker URL", label)
			}
		} else {
			// A remote worker is never owned: nothing here may start, stop or signal it.
			if len(w.Command) > 0 {
				return fmt.Errorf("%s: a remote https:// worker cannot have a command; the runtime never owns a remote process", label)
			}
			if !o.Has("tls") {
				return fmt.Errorf("%s: an https:// worker needs tls with ca_file, cert_file and key_file", label)
			}
			t := o.Obj("tls")
			w.TLS = &WorkerTLS{CAFile: absolute(t.Str("ca_file")), CertFile: absolute(t.Str("cert_file")), KeyFile: absolute(t.Str("key_file")),
				PinnedSHA256: strings.ToLower(t.Str("pinned_sha256"))}
			w.RemoteTimeoutMS = num(o, "remote_timeout_ms")
		}
		if ids[w.WorkerID] {
			return fmt.Errorf("Worker IDs must be unique: %s", w.WorkerID)
		}
		ids[w.WorkerID] = true
		if w.Device != "" {
			if devices[w.Device] {
				return fmt.Errorf("Device %s is listed for more than one worker", w.Device)
			}
			devices[w.Device] = true
		}
		// An owned worker announces its own port; an external one is reached at its URL.
		if len(w.Command) == 0 {
			address := strings.ToLower(strings.TrimSuffix(w.URL, "/"))
			if urls[address] {
				return fmt.Errorf("Workers must use distinct URLs: %s", w.URL)
			}
			urls[address] = true
		}
		cfg.Workers = append(cfg.Workers, w)
	}
	cfg.Worker = cfg.Workers[0]
	return nil
}

func num(o *jsonv.Object, k string) int { f, _ := o.Num(k); return int(f) }

// CheckBind refuses a plain-HTTP listener on a non-loopback address: network mode is TLS only.
func CheckBind(bind string, tls *ServerTLS) error {
	if bind == "" {
		bind = DefaultBind
	}
	if net.ParseIP(bind) == nil {
		return fmt.Errorf("bind must be an IP address such as 127.0.0.1 or 0.0.0.0, not %q", bind)
	}
	if !IsLoopback(bind) && tls == nil {
		return fmt.Errorf("Refusing to listen on non-loopback address %s without tls (cert_file and key_file): the runtime never serves plain HTTP beyond loopback", bind)
	}
	return nil
}

// Read loads the file named by the argument, $MEDIA_RUNTIME_CONFIG or runtime.config.json.
func Read(file string) (*Config, error) {
	if file == "" {
		file = os.Getenv("MEDIA_RUNTIME_CONFIG")
	}
	if file == "" {
		file = "runtime.config.json"
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	raw, err := jsonv.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("Configuration is not valid JSON: %w", err)
	}
	return Parse(raw, os.Getenv)
}

// Parse validates a decoded configuration and resolves credentials from the environment.
func Parse(raw any, env func(string) string) (*Config, error) {
	parsed, err := c.Validate(schema, raw)
	if err != nil {
		var api *c.ApiError
		if errors.As(err, &api) && api.Field != "" {
			return nil, fmt.Errorf("Invalid configuration field %s", api.Field)
		}
		return nil, errors.New("Invalid configuration")
	}
	cfg := &Config{
		Port:                num(parsed, "port"),
		Bind:                parsed.Str("bind"),
		Directory:           parsed.Str("directory"),
		QueueCapacity:       num(parsed, "queue_capacity"),
		CancellationGraceMS: num(parsed, "cancellation_grace_ms"),
		PollIntervalMS:      num(parsed, "poll_interval_ms"),
		WorkerTimeoutMS:     num(parsed, "worker_timeout_ms"),
		MaxOutputBytes:      DefaultOutputLimits(),
		MaxInputBytes:       num(parsed, "max_input_bytes"),
		ArtifactCatalog:     parsed.Obj("artifact_catalog"),
		StrictDevices:       parsed.Bool("strict_devices"),
		AllowReordering:     parsed.Bool("allow_reordering"),
	}
	switch limits := parsed.Get("max_output_bytes").(type) {
	case float64:
		cfg.MaxOutputBytes["image/png"] = int(limits)
	case *jsonv.Object:
		for _, mime := range limits.Keys() {
			cfg.MaxOutputBytes[mime] = num(limits, mime)
		}
	}
	if days, ok := parsed.Num("output_retention_days"); ok {
		cfg.OutputRetentionDays = &days
	}
	if err := workers(parsed, cfg); err != nil {
		return nil, err
	}
	if parsed.Has("tls") {
		t := parsed.Obj("tls")
		cfg.TLS = &ServerTLS{CertFile: absolute(t.Str("cert_file")), KeyFile: absolute(t.Str("key_file"))}
	}
	if err := CheckBind(cfg.Bind, cfg.TLS); err != nil {
		return nil, err
	}
	for _, raw := range parsed.Arr("allowed_origins") {
		origin, _ := NormalizeOrigin(raw.(string))
		cfg.AllowedOrigins = append(cfg.AllowedOrigins, origin)
	}
	seen := map[string]bool{}
	for _, raw := range parsed.Arr("credentials") {
		o := raw.(*jsonv.Object)
		cred := Credential{TokenEnv: o.Str("token_env"), Namespace: o.Str("namespace"), Operator: o.Bool("operator"), AllowFallbacks: o.Bool("allow_fallbacks")}
		for _, n := range o.Arr("allowed_namespaces") {
			cred.AllowedNamespaces = append(cred.AllowedNamespaces, n.(string))
		}
		cfg.Credentials = append(cfg.Credentials, cred)
		token := env(cred.TokenEnv)
		if len(token) < 32 {
			return nil, fmt.Errorf("Set %s to at least 32 random characters", cred.TokenEnv)
		}
		if seen[token] {
			return nil, errors.New("Credentials must be unique")
		}
		seen[token] = true
		cfg.Tokens = append(cfg.Tokens, Token{Token: token, Namespace: cred.Namespace, Operator: cred.Operator, AllowedNamespaces: cred.AllowedNamespaces, AllowFallbacks: cred.AllowFallbacks})
	}
	abs, err := filepath.Abs(cfg.Directory)
	if err != nil {
		return nil, err
	}
	cfg.Directory = abs
	return cfg, nil
}
