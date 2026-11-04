package integration

import (
	"os"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestDAGForeachExecutesExpandedGraph(t *testing.T) {
	e := bootLinear(t)
	recipe := portrait4k().Set("schema_version", 3.0).Set("recipe_id", "batch-upscale")
	generate, enlarge := operation(recipe, 0), operation(recipe, 1)
	generate.Obj("defaults").Set("count", 3.0)
	enlarge.Set("inputs", jsonv.NewObject()).Set("foreach", jsonv.O("from", "op:generate/image"))
	// Forward declaration order exercises topological planning, not just a linear recipe.
	recipe.Set("operations", []any{enlarge, generate})
	must[any](t)(e.client.JSON("/v1/recipes", "POST", recipe))
	request := jsonv.O("recipe_id", "batch-upscale", "model_id", "fixture", "models", jsonv.O("enlarge", "upscaler"), "inputs", jsonv.O("prompt", "three lighthouses"), "parameters", jsonv.O("seed", 7.0))
	job := e.wait(must[*jsonv.Object](t)(e.client.Submit(request, "")).Str("job_id"), nil)
	equal(t, job.Str("state"), "succeeded", job.Get("error"))
	equal(t, len(job.Arr("operations")), 4)
	equal(t, len(job.Arr("outputs")), 3)
	for i := 1; i < 4; i++ {
		equal(t, operation(job, i).Str("source_op_id"), "enlarge")
	}
	manifest := e.rt.Store.Get(job.Str("job_id"), "").Obj("manifest")
	equal(t, manifest.Get("schema_version"), 6.0)
	equal(t, len(manifest.Obj("recipe").Arr("source_operations")), 2)
}

func TestIntermediateCacheIsOptInScopedAndVerified(t *testing.T) {
	e := bootLinear(t)
	request := jsonv.O("recipe_id", "portrait-4k", "model_id", "fixture", "models", jsonv.O("enlarge", "upscaler"), "inputs", jsonv.O("prompt", "cached lighthouse"), "parameters", jsonv.O("seed", 7.0), "reuse_intermediates", true)
	run := func(request *jsonv.Object) *jsonv.Object {
		t.Helper()
		job := e.wait(must[*jsonv.Object](t)(e.client.Submit(request, "")).Str("job_id"), nil)
		equal(t, job.Str("state"), "succeeded", job.Get("error"))
		return job
	}
	first := run(request)
	second := run(request)
	cache := operation(second, 0).Obj("cache")
	if cache == nil {
		t.Fatal("identical intermediate was executed instead of reused")
	}
	equal(t, cache.Str("source_job_id"), first.Str("job_id"))
	equal(t, operation(second, 0).Str("execution_id"), "")
	if operation(second, 1).Str("execution_id") == "" {
		t.Fatal("final operation must execute")
	}
	equal(t, operation(first, 0).Arr("outputs")[0].(*jsonv.Object).Str("sha256"), operation(second, 0).Arr("outputs")[0].(*jsonv.Object).Str("sha256"))
	changed := run(request.Clone().Set("parameters", jsonv.O("seed", 8.0)))
	if operation(changed, 0).Obj("cache") != nil {
		t.Fatal("changed seed reused an intermediate")
	}
	disabled := run(request.Clone().Set("reuse_intermediates", false))
	if operation(disabled, 0).Obj("cache") != nil {
		t.Fatal("cache used without explicit opt-in")
	}
	// A corrupted source must never be returned as successful cached output.
	for _, job := range []*jsonv.Object{first, second} {
		output := operation(e.rt.Store.Get(job.Str("job_id"), ""), 0).Arr("outputs")[0].(*jsonv.Object)
		if err := os.WriteFile(e.rt.OutputFile(job.Str("job_id"), output), []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	failed := e.wait(must[*jsonv.Object](t)(e.client.Submit(request, "")).Str("job_id"), nil)
	equal(t, failed.Str("state"), "failed")
	equal(t, failed.Obj("error").Str("code"), "output_unavailable")
}
