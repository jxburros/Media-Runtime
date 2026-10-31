package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/integration"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

const token = "runtime-test-token-not-a-secret-000000000000"

// binary builds the media-runtime executable once per test run.
func binary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "media-runtime")
	build := exec.Command("go", "build", "-o", path, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return path
}

type service struct {
	bin, url string
	cmd      *exec.Cmd
}

// start serves a state directory against an externally managed fixture worker.
func start(t *testing.T, bin, root, directory string, fixture *integration.Fixture) *service {
	t.Helper()
	config := filepath.Join(root, "runtime.config.json")
	entries := jsonv.O("fixture", filepath.Join(root, "model"))
	// Only fixtures from Engine 0.4.0 create the upscaler and from 0.6.0 the video bundle;
	// an absent handle fails inspection.
	for _, handle := range []string{"upscaler", "video"} {
		if _, err := os.Stat(filepath.Join(root, handle)); err == nil {
			entries.Set(handle, filepath.Join(root, handle))
		}
	}
	catalog := string(jsonv.MustMarshal(entries))
	doc := fmt.Sprintf(`{"port":0,"directory":%q,"poll_interval_ms":20,"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner","operator":true}],"artifact_catalog":%s,"worker":{"url":%q,"token_env":"MEDIA_TEST_WORKER_TOKEN"}}`,
		directory, catalog, fixture.URL)
	if err := os.WriteFile(config, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", "--config", config)
	cmd.Env = append(os.Environ(), "MEDIA_RUNTIME_TOKEN="+token, "MEDIA_TEST_WORKER_TOKEN="+integration.WorkerToken)
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	var record struct {
		Service string
		Version string
		Port    int
	}
	if err := json.Unmarshal([]byte(line), &record); err != nil || record.Service != "media-runtime" {
		_ = cmd.Process.Kill()
		t.Fatalf("serve did not start: %q %s", line, stderr.String())
	}
	s := &service{bin: bin, url: fmt.Sprintf("http://127.0.0.1:%d", record.Port), cmd: cmd}
	t.Cleanup(s.stop)
	return s
}

func (s *service) stop() {
	if s.cmd.ProcessState != nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

// cli runs one CLI command and returns stdout, stderr and the exit code.
func (s *service) cli(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = append(os.Environ(), "MEDIA_RUNTIME_TOKEN="+token, "MEDIA_RUNTIME_URL="+s.url)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

func lastJSON(t *testing.T, text string) *jsonv.Object {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	v, err := jsonv.Parse([]byte(lines[len(lines)-1]))
	if err != nil {
		t.Fatalf("not JSON: %q", text)
	}
	return v.(*jsonv.Object)
}

func TestCommandLineAgainstTheEngineFixture(t *testing.T) {
	bin := binary(t)
	root := t.TempDir()
	fixture := integration.StartFixture(t, root)
	directory := filepath.Join(root, "state")
	s := start(t, bin, root, directory, fixture)

	if out, _, code := s.cli(t, "--version"); code != 0 || strings.TrimSpace(out) != c.VERSION {
		t.Fatalf("version %q %d", out, code)
	}
	if _, _, code := s.cli(t, "doctor"); code != 3 {
		t.Fatalf("doctor without a model exits 3, got %d", code)
	}
	if out, _, code := s.cli(t, "models", "inspect"); code != 0 || !strings.Contains(out, "fixture: complete SDXL base bundle") {
		t.Fatalf("inspect %d %s", code, out)
	}
	model := filepath.Join(root, "model.json")
	_ = os.WriteFile(model, []byte(`{"model_id":"fixture","artifact_handle":"fixture"}`), 0o600)
	if _, errText, code := s.cli(t, "models", "register", model); code != 0 {
		t.Fatalf("register %d %s", code, errText)
	}
	if out, _, code := s.cli(t, "doctor"); code != 0 || !strings.Contains(out, "Result: ready to generate.") {
		t.Fatalf("doctor %d %s", code, out)
	}
	out, errText, code := s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "cli", "--width", "64", "--height", "64", "--steps", "3", "--seed", "42", "--wait", "--json")
	if code != 0 {
		t.Fatalf("run %d %s %s", code, out, errText)
	}
	job := lastJSON(t, out)
	if job.Str("state") != "succeeded" {
		t.Fatalf("job %s", out)
	}
	exports := filepath.Join(root, "exports")
	if _, errText, code := s.cli(t, "outputs", "export", job.Str("job_id"), "--directory", exports); code != 0 {
		t.Fatalf("export %d %s", code, errText)
	}
	png, err := os.ReadFile(filepath.Join(exports, job.Str("job_id")+"-image.png"))
	if err != nil || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("exported image missing")
	}
	if _, _, code := s.cli(t, "outputs", "export", job.Str("job_id"), "--directory", exports); code != 2 {
		t.Fatalf("export refuses to overwrite, got %d", code)
	}
	out, _, code = s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "fail", "--width", "64", "--height", "64", "--steps", "3", "--wait", "--json")
	if code != 4 || lastJSON(t, out).Str("state") != "failed" {
		t.Fatalf("failed job exits 4, got %d %s", code, out)
	}
	if _, errText, code := s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "x", "--width", "65"); code != 2 || !strings.Contains(errText, "Invalid or unsupported request field") {
		t.Fatalf("invalid input exits 2, got %d %s", code, errText)
	}
	if out, _, code := s.cli(t, "recipes", "inspect", "variation"); code != 0 || !strings.Contains(out, `"strength": 0.6`) {
		t.Fatalf("recipes inspect %d %s", code, out)
	}
	if out, _, code := s.cli(t, "jobs", "list", "--json"); code != 0 || len(lastJSON(t, out).Arr("jobs")) != 2 {
		t.Fatalf("jobs list %d %s", code, out)
	}
	// A legacy single-worker configuration is a pool of one named "default".
	out, _, code = s.cli(t, "workers", "list", "--json")
	if workers, err := jsonv.Parse([]byte(strings.TrimSpace(out))); code != 0 || err != nil || len(workers.([]any)) != 1 ||
		workers.([]any)[0].(*jsonv.Object).Str("worker_id") != "default" {
		t.Fatalf("workers list %d %s", code, out)
	}
	out, errText, code = s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "pinned", "--width", "64", "--height", "64", "--steps", "3", "--worker", "default", "--wait", "--json")
	if code != 0 || lastJSON(t, out).Str("worker_id") != "default" {
		t.Fatalf("run --worker %d %s %s", code, out, errText)
	}
	if _, errText, code := s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "x", "--worker", "gpu9"); code != 2 || !strings.Contains(errText, "Worker gpu9 is not configured") {
		t.Fatalf("an unknown worker exits 2, got %d %s", code, errText)
	}
	if out, _, code := s.cli(t, "jobs", "list", "--worker", "default", "--json"); code != 0 || len(lastJSON(t, out).Arr("jobs")) != 3 {
		t.Fatalf("jobs list --worker %d %s", code, out)
	}
	// The state directory belongs to one live service.
	second := exec.Command(bin, "serve", "--config", filepath.Join(root, "runtime.config.json"))
	second.Env = append(os.Environ(), "MEDIA_RUNTIME_TOKEN="+token, "MEDIA_TEST_WORKER_TOKEN="+integration.WorkerToken)
	if text, err := second.CombinedOutput(); err == nil || !strings.Contains(string(text), "Another runtime owns this state directory") {
		t.Fatalf("second serve must refuse: %v %s", err, text)
	}
	s.stop()
	if _, err := os.Stat(filepath.Join(directory, "service.lock")); !os.IsNotExist(err) {
		t.Fatal("lock not released on SIGTERM")
	}
	if _, _, code := s.cli(t, "jobs", "list"); code != 3 {
		t.Fatalf("unavailable service exits 3, got %d", code)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errorsJoin(err, out.Close())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func errorsJoin(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// testdata/state-0.3.0 was written by the TypeScript 0.3.0 service against Engine 0.3.0's
// fixture: one registered model and one succeeded job with idempotency key "keep".
func TestServesATypeScriptStateDirectoryUnchanged(t *testing.T) {
	bin := binary(t)
	root := t.TempDir()
	fixture := integration.StartFixture(t, root)
	directory := filepath.Join(root, "state")
	copyTree(t, filepath.Join("..", "..", "testdata", "state-0.3.0"), directory)
	s := start(t, bin, root, directory, fixture)
	const old = "job_78493b9b-d206-489c-b549-e8eb6cf33419"
	out, errText, code := s.cli(t, "jobs", "list", "--json")
	if code != 0 {
		t.Fatalf("jobs list %d %s", code, errText)
	}
	jobs := lastJSON(t, out).Arr("jobs")
	if len(jobs) != 1 || jobs[0].(*jsonv.Object).Str("job_id") != old || jobs[0].(*jsonv.Object).Str("state") != "succeeded" {
		t.Fatalf("jobs %s", out)
	}
	exports := filepath.Join(root, "exports")
	if _, errText, code := s.cli(t, "outputs", "export", old, "--directory", exports); code != 0 {
		t.Fatalf("export %d %s", code, errText)
	}
	manifest, err := os.ReadFile(filepath.Join(exports, old+"-manifest.json"))
	if err != nil || !strings.Contains(string(manifest), `"runtime_version": "0.3.0"`) {
		t.Fatal("0.3.0 manifest not served unchanged")
	}
	// The idempotency key recorded by 0.3.0 still maps the same request to the old job.
	out, _, code = s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "migrated", "--width", "64", "--height", "64", "--steps", "3", "--seed", "7", "--idempotency-key", "keep", "--json")
	if code != 0 || lastJSON(t, out).Str("job_id") != old {
		t.Fatalf("idempotent replay %d %s", code, out)
	}
	out, errText, code = s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "new", "--width", "64", "--height", "64", "--steps", "3", "--wait", "--json")
	if code != 0 || lastJSON(t, out).Str("state") != "succeeded" {
		t.Fatalf("new job on migrated state %d %s %s", code, out, errText)
	}
}

func TestCommandLineMultipleOutputsAndUpscale(t *testing.T) {
	bin := binary(t)
	root := t.TempDir()
	fixture := integration.StartFixture(t, root)
	if _, err := os.Stat(filepath.Join(root, "upscaler")); err != nil {
		t.Skip("the Engine fixture predates protocol 1.2")
	}
	s := start(t, bin, root, filepath.Join(root, "state"), fixture)
	for name, doc := range map[string]string{
		"model.json":    `{"model_id":"fixture","artifact_handle":"fixture"}`,
		"upscaler.json": `{"model_id":"upscaler","artifact_handle":"upscaler","family":"esrgan"}`,
	} {
		path := filepath.Join(root, name)
		_ = os.WriteFile(path, []byte(doc), 0o600)
		if _, errText, code := s.cli(t, "models", "register", path); code != 0 {
			t.Fatalf("register %s %d %s", name, code, errText)
		}
	}
	if out, _, code := s.cli(t, "doctor"); code != 0 || !strings.Contains(out, "Model upscaler (upscaler, esrgan ×2)") || !strings.Contains(out, "Device identity:") {
		t.Fatalf("doctor %d %s", code, out)
	}
	out, errText, code := s.cli(t, "run", "portrait", "--model", "fixture", "--prompt", "batch", "--width", "64", "--height", "64", "--steps", "3", "--count", "3", "--wait", "--json")
	if code != 0 {
		t.Fatalf("run --count %d %s %s", code, out, errText)
	}
	job := lastJSON(t, out)
	exports := filepath.Join(root, "exports")
	if _, errText, code := s.cli(t, "outputs", "export", job.Str("job_id"), "--directory", exports); code != 0 {
		t.Fatalf("export %d %s", code, errText)
	}
	for _, id := range []string{"image", "image_2", "image_3"} {
		if _, err := os.Stat(filepath.Join(exports, job.Str("job_id")+"-"+id+".png")); err != nil {
			t.Fatalf("export lacks %s", id)
		}
	}
	png := filepath.Join(root, "in.png")
	_ = os.WriteFile(png, integration.MakePNG(64, 64), 0o600)
	out, errText, code = s.cli(t, "run", "enlarge", "--model", "upscaler", "--image", png, "--tile", "0", "--wait", "--json")
	if code != 0 {
		t.Fatalf("run enlarge %d %s %s", code, out, errText)
	}
	enlarged := lastJSON(t, out)
	if output := enlarged.Arr("outputs")[0].(*jsonv.Object); output.Get("width") != 128.0 {
		t.Fatalf("enlarge %s", out)
	}
	if _, errText, code := s.cli(t, "run", "enlarge", "--model", "upscaler", "--image", png, "--seed", "3"); code != 2 || !strings.Contains(errText, "no seed") {
		t.Fatalf("enlarge --seed must be refused: %d %s", code, errText)
	}

	// A linear recipe: generate, then enlarge, from the shipped example.
	if _, errText, code := s.cli(t, "recipes", "register", filepath.Join("..", "..", "examples", "recipe.portrait-4k.json")); code != 0 {
		t.Fatalf("register portrait-4k %d %s", code, errText)
	}
	if out, _, code := s.cli(t, "recipes", "inspect", "portrait-4k"); code != 0 || !strings.Contains(out, `"from": "op:generate/image"`) {
		t.Fatalf("recipes inspect portrait-4k %d %s", code, out)
	}
	if _, errText, code := s.cli(t, "run", "portrait-4k", "--model", "fixture", "--prompt", "x"); code != 2 || !strings.Contains(errText, "Select a model for operation enlarge") {
		t.Fatalf("a missing --model-for must be refused: %d %s", code, errText)
	}
	// The default tile is 512; the fixture's 128-pixel result needs no tiling.
	out, errText, code = s.cli(t, "run", "portrait-4k", "--model", "fixture", "--model-for", "enlarge=upscaler", "--input", "prompt=a harbour",
		"--width", "64", "--height", "64", "--steps", "3", "--wait", "--json")
	if code != 0 {
		t.Fatalf("run portrait-4k %d %s %s", code, out, errText)
	}
	linear := lastJSON(t, out)
	if linear.Str("current_op_id") != "enlarge" || len(linear.Arr("outputs")) != 1 || linear.Arr("outputs")[0].(*jsonv.Object).Get("width") != 128.0 {
		t.Fatalf("portrait-4k %s", out)
	}
	linearExports := filepath.Join(root, "linear")
	if _, errText, code := s.cli(t, "outputs", "export", linear.Str("job_id"), "--directory", linearExports); code != 0 {
		t.Fatalf("export portrait-4k %d %s", code, errText)
	}
	if entries, _ := os.ReadDir(linearExports); len(entries) != 2 {
		t.Fatalf("export writes the published image and the manifest, got %d files", len(entries))
	}
}

func TestCommandLineVideo(t *testing.T) {
	bin := binary(t)
	root := t.TempDir()
	fixture := integration.StartFixture(t, root)
	if _, err := os.Stat(filepath.Join(root, "video")); err != nil {
		t.Skip("the Engine fixture predates image-to-video (Engine 0.6.0)")
	}
	s := start(t, bin, root, filepath.Join(root, "state"), fixture)
	if out, _, code := s.cli(t, "models", "inspect", "video"); code != 0 || !strings.Contains(out, "video: complete svd image-to-video bundle") || !strings.Contains(out, `"family":"svd"`) {
		t.Fatalf("inspect video %d %s", code, out)
	}
	// The shipped example, pointed at the fixture's handle.
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "model.svd.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(strings.Replace(string(example), `"artifact_handle": "svd-xt"`, `"artifact_handle": "video"`, 1), `"model_id": "svd-xt"`, `"model_id": "svd"`, 1)
	path := filepath.Join(root, "svd.json")
	_ = os.WriteFile(path, []byte(doc), 0o600)
	if out, errText, code := s.cli(t, "models", "register", path); code != 0 || !strings.Contains(out, `"default_frames": 2`) {
		t.Fatalf("register svd %d %s %s", code, out, errText)
	}
	_ = os.WriteFile(filepath.Join(root, "model.json"), []byte(`{"model_id":"fixture","artifact_handle":"fixture"}`), 0o600)
	if _, errText, code := s.cli(t, "models", "register", filepath.Join(root, "model.json")); code != 0 {
		t.Fatalf("register fixture %d %s", code, errText)
	}
	if out, _, code := s.cli(t, "doctor"); code != 0 || !strings.Contains(out, "Model svd (video, svd)") || !strings.Contains(out, "video sizes 64×64, 1024×576, 576×1024; default 2 frames") || !strings.Contains(out, "image-to-video") {
		t.Fatalf("doctor %d %s", code, out)
	}
	png := filepath.Join(root, "frame.png")
	_ = os.WriteFile(png, integration.MakePNG(64, 64), 0o600)
	out, errText, code := s.cli(t, "run", "animate", "--model", "svd", "--image", png, "--frames", "3", "--fps", "6", "--steps", "2", "--wait", "--json")
	if code != 0 {
		t.Fatalf("run animate %d %s %s", code, out, errText)
	}
	job := lastJSON(t, out)
	if media := job.Obj("media"); media.Get("frames") != 3.0 || media.Get("fps") != 6.0 || media.Get("duration_seconds") != 0.5 {
		t.Fatalf("animate %s", out)
	}
	if _, errText, code := s.cli(t, "run", "animate", "--model", "svd", "--image", png, "--prompt", "x"); code != 2 || !strings.Contains(errText, "no prompt") {
		t.Fatalf("animate --prompt must be refused: %d %s", code, errText)
	}
	exports := filepath.Join(root, "exports")
	if _, errText, code := s.cli(t, "outputs", "export", job.Str("job_id"), "--directory", exports); code != 0 {
		t.Fatalf("export %d %s", code, errText)
	}
	for _, name := range []string{"frames.zip", "poster.png", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(exports, job.Str("job_id")+"-"+name)); err != nil {
			t.Fatalf("export lacks %s", name)
		}
	}
	if entries, _ := os.ReadDir(exports); len(entries) != 3 {
		t.Fatalf("export writes the archive, the poster and the manifest, got %d files", len(entries))
	}
}
