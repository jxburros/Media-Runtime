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
