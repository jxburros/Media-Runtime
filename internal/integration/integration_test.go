package integration

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jxburros/media-runtime/internal/api"
	"github.com/jxburros/media-runtime/internal/client"
	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/runtime"
	"github.com/jxburros/media-runtime/internal/worker"
)

const token = "runtime-test-token-not-a-secret-000000000000"
const readerToken = "reader-token-not-a-secret-00000000000000"

func submit() *jsonv.Object {
	return jsonv.O(
		"recipe_id", "portrait",
		"model_id", "fixture",
		"inputs", jsonv.O("prompt", "test"),
		"parameters", jsonv.O("width", 64.0, "height", 64.0, "steps", 3.0),
	)
}

func withSteps(steps float64) *jsonv.Object {
	s := submit()
	s.Obj("parameters").Set("steps", steps)
	return s
}

func withPrompt(prompt string) *jsonv.Object {
	s := submit()
	s.Set("inputs", jsonv.O("prompt", prompt))
	return s
}

type env struct {
	t       *testing.T
	root    string
	cfg     *config.Config
	rt      *runtime.Runtime
	server  *api.Server
	client  *client.Client
	fixture *Fixture
}

func (e *env) boot() {
	rt, err := runtime.New(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := rt.Start(); err != nil {
		e.t.Fatal(err)
	}
	server := api.New(rt)
	if err := server.Listen("127.0.0.1:0"); err != nil {
		e.t.Fatal(err)
	}
	e.rt, e.server = rt, server
	e.client = client.New(server.URL(), token, "")
}

func (e *env) shutdown() {
	e.server.Close()
	e.rt.Stop()
}

func (e *env) as(tok, namespace string) *client.Client {
	return client.New(e.client.URL, tok, namespace)
}

// wait polls a job until the predicate holds (default: terminal).
func (e *env) wait(id string, predicate func(*jsonv.Object) bool) *jsonv.Object {
	e.t.Helper()
	if predicate == nil {
		predicate = func(j *jsonv.Object) bool { return c.Terminal(j.Str("state")) }
	}
	start := time.Now()
	for time.Since(start) < 10*time.Second {
		job, err := e.client.Status(id)
		if err != nil {
			e.t.Fatal(err)
		}
		if predicate(job) {
			return job
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatalf("job timed out: %s", jsonv.MustMarshal(e.rt.Store.Get(id, "")))
	return nil
}

func hasExecution(j *jsonv.Object) bool { return j.Str("execution_id") != "" }

// rejects asserts an API error with the status and, when given, code and field.
func rejects(t *testing.T, err error, status int, codeField ...string) {
	t.Helper()
	var e *client.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected an API error %d, got %v", status, err)
	}
	if e.Status != status {
		t.Fatalf("expected status %d, got %d (%s %s)", status, e.Status, e.Code, e.Message)
	}
	if len(codeField) > 0 && codeField[0] != "" && e.Code != codeField[0] {
		t.Fatalf("expected code %s, got %s", codeField[0], e.Code)
	}
	if len(codeField) > 1 && e.Field != codeField[1] {
		t.Fatalf("expected field %s, got %q", codeField[1], e.Field)
	}
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func equal(t *testing.T, got, want any, context ...any) {
	t.Helper()
	if string(jsonv.MustMarshal(jsonv.From(got))) != string(jsonv.MustMarshal(jsonv.From(want))) {
		t.Fatalf("got %s, want %s %v", jsonv.MustMarshal(jsonv.From(got)), jsonv.MustMarshal(jsonv.From(want)), context)
	}
}

func get(o *jsonv.Object, path ...string) any {
	var v any = o
	for _, p := range path {
		obj, _ := v.(*jsonv.Object)
		if obj == nil {
			if arr, ok := v.([]any); ok && p == "0" && len(arr) > 0 {
				v = arr[0]
				continue
			}
			return nil
		}
		v = obj.Get(p)
	}
	return v
}

func TestRuntimeAndEngineCooperate(t *testing.T) {
	root := t.TempDir()
	fixture := StartFixture(t, root)
	os.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	e := &env{t: t, root: root, fixture: fixture, cfg: &config.Config{
		Directory:           filepath.Join(root, "runtime"),
		QueueCapacity:       2,
		CancellationGraceMS: 500,
		PollIntervalMS:      20,
		WorkerTimeoutMS:     1000,
		MaxOutputBytes:      config.DefaultOutputLimits(),
		MaxInputBytes:       32 * 1024 * 1024,
		ArtifactCatalog: jsonv.O(
			"fixture", filepath.Join(root, "model"),
			"fixture-lora", filepath.Join(root, "lora"),
			"absent", filepath.Join(root, "nope"),
		),
		Worker: config.Worker{URL: fixture.URL, TokenEnv: "MEDIA_TEST_WORKER_TOKEN"},
		Tokens: []config.Token{
			{Token: token, Namespace: "owner", Operator: true, AllowedNamespaces: []string{"studio:*"}},
			{Token: readerToken, Namespace: "reader"},
		},
	}}
	e.boot()
	defer func() { e.shutdown() }()
	ok := must[any](t)
	okObj := must[*jsonv.Object](t)
	ok(e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "fixture", "artifact_handle", "fixture", "source", jsonv.O("license", "test-only"))))

	t.Run("strict validation, auth and namespace boundaries", func(t *testing.T) {
		statusOf := func(path string, headers map[string]string) int {
			req, _ := newRequest(e.client.URL+path, headers)
			return req
		}
		equal(t, statusOf("/health", nil), 200)
		equal(t, statusOf("/v1/models", nil), 401)
		equal(t, statusOf("/health", map[string]string{"Origin": "https://example.test"}), 403)
		_, err := e.client.Submit(submit().Set("namespace", "someone"), "")
		rejects(t, err, 400)
		s := submit()
		s.Set("parameters", jsonv.O("unknown", true))
		_, err = e.client.Submit(s, "")
		rejects(t, err, 400)
		_, err = e.as(readerToken, "").JSON("/v1/models", "POST", jsonv.O("model_id", "x", "artifact_handle", "fixture"))
		rejects(t, err, 403)
		scoped := e.as(token, "studio:personal:user-a")
		job := okObj(scoped.Submit(submit(), ""))
		_, err = e.client.Status(job.Str("job_id"))
		rejects(t, err, 404)
		_, err = e.as(readerToken, "studio:personal:user-a").Status(job.Str("job_id"))
		rejects(t, err, 403)
		okObj(scoped.Cancel(job.Str("job_id")))
	})

	t.Run("durable PNG, provenance, idempotency and replayable SSE", func(t *testing.T) {
		accepted := okObj(e.client.Submit(submit(), "one"))
		retried := okObj(e.client.Submit(submit(), "one"))
		equal(t, accepted.Str("job_id"), retried.Str("job_id"))
		_, err := e.client.Submit(withPrompt("different"), "one")
		rejects(t, err, 409)
		done := e.wait(accepted.Str("job_id"), nil)
		equal(t, done.Str("state"), "succeeded")
		res, err := e.client.Output(done.Str("job_id"), "image")
		if err != nil {
			t.Fatal(err)
		}
		bytes, _ := io.ReadAll(res.Body)
		res.Body.Close()
		equal(t, string(bytes[:8]), "\x89PNG\r\n\x1a\n")
		manifest := ok(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
		equal(t, get(manifest, "worker", "test_fixture"), true)
		equal(t, get(manifest, "request", "parameters", "seed"), get(done, "request", "parameters", "seed"))
		text := string(jsonv.MustMarshal(manifest))
		if strings.Contains(text, root) || strings.Contains(text, token) {
			t.Fatal("manifest leaks a path or credential")
		}
		res, err = e.client.Response("/v1/jobs/"+done.Str("job_id")+"/events", "GET", nil, map[string]string{"Last-Event-ID": "1"})
		if err != nil {
			t.Fatal(err)
		}
		events, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(events), "event: progress") || strings.Contains(string(events), "id: 1\n") {
			t.Fatalf("unexpected events: %s", events)
		}
		_, err = e.client.Response("/v1/jobs/"+done.Str("job_id")+"/events", "GET", nil, map[string]string{"Last-Event-ID": "9999"})
		rejects(t, err, 409)
		if err := os.Remove(filepath.Join(e.cfg.Directory, "outputs", done.Str("job_id"), "image.png")); err != nil {
			t.Fatal(err)
		}
		_, err = e.client.Output(done.Str("job_id"), "image")
		rejects(t, err, 410)
	})

	t.Run("an event stream outlives the request read timeout", func(t *testing.T) {
		short := api.NewWithTimeouts(e.rt, time.Second, 200*time.Millisecond)
		if err := short.Listen("127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		defer short.Close()
		accepted := okObj(e.client.Submit(withSteps(40), ""))
		res, err := client.New(short.URL(), token, "").Response("/v1/jobs/"+accepted.Str("job_id")+"/events", "GET", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		stream, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if time.Since(started) < 300*time.Millisecond || !strings.Contains(string(stream), `"state":"succeeded"`) {
			t.Fatalf("stream ended early after %v: %s", time.Since(started), stream)
		}
	})

	t.Run("bounded FIFO, queued cancellation and running cancellation settle", func(t *testing.T) {
		first := okObj(e.client.Submit(withSteps(100), ""))
		e.wait(first.Str("job_id"), hasExecution)
		second := okObj(e.client.Submit(submit(), ""))
		third := okObj(e.client.Submit(submit(), ""))
		_, err := e.client.Submit(submit(), "")
		rejects(t, err, 429)
		equal(t, okObj(e.client.Cancel(second.Str("job_id"))).Str("state"), "cancelled")
		cancelled := okObj(e.client.Cancel(first.Str("job_id")))
		equal(t, cancelled.Get("cancel_requested"), true)
		equal(t, e.wait(first.Str("job_id"), nil).Str("state"), "cancelled")
		equal(t, e.wait(third.Str("job_id"), nil).Str("state"), "succeeded")
	})

	t.Run("restart reconciles surviving execution and keeps queued work", func(t *testing.T) {
		first := okObj(e.client.Submit(withSteps(25), ""))
		active := e.wait(first.Str("job_id"), hasExecution)
		queued := okObj(e.client.Submit(submit(), ""))
		e.shutdown()
		time.Sleep(120 * time.Millisecond)
		e.boot()
		recovered := e.wait(first.Str("job_id"), nil)
		equal(t, recovered.Str("state"), "succeeded")
		equal(t, recovered.Str("attempt_id"), active.Str("attempt_id"))
		equal(t, recovered.Str("execution_id"), active.Str("execution_id"))
		equal(t, e.wait(queued.Str("job_id"), nil).Str("state"), "succeeded")
	})

	t.Run("lost acceptance response reuses one session attempt", func(t *testing.T) {
		var intercepted atomic.Bool
		e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
			if path == "/v1/executions" && method == "POST" && intercepted.CompareAndSwap(false, true) {
				return nil, c.Err(503, "backend_unavailable", "lost response")
			}
			return result, nil
		}, nil)
		defer e.rt.Worker.SetHooks(nil, nil)
		accepted := okObj(e.client.Submit(submit(), ""))
		equal(t, e.wait(accepted.Str("job_id"), nil).Str("state"), "succeeded")
		equal(t, intercepted.Load(), true)
	})

	t.Run("image-to-image flows through durable, namespace-owned inputs", func(t *testing.T) {
		record := okObj(e.client.UploadInput(MakePNG(64, 64)))
		equal(t, record.Get("width"), 64.0)
		equal(t, record.Has("namespace"), false)
		_, err := e.as(readerToken, "").Input(record.Str("input_id"))
		rejects(t, err, 404)
		_, err = e.client.Submit(submit().Set("inputs", jsonv.O("prompt", "test", "image", record.Str("input_id"))), "")
		rejects(t, err, 400, "", "inputs.image")
		_, err = e.client.Submit(jsonv.O("recipe_id", "variation", "model_id", "fixture",
			"inputs", jsonv.O("prompt", "test", "image", record.Str("input_id")),
			"parameters", jsonv.O("width", 128.0, "steps", 4.0)), "")
		rejects(t, err, 400, "", "parameters.width")
		_, err = e.client.Submit(jsonv.O("recipe_id", "variation", "model_id", "fixture",
			"inputs", jsonv.O("prompt", "test"), "parameters", jsonv.O("steps", 4.0)), "")
		rejects(t, err, 400, "", "inputs.image")
		accepted := okObj(e.client.Submit(jsonv.O("recipe_id", "variation", "model_id", "fixture",
			"inputs", jsonv.O("prompt", "test", "image", record.Str("input_id")),
			"parameters", jsonv.O("steps", 4.0)), ""))
		e.wait(accepted.Str("job_id"), hasExecution)
		_, err = e.client.DeleteInput(record.Str("input_id"))
		rejects(t, err, 409)
		done := e.wait(accepted.Str("job_id"), nil)
		equal(t, done.Str("state"), "succeeded", done.Get("error"))
		equal(t, done.Str("task"), "image-to-image")
		equal(t, get(done, "request", "parameters", "width"), 64.0)
		equal(t, get(done, "request", "parameters", "strength"), 0.6)
		equal(t, get(done, "input", "sha256"), record.Get("sha256"))
		manifest := ok(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
		equal(t, manifest.Str("task"), "image-to-image")
		equal(t, get(manifest, "inputs", "0", "sha256"), record.Get("sha256"))
		equal(t, get(manifest, "effective_settings", "effective_steps"), 2.0)
		equal(t, get(manifest, "backend", "protocol"), e.fixture.Highest())
		e.wait(done.Str("job_id"), func(j *jsonv.Object) bool { return j.Bool("released") })
		equal(t, okObj(e.client.DeleteInput(record.Str("input_id"))).Get("deleted"), true)
		equal(t, len(ok(e.client.Inputs()).([]any)), 0)
	})

	t.Run("LoRA adapters are registered, validated and part of the load identity", func(t *testing.T) {
		lora := func(uses ...*jsonv.Object) *jsonv.Object {
			list := []any{}
			for _, u := range uses {
				list = append(list, u)
			}
			return submit().Set("adapters", list)
		}
		_, err := e.client.Submit(lora(jsonv.O("adapter_id", "fixture-lora")), "")
		rejects(t, err, 400, "unknown_adapter")
		registered := ok(e.client.JSON("/v1/adapters", "POST", jsonv.O("adapter_id", "fixture-lora", "artifact_handle", "fixture-lora", "source", jsonv.O("license", "test-only")))).(*jsonv.Object)
		equal(t, get(registered, "layout", "format"), "peft-lora")
		equal(t, get(registered, "layout", "targets"), []any{"text_encoder", "unet"})
		equal(t, get(ok(e.client.JSON("/v1/adapters", "GET", nil)).([]any)[0].(*jsonv.Object), "adapter_id"), "fixture-lora")
		_, err = e.client.Submit(lora(jsonv.O("adapter_id", "fixture-lora"), jsonv.O("adapter_id", "fixture-lora")), "")
		rejects(t, err, 400)
		_, err = e.client.Submit(lora(jsonv.O("adapter_id", "fixture-lora", "scale", 5.0)), "")
		rejects(t, err, 400)
		before := get(okObj(e.client.Health()), "worker", "loaded_bundle_digest")
		accepted := okObj(e.client.Submit(lora(jsonv.O("adapter_id", "fixture-lora", "scale", 0.7)), ""))
		done := e.wait(accepted.Str("job_id"), nil)
		equal(t, done.Str("state"), "succeeded", done.Get("error"))
		equal(t, done.Get("adapters"), []any{jsonv.O("adapter_id", "fixture-lora", "digest", registered.Get("digest"), "scale", 0.7)})
		manifest := ok(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
		equal(t, get(manifest, "adapters", "0", "scale"), 0.7)
		equal(t, len(get(manifest, "backend", "memory_estimate", "adapters").([]any)), 1)
		info := must[*jsonv.Object](t)(e.rt.Worker.Obj("/v1/info", "GET", nil))
		equal(t, get(info, "loaded_bundle", "adapters", "0", "scale"), 0.7)
		equal(t, get(info, "loaded_bundle", "bundle_digest"), before)
		// A job without adapters needs a fresh load; the identity includes the adapter set.
		plain := e.wait(okObj(e.client.Submit(submit(), "")).Str("job_id"), nil)
		equal(t, plain.Str("state"), "succeeded")
		equal(t, plain.Get("adapters"), []any{})
		info = must[*jsonv.Object](t)(e.rt.Worker.Obj("/v1/info", "GET", nil))
		equal(t, len(get(info, "loaded_bundle", "adapters").([]any)), 0)
		_, err = e.as(readerToken, "").JSON("/v1/adapters", "POST", jsonv.O("adapter_id", "x", "artifact_handle", "fixture-lora"))
		rejects(t, err, 403)
	})

	t.Run("protocol negotiation serves a 1.0 worker without image-to-image", func(t *testing.T) {
		equal(t, get(okObj(e.client.Health()), "worker", "protocol"), e.fixture.Highest())
		e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
			if o, ok := result.(*jsonv.Object); ok && (path == "/v1/info" || path == "/v1/capabilities") {
				o.Delete("supported_protocols")
				o.Set("protocol", "media-worker/1.0")
			}
			return result, nil
		}, nil)
		defer e.rt.Worker.SetHooks(nil, nil)
		equal(t, get(okObj(e.client.Health()), "worker", "protocol"), "media-worker/1.0")
		record := okObj(e.client.UploadInput(MakePNG(64, 64)))
		_, err := e.client.Submit(jsonv.O("recipe_id", "variation", "model_id", "fixture", "inputs", jsonv.O("prompt", "test", "image", record.Str("input_id"))), "")
		rejects(t, err, 400, "unsupported")
		_, err = e.client.Submit(submit().Set("adapters", []any{jsonv.O("adapter_id", "fixture-lora")}), "")
		rejects(t, err, 400, "unsupported")
		legacy := okObj(e.client.Submit(submit(), ""))
		equal(t, e.wait(legacy.Str("job_id"), nil).Str("state"), "succeeded")
		equal(t, get(ok(e.client.Manifest(legacy.Str("job_id"))).(*jsonv.Object), "backend", "protocol"), "media-worker/1.0")
		okObj(e.client.DeleteInput(record.Str("input_id")))
	})

	t.Run("catalog inspection and model verification guide installation", func(t *testing.T) {
		entries := ok(e.client.Catalog("")).([]any)
		find := func(handle string) *jsonv.Object {
			for _, raw := range entries {
				if o := raw.(*jsonv.Object); o.Str("artifact_handle") == handle {
					return o
				}
			}
			t.Fatalf("no catalog entry %s", handle)
			return nil
		}
		fx := find("fixture")
		equal(t, fx.Get("complete"), true)
		equal(t, fx.Get("registration"), jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
		absent := find("absent")
		equal(t, absent.Get("present"), false)
		equal(t, absent.Get("complete"), false)
		if strings.Contains(string(jsonv.MustMarshal(entries)), root) {
			t.Fatal("catalog leaks a path")
		}
		_, err := e.as(readerToken, "").Catalog("")
		rejects(t, err, 403)
		detail := ok(e.client.Model("fixture", true)).(*jsonv.Object)
		equal(t, get(detail, "verification", "valid"), true)
		equal(t, get(detail, "verification", "memory_estimate", "source"), "safetensors_headers")
		equal(t, len(detail.Arr("components")), 7)
		newest := okObj(e.client.Jobs(2, 0, true))
		jobs := newest.Arr("jobs")
		equal(t, len(jobs), 2)
		equal(t, newest.Get("next_cursor"), nil)
		if jobs[0].(*jsonv.Object).Str("created_at") < jobs[1].(*jsonv.Object).Str("created_at") {
			t.Fatal("newest order is wrong")
		}
	})

	t.Run("failure is private and lost session never regenerates", func(t *testing.T) {
		failed := okObj(e.client.Submit(withPrompt("fail"), ""))
		result := e.wait(failed.Str("job_id"), nil)
		equal(t, result.Str("state"), "failed")
		equal(t, get(result, "error", "code"), "execution_failed")
		if strings.Contains(string(jsonv.MustMarshal(result.Get("error"))), "secret") {
			t.Fatal("error leaks worker detail")
		}
		accepted := okObj(e.client.Submit(withSteps(100), ""))
		e.wait(accepted.Str("job_id"), hasExecution)
		// Simulate the discovery of a replacement session while preserving the worker itself for cleanup.
		e.rt.Worker.SetHooks(nil, func(p *worker.Probe) (*worker.Probe, error) {
			p.Info.Set("session_id", "replacement")
			p.Caps.Set("session_id", "replacement")
			return p, nil
		})
		defer e.rt.Worker.SetHooks(nil, nil)
		equal(t, get(e.wait(accepted.Str("job_id"), nil), "error", "code"), "execution_interrupted")
	})
}

func TestOwnedWorkerCrashAndCancellationDeadline(t *testing.T) {
	root := t.TempDir()
	engine := EngineRepo(t)
	t.Setenv("PYTHONPATH", filepath.Join(engine, "src"))
	cfg := &config.Config{
		Directory:           filepath.Join(root, "runtime"),
		QueueCapacity:       2,
		CancellationGraceMS: 80,
		PollIntervalMS:      20,
		WorkerTimeoutMS:     500,
		MaxOutputBytes:      config.DefaultOutputLimits(),
		MaxInputBytes:       33554432,
		ArtifactCatalog:     jsonv.O("fixture", filepath.Join(root, "model")),
		Worker: config.Worker{
			URL:      "http://127.0.0.1:7811",
			TokenEnv: "IGNORED_OWNED_TOKEN",
			Command:  FixtureCommand(engine, root),
		},
		Tokens: []config.Token{{Token: token, Namespace: "owner", Operator: true}},
	}
	rt, err := runtime.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	server := api.New(rt)
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer func() { server.Close(); rt.Stop() }()
	cl := client.New(server.URL(), token, "")
	if _, err := cl.JSON("/v1/models", "POST", jsonv.O("model_id", "fixture", "artifact_handle", "fixture")); err != nil {
		t.Fatal(err)
	}
	wait := func(id string, predicate func(*jsonv.Object) bool) *jsonv.Object {
		for i := 0; i < 300; i++ {
			j, err := cl.Status(id)
			if err != nil {
				t.Fatal(err)
			}
			if predicate(j) {
				return j
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("owned job timed out")
		return nil
	}
	okObj := must[*jsonv.Object](t)
	first := okObj(cl.Submit(withSteps(100), ""))
	active := wait(first.Str("job_id"), hasExecution)
	rt.Worker.Child().Kill(syscall.SIGKILL)
	failed := wait(first.Str("job_id"), func(j *jsonv.Object) bool { return j.Str("state") == "failed" })
	equal(t, get(failed, "error", "code"), "execution_interrupted")
	for i := 0; i < 100; i++ {
		if child := rt.Worker.Child(); child != nil && !child.Exited() && rt.Worker.DiscoveredSession() != active.Str("session_id") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	next := okObj(cl.Submit(withPrompt("uncancellable"), ""))
	wait(next.Str("job_id"), hasExecution)
	okObj(cl.Cancel(next.Str("job_id")))
	stopped := wait(next.Str("job_id"), func(j *jsonv.Object) bool { return j.Str("state") == "failed" })
	equal(t, get(stopped, "error", "code"), "execution_interrupted")
	equal(t, get(stopped, "error", "forced_termination"), true)
}
