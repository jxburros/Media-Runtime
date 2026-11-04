package integration

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

// TestInferenceBackends exercises genuine inference through Runtime's public API.
// The fixture suite remains responsible for fault injection and families/transports
// not advertised by native-sdxl. Setting either opt-in makes missing dependencies
// a failure, rather than silently skipping acceptance checks in CI.
func TestInferenceBackends(t *testing.T) {
	native := os.Getenv("MEDIA_ENGINE_NATIVE")
	if native == "" && os.Getenv("MEDIA_ENGINE_INFERENCE") != "1" {
		t.Skip("set MEDIA_ENGINE_NATIVE or MEDIA_ENGINE_INFERENCE=1 for real CPU inference")
	}
	engine := EngineRepo(t)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prepare := exec.CommandContext(ctx, Python(), filepath.Join(engine, "tests", "prepare_runtime.py"), root)
	prepare.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(engine, "src"))
	if output, err := prepare.CombinedOutput(); err != nil {
		t.Fatalf("prepare real bundles: %v\n%s", err, output)
	}
	backends := []string{"diffusers-sdxl", "owned-sdxl"}
	if native != "" {
		var err error
		native, err = filepath.Abs(native)
		if err != nil {
			t.Fatal(err)
		}
		backends = append(backends, "native-sdxl")
	}
	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			command := []string{Python(), "-m", "media_engine.server", "--backend", backend}
			if backend == "native-sdxl" {
				command = []string{native}
			}
			command = append(command, "--catalog", filepath.Join(root, "catalog.json"), "--directory", filepath.Join(root, backend), "--device", "cpu", "--port", "0")
			worker := StartCommand(t, engine, command)
			t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
			e := &env{t: t, root: root, fixture: worker, cfg: &config.Config{
				Directory: filepath.Join(root, "runtime-"+backend), QueueCapacity: 4, CancellationGraceMS: 30000,
				PollIntervalMS: 20, WorkerTimeoutMS: 30000, MaxOutputBytes: config.DefaultOutputLimits(), MaxInputBytes: 32 * 1024 * 1024,
				ArtifactCatalog: jsonv.O("fixture", filepath.Join(root, "model"), "fixture-lora", filepath.Join(root, "lora")),
				Worker:          config.Worker{URL: worker.URL, TokenEnv: "MEDIA_TEST_WORKER_TOKEN", BackendID: backend},
				Tokens:          []config.Token{{Token: token, Namespace: "owner", Operator: true}, {Token: readerToken, Namespace: "reader"}},
			}}
			e.boot()
			defer func() { e.shutdown() }()
			obj := must[*jsonv.Object](t)
			anyResult := must[any](t)
			wait := func(id string, predicate func(*jsonv.Object) bool) *jsonv.Object {
				t.Helper()
				deadline := time.Now().Add(2 * time.Minute)
				for time.Now().Before(deadline) {
					j := obj(e.client.Status(id))
					if (predicate == nil && c.Terminal(j.Str("state"))) || (predicate != nil && predicate(j)) {
						return j
					}
					time.Sleep(25 * time.Millisecond)
				}
				t.Fatalf("real inference job %s timed out", id)
				return nil
			}
			ownership := map[string]string{"diffusers-sdxl": "reference", "owned-sdxl": "owned-pipeline", "native-sdxl": "engine"}[backend]
			equal(t, get(obj(e.client.Health()), "worker", "backend_id"), backend)
			equal(t, get(obj(e.client.Health()), "worker", "ownership"), ownership)
			anyResult(e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "fixture", "artifact_handle", "fixture")))
			anyResult(e.client.JSON("/v1/adapters", "POST", jsonv.O("adapter_id", "fixture-lora", "artifact_handle", "fixture-lora")))
			request := func() *jsonv.Object {
				s := submit().Set("backend_id", backend).Set("backend_options", jsonv.O(backend, jsonv.O("precision", "float32")))
				s.Obj("parameters").Set("seed", 7.0).Set("steps", 2.0)
				return s
			}
			other := "native-sdxl"
			if backend == other {
				other = "owned-sdxl"
			}
			_, err := e.client.Submit(request().Set("backend_options", jsonv.O(other, jsonv.O("precision", "float32"))), "")
			rejects(t, err, 400, "unsupported_backend", "backend_options")
			batch := request()
			batch.Obj("parameters").Set("count", 2.0)
			accepted := obj(e.client.Submit(batch, "real-batch"))
			equal(t, obj(e.client.Submit(batch, "real-batch")).Str("job_id"), accepted.Str("job_id"))
			_, err = e.as(readerToken, "").Status(accepted.Str("job_id"))
			rejects(t, err, 404)
			done := wait(accepted.Str("job_id"), nil)
			equal(t, done.Str("state"), "succeeded", done.Get("error"))
			equal(t, len(done.Arr("outputs")), 2)
			manifest := anyResult(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
			equal(t, get(manifest, "backend", "backend_id"), backend)
			equal(t, get(manifest, "backend", "ownership"), ownership)
			equal(t, get(manifest, "backend", "protocol"), worker.Highest())
			events, err := e.client.Response("/v1/jobs/"+done.Str("job_id")+"/events", "GET", nil, map[string]string{"Last-Event-ID": "0"})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := io.ReadAll(events.Body)
			events.Body.Close()
			if err != nil || !strings.Contains(string(stream), "event: progress") || !strings.Contains(string(stream), "event: job_state") {
				t.Fatalf("real inference event stream: %v %s", err, stream)
			}
			var firstDigest string
			for i, raw := range done.Arr("outputs") {
				out := raw.(*jsonv.Object)
				response, err := e.client.Output(done.Str("job_id"), out.Str("output_id"))
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				equal(t, response.Header.Get("Content-Type"), "image/png")
				equal(t, c.SHA256Hex(data), out.Str("sha256"))
				if i == 0 {
					firstDigest = out.Str("sha256")
				}
			}
			// A committed result and idempotency key survive a Runtime restart.
			e.shutdown()
			e.boot()
			equal(t, obj(e.client.Submit(batch, "real-batch")).Str("job_id"), done.Str("job_id"))
			equal(t, obj(e.client.Status(done.Str("job_id"))).Str("state"), "succeeded")
			repeat := wait(obj(e.client.Submit(request(), "")).Str("job_id"), nil)
			equal(t, repeat.Str("state"), "succeeded", repeat.Get("error"))
			equal(t, get(repeat, "outputs", "0", "sha256"), firstDigest)
			image := obj(e.client.UploadInput(MakePNG(64, 64)))
			variation := request().Set("recipe_id", "variation").Set("inputs", jsonv.O("prompt", "test", "image", image.Str("input_id")))
			variation.Obj("parameters").Set("strength", 0.6)
			changed := wait(obj(e.client.Submit(variation, "")).Str("job_id"), nil)
			equal(t, changed.Str("state"), "succeeded", changed.Get("error"))
			equal(t, get(changed, "input", "sha256"), image.Get("sha256"))
			adapted := request().Set("adapters", []any{jsonv.O("adapter_id", "fixture-lora", "scale", 0.8)})
			lora := wait(obj(e.client.Submit(adapted, "")).Str("job_id"), nil)
			equal(t, lora.Str("state"), "succeeded", lora.Get("error"))
			if get(lora, "outputs", "0", "sha256") == firstDigest {
				t.Fatal("LoRA did not change generated pixels")
			}
			equal(t, get(anyResult(e.client.Manifest(lora.Str("job_id"))).(*jsonv.Object), "adapters", "0", "scale"), 0.8)
			anyResult(e.client.JSON("/v1/recipes", "POST", refined()))
			chain := request().Set("recipe_id", "refined").
				Set("models", jsonv.O("refine", "fixture")).
				Set("inputs", jsonv.O("prompt", "test", "refine_prompt", "window", "refine_steps", 2.0))
			chained := wait(obj(e.client.Submit(chain, "")).Str("job_id"), nil)
			equal(t, chained.Str("state"), "succeeded", chained.Get("error"))
			equal(t, len(chained.Arr("outputs")), 2)
			equal(t, len(chained.Arr("operations")), 2)
			long := request()
			long.Obj("parameters").Set("steps", 100.0)
			active := obj(e.client.Submit(long, ""))
			wait(active.Str("job_id"), hasExecution)
			obj(e.client.Cancel(active.Str("job_id")))
			cancelled := wait(active.Str("job_id"), nil)
			equal(t, cancelled.Str("state"), "cancelled", cancelled.Get("error"))
			equal(t, len(cancelled.Arr("outputs")), 0)
		})
	}
}
