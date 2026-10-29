package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func env(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

var tokens = env(map[string]string{
	"MEDIA_RUNTIME_TOKEN": strings.Repeat("a", 32),
	"MEDIA_STUDIO_TOKEN":  strings.Repeat("b", 32),
})

func TestExampleConfigurationParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "runtime.config.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jsonv.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(raw, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 7810 || cfg.QueueCapacity != 32 || cfg.OutputRetentionDays != nil || cfg.Worker.BackendID != "diffusers-sdxl" ||
		len(cfg.Tokens) != 2 || cfg.Tokens[1].AllowedNamespaces[0] != "studio:*" || !filepath.IsAbs(cfg.Directory) {
		t.Fatalf("%+v", cfg)
	}
	if !cfg.Tokens[0].AllowFallbacks || cfg.Tokens[1].AllowFallbacks || cfg.OutputLimit("application/zip") != 268435456 {
		t.Fatalf("fallback grants or output limits: %+v", cfg)
	}
	if got := strings.Join(cfg.ArtifactCatalog.Keys(), ","); got != "sdxl-base,studio-style-lora,esrgan-x2" {
		t.Fatal(got)
	}
	if pool := cfg.Pool(); len(pool) != 1 || pool[0].WorkerID != "default" {
		t.Fatalf("the example's single worker: %+v", pool)
	}
	data, err = os.ReadFile(filepath.Join("..", "..", "examples", "runtime.pool.config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if raw, err = jsonv.Parse(data); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Parse(raw, tokens); err != nil {
		t.Fatal(err)
	}
	if pool := cfg.Pool(); len(pool) != 2 || pool[1].WorkerID != "gpu1" || pool[1].Device != "cuda:1" || pool[1].MemoryBudgetMB != 22000 ||
		len(pool[0].Command) == 0 || !cfg.StrictDevices || cfg.AllowReordering {
		t.Fatalf("the two-worker example: %+v", cfg)
	}
}

func TestRemoteExampleParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "runtime.remote.config.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jsonv.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(raw, tokens)
	if err != nil {
		t.Fatal(err)
	}
	pool := cfg.Pool()
	if cfg.Bind != "0.0.0.0" || cfg.TLS == nil || cfg.TLS.KeyFile != "/etc/media-runtime/runtime.key" || len(cfg.AllowedOrigins) != 0 ||
		len(pool) != 2 || pool[0].Remote || !pool[1].Remote || pool[1].TLS.CAFile != "/etc/media-runtime/workers-ca.pem" ||
		pool[1].TLS.PinnedSHA256 != strings.Repeat("0", 64) || pool[1].RemoteTimeoutMS != 60000 || len(pool[1].Command) != 0 {
		t.Fatalf("the remote example: %+v %+v", cfg, pool)
	}
}

// Remote workers speak mutual TLS and are never owned; a non-loopback API bind needs TLS.
func TestNetworkModeConfiguration(t *testing.T) {
	const creds = `"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner"}],"artifact_catalog":{}`
	const material = `"tls":{"ca_file":"/c/ca.pem","cert_file":"/c/client.pem","key_file":"/c/client.key"}`
	parse := func(text string) (*Config, error) {
		raw, err := jsonv.Parse([]byte("{" + creds + "," + text + "}"))
		if err != nil {
			t.Fatal(err)
		}
		return Parse(raw, tokens)
	}
	cfg, err := parse(`"workers":[{"worker_id":"far","url":"https://10.0.0.2:7811",` + material + `}]`)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Pool()[0]; !w.Remote || w.TLS.KeyFile != "/c/client.key" || w.TLS.PinnedSHA256 != "" || w.RemoteTimeoutMS != 0 {
		t.Fatalf("%+v", w)
	}
	if cfg.Bind != "127.0.0.1" || cfg.TLS != nil || len(cfg.AllowedOrigins) != 0 {
		t.Fatalf("API defaults: %+v", cfg)
	}
	pin := strings.Repeat("AB", 32)
	cfg, err = parse(`"worker":{"url":"https://render.internal:7811","remote_timeout_ms":90000,"tls":{"ca_file":"ca.pem","cert_file":"c.pem","key_file":"c.key","pinned_sha256":"` + pin + `"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Pool()[0]; w.TLS.PinnedSHA256 != strings.ToLower(pin) || w.RemoteTimeoutMS != 90000 || !filepath.IsAbs(w.TLS.CAFile) {
		t.Fatalf("%+v", w.TLS)
	}
	cfg, err = parse(`"bind":"0.0.0.0","tls":{"cert_file":"/s/runtime.pem","key_file":"/s/runtime.key"},"allowed_origins":["https://Studio.Example","http://localhost:5173"],"worker":{}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.CertFile != "/s/runtime.pem" || strings.Join(cfg.AllowedOrigins, " ") != "https://studio.example http://localhost:5173" {
		t.Fatalf("%+v", cfg)
	}
	for text, want := range map[string]string{
		`"workers":[{"worker_id":"far","url":"https://10.0.0.2:7811","command":["engine"],` + material + `}]`:                                            "cannot have a command",
		`"worker":{"url":"https://127.0.0.1:7811","command":["engine"],` + material + `}`:                                                                "never owns a remote process",
		`"workers":[{"worker_id":"far","url":"https://10.0.0.2:7811"}]`:                                                                                  "needs tls",
		`"workers":[{"worker_id":"near","url":"http://127.0.0.1:7811",` + material + `}]`:                                                                "need an https:// worker URL",
		`"workers":[{"worker_id":"near","url":"http://127.0.0.1:7811","remote_timeout_ms":1000}]`:                                                        "need an https:// worker URL",
		`"workers":[{"worker_id":"far","url":"https://10.0.0.2:7811/v1",` + material + `}]`:                                                              "https:// URL with tls",
		`"workers":[{"worker_id":"far","url":"ftp://10.0.0.2:7811",` + material + `}]`:                                                                   "https:// URL with tls",
		`"workers":[{"worker_id":"far","url":"https://10.0.0.2:7811","tls":{"ca_file":"/c/ca.pem","cert_file":"/c/client.pem"}}]`:                        "workers.0.tls.key_file",
		`"workers":[{"worker_id":"far","url":"https://10.0.0.2:7811","tls":{"ca_file":"a","cert_file":"b","key_file":"c","pinned_sha256":"sha256:00"}}]`: "workers.0.tls.pinned_sha256",
		`"workers":[{"worker_id":"far","url":"https://10.0.0.2:7811","remote_timeout_ms":0,` + material + `}]`:                                           "workers.0.remote_timeout_ms",
		`"bind":"0.0.0.0","worker":{}`:                              "Refusing to listen on non-loopback address 0.0.0.0 without tls",
		`"bind":"192.168.1.20","worker":{}`:                         "non-loopback",
		`"bind":"::","worker":{}`:                                   "non-loopback",
		`"bind":"runtime.local","worker":{}`:                        "Invalid configuration field bind",
		`"tls":{"cert_file":"a"},"worker":{}`:                       "tls.key_file",
		`"allowed_origins":["*"],"worker":{}`:                       "allowed_origins.0",
		`"allowed_origins":["https://studio.example/"],"worker":{}`: "allowed_origins.0",
	} {
		if _, err := parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", text, err, want)
		}
	}
	// Loopback binds stay plain HTTP; TLS there is accepted.
	for _, text := range []string{`"bind":"127.0.0.1","worker":{}`, `"bind":"::1","worker":{}`, `"bind":"127.0.0.2","tls":{"cert_file":"a","key_file":"b"},"worker":{}`} {
		if _, err := parse(text); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
	}
}

func TestConfigurationIsStrict(t *testing.T) {
	cases := map[string]string{
		`{"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner"}],"artifact_catalog":{},"worker":{"url":"http://10.0.0.2:7811"}}`:                     "loopback",
		`{"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner"}],"artifact_catalog":{},"worker":{},"extra":1}`:                                       "Invalid configuration",
		`{"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner"}],"artifact_catalog":{"x":"relative/path"},"worker":{}}`:                              "artifact_catalog.x",
		`{"credentials":[{"token_env":"UNSET","namespace":"owner"}],"artifact_catalog":{},"worker":{}}`:                                                               "Set UNSET",
		`{"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"a"},{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"b"}],"artifact_catalog":{},"worker":{}}`: "unique",
		`{"credentials":[],"artifact_catalog":{},"worker":{}}`:                                                                                                        "credentials",
	}
	for text, want := range cases {
		raw, _ := jsonv.Parse([]byte(text))
		if _, err := Parse(raw, tokens); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", text, err, want)
		}
	}
	raw, _ := jsonv.Parse([]byte(`{"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner"}],"artifact_catalog":{},"worker":{"url":"http://localhost:7811/"}}`))
	cfg, err := Parse(raw, tokens)
	if err != nil || cfg.Port != 7810 || cfg.Worker.TokenEnv != "MEDIA_ENGINE_TOKEN" || cfg.MaxOutputBytes["image/png"] != 33554432 || cfg.MaxOutputBytes["video/mp4"] != 268435456 {
		t.Fatalf("defaults: %v %+v", err, cfg)
	}
	raw, _ = jsonv.Parse([]byte(`{"max_output_bytes":1000,"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner"}],"artifact_catalog":{},"worker":{}}`))
	if cfg, err := Parse(raw, tokens); err != nil || cfg.OutputLimit("image/png") != 1000 || cfg.OutputLimit("audio/wav") != 67108864 {
		t.Fatalf("0.4.0 numeric output limit: %v %+v", err, cfg)
	}
}

func TestWorkerPoolConfiguration(t *testing.T) {
	const creds = `"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner"}],"artifact_catalog":{}`
	parse := func(text string) (*Config, error) {
		if text != "" {
			text = "," + text
		}
		raw, err := jsonv.Parse([]byte("{" + creds + text + "}"))
		if err != nil {
			t.Fatal(err)
		}
		return Parse(raw, tokens)
	}
	cfg, err := parse(`"workers":[{"worker_id":"gpu0","url":"http://127.0.0.1:7811","device":"cuda:0","memory_budget_mb":20000},
		{"worker_id":"gpu1","url":"http://127.0.0.1:7812","token_env":"GPU1_TOKEN","backend_id":"owned-sdxl","device":"cuda:1"}]`)
	if err != nil {
		t.Fatal(err)
	}
	pool := cfg.Pool()
	if len(pool) != 2 || pool[0].WorkerID != "gpu0" || pool[0].TokenEnv != "MEDIA_ENGINE_TOKEN" || pool[0].MemoryBudgetMB != 20000 ||
		pool[1].Device != "cuda:1" || pool[1].BackendID != "owned-sdxl" || pool[1].TokenEnv != "GPU1_TOKEN" || cfg.Worker.WorkerID != "gpu0" {
		t.Fatalf("%+v", pool)
	}
	if !cfg.StrictDevices || cfg.AllowReordering {
		t.Fatalf("defaults: strict %v reordering %v", cfg.StrictDevices, cfg.AllowReordering)
	}
	// The legacy single worker is a pool of one named "default", every field as before.
	cfg, err = parse(`"strict_devices":false,"allow_reordering":true,"worker":{"url":"http://127.0.0.1:7811","command":["engine","--port","0"],"backend_id":"diffusers-sdxl"}`)
	if err != nil {
		t.Fatal(err)
	}
	if pool := cfg.Pool(); len(pool) != 1 || pool[0].WorkerID != DefaultWorkerID || pool[0].Command[2] != "0" || pool[0].BackendID != "diffusers-sdxl" ||
		cfg.StrictDevices || !cfg.AllowReordering {
		t.Fatalf("legacy: %+v", cfg)
	}
	// A configuration built in process with only Worker is the same pool of one.
	if pool := (&Config{Worker: Worker{URL: "http://127.0.0.1:1"}}).Pool(); len(pool) != 1 || pool[0].WorkerID != "default" {
		t.Fatalf("%+v", pool)
	}
	for text, want := range map[string]string{
		`"workers":[]`: "workers",
		`"workers":[{"url":"http://127.0.0.1:7811"}]`: "workers.0.worker_id",
		`"workers":[{"worker_id":"a b"}]`:             "workers.0.worker_id",
		`"workers":[{"worker_id":"a","url":"http://127.0.0.1:1"},{"worker_id":"a","url":"http://127.0.0.1:2"}]`:                                     "Worker IDs must be unique",
		`"workers":[{"worker_id":"a"},{"worker_id":"b"}]`:                                                                                           "distinct URLs",
		`"workers":[{"worker_id":"a","url":"http://127.0.0.1:1","device":"cuda:0"},{"worker_id":"b","url":"http://127.0.0.1:2","device":"cuda:0"}]`: "Device cuda:0",
		`"workers":[{"worker_id":"a","url":"http://10.0.0.2:7811"}]`:                                                                                "loopback",
		`"workers":[{"worker_id":"a","memory_budget_mb":0}]`:                                                                                        "workers.0.memory_budget_mb",
		`"worker":{"worker_id":"x"}`:                "Invalid configuration",
		`"worker":{},"workers":[{"worker_id":"a"}]`: "not both",
		`"strict_devices":"yes","worker":{}`:        "strict_devices",
	} {
		if _, err := parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", text, err, want)
		}
	}
	// Owned workers announce their own ports, so they may share the default URL.
	if _, err := parse(`"workers":[{"worker_id":"a","command":["engine"]},{"worker_id":"b","command":["engine"]}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(""); err == nil || !strings.Contains(err.Error(), "workers") {
		t.Fatalf("a configuration without workers: %v", err)
	}
}
