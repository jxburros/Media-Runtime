package integration

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jxburros/media-runtime/internal/api"
	"github.com/jxburros/media-runtime/internal/client"
	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/transport"
)

// remoteEntry is a pool entry for a fixture in network mode: mutual TLS with the test client
// certificate and CA, and an optional pin.
func remoteEntry(certs, url, pin string) config.Worker {
	return config.Worker{WorkerID: "remote", URL: url, TokenEnv: "MEDIA_TEST_WORKER_TOKEN", Remote: true, RemoteTimeoutMS: 5000,
		TLS: &config.WorkerTLS{CAFile: filepath.Join(certs, "ca.pem"), CertFile: filepath.Join(certs, "client.pem"),
			KeyFile: filepath.Join(certs, "client.key"), PinnedSHA256: pin}}
}

// startRemote launches a fixture in network mode under dir, presenting certs/<server>.pem.
func startRemote(t *testing.T, certs, dir, server string) *Fixture {
	t.Helper()
	t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	fixture := StartFixture(t, dir, NetworkMode(t, certs, server)...)
	if !strings.HasPrefix(fixture.URL, "https://") {
		t.Fatalf("the fixture did not start in network mode: %s", jsonv.MustMarshal(fixture.Discovery))
	}
	return fixture
}

func (e *env) report(t *testing.T) *jsonv.Object {
	t.Helper()
	report := must[*jsonv.Object](t)(e.client.Health())
	return report.Obj("worker")
}

func TestJobOverMutualTLSToARemoteWorker(t *testing.T) {
	certs := Certs(t)
	root := t.TempDir()
	fixture := startRemote(t, certs, filepath.Join(root, "w0"), "worker")
	fingerprint := CertificateSHA256(t, certs, "worker")
	equal(t, fixture.Discovery.Str("certificate_sha256"), fingerprint)
	e := &env{t: t, root: root, fixture: fixture, cfg: poolConfig(root, []config.Worker{remoteEntry(certs, fixture.URL, "")})}
	e.boot()
	t.Cleanup(func() { e.shutdown() })
	ok := must[*jsonv.Object](t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))

	t.Run("first contact records the fingerprint and doctor reports the transport", func(t *testing.T) {
		w := e.report(t)
		equal(t, w.Get("ready"), true, w)
		equal(t, w.Get("transport"), "mtls")
		equal(t, w.Get("remote"), true)
		equal(t, w.Get("owned_process"), false)
		equal(t, w.Get("ownership_available"), false)
		equal(t, w.Get("certificate_sha256"), fingerprint)
		equal(t, w.Get("presented_sha256"), fingerprint)
		equal(t, w.Get("certificate_source"), "first_contact")
		if latency, _ := w.Num("latency_ms"); latency <= 0 {
			t.Fatalf("latency_ms: %v", w.Get("latency_ms"))
		}
		equal(t, e.rt.Store.CertificateFingerprint("remote"), fingerprint)
		workers := e.workers(t)
		equal(t, workers[0].Get("transport"), "mtls")
		equal(t, workers[0].Get("certificate_sha256"), fingerprint)
	})

	t.Run("a job runs with identical semantics", func(t *testing.T) {
		accepted := ok(e.client.Submit(submit(), ""))
		done := e.wait(accepted.Str("job_id"), nil)
		equal(t, done.Str("state"), "succeeded", done.Get("error"))
		equal(t, done.Str("worker_id"), "remote")
		res, err := e.client.Output(done.Str("job_id"), "image")
		if err != nil {
			t.Fatal(err)
		}
		png, _ := io.ReadAll(res.Body)
		res.Body.Close()
		equal(t, string(png[:8]), "\x89PNG\r\n\x1a\n")
		manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
		equal(t, get(manifest, "worker_id"), "remote")
		equal(t, get(manifest, "worker", "test_fixture"), true)
	})

	t.Run("a restart reconciles over a reconnect held to the recorded fingerprint", func(t *testing.T) {
		accepted := ok(e.client.Submit(withSteps(25), ""))
		active := e.wait(accepted.Str("job_id"), hasExecution)
		e.shutdown()
		e.boot()
		if transport := e.rt.Worker.Transport(); transport.Expected != fingerprint || transport.Pinned {
			t.Fatalf("the recorded fingerprint was not reloaded: %+v", transport)
		}
		recovered := e.wait(accepted.Str("job_id"), nil)
		equal(t, recovered.Str("state"), "succeeded", recovered.Get("error"))
		equal(t, recovered.Str("attempt_id"), active.Str("attempt_id"))
		equal(t, fixture.Alive(), true)
	})

	t.Run("a cancellation past the deadline never terminates the remote worker", func(t *testing.T) {
		// phase 1 hides the worker's terminal state, as a worker that has not yet stopped would;
		// phase 2 lets it report a terminal state other than cancelled.
		var phase atomic.Int32
		e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
			o, isObject := result.(*jsonv.Object)
			if !isObject || !strings.HasPrefix(path, "/v1/executions/") || method == "DELETE" {
				return result, nil
			}
			switch phase.Load() {
			case 1:
				if c.Terminal(o.Str("state")) {
					return o.Clone().Set("state", "running"), nil
				}
			case 2:
				if o.Str("state") == "cancelled" {
					return o.Clone().Set("state", "succeeded"), nil
				}
			}
			return result, nil
		}, nil)
		defer e.rt.Worker.SetHooks(nil, nil)
		session := e.report(t).Get("session_id")
		accepted := ok(e.client.Submit(withSteps(100), ""))
		e.wait(accepted.Str("job_id"), running)
		phase.Store(1)
		equal(t, ok(e.client.Cancel(accepted.Str("job_id"))).Get("cancel_requested"), true)
		time.Sleep(time.Duration(e.cfg.CancellationGraceMS)*time.Millisecond + 700*time.Millisecond)
		job := ok(e.client.Status(accepted.Str("job_id")))
		equal(t, job.Str("state"), "running", "a remote job settled before the worker reported a terminal state")
		equal(t, fixture.Alive(), true)
		equal(t, e.report(t).Get("session_id"), session)
		phase.Store(2)
		done := e.wait(accepted.Str("job_id"), nil)
		equal(t, done.Str("state"), "failed")
		equal(t, get(done, "error", "code"), "execution_interrupted")
		equal(t, get(done, "error", "forced_termination"), false)
		equal(t, len(done.Arr("outputs")), 0)
		equal(t, fixture.Alive(), true)
		phase.Store(0)
		// Within the deadline a confirmed cancellation is a cancellation, as for any worker.
		accepted = ok(e.client.Submit(withSteps(100), ""))
		e.wait(accepted.Str("job_id"), running)
		ok(e.client.Cancel(accepted.Str("job_id")))
		equal(t, e.wait(accepted.Str("job_id"), nil).Str("state"), "cancelled")
	})

	t.Run("stopping the runtime leaves the remote worker running", func(t *testing.T) {
		e.shutdown()
		equal(t, fixture.Alive(), true)
		e.boot()
	})
}

func TestRemoteWorkerFingerprintMismatchIsRefused(t *testing.T) {
	certs := Certs(t)
	root := t.TempDir()
	first := startRemote(t, certs, filepath.Join(root, "w0"), "worker")
	rotated := startRemote(t, certs, filepath.Join(root, "w1"), "worker-rotated")
	original, replacement := CertificateSHA256(t, certs, "worker"), CertificateSHA256(t, certs, "worker-rotated")
	e := &env{t: t, root: root, cfg: poolConfig(root, []config.Worker{remoteEntry(certs, first.URL, "")})}
	boot := func(url, pin string) *jsonv.Object {
		t.Helper()
		e.cfg.Workers = []config.Worker{remoteEntry(certs, url, pin)}
		e.boot()
		t.Cleanup(e.shutdown)
		w := e.report(t)
		e.shutdown()
		return w
	}
	w := boot(first.URL, "")
	equal(t, w.Get("ready"), true, w)
	equal(t, w.Get("certificate_sha256"), original)

	t.Run("a different certificate from the same CA is refused after first contact", func(t *testing.T) {
		w := boot(rotated.URL, "")
		equal(t, w.Get("ready"), false)
		equal(t, w.Get("error"), "certificate_mismatch", w)
		equal(t, w.Get("certificate_sha256"), original)
		equal(t, w.Get("presented_sha256"), replacement)
		equal(t, w.Get("certificate_source"), "first_contact")
		if !strings.Contains(w.Str("message"), replacement) {
			t.Fatalf("message: %s", w.Str("message"))
		}
		// Nothing is admitted to a worker whose identity is refused.
		e.boot()
		_, err := e.client.Submit(submit(), "")
		var apiErr *client.Error
		if err == nil || !errors.As(err, &apiErr) || apiErr.Status < 400 {
			t.Fatalf("a submission against a refused worker: %v", err)
		}
		e.shutdown()
	})

	t.Run("a wrong pin is refused even for the recorded certificate", func(t *testing.T) {
		pin := CertificateSHA256(t, certs, "runtime")
		w := boot(first.URL, pin)
		equal(t, w.Get("error"), "certificate_mismatch", w)
		equal(t, w.Get("certificate_source"), "pinned")
		equal(t, w.Get("certificate_sha256"), pin)
		equal(t, w.Get("presented_sha256"), original)
	})

	t.Run("pinning the rotated certificate accepts it and records it", func(t *testing.T) {
		w := boot(rotated.URL, replacement)
		equal(t, w.Get("ready"), true, w)
		equal(t, w.Get("certificate_source"), "pinned")
		w = boot(rotated.URL, "")
		equal(t, w.Get("ready"), true, w)
		equal(t, w.Get("certificate_sha256"), replacement)
		w = boot(first.URL, "")
		equal(t, w.Get("error"), "certificate_mismatch", w)
	})
}

// caClient is an HTTP client trusting only the test CA.
func caClient(t *testing.T, certs string) *http.Client {
	t.Helper()
	roots, err := transport.CertPool(filepath.Join(certs, "ca.pem"), "ca")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
}

func TestRuntimeAPIOverTLS(t *testing.T) {
	certs := Certs(t)
	root := t.TempDir()
	t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	fixture := StartFixture(t, filepath.Join(root, "w0"))
	e := &env{t: t, root: root, fixture: fixture, cfg: poolConfig(root, []config.Worker{{WorkerID: "a", URL: fixture.URL, TokenEnv: "MEDIA_TEST_WORKER_TOKEN"}})}
	e.cfg.AllowedOrigins = []string{"https://studio.example"}
	e.boot()
	t.Cleanup(e.shutdown)
	material := &config.ServerTLS{CertFile: filepath.Join(certs, "runtime.pem"), KeyFile: filepath.Join(certs, "runtime.key")}
	server := api.New(e.rt)
	if err := server.Serve("127.0.0.1", 0, material); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if !strings.HasPrefix(server.URL(), "https://127.0.0.1:") || server.Fingerprint() != CertificateSHA256(t, certs, "runtime") {
		t.Fatalf("%s %s", server.URL(), server.Fingerprint())
	}
	secure := client.New(server.URL(), token, "")
	if err := secure.Secure(filepath.Join(certs, "ca.pem")); err != nil {
		t.Fatal(err)
	}
	ok := must[*jsonv.Object](t)
	raw := caClient(t, certs)
	status := func(method, url string, headers map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := raw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}

	t.Run("a client that trusts the CA and presents the token", func(t *testing.T) {
		equal(t, ok(secure.Health()).Str("runtime_version"), c.VERSION)
		must[any](t)(secure.JSON("/v1/models", "POST", jsonv.O("model_id", "fixture", "artifact_handle", "fixture")))
		accepted := ok(secure.Submit(submit(), ""))
		// The event stream is served over the same TLS listener until the job is terminal.
		res, err := secure.Response("/v1/jobs/"+accepted.Str("job_id")+"/events", "GET", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		stream, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.TLS == nil || !strings.Contains(string(stream), `"state":"succeeded"`) {
			t.Fatalf("stream: %s", stream)
		}
		equal(t, status("GET", server.URL()+"/v1/models", nil).StatusCode, 401)
		equal(t, status("GET", server.URL()+"/health", nil).StatusCode, 200)
	})

	t.Run("no certificate trust, no plain HTTP", func(t *testing.T) {
		_, err := client.New(server.URL(), token, "").Health()
		if err != client.ErrUnavailable {
			t.Fatalf("a client without the CA: %v", err)
		}
		res, err := http.Get(strings.Replace(server.URL(), "https://", "http://", 1) + "/health")
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == 200 {
				t.Fatalf("plain HTTP was served: %s", body)
			}
		}
		if err := client.New(strings.Replace(server.URL(), "127.0.0.1", "192.0.2.1", 1), token, "").Secure(""); err != nil {
			t.Fatalf("https to a non-loopback host needs no CA override: %v", err)
		}
		if err := client.New("http://192.0.2.1:7810", token, "").Secure(""); err == nil || !strings.Contains(err.Error(), "https://") {
			t.Fatalf("plain HTTP to a non-loopback runtime: %v", err)
		}
	})

	t.Run("allowed_origins is an exact allow-list", func(t *testing.T) {
		res := status("GET", server.URL()+"/health", map[string]string{"Origin": "https://studio.example"})
		equal(t, res.StatusCode, 200)
		equal(t, res.Header.Get("Access-Control-Allow-Origin"), "https://studio.example")
		res = status("OPTIONS", server.URL()+"/v1/jobs", map[string]string{"Origin": "https://studio.example", "Access-Control-Request-Method": "POST"})
		equal(t, res.StatusCode, 204)
		if !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Fatalf("preflight: %v", res.Header)
		}
		// An allowed origin still needs the credential.
		equal(t, status("GET", server.URL()+"/v1/models", map[string]string{"Origin": "https://studio.example"}).StatusCode, 401)
		for _, origin := range []string{"https://evil.example", "http://studio.example", "https://studio.example:8443", "null"} {
			res := status("GET", server.URL()+"/health", map[string]string{"Origin": origin})
			equal(t, res.StatusCode, 403, origin)
			equal(t, res.Header.Get("Access-Control-Allow-Origin"), "")
		}
	})

	t.Run("a non-loopback bind needs TLS", func(t *testing.T) {
		for _, bind := range []string{"0.0.0.0", "::", "192.0.2.1"} {
			plain := api.New(e.rt)
			if err := plain.Serve(bind, 0, nil); err == nil || !strings.Contains(err.Error(), "non-loopback") {
				t.Fatalf("plain HTTP on %s: %v", bind, err)
			}
		}
		if err := api.New(e.rt).Listen("0.0.0.0:0"); err == nil {
			t.Fatal("Listen accepted a non-loopback address")
		}
		wide := api.New(e.rt)
		if err := wide.Serve("0.0.0.0", 0, material); err != nil {
			t.Fatal(err)
		}
		defer wide.Close()
		equal(t, wide.Scheme(), "https")
		equal(t, status("GET", wide.URL()+"/health", nil).StatusCode, 200)
	})
}
