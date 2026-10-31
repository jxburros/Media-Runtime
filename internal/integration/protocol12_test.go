package integration

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

const fallbackToken = "fallback-token-not-a-secret-0000000000000"

// boot12 starts a runtime against a fixture launched with args; the owner credential may
// authorize fallbacks, the reader credential may not.
func boot12(t *testing.T, args ...string) *env {
	t.Helper()
	root := t.TempDir()
	fixture := StartFixture(t, root, args...)
	if _, err := os.Stat(filepath.Join(root, "upscaler")); err != nil {
		// Engine before 0.4.0: its fixture has no 1.2 surface; the main suite covers 1.0/1.1.
		t.Skip("the Engine fixture predates protocol 1.2")
	}
	t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	catalog := jsonv.NewObject()
	for _, handle := range []string{"fixture", "fixture-lora", "upscaler", "converted", "checkpoint", "video", "audio"} {
		dir := map[string]string{"fixture": "model", "fixture-lora": "lora"}[handle]
		if dir == "" {
			dir = handle
		}
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil && (handle == "video" || handle == "audio") {
			continue // created by Engine 0.6.0 (video) and 0.7.0 (audio) fixtures and later only
		}
		catalog.Set(handle, filepath.Join(root, dir))
	}
	e := &env{t: t, root: root, fixture: fixture, cfg: &config.Config{
		Directory:           filepath.Join(root, "runtime"),
		QueueCapacity:       4,
		CancellationGraceMS: 500,
		PollIntervalMS:      20,
		WorkerTimeoutMS:     2000,
		MaxOutputBytes:      config.DefaultOutputLimits(),
		MaxInputBytes:       32 * 1024 * 1024,
		ArtifactCatalog:     catalog,
		Worker:              config.Worker{URL: fixture.URL, TokenEnv: "MEDIA_TEST_WORKER_TOKEN"},
		Tokens: []config.Token{
			{Token: token, Namespace: "owner", Operator: true, AllowFallbacks: true},
			{Token: readerToken, Namespace: "owner"},
		},
	}}
	e.boot()
	t.Cleanup(e.shutdown)
	return e
}

func (e *env) register(t *testing.T, doc *jsonv.Object) *jsonv.Object {
	t.Helper()
	v, err := e.client.JSON("/v1/models", "POST", doc)
	if err != nil {
		t.Fatalf("register %s: %v", jsonv.MustMarshal(doc), err)
	}
	return v.(*jsonv.Object)
}

func (e *env) backendID(t *testing.T) string {
	t.Helper()
	p, err := e.rt.Worker.Probe()
	if err != nil {
		t.Fatal(err)
	}
	return p.Caps.Str("backend_id")
}

func (e *env) events(t *testing.T, jobID string) string {
	t.Helper()
	res, err := e.client.Response("/v1/jobs/"+jobID+"/events", "GET", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(res.Body)
	return string(text)
}

func TestProtocol12MultipleOutputs(t *testing.T) {
	e := boot12(t)
	ok := must[*jsonv.Object](t)
	health := ok(e.client.Health())
	// The highest minor the fixture and the runtime share: 1.2 against Engine 0.4.0–0.5.0, 1.3 from 0.6.0.
	equal(t, get(health, "worker", "protocol"), e.fixture.Highest())
	if get(health, "worker", "device_identity", "host_id") == nil {
		t.Fatal("doctor lacks the worker's device identity")
	}
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))

	s := submit()
	s.Obj("parameters").Set("count", 3.0).Set("seed", 40.0)
	done := e.wait(ok(e.client.Submit(s, "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	outputs := done.Arr("outputs")
	equal(t, len(outputs), 3)
	digests := map[string]bool{}
	for k, raw := range outputs {
		o := raw.(*jsonv.Object)
		equal(t, o.Str("output_id"), []string{"image", "image_2", "image_3"}[k])
		equal(t, o.Get("seed"), 40.0+float64(k))
		digests[o.Str("sha256")] = true
		res, err := e.client.Output(done.Str("job_id"), o.Str("output_id"))
		if err != nil {
			t.Fatal(err)
		}
		bytes, _ := io.ReadAll(res.Body)
		res.Body.Close()
		equal(t, c.SHA256Hex(bytes), o.Str("sha256"))
	}
	equal(t, len(digests), 3)
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("schema_version"), 3.0)
	equal(t, len(manifest.Arr("outputs")), 3)
	equal(t, get(manifest, "backend", "protocol"), e.fixture.Highest())
	if manifest.Get("device_identity") == nil {
		t.Fatal("manifest lacks device identity")
	}
	equal(t, strings.Count(e.events(t, done.Str("job_id")), "event: output\n"), 3)

	t.Run("a missing advertised output fails the job", func(t *testing.T) {
		e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
			if o, isObj := result.(*jsonv.Object); isObj && path == "/v1/capabilities" {
				tp := o.Obj("task_parameters").Obj("text-to-image")
				tp.Set("outputs", append(tp.Arr("outputs"), "extra"))
			}
			return result, nil
		}, nil)
		defer e.rt.Worker.SetHooks(nil, nil)
		failed := e.wait(ok(e.client.Submit(submit(), "")).Str("job_id"), nil)
		equal(t, failed.Str("state"), "failed")
		equal(t, get(failed, "error", "code"), "output_unavailable")
	})

	t.Run("count is text-to-image only and bounded by the worker", func(t *testing.T) {
		s := submit()
		s.Obj("parameters").Set("count", 5.0)
		_, err := e.client.Submit(s, "")
		rejects(t, err, 400, "invalid_request", "parameters.count")
	})
}

func TestProtocol12Upscale(t *testing.T) {
	e := boot12(t)
	ok := must[*jsonv.Object](t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	registered := e.register(t, jsonv.O("model_id", "upscaler", "artifact_handle", "upscaler", "family", "esrgan"))
	equal(t, registered.Get("family"), "esrgan")
	equal(t, registered.Get("scale"), 2.0)
	detail := must[any](t)(e.client.Model("upscaler", false)).(*jsonv.Object)
	equal(t, detail.Get("scale"), 2.0)
	equal(t, detail.Get("tasks"), []any{"upscale"})

	record := ok(e.client.UploadInput(MakePNG(64, 64)))
	enlarge := func() *jsonv.Object {
		return jsonv.O("recipe_id", "enlarge", "model_id", "upscaler", "inputs", jsonv.O("image", record.Str("input_id")))
	}
	_, err := e.client.Submit(enlarge().Set("parameters", jsonv.O("seed", 1.0)), "")
	rejects(t, err, 400, "invalid_request", "parameters.seed")
	_, err = e.client.Submit(enlarge().Set("inputs", jsonv.O("image", record.Str("input_id"), "prompt", "x")), "")
	rejects(t, err, 400, "invalid_request", "inputs.prompt")
	_, err = e.client.Submit(enlarge().Set("parameters", jsonv.O("width", 128.0)), "")
	rejects(t, err, 400, "invalid_request", "parameters.width")
	_, err = e.client.Submit(enlarge().Set("model_id", "fixture"), "")
	rejects(t, err, 400, "unsupported", "model_id")
	_, err = e.client.Submit(submit().Set("model_id", "upscaler"), "")
	rejects(t, err, 400, "unsupported", "model_id")

	done := e.wait(ok(e.client.Submit(enlarge(), "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	equal(t, done.Str("task"), "upscale")
	output := done.Arr("outputs")[0].(*jsonv.Object)
	equal(t, output.Get("width"), 128.0)
	equal(t, output.Get("height"), 128.0)
	equal(t, output.Has("seed"), false)
	equal(t, done.Obj("request").Obj("parameters").Has("seed"), false)
	events := e.events(t, done.Str("job_id"))
	if !strings.Contains(events, `"stage":"upscaling"`) || !strings.Contains(events, `"unit":"tiles"`) {
		t.Fatalf("no tile progress: %s", events)
	}
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("scale"), 2.0)
	equal(t, manifest.Get("family"), "esrgan")

	t.Run("an oversized result is refused at validation", func(t *testing.T) {
		e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
			if o, isObj := result.(*jsonv.Object); isObj && path == "/v1/capabilities" {
				o.Obj("limits").Set("output_side", 100.0)
			}
			return result, nil
		}, nil)
		defer e.rt.Worker.SetHooks(nil, nil)
		_, err := e.client.Submit(enlarge(), "")
		rejects(t, err, 400, "invalid_request", "output_side")
	})
}

func TestProtocol12ConversionProvenance(t *testing.T) {
	e := boot12(t)
	ok := must[*jsonv.Object](t)
	entries := must[any](t)(e.client.Catalog("")).([]any)
	byHandle := map[string]*jsonv.Object{}
	for _, raw := range entries {
		o := raw.(*jsonv.Object)
		byHandle[o.Str("artifact_handle")] = o
	}
	converted := byHandle["converted"]
	equal(t, converted.Get("complete"), true, converted)
	if converted.Obj("conversion").Str("source_sha256") == "" {
		t.Fatalf("no provenance: %s", jsonv.MustMarshal(converted))
	}
	equal(t, get(converted, "registration", "source", "conversion"), converted.Get("conversion"))
	equal(t, byHandle["checkpoint"].Get("complete"), false)
	equal(t, byHandle["checkpoint"].Get("format"), "single-file-checkpoint")
	equal(t, byHandle["upscaler"].Get("family"), "esrgan")
	equal(t, byHandle["upscaler"].Get("scale"), 2.0)
	equal(t, get(byHandle["upscaler"], "registration", "family"), "esrgan")

	registered := e.register(t, converted.Obj("registration").Clone().Set("model_id", "converted"))
	equal(t, registered.Get("conversion"), converted.Get("conversion"))
	_, err := e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "checkpoint", "artifact_handle", "checkpoint"))
	rejects(t, err, 400, "unsupported", "format")
	s := submit().Set("model_id", "converted")
	done := e.wait(ok(e.client.Submit(s, "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, get(manifest, "model", "source", "conversion"), converted.Get("conversion"))
}

func TestProtocol12AutomaticPrecision(t *testing.T) {
	e := boot12(t)
	ok := must[*jsonv.Object](t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	s := submit().Set("backend_options", jsonv.O(e.backendID(t), jsonv.O("precision", "auto")))
	done := e.wait(ok(e.client.Submit(s, "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, get(manifest, "request", "backend_options", e.backendID(t), "precision"), "auto")
	if p := get(manifest, "resolved_options", "precision"); p != "float32" && p != "float16" {
		t.Fatalf("auto not resolved: %s", jsonv.MustMarshal(manifest.Get("resolved_options")))
	}
	if len(manifest.Arr("resolutions")) == 0 {
		t.Fatal("no resolution reasons recorded")
	}
	// A repeated identical request resolves identically and does not reload.
	again := e.wait(ok(e.client.Submit(s, "")).Str("job_id"), nil)
	equal(t, get(again, "backend", "resolved_options"), get(done, "backend", "resolved_options"))
	if strings.Contains(e.events(t, again.Str("job_id")), `"stage":"loading"`) {
		t.Fatal("a repeated auto request reloaded the model")
	}
}

func TestProtocol12AuthorizedFallbacks(t *testing.T) {
	e := boot12(t, "--device-unavailable")
	ok := must[*jsonv.Object](t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	// Without authorization the unavailable device is refused exactly as before.
	_, err := e.client.Submit(submit(), "")
	rejects(t, err, 503, "backend_unavailable")
	withCPU := func() *jsonv.Object { return submit().Set("authorized_fallbacks", []any{"cpu"}) }
	_, err = e.as(readerToken, "").Submit(withCPU(), "")
	rejects(t, err, 403, "", "authorized_fallbacks")
	done := e.wait(ok(e.client.Submit(withCPU(), "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	applied := done.Arr("applied_fallbacks")
	if len(applied) != 1 || applied[0].(*jsonv.Object).Str("fallback") != "cpu" || applied[0].(*jsonv.Object).Str("reason") == "" {
		t.Fatalf("applied fallbacks %s", jsonv.MustMarshal(applied))
	}
	if !strings.Contains(e.events(t, done.Str("job_id")), `"stage":"loading","fallback":[{"fallback":"cpu"`) {
		t.Fatal("no fallback stage event")
	}
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("applied_fallbacks"), applied)
	equal(t, done.Obj("request").Obj("parameters").Get("width"), 64.0)
}

func TestProtocol11WorkerRefusesTheNewSurface(t *testing.T) {
	e := boot12(t, "--protocol", "media-worker/1.1")
	ok := must[*jsonv.Object](t)
	equal(t, get(ok(e.client.Health()), "worker", "protocol"), "media-worker/1.1")
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	s := submit()
	s.Obj("parameters").Set("count", 2.0)
	_, err := e.client.Submit(s, "")
	rejects(t, err, 400, "unsupported", "parameters.count")
	_, err = e.client.Submit(submit().Set("backend_options", jsonv.O(e.backendID(t), jsonv.O("precision", "auto"))), "")
	rejects(t, err, 400, "unsupported", "precision")
	_, err = e.client.Submit(submit().Set("authorized_fallbacks", []any{"cpu"}), "")
	rejects(t, err, 400, "unsupported", "authorized_fallbacks")
	_, err = e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "upscaler", "artifact_handle", "upscaler", "family", "esrgan"))
	rejects(t, err, 503, "unsupported", "family")
	record := ok(e.client.UploadInput(MakePNG(64, 64)))
	_, err = e.client.Submit(jsonv.O("recipe_id", "enlarge", "model_id", "fixture", "inputs", jsonv.O("image", record.Str("input_id"))), "")
	rejects(t, err, 400, "unsupported", "recipe_id")
	done := e.wait(ok(e.client.Submit(submit(), "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, get(manifest, "backend", "protocol"), "media-worker/1.1")
	equal(t, len(manifest.Arr("outputs")), 1)
}
