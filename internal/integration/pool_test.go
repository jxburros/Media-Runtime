package integration

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/runtime"
)

// poolConfig is a runtime configuration over the given workers, with the catalog of the first
// fixture's root (every fixture writes the same bundles under its own root).
func poolConfig(root string, workers []config.Worker) *config.Config {
	catalog := jsonv.NewObject()
	for handle, dir := range map[string]string{"fixture": "model", "fixture-lora": "lora", "upscaler": "upscaler"} {
		catalog.Set(handle, filepath.Join(root, "w0", dir))
	}
	return &config.Config{
		Directory:           filepath.Join(root, "runtime"),
		QueueCapacity:       8,
		CancellationGraceMS: 500,
		PollIntervalMS:      20,
		WorkerTimeoutMS:     2000,
		MaxOutputBytes:      config.DefaultOutputLimits(),
		MaxInputBytes:       32 * 1024 * 1024,
		ArtifactCatalog:     catalog,
		Workers:             workers,
		StrictDevices:       true,
		Tokens:              []config.Token{{Token: token, Namespace: "owner", Operator: true}, {Token: readerToken, Namespace: "reader"}},
	}
}

// startPool launches one external fixture per device index (each under its own root) and returns
// the pool entries a, b … for them. It skips against an Engine fixture before protocol 1.2,
// which reports no device identity.
func startPool(t *testing.T, root string, indexes ...int) []config.Worker {
	t.Helper()
	t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	workers := []config.Worker{}
	for i, index := range indexes {
		dir := filepath.Join(root, "w"+string(rune('0'+i)))
		var fixture *Fixture
		if index == 0 {
			fixture = StartFixture(t, dir)
		} else {
			fixture = StartFixture(t, dir, "--device-index", string(rune('0'+index)))
		}
		if _, err := os.Stat(filepath.Join(dir, "upscaler")); err != nil {
			t.Skip("the Engine fixture predates protocol 1.2")
		}
		workers = append(workers, config.Worker{WorkerID: string(rune('a' + i)), URL: fixture.URL, TokenEnv: "MEDIA_TEST_WORKER_TOKEN"})
	}
	return workers
}

func bootPool(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, cfg: poolConfig(root, startPool(t, root, 0, 1))}
	e.boot()
	t.Cleanup(e.shutdown)
	return e
}

func running(j *jsonv.Object) bool { return j.Str("state") == "running" && j.Str("execution_id") != "" }

func (e *env) workers(t *testing.T) []*jsonv.Object {
	t.Helper()
	raw := must[any](t)(e.client.Workers()).([]any)
	list := []*jsonv.Object{}
	for _, w := range raw {
		list = append(list, w.(*jsonv.Object))
	}
	return list
}

func TestWorkerPoolSchedulesAcrossWorkers(t *testing.T) {
	e := bootPool(t)
	ok := must[*jsonv.Object](t)
	workers := e.workers(t)
	equal(t, len(workers), 2)
	for i, w := range workers {
		equal(t, w.Str("worker_id"), []string{"a", "b"}[i])
		equal(t, w.Get("ready"), true)
		equal(t, w.Get("busy"), false)
		equal(t, get(w, "device_identity", "index"), float64(i))
	}
	health := ok(e.client.Health())
	equal(t, len(health.Arr("workers")), 2)
	equal(t, get(health, "worker", "worker_id"), "a")
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	e.register(t, jsonv.O("model_id", "upscaler", "artifact_handle", "upscaler", "family", "esrgan"))

	t.Run("two queued jobs run concurrently, one per worker", func(t *testing.T) {
		first := ok(e.client.Submit(withSteps(40), ""))
		second := ok(e.client.Submit(withSteps(40), ""))
		e.wait(first.Str("job_id"), running)
		two := e.wait(second.Str("job_id"), running)
		one := ok(e.client.Status(first.Str("job_id")))
		equal(t, one.Str("state"), "running", "the first job finished before the second started")
		equal(t, []any{one.Get("worker_id"), two.Get("worker_id")}, []any{"a", "b"})
		busy := e.workers(t)
		equal(t, []any{busy[0].Get("running_job_id"), busy[1].Get("running_job_id")}, []any{one.Get("job_id"), two.Get("job_id")})
		// Another namespace sees that a worker is busy, never which job it runs.
		other := must[any](t)(e.as(readerToken, "").Workers()).([]any)
		equal(t, get(other[0].(*jsonv.Object), "busy"), true)
		equal(t, get(other[0].(*jsonv.Object), "running_job_id"), nil)
		for i, id := range []string{first.Str("job_id"), second.Str("job_id")} {
			done := e.wait(id, nil)
			equal(t, done.Str("state"), "succeeded", done.Get("error"))
			worker := []string{"a", "b"}[i]
			equal(t, done.Str("worker_id"), worker)
			equal(t, get(done, "device_identity", "index"), float64(i))
			equal(t, get(done, "backend", "device_identity", "index"), float64(i))
			equal(t, get(done, "operations", "0", "worker_id"), worker)
			equal(t, done.Arr("candidates"), []any{jsonv.O("worker_id", "a", "protocol", c.PROTOCOL), jsonv.O("worker_id", "b", "protocol", c.PROTOCOL)})
			manifest := must[any](t)(e.client.Manifest(id)).(*jsonv.Object)
			equal(t, manifest.Get("worker_id"), worker)
			equal(t, get(manifest, "device_identity", "index"), float64(i))
			if !strings.Contains(e.events(t, id), `"state":"running","worker_id":"`+worker+`"`) {
				t.Fatalf("the running event does not name worker %s", worker)
			}
		}
		listed := ok(e.client.WorkerJobs(0, 0, false, "b")).Arr("jobs")
		equal(t, len(listed), 1)
		equal(t, get(listed[0].(*jsonv.Object), "job_id"), second.Get("job_id"))
		equal(t, len(ok(e.client.WorkerJobs(0, 0, false, "nowhere")).Arr("jobs")), 0)
		_, err := e.client.JSON("/v1/jobs?worker_id=a%20b", "GET", nil)
		rejects(t, err, 400, "invalid_request", "worker_id")
	})

	t.Run("a pinned job waits for its worker and FIFO holds later jobs", func(t *testing.T) {
		_, err := e.client.Submit(submit().Set("worker_id", "c"), "")
		rejects(t, err, 400, "unknown_worker", "worker_id")
		long := ok(e.client.Submit(withSteps(40).Set("worker_id", "a"), ""))
		e.wait(long.Str("job_id"), running)
		pinned := ok(e.client.Submit(submit().Set("worker_id", "a"), ""))
		later := ok(e.client.Submit(submit(), ""))
		validated := must[any](t)(e.client.Validate(submit().Set("worker_id", "b"))).(*jsonv.Object)
		equal(t, validated.Get("candidate_workers"), []any{"b"})
		// Worker b is idle the whole time, but the head is pinned to a and reordering is off.
		time.Sleep(300 * time.Millisecond)
		equal(t, ok(e.client.Status(pinned.Str("job_id"))).Str("state"), "queued")
		equal(t, ok(e.client.Status(later.Str("job_id"))).Str("state"), "queued")
		equal(t, ok(e.client.Status(long.Str("job_id"))).Str("state"), "running")
		done := e.wait(pinned.Str("job_id"), nil)
		equal(t, done.Str("state"), "succeeded", done.Get("error"))
		equal(t, done.Str("worker_id"), "a")
		equal(t, done.Obj("request").Str("worker_id"), "a")
		equal(t, e.wait(later.Str("job_id"), nil).Str("state"), "succeeded")
	})

	t.Run("affinity keeps a matching loaded identity on its worker", func(t *testing.T) {
		if _, err := e.client.JSON("/v1/adapters", "POST", jsonv.O("adapter_id", "fixture-lora", "artifact_handle", "fixture-lora")); err != nil {
			t.Fatal(err)
		}
		lora := func() *jsonv.Object {
			return submit().Set("adapters", []any{jsonv.O("adapter_id", "fixture-lora", "scale", 0.7)})
		}
		// Worker a holds the adapter set, worker b the plain bundle.
		equal(t, e.wait(ok(e.client.Submit(lora().Set("worker_id", "a"), "")).Str("job_id"), nil).Str("state"), "succeeded")
		equal(t, e.wait(ok(e.client.Submit(submit().Set("worker_id", "b"), "")).Str("job_id"), nil).Str("state"), "succeeded")
		workers := e.workers(t)
		equal(t, get(workers[0], "loaded", "adapters", "0", "scale"), 0.7)
		equal(t, get(workers[1], "loaded", "model_ids"), []any{"fixture"})
		// Both are idle: configuration order would pick a, the loaded identity picks b.
		plain := e.wait(ok(e.client.Submit(submit(), "")).Str("job_id"), nil)
		equal(t, plain.Str("worker_id"), "b")
		styled := e.wait(ok(e.client.Submit(lora(), "")).Str("job_id"), nil)
		equal(t, styled.Str("worker_id"), "a")
		for _, id := range []string{plain.Str("job_id"), styled.Str("job_id")} {
			if strings.Contains(e.events(t, id), `"stage":"loading"`) {
				t.Fatalf("job %s reloaded its worker", id)
			}
		}
	})

	t.Run("a linear job runs every operation on the worker it started on", func(t *testing.T) {
		if _, err := e.client.JSON("/v1/recipes", "POST", portrait4k()); err != nil {
			t.Fatal(err)
		}
		job := e.wait(ok(e.client.Submit(jsonv.O("recipe_id", "portrait-4k", "model_id", "fixture", "models", jsonv.O("enlarge", "upscaler"),
			"inputs", jsonv.O("prompt", "pool")), "")).Str("job_id"), nil)
		equal(t, job.Str("state"), "succeeded", job.Get("error"))
		equal(t, get(job, "operations", "0", "worker_id"), job.Get("worker_id"))
		equal(t, operation(job, 1).Get("worker_id"), job.Get("worker_id"))
		manifest := must[any](t)(e.client.Manifest(job.Str("job_id"))).(*jsonv.Object)
		equal(t, manifest.Get("worker_id"), job.Get("worker_id"))
		equal(t, get(manifest, "operations", "0", "worker_id"), job.Get("worker_id"))
	})

	t.Run("restart reconciles each worker's running job", func(t *testing.T) {
		first := ok(e.client.Submit(withSteps(30).Set("worker_id", "b"), ""))
		active := e.wait(first.Str("job_id"), running)
		e.shutdown()
		time.Sleep(120 * time.Millisecond)
		e.boot()
		recovered := e.wait(first.Str("job_id"), nil)
		equal(t, recovered.Str("state"), "succeeded")
		equal(t, recovered.Str("worker_id"), "b")
		equal(t, recovered.Str("execution_id"), active.Str("execution_id"))
	})
}

func TestWorkerPoolLosesOnlyTheCrashedWorkersJob(t *testing.T) {
	root := t.TempDir()
	engine := EngineRepo(t)
	if launcher, _ := os.ReadFile(filepath.Join(engine, "tests", "serve_fixture.py")); !strings.Contains(string(launcher), "--device-index") {
		t.Skip("the Engine fixture predates protocol 1.2")
	}
	t.Setenv("PYTHONPATH", filepath.Join(engine, "src"))
	cfg := poolConfig(root, []config.Worker{
		{WorkerID: "a", URL: "http://127.0.0.1:7811", TokenEnv: "IGNORED", Command: FixtureCommand(engine, filepath.Join(root, "w0"))},
		{WorkerID: "b", URL: "http://127.0.0.1:7811", TokenEnv: "IGNORED", Command: FixtureCommand(engine, filepath.Join(root, "w1"), "--device-index", "1")},
	})
	e := &env{t: t, root: root, cfg: cfg}
	e.boot()
	t.Cleanup(e.shutdown)
	ok := must[*jsonv.Object](t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	doomed := ok(e.client.Submit(withSteps(100).Set("worker_id", "a"), ""))
	survivor := ok(e.client.Submit(withSteps(40).Set("worker_id", "b"), ""))
	e.wait(doomed.Str("job_id"), running)
	e.wait(survivor.Str("job_id"), running)
	e.rt.Pool.Get("a").Client.Child().Kill(syscall.SIGKILL)
	failed := e.wait(doomed.Str("job_id"), nil)
	equal(t, failed.Str("state"), "failed")
	equal(t, get(failed, "error", "code"), "execution_interrupted")
	equal(t, e.wait(survivor.Str("job_id"), nil).Str("state"), "succeeded")
	// Worker a was restarted under a new session and takes work again.
	again := e.wait(ok(e.client.Submit(submit().Set("worker_id", "a"), "")).Str("job_id"), nil)
	equal(t, again.Str("state"), "succeeded", again.Get("error"))
	if again.Str("session_id") == failed.Str("session_id") {
		t.Fatal("the restarted worker kept its session")
	}
}

func TestWorkerPoolRefusesADuplicateDeviceIdentity(t *testing.T) {
	root := t.TempDir()
	// Two fixtures on one host reporting device index 0: one device listed twice.
	workers := startPool(t, root, 0, 0)
	strict := poolConfig(root, workers)
	rt, err := runtime.New(strict)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(); err == nil || !strings.Contains(err.Error(), "Workers a and b report the same device identity") {
		t.Fatalf("strict_devices must refuse to start: %v", err)
	}
	rt.Stop()

	lenient := poolConfig(root, workers)
	lenient.StrictDevices = false
	e := &env{t: t, root: root, cfg: lenient}
	e.boot()
	t.Cleanup(e.shutdown)
	for _, w := range e.workers(t) {
		equal(t, w.Get("ready"), false)
		equal(t, w.Get("error"), "backend_unavailable")
		if !strings.Contains(w.Str("message"), "same device identity") {
			t.Fatalf("unexpected message %q", w.Str("message"))
		}
	}
	equal(t, get(must[*jsonv.Object](t)(e.client.Health()), "workers", "0", "duplicate_of"), "b")
	_, err = e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	rejects(t, err, 503, "backend_unavailable")
}

func TestLegacyWorkerConfigurationIsAPoolOfOne(t *testing.T) {
	root := t.TempDir()
	fixture := StartFixture(t, root)
	t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	raw, err := jsonv.Parse([]byte(`{"directory":` + string(jsonv.MustMarshal(filepath.Join(root, "runtime"))) + `,"poll_interval_ms":20,
		"credentials":[{"token_env":"POOL_TEST_TOKEN","namespace":"owner","operator":true}],
		"artifact_catalog":{"fixture":` + string(jsonv.MustMarshal(filepath.Join(root, "model"))) + `},
		"worker":{"url":"` + fixture.URL + `","token_env":"MEDIA_TEST_WORKER_TOKEN"}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(raw, func(k string) string { return map[string]string{"POOL_TEST_TOKEN": token}[k] })
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, root: root, cfg: cfg}
	e.boot()
	t.Cleanup(e.shutdown)
	workers := e.workers(t)
	equal(t, len(workers), 1)
	equal(t, workers[0].Str("worker_id"), "default")
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	done := e.wait(must[*jsonv.Object](t)(e.client.Submit(submit(), "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	equal(t, done.Str("worker_id"), "default")
	equal(t, must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object).Get("worker_id"), "default")
	equal(t, len(must[*jsonv.Object](t)(e.client.WorkerJobs(0, 0, false, "default")).Arr("jobs")), 1)
}
