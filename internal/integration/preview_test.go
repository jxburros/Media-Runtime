package integration

import (
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/jxburros/media-runtime/internal/config"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestLivePreviewsAreNamespaceOwnedAndEphemeral(t *testing.T) {
	root := t.TempDir()
	fixture := StartFixture(t, root)
	if fixture.Highest() != "media-worker/1.4" {
		t.Skip("previews require Engine protocol1.4")
	}
	t.Setenv("MEDIA_TEST_WORKER_TOKEN", WorkerToken)
	e := &env{t: t, root: root, fixture: fixture, cfg: &config.Config{
		Directory: filepath.Join(root, "runtime"), QueueCapacity: 2, CancellationGraceMS: 2000, PollIntervalMS: 20, WorkerTimeoutMS: 1000,
		MaxOutputBytes: config.DefaultOutputLimits(), MaxInputBytes: 32 * 1024 * 1024,
		ArtifactCatalog: jsonv.O("fixture", filepath.Join(root, "model")),
		Worker:          config.Worker{URL: fixture.URL, TokenEnv: "MEDIA_TEST_WORKER_TOKEN"},
		Tokens:          []config.Token{{Token: token, Namespace: "owner", Operator: true}, {Token: readerToken, Namespace: "reader"}},
	}}
	e.boot()
	defer e.shutdown()
	obj := must[*jsonv.Object](t)
	must[any](t)(e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "fixture", "artifact_handle", "fixture")))
	request := withSteps(50)
	request.Obj("parameters").Set("preview_interval", 1.0)
	accepted := obj(e.client.Submit(request, ""))
	job := e.wait(accepted.Str("job_id"), func(job *jsonv.Object) bool { return len(job.Arr("previews")) > 0 })
	records := job.Arr("previews")
	preview := records[len(records)-1].(*jsonv.Object)
	path := "/v1/jobs/" + job.Str("job_id") + "/previews/" + preview.Str("preview_id")
	get := func(tok string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest("GET", e.client.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, data
	}
	status, data := get(token)
	equal(t, status, 200)
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("preview did not return PNG")
	}
	status, _ = get(readerToken)
	equal(t, status, 404)
	obj(e.client.Cancel(job.Str("job_id")))
	done := e.wait(job.Str("job_id"), nil)
	equal(t, len(done.Arr("outputs")), 0)
	status, _ = get(token)
	equal(t, status, 410)
}
