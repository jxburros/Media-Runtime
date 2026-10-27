package main

import (
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func obj(t *testing.T, text string) *jsonv.Object {
	t.Helper()
	v, err := jsonv.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsonv.Object)
}

func contains(lines []string, text string) bool {
	for _, l := range lines {
		if strings.Contains(l, text) {
			return true
		}
	}
	return false
}

func TestDoctorFlagsUnavailableWorkersAndMissingModels(t *testing.T) {
	lines, healthy := ExplainDoctor(obj(t, `{"runtime_version":"0.2.0","protocols":["media-worker/1.0","media-worker/1.1"],"worker":{"error":"backend_unavailable","message":"Worker is unavailable"},"models":[],"recipes":[{"recipe_id":"portrait","pipeline":"text-to-image"}],"storage":{"writable":true,"available_bytes":10737418240,"output_retention_days":null},"queue":{"capacity":32,"queued":0,"running":0}}`))
	if healthy || !contains(lines, "UNAVAILABLE") {
		t.Fatal(lines)
	}
	lines, healthy = ExplainDoctor(obj(t, `{"runtime_version":"0.2.0","protocols":["media-worker/1.0","media-worker/1.1"],"worker":{"backend_id":"diffusers-sdxl","ownership":"reference","worker_version":"0.2.0","protocol":"media-worker/1.1","owned_process":true,"device":"cuda:0","device_ready":true,"state":"ready","tasks":["text-to-image","image-to-image"],"missing_dependencies":[],"dependencies":{"torch":"2.6.0"},"load_defaults":{"precision":"float16"},"precisions":["float16","float32"],"memory_budget_mb":null,"memory_estimate":{"value_mb":6500,"precision":"float16","source":"safetensors_headers","uncertainty":"excludes activations"}},"models":[{"model_id":"studio-sdxl","artifact_handle":"sdxl-base","available":true,"missing_roles":[],"loaded":true}],"recipes":[{"recipe_id":"portrait","pipeline":"text-to-image"}],"storage":{"writable":true,"available_bytes":10737418240,"output_retention_days":7},"queue":{"capacity":32,"queued":0,"running":0}}`))
	if !healthy || !contains(lines, "6500 MiB parameters at float16") {
		t.Fatal(lines)
	}
	// Produced by explainDoctor() of the TypeScript 0.3.0 release for the same report.
	want := []string{
		"Media Runtime 0.2.0; protocols media-worker/1.0, media-worker/1.1",
		"Worker: diffusers-sdxl 0.2.0 (reference stage), negotiated media-worker/1.1, owned child process",
		"  Device cuda:0: ready; service state ready; tasks text-to-image, image-to-image",
		"  Dependencies: torch=2.6.0",
		`  Load defaults: {"precision":"float16"}; precisions float16/float32; memory budget not set MiB`,
		"  Memory estimate: 6500 MiB parameters at float16 (source: safetensors_headers; excludes activations)",
		"Model studio-sdxl (sdxl-base): all registered files present; loaded in the worker",
		"Recipes: portrait (text-to-image)",
		"Storage: writable, 10.0 GiB free; output retention 7 days",
		"Queue: 0 running, 0 queued, capacity 32",
		"Result: ready to generate.",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s", strings.Join(lines, "\n"))
	}
	catalog := ExplainCatalog([]any{obj(t, `{"artifact_handle":"sdxl-base","complete":false,"present":true,"file_count":3,"total_bytes":0,"problems":["Missing or weightless components: unet"]}`)})
	if !strings.Contains(catalog[0], "NOT READY") || !strings.Contains(catalog[1], "unet") {
		t.Fatal(catalog)
	}
}
