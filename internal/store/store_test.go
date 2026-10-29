package store

import (
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// A job written before 0.6.0 is read as one operation carrying its worker identities, so a job
// that was running at upgrade is reconciled through the same attempt.
func TestMigrateJobReadsOneOperation(t *testing.T) {
	raw, err := jsonv.Parse([]byte(`{"job_id":"job_1","state":"running","task":"upscale","recipe":{"recipe_id":"enlarge"},
		"request":{"inputs":{"image":"input_x"},"parameters":{"width":64}},"input":{"input_id":"input_x"},
		"model":{"model_id":"upscaler"},"adapters":[],"backend":{"backend_id":"diffusers-sdxl"},
		"attempt_id":"attempt_1","session_id":"s","execution_id":"e","dispatch_phase":"executing","worker_sequence":4,"outputs":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	job := migrateJob(raw.(*jsonv.Object))
	if n, _ := job.Num("current_operation"); n != 0 || len(job.Arr("operations")) != 1 {
		t.Fatalf("not one operation: %s", jsonv.MustMarshal(job))
	}
	op := job.Arr("operations")[0].(*jsonv.Object)
	for k, want := range map[string]any{"op_id": "enlarge", "task": "upscale", "state": "running", "attempt_id": "attempt_1", "execution_id": "e", "worker_sequence": 4.0} {
		if op.Get(k) != want {
			t.Fatalf("%s = %v", k, op.Get(k))
		}
	}
	if op.Obj("inputs").Str("image") != "input_x" || op.Obj("input").Str("input_id") != "input_x" {
		t.Fatal("inputs not carried")
	}
	if migrateJob(job) != job || len(job.Arr("operations")) != 1 {
		t.Fatal("migration is not idempotent")
	}
}

// A job written before 0.7.0 ran on the single worker, "default"; one never dispatched has none.
func TestMigrateJobAssignsTheLegacyWorker(t *testing.T) {
	for text, want := range map[string]any{
		`{"job_id":"j","state":"succeeded","recipe":{"recipe_id":"portrait"},"request":{},"attempt_id":"a"}`:                      "default",
		`{"job_id":"j","state":"queued","recipe":{"recipe_id":"portrait"},"request":{}}`:                                          nil,
		`{"job_id":"j","state":"running","operations":[{"op_id":"a","attempt_id":"x"},{"op_id":"b"}],"current_operation":1}`:      "default",
		`{"job_id":"j","state":"cancelled","operations":[{"op_id":"a"}],"current_operation":0}`:                                   nil,
		`{"job_id":"j","state":"running","operations":[{"op_id":"a","attempt_id":"x"}],"current_operation":0,"worker_id":"gpu1"}`: "gpu1",
	} {
		raw, err := jsonv.Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if job := migrateJob(raw.(*jsonv.Object)); !job.Has("worker_id") || job.Get("worker_id") != want {
			t.Fatalf("%s: worker_id %v, want %v", text, job.Get("worker_id"), want)
		}
	}
}
