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
