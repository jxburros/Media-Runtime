package integration

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/config"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestManagedKeysEnforcePolicyAndRevokeWithoutRestart(t *testing.T) {
	root := t.TempDir()
	fixture := StartFixture(t, root)
	t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	e := &env{t: t, root: root, fixture: fixture, cfg: &config.Config{
		Directory: filepath.Join(root, "runtime"), QueueCapacity: 8, CancellationGraceMS: 500, PollIntervalMS: 20, WorkerTimeoutMS: 1000,
		MaxOutputBytes: config.DefaultOutputLimits(), MaxInputBytes: 32 * 1024 * 1024,
		ArtifactCatalog: jsonv.O("fixture", filepath.Join(root, "model")), Worker: config.Worker{URL: fixture.URL, TokenEnv: "MEDIA_TEST_WORKER_TOKEN"},
		Tokens: []config.Token{{Token: token, Namespace: "owner", Operator: true}, {Token: readerToken, Namespace: "reader"}},
	}}
	e.boot()
	defer func() { e.shutdown() }()
	anyResult := must[any](t)
	obj := must[*jsonv.Object](t)
	anyResult(e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "fixture", "artifact_handle", "fixture")))
	policy := func() *jsonv.Object {
		return jsonv.O("name", "application", "namespace", "owner", "permissions", []any{"jobs.submit", "jobs.read", "models.read"}, "models", []any{"fixture"}, "allowed_namespaces", []any{"studio:*"}, "concurrency_limit", 1.0, "queue_limit", 2.0, "rate_limit", 1000.0)
	}
	create := func(p *jsonv.Object) *jsonv.Object {
		return anyResult(e.client.JSON("/v1/keys", "POST", p)).(*jsonv.Object)
	}
	key := create(policy())
	secret := key.Str("token")
	id := key.Obj("key").Str("key_id")
	cl := e.as(secret, "")
	if len(secret) < 64 {
		t.Fatal("key lacks entropy")
	}
	list := anyResult(e.client.JSON("/v1/keys", "GET", nil))
	if strings.Contains(string(jsonv.MustMarshal(list)), secret) || strings.Contains(string(jsonv.MustMarshal(list)), "token_hash") {
		t.Fatal("list discloses key material")
	}
	_, err := cl.JSON("/v1/keys", "GET", nil)
	rejects(t, err, 403)
	_, err = e.as(readerToken, "").JSON("/metrics", "GET", nil)
	rejects(t, err, 403)
	_, err = e.as(secret, "other").Jobs(1, 0, false)
	rejects(t, err, 403, "forbidden_namespace")
	_, err = cl.Submit(submit().Set("model_id", "other"), "")
	rejects(t, err, 403, "model_forbidden")
	_, err = cl.Submit(submit().Set("authorized_fallbacks", []any{"cpu"}), "")
	rejects(t, err, 403)
	accepted := obj(cl.Submit(submit(), "scoped"))
	done := e.wait(accepted.Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	if done.Has("key_id") {
		t.Fatal("job exposes credential identity")
	}
	e.shutdown()
	e.boot()
	cl = e.as(secret, "")
	equal(t, obj(cl.Status(done.Str("job_id"))).Str("state"), "succeeded")
	anyResult(e.client.JSON("/v1/keys/"+id, "DELETE", nil))
	_, err = cl.Jobs(1, 0, false)
	rejects(t, err, 401)
	// Hash-only persistence and immediate CIDR enforcement, without trusting headers.
	b, err := os.ReadFile(filepath.Join(root, "runtime", "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("plaintext token persisted")
	}
	blocked := create(policy().Set("cidrs", []any{"192.0.2.0/24"}))
	_, err = e.as(blocked.Str("token"), "").Jobs(1, 0, false)
	rejects(t, err, 401)
	limited := create(policy().Set("rate_limit", 1.0))
	limitedClient := e.as(limited.Str("token"), "")
	anyResult(limitedClient.Jobs(1, 0, false))
	_, err = limitedClient.Jobs(1, 0, false)
	rejects(t, err, 429, "key_rate_limited")
	// Pairing grants only its operator-selected policy and can be redeemed once.
	pairing := anyResult(e.client.JSON("/v1/pairing", "POST", policy())).(*jsonv.Object)
	redeemed := anyResult(e.as("", "").JSON("/v1/pairing/redeem", "POST", jsonv.O("code", pairing.Str("code")))).(*jsonv.Object)
	anyResult(e.as(redeemed.Str("token"), "").Jobs(1, 0, false))
	_, err = e.as("", "").JSON("/v1/pairing/redeem", "POST", jsonv.O("code", pairing.Str("code")))
	rejects(t, err, 401)
	// Private diagnostics and bounded audit have neither prompts nor credentials.
	diagnostics := anyResult(e.client.JSON("/v1/diagnostics", "GET", nil))
	text := string(jsonv.MustMarshal(diagnostics))
	for _, forbidden := range []string{"prompt", "outputs", "catalog", root, secret} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("diagnostics includes %q", forbidden)
		}
	}
	audit, err := os.ReadFile(filepath.Join(root, "runtime", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), "key.revoked") || strings.Contains(string(audit), secret) {
		t.Fatal("unsafe or incomplete audit")
	}
	req, _ := http.NewRequest("GET", e.client.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	equal(t, res.StatusCode, 200)
	if !strings.Contains(string(data), "media_runtime_jobs{") || !strings.Contains(string(data), "media_runtime_worker_busy{") {
		t.Fatal("metrics missing runtime observations")
	}
	req, _ = http.NewRequest("GET", e.client.URL+"/v1/jobs", nil)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.Header.Get("X-Request-Id") == "" || !strings.Contains(string(data), res.Header.Get("X-Request-Id")) {
		t.Fatal("request ID not correlated")
	}
}
