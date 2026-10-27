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
	doc := fmt.Sprintf(`{"port":0,"directory":%q,"poll_interval_ms":20,"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner","operator":true}],"artifact_catalog":{"fixture":%q},"worker":{"url":%q,"token_env":"MEDIA_TEST_WORKER_TOKEN"}}`,
		directory, filepath.Join(root, "model"), fixture.URL)
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

	if out, _, code := s.cli(t, "--version"); code != 0 || strings.TrimSpace(out) != "0.4.0" {
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
