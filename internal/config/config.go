// Package config reads and validates runtime.config.json.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

type Worker struct {
	URL       string
	TokenEnv  string
	Command   []string
	BackendID string
}

type Config struct {
	Port                int
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
	Worker          Worker
	Tokens          []Token
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

var schema = c.StrictObject(
	c.Field{Name: "port", Check: c.Num(c.NumOpts{Int: true, Min: c.F(0), Max: c.F(65535)}), Default: def(7810.0)},
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
	c.Field{Name: "worker", Check: c.StrictObject(
		c.Field{Name: "url", Check: c.Str(c.StrOpts{Refine: func(s string) bool { u, err := url.Parse(s); return err == nil && u.Scheme != "" && u.Host != "" }}), Default: def("http://127.0.0.1:7811")},
		c.Field{Name: "token_env", Check: c.Str(c.StrOpts{}), Default: def("MEDIA_ENGINE_TOKEN")},
		c.Field{Name: "command", Check: c.Array(c.Str(c.StrOpts{}), c.ArrOpts{Min: 1}), Optional: true},
		c.Field{Name: "backend_id", Check: c.Enum(c.BACKENDS...), Optional: true},
	)},
)

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
	num := func(o *jsonv.Object, k string) int { f, _ := o.Num(k); return int(f) }
	w := parsed.Obj("worker")
	cfg := &Config{
		Port:                num(parsed, "port"),
		Directory:           parsed.Str("directory"),
		QueueCapacity:       num(parsed, "queue_capacity"),
		CancellationGraceMS: num(parsed, "cancellation_grace_ms"),
		PollIntervalMS:      num(parsed, "poll_interval_ms"),
		WorkerTimeoutMS:     num(parsed, "worker_timeout_ms"),
		MaxOutputBytes:      DefaultOutputLimits(),
		MaxInputBytes:       num(parsed, "max_input_bytes"),
		ArtifactCatalog:     parsed.Obj("artifact_catalog"),
		Worker:              Worker{URL: w.Str("url"), TokenEnv: w.Str("token_env"), BackendID: w.Str("backend_id")},
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
	for _, part := range w.Arr("command") {
		cfg.Worker.Command = append(cfg.Worker.Command, part.(string))
	}
	u, _ := url.Parse(cfg.Worker.URL)
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "http" || (host != "127.0.0.1" && host != "localhost" && host != "::1") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "/" && u.Path != "") {
		return nil, errors.New("Worker must use an authenticated loopback HTTP URL")
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
