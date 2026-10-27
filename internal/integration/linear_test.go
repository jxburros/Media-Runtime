package integration

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

// portrait4k is the specification's example: generate, then enlarge the generated image.
func portrait4k() *jsonv.Object {
	return mustObject(`{
	  "schema_version": 2, "recipe_id": "portrait-4k", "revision": 1,
	  "inputs": { "prompt": { "type": "string", "required": true } },
	  "operations": [
	    { "op_id": "generate", "task": "text-to-image", "model_family": "sdxl-base",
	      "inputs": { "prompt": { "from": "input:prompt" } },
	      "defaults": { "width": 64, "height": 64, "steps": 3, "guidance": 6 } },
	    { "op_id": "enlarge", "task": "upscale", "model_family": "esrgan",
	      "inputs": { "image": { "from": "op:generate/image" } } }
	  ],
	  "publish": ["enlarge/image"]
	}`)
}

// refined generates and then refines with image-to-image, publishing both images.
func refined() *jsonv.Object {
	return mustObject(`{
	  "schema_version": 2, "recipe_id": "refined", "revision": 1,
	  "inputs": {
	    "prompt": { "type": "string" },
	    "refine_prompt": { "type": "string" },
	    "strength": { "type": "number", "required": false, "minimum": 0.1, "maximum": 1 },
	    "refine_steps": { "type": "number", "required": false }
	  },
	  "operations": [
	    { "op_id": "generate", "task": "text-to-image", "model_family": "sdxl-base",
	      "inputs": { "prompt": { "from": "input:prompt" } },
	      "defaults": { "width": 64, "height": 64, "steps": 3, "guidance": 6 } },
	    { "op_id": "refine", "task": "image-to-image", "model_family": "sdxl-base",
	      "inputs": { "prompt": { "from": "input:refine_prompt" }, "image": { "from": "op:generate/image" },
	                  "strength": { "from": "input:strength" }, "steps": { "from": "input:refine_steps" } },
	      "defaults": { "steps": 4, "strength": 0.5 } }
	  ],
	  "publish": ["generate/image", "refine/image"]
	}`)
}

func mustObject(text string) *jsonv.Object {
	v, err := jsonv.Parse([]byte(text))
	if err != nil {
		panic(err)
	}
	return v.(*jsonv.Object)
}

func bootLinear(t *testing.T) *env {
	t.Helper()
	e := boot12(t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	e.register(t, jsonv.O("model_id", "upscaler", "artifact_handle", "upscaler", "family", "esrgan"))
	for _, recipe := range []*jsonv.Object{portrait4k(), refined()} {
		if _, err := e.client.JSON("/v1/recipes", "POST", recipe); err != nil {
			t.Fatalf("register %s: %v", recipe.Str("recipe_id"), err)
		}
	}
	return e
}

func operation(job *jsonv.Object, i int) *jsonv.Object {
	return job.Arr("operations")[i].(*jsonv.Object)
}

func TestLinearRecipeRegistration(t *testing.T) {
	e := boot12(t)
	// Replacements of the second operation's inputs and the field each one is refused at.
	invalid := map[string]string{
		`{"image":{"from":"op:enlarge/image"}}`:                                   "operations.1.inputs.image.from",
		`{"image":{"from":"op:generate/video"}}`:                                  "operations.1.inputs.image.from",
		`{"image":{"from":"op:generate/image"},"prompt":{"from":"input:prompt"}}`: "operations.1.inputs.prompt",
		`{"image":{"from":"input:prompt"}}`:                                       "operations.1.inputs.image.from",
	}
	for replacement, field := range invalid {
		doc := portrait4k()
		operation(doc, 1).Set("inputs", mustObject(replacement))
		_, err := e.client.JSON("/v1/recipes", "POST", doc)
		rejects(t, err, 400, "invalid_request", field)
	}
	for _, mutate := range []struct {
		field string
		apply func(*jsonv.Object)
	}{
		{"operations.1.op_id", func(r *jsonv.Object) { operation(r, 1).Set("op_id", "generate") }},
		{"operations.1.model_family", func(r *jsonv.Object) { operation(r, 1).Set("model_family", "sdxl-base") }},
		{"publish.0", func(r *jsonv.Object) { r.Set("publish", []any{"missing/image"}) }},
		{"inputs.unused", func(r *jsonv.Object) { r.Obj("inputs").Set("unused", jsonv.O("type", "string")) }},
		{"inputs.prompt.type", func(r *jsonv.Object) { r.Obj("inputs").Set("prompt", jsonv.O("type", "number")) }},
		{"operations.0.op_id", func(r *jsonv.Object) { operation(r, 0).Set("op_id", "two__parts") }},
	} {
		doc := portrait4k()
		mutate.apply(doc)
		_, err := e.client.JSON("/v1/recipes", "POST", doc)
		rejects(t, err, 400, "invalid_request", mutate.field)
	}
	// An optional declaration cannot satisfy a task's required input.
	doc := portrait4k()
	doc.Obj("inputs").Obj("prompt").Set("required", false)
	_, err := e.client.JSON("/v1/recipes", "POST", doc)
	rejects(t, err, 400, "invalid_request", "operations.0.inputs.prompt.from")

	registered := must[any](t)(e.client.JSON("/v1/recipes", "POST", portrait4k())).(*jsonv.Object)
	equal(t, registered.Get("schema_version"), 2.0)
	detail := must[*jsonv.Object](t)(e.client.Recipe("portrait-4k"))
	equal(t, detail.Get("digest"), registered.Get("digest"))
	equal(t, get(detail, "inputs", "prompt", "type"), "string")
	equal(t, get(detail, "operations", "0", "op_id"), "generate")
	equal(t, len(detail.Arr("operations")), 2)
	equal(t, detail.Get("publish"), []any{"enlarge/image"})
	// Schema-1 recipes are returned as their one-operation form with typed declarations.
	variation := must[*jsonv.Object](t)(e.client.Recipe("variation"))
	equal(t, variation.Get("schema_version"), 1.0)
	equal(t, get(variation, "inputs", "image", "type"), "image")
	equal(t, get(variation, "inputs", "negative_prompt", "required"), false)
	equal(t, get(variation, "operations", "0", "task"), "image-to-image")
	equal(t, get(variation, "operations", "0", "inputs", "image", "from"), "input:image")
	equal(t, variation.Get("publish"), []any{"variation/image"})
	_, err = e.client.Recipe("absent")
	rejects(t, err, 404)
}

func TestLinearRecipeRunsEachOperation(t *testing.T) {
	e := bootLinear(t)
	ok := must[*jsonv.Object](t)
	s := jsonv.O("recipe_id", "portrait-4k", "model_id", "fixture", "models", jsonv.O("enlarge", "upscaler"),
		"inputs", jsonv.O("prompt", "a lighthouse"), "parameters", jsonv.O("seed", 7.0))

	t.Run("admission resolves every operation up front", func(t *testing.T) {
		_, err := e.client.Submit(jsonv.O("recipe_id", "portrait-4k", "model_id", "fixture", "inputs", jsonv.O("prompt", "x")), "")
		rejects(t, err, 400, "invalid_request", "models.enlarge")
		_, err = e.client.Submit(s.Clone().Set("models", jsonv.O("enlarge", "fixture")), "")
		rejects(t, err, 400, "unsupported", "operations.enlarge.model_id")
		_, err = e.client.Submit(s.Clone().Set("models", jsonv.O("enlarge", "upscaler", "generate", "fixture")), "")
		rejects(t, err, 400, "invalid_request", "models.generate")
		_, err = e.client.Submit(s.Clone().Set("models", jsonv.O("enlarge", "nobody")), "")
		rejects(t, err, 400, "unknown_model", "models.enlarge")
		_, err = e.client.Submit(s.Clone().Set("inputs", jsonv.O("prompt", "x", "colour", "red")), "")
		rejects(t, err, 400, "invalid_request", "inputs.colour")
		_, err = e.client.Submit(s.Clone().Set("inputs", jsonv.NewObject()), "")
		rejects(t, err, 400, "invalid_request", "inputs.prompt")
		_, err = e.client.Submit(jsonv.O("recipe_id", "portrait", "model_id", "fixture", "models", jsonv.O("x", "y"), "inputs", jsonv.O("prompt", "x")), "")
		rejects(t, err, 400, "invalid_request", "models")
		// The generated image is enlarged by 2; a limit below 128 refuses the second operation.
		e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
			if o, isObj := result.(*jsonv.Object); isObj && path == "/v1/capabilities" {
				o.Obj("limits").Set("output_side", 100.0)
			}
			return result, nil
		}, nil)
		_, err = e.client.Submit(s, "")
		e.rt.Worker.SetHooks(nil, nil)
		rejects(t, err, 400, "invalid_request", "operations.enlarge.output_side")
		plan := must[any](t)(e.client.Validate(s)).(*jsonv.Object)
		equal(t, get(plan, "operations", "0", "model_id"), "fixture")
		equal(t, plan.Arr("operations")[1].(*jsonv.Object).Get("model_id"), "upscaler")
		equal(t, get(plan.Arr("operations")[1].(*jsonv.Object), "parameters", "width"), 64.0)
		equal(t, get(plan.Arr("operations")[1].(*jsonv.Object), "input_from", "op_id"), "generate")
	})

	done := e.wait(ok(e.client.Submit(s, "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	equal(t, done.Get("current_op_id"), "enlarge")
	equal(t, len(done.Arr("operations")), 2)
	generate, enlarge := operation(done, 0), operation(done, 1)
	equal(t, generate.Str("state"), "succeeded")
	equal(t, enlarge.Str("state"), "succeeded")
	equal(t, get(generate, "model", "model_id"), "fixture")
	equal(t, get(enlarge, "model", "model_id"), "upscaler")
	equal(t, get(generate, "parameters", "seed"), 7.0)
	if generate.Str("execution_id") == "" || generate.Str("execution_id") == enlarge.Str("execution_id") ||
		generate.Str("attempt_id") == enlarge.Str("attempt_id") {
		t.Fatal("each operation runs as its own worker execution")
	}
	// Only the published output is under outputs; the intermediate is listed separately.
	equal(t, len(done.Arr("outputs")), 1)
	published := done.Arr("outputs")[0].(*jsonv.Object)
	equal(t, published.Get("output_id"), "image")
	equal(t, published.Get("op_id"), "enlarge")
	equal(t, published.Get("width"), 128.0)
	intermediate := done.Arr("intermediate_outputs")[0].(*jsonv.Object)
	equal(t, intermediate.Get("output_id"), "generate__image")
	equal(t, intermediate.Get("published"), false)
	equal(t, intermediate.Get("width"), 64.0)
	equal(t, intermediate.Get("seed"), 7.0)
	// The second operation received the committed intermediate as an uploaded worker input.
	if enlarge.Str("worker_input_id") == "" || get(enlarge, "input_from", "output_id") != "image" {
		t.Fatalf("enlarge was not handed the intermediate: %s", jsonv.MustMarshal(enlarge))
	}
	for _, record := range []*jsonv.Object{published, intermediate} {
		res, err := e.client.Output(done.Str("job_id"), record.Str("output_id"))
		if err != nil {
			t.Fatal(err)
		}
		bytes, _ := io.ReadAll(res.Body)
		res.Body.Close()
		equal(t, c.SHA256Hex(bytes), record.Str("sha256"))
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Directory, "outputs", done.Str("job_id"), "generate", "image.png")); err != nil {
		t.Fatal("intermediate is not stored under its operation's directory")
	}

	events := e.events(t, done.Str("job_id"))
	for _, want := range []string{`"op_id":"generate"`, `"op_id":"enlarge"`, `"operation_index":1`, `"operation_count":2`, `"stage":"upscaling"`} {
		if !strings.Contains(events, want) {
			t.Fatalf("events lack %s: %s", want, events)
		}
	}
	// The intermediate is committed (an output event) before the second operation loads.
	if strings.Index(events, `"output_id":"generate__image"`) > strings.Index(events, `"stage":"upscaling"`) {
		t.Fatal("the second operation started before the intermediate was committed")
	}
	// The worker was reloaded for the upscaler, and the model swap is visible as a loading stage.
	if strings.Count(events, `"stage":"loading"`) < 2 {
		t.Fatalf("no reload between operations: %s", events)
	}

	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("schema_version"), 4.0)
	equal(t, len(manifest.Arr("operations")), 2)
	equal(t, get(manifest.Arr("operations")[1].(*jsonv.Object), "scale"), 2.0)
	equal(t, len(manifest.Arr("outputs")), 1)
	equal(t, len(manifest.Arr("intermediate_outputs")), 1)

	t.Run("retry runs every operation again", func(t *testing.T) {
		retried := e.wait(ok(e.client.Submit(done.Obj("request").Clone().Set("retry_of", done.Get("job_id")), "")).Str("job_id"), nil)
		equal(t, retried.Str("state"), "succeeded", retried.Get("error"))
		equal(t, get(operation(retried, 0), "parameters", "seed"), 7.0)
		if operation(retried, 0).Str("execution_id") == generate.Str("execution_id") {
			t.Fatal("retry reused an execution")
		}
	})

	t.Run("published outputs that share an ID are prefixed by operation", func(t *testing.T) {
		job := e.wait(ok(e.client.Submit(jsonv.O("recipe_id", "refined", "model_id", "fixture", "models", jsonv.O("refine", "fixture"),
			"inputs", jsonv.O("prompt", "a", "refine_prompt", "b", "strength", 0.5)), "")).Str("job_id"), nil)
		equal(t, job.Str("state"), "succeeded", job.Get("error"))
		ids := []any{}
		for _, raw := range job.Arr("outputs") {
			ids = append(ids, raw.(*jsonv.Object).Get("output_id"))
		}
		equal(t, ids, []any{"generate__image", "refine__image"})
		equal(t, len(job.Arr("intermediate_outputs")), 0)
		equal(t, get(operation(job, 1), "parameters", "strength"), 0.5)
		// The same model and options: the second operation does not reload the worker.
		if strings.Count(e.events(t, job.Str("job_id")), `"stage":"loading"`) > 1 {
			t.Fatal("an identical load identity reloaded the worker")
		}
		_, err := e.client.Submit(jsonv.O("recipe_id", "refined", "model_id", "fixture", "models", jsonv.O("refine", "fixture"),
			"inputs", jsonv.O("prompt", "a", "refine_prompt", "b", "strength", 2.0)), "")
		rejects(t, err, 400, "invalid_request", "inputs.strength")
	})
}

func TestLinearRecipeCancellationFailureAndRestart(t *testing.T) {
	e := bootLinear(t)
	ok := must[*jsonv.Object](t)
	refine := func(prompt string, steps float64) *jsonv.Object {
		return jsonv.O("recipe_id", "refined", "model_id", "fixture", "models", jsonv.O("refine", "fixture"),
			"inputs", jsonv.O("prompt", "a", "refine_prompt", prompt, "strength", 1.0, "refine_steps", steps))
	}

	t.Run("a failed operation fails the job and names it", func(t *testing.T) {
		job := e.wait(ok(e.client.Submit(refine("fail", 4), "")).Str("job_id"), nil)
		equal(t, job.Str("state"), "failed")
		equal(t, get(job, "error", "code"), "execution_failed")
		equal(t, get(job, "error", "operation"), "refine")
		equal(t, operation(job, 0).Str("state"), "succeeded")
		equal(t, operation(job, 1).Str("state"), "failed")
		equal(t, len(job.Arr("outputs")), 0)
	})

	t.Run("cancellation between operations is honored before the next dispatch", func(t *testing.T) {
		var jobID string
		e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
			// The second operation's load: the first operation's outputs are already committed.
			if path == "/v1/model/load" && jobID != "" {
				if job := e.rt.Store.Get(jobID, ""); job != nil {
					if index, _ := job.Num("current_operation"); index == 1 {
						_, _ = e.rt.Cancel(jobID, "owner")
					}
				}
			}
			return result, nil
		}, nil)
		defer e.rt.Worker.SetHooks(nil, nil)
		// The upscaler is a different bundle, so the second operation of portrait-4k loads it.
		jobID = ok(e.client.Submit(jsonv.O("recipe_id", "portrait-4k", "model_id", "fixture", "models", jsonv.O("enlarge", "upscaler"),
			"inputs", jsonv.O("prompt", "c")), "")).Str("job_id")
		job := e.wait(jobID, nil)
		equal(t, job.Str("state"), "cancelled")
		equal(t, operation(job, 0).Str("state"), "succeeded")
		equal(t, operation(job, 1).Str("state"), "cancelled")
		equal(t, operation(job, 1).Str("execution_id"), "")
	})

	t.Run("a restart during operation two reconciles only that attempt", func(t *testing.T) {
		accepted := ok(e.client.Submit(refine("slow", 100), ""))
		active := e.wait(accepted.Str("job_id"), func(j *jsonv.Object) bool {
			index, _ := j.Num("current_operation")
			return index == 1 && operation(j, 1).Str("execution_id") != ""
		})
		e.shutdown()
		e.boot()
		recovered := e.wait(accepted.Str("job_id"), nil)
		equal(t, recovered.Str("state"), "succeeded", recovered.Get("error"))
		equal(t, operation(recovered, 0).Get("execution_id"), operation(active, 0).Get("execution_id"))
		equal(t, operation(recovered, 1).Get("attempt_id"), operation(active, 1).Get("attempt_id"))
		equal(t, operation(recovered, 1).Get("execution_id"), operation(active, 1).Get("execution_id"))
	})
}
