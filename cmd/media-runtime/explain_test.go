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

func TestDoctorPrintsOneSectionPerWorker(t *testing.T) {
	report := obj(t, `{"runtime_version":"0.7.0","protocols":["media-worker/1.2"],
		"worker":{"worker_id":"gpu0","backend_id":"diffusers-sdxl","ownership":"reference","worker_version":"0.4.0","protocol":"media-worker/1.2","device":"cuda:0","device_ready":true,"state":"ready","tasks":["text-to-image"],"missing_dependencies":[],"dependencies":{},"precisions":["float16"],"memory_budget_mb":null,"memory_estimate":{}},
		"workers":[
		  {"worker_id":"gpu0","backend_id":"diffusers-sdxl","ownership":"reference","worker_version":"0.4.0","protocol":"media-worker/1.2","device":"cuda:0","device_ready":true,"state":"ready","tasks":["text-to-image"],"missing_dependencies":[],"dependencies":{},"precisions":["float16"],"memory_budget_mb":null,"memory_estimate":{},
		   "device_identity":{"name":"RTX 4090","device":"cuda:0","total_memory_mb":24564,"host_id":"0123456789abcdef","pid":11}},
		  {"worker_id":"gpu1","error":"backend_unavailable","message":"Workers gpu1 and gpu0 report the same device identity"}],
		"models":[{"model_id":"studio-sdxl","artifact_handle":"sdxl-base","available":true,"missing_roles":[],"loaded":true,"loaded_on":["gpu0"]}],
		"recipes":[],"storage":{"writable":true,"available_bytes":1073741824,"output_retention_days":null},"queue":{"capacity":32,"queued":0,"running":0}}`)
	lines, healthy := ExplainDoctor(report)
	if healthy {
		t.Fatal("an unavailable pool worker needs attention")
	}
	for _, want := range []string{
		"Worker gpu0: diffusers-sdxl 0.4.0 (reference stage), negotiated media-worker/1.2, externally managed",
		"  Device identity: RTX 4090 (cuda:0), 24564 MiB, host 0123456789ab, pid 11",
		"Worker gpu1: UNAVAILABLE (backend_unavailable) — Workers gpu1 and gpu0 report the same device identity",
		"  Start Media Engine (or set workers[].command) and check its token and port.",
		"Model studio-sdxl (sdxl-base): all registered files present; loaded in worker gpu0",
	} {
		if !contains(lines, want) {
			t.Fatalf("missing %q in\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if contains(lines, "Worker: ") {
		t.Fatal("a pool has no single worker section")
	}
}

func TestDoctorReportsTransportPerWorker(t *testing.T) {
	report := obj(t, `{"runtime_version":"0.8.0","protocols":["media-worker/1.2"],
		"worker":{"worker_id":"local"},
		"workers":[
		  {"worker_id":"local","backend_id":"diffusers-sdxl","ownership":"reference","worker_version":"0.5.0","protocol":"media-worker/1.2","owned_process":true,"device":"cuda:0","device_ready":true,"state":"ready","tasks":["text-to-image"],"missing_dependencies":[],"dependencies":{},"precisions":["float16"],"memory_budget_mb":null,"memory_estimate":{},
		   "transport":"http","remote":false,"latency_ms":0.84},
		  {"worker_id":"far","backend_id":"diffusers-sdxl","ownership":"reference","worker_version":"0.5.0","protocol":"media-worker/1.2","owned_process":false,"device":"cuda:0","device_ready":true,"state":"ready","tasks":["text-to-image"],"missing_dependencies":[],"dependencies":{},"precisions":["float16"],"memory_budget_mb":null,"memory_estimate":{},
		   "transport":"mtls","remote":true,"certificate_sha256":"ab12","certificate_source":"pinned","presented_sha256":"ab12","latency_ms":12.5},
		  {"worker_id":"moved","error":"certificate_mismatch","message":"Worker moved presented certificate sha256 cd34","owned_process":false,"ready":false,
		   "transport":"mtls","remote":true,"certificate_sha256":"ab12","certificate_source":"first_contact","presented_sha256":"cd34","latency_ms":null}],
		"models":[],"recipes":[],"storage":{"writable":true,"available_bytes":1073741824,"output_retention_days":null},"queue":{"capacity":32,"queued":0,"running":0}}`)
	lines, healthy := ExplainDoctor(report)
	if healthy {
		t.Fatal("a refused worker needs attention")
	}
	for _, want := range []string{
		"Worker local: diffusers-sdxl 0.5.0 (reference stage), negotiated media-worker/1.2, owned child process",
		"  Transport: loopback HTTP; latency 0.84 ms",
		"Worker far: diffusers-sdxl 0.5.0 (reference stage), negotiated media-worker/1.2, remote, never owned",
		"  Transport: mutual TLS; certificate sha256 ab12 (pinned_sha256); latency 12.5 ms",
		"Worker moved: UNAVAILABLE (certificate_mismatch) — Worker moved presented certificate sha256 cd34",
		"  Check the remote worker's network mode, URL, TLS files and token; the runtime never starts or stops it.",
		"  Presented certificate sha256 cd34; expected ab12 (recorded on first contact).",
		"  Transport: mutual TLS; certificate sha256 ab12 (recorded on first contact); latency not measured",
	} {
		if !contains(lines, want) {
			t.Fatalf("missing %q in\n%s", want, strings.Join(lines, "\n"))
		}
	}
}
