package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/integration"
)

// serveConfig writes a configuration document and runs serve on it; it returns the startup record
// or, when serve exits first, its combined output.
func serveConfig(t *testing.T, bin, root, doc string) (*service, map[string]any, string) {
	t.Helper()
	config := filepath.Join(root, "runtime.config.json")
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
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		_ = cmd.Wait()
		return nil, nil, stderr.String()
	}
	s := &service{bin: bin, cmd: cmd}
	t.Cleanup(s.stop)
	return s, record, ""
}

// cliWith runs one CLI command with extra environment entries.
func (s *service) cliWith(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = append(append(os.Environ(), "MEDIA_RUNTIME_TOKEN="+token, "MEDIA_RUNTIME_URL="+s.url), env...)
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

func TestNetworkModeFromTheCommandLine(t *testing.T) {
	bin := binary(t)
	certs := integration.Certs(t)
	root := t.TempDir()
	fixture := integration.StartFixture(t, filepath.Join(root, "w0"), integration.NetworkMode(t, certs, "worker")...)
	worker := integration.CertificateSHA256(t, certs, "worker")
	cert := func(name string) string { return filepath.Join(certs, name) }
	doc := fmt.Sprintf(`{"port":0,"bind":"127.0.0.1","tls":{"cert_file":%q,"key_file":%q},"directory":%q,"poll_interval_ms":20,
		"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner","operator":true}],"artifact_catalog":{"fixture":%q},
		"workers":[{"worker_id":"far","url":%q,"token_env":"MEDIA_TEST_WORKER_TOKEN","remote_timeout_ms":5000,
		  "tls":{"ca_file":%q,"cert_file":%q,"key_file":%q}}]}`,
		cert("runtime.pem"), cert("runtime.key"), filepath.Join(root, "state"), filepath.Join(root, "w0", "model"), fixture.URL,
		cert("ca.pem"), cert("client.pem"), cert("client.key"))
	s, record, failure := serveConfig(t, bin, root, doc)
	if s == nil {
		t.Fatalf("serve did not start: %s", failure)
	}
	if record["scheme"] != "https" || record["certificate_sha256"] != integration.CertificateSHA256(t, certs, "runtime") || record["bind"] != "127.0.0.1" {
		t.Fatalf("startup record %v", record)
	}
	s.url = fmt.Sprintf("https://127.0.0.1:%v", record["port"])
	ca := []string{"MEDIA_RUNTIME_CA=" + cert("ca.pem")}
	model := filepath.Join(root, "model.json")
	_ = os.WriteFile(model, []byte(`{"model_id":"fixture","artifact_handle":"fixture"}`), 0o600)
	if _, errText, code := s.cliWith(t, ca, "models", "register", model); code != 0 {
		t.Fatalf("register over TLS %d %s", code, errText)
	}
	out, _, code := s.cliWith(t, ca, "doctor")
	for _, want := range []string{
		"remote, never owned",
		"  Transport: mutual TLS; certificate sha256 " + worker + " (recorded on first contact); latency ",
		"Result: ready to generate.",
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("doctor %d: missing %q in\n%s", code, want, out)
		}
	}
	out, errText, code := s.cliWith(t, ca, "run", "portrait", "--model", "fixture", "--prompt", "remote", "--width", "64", "--height", "64", "--steps", "3", "--wait", "--json")
	if code != 0 || lastJSON(t, out).Str("state") != "succeeded" || lastJSON(t, out).Str("worker_id") != "far" {
		t.Fatalf("run over TLS %d %s %s", code, out, errText)
	}
	// Without the private CA the runtime's certificate is not trusted: unavailable, exit 3.
	if _, _, code := s.cliWith(t, nil, "doctor"); code != 3 {
		t.Fatalf("doctor without MEDIA_RUNTIME_CA exits 3, got %d", code)
	}
	if _, errText, code := s.cliWith(t, []string{"MEDIA_RUNTIME_URL=http://192.0.2.1:7810"}, "doctor"); code != 2 || !strings.Contains(errText, "https://") {
		t.Fatalf("plain HTTP to a non-loopback runtime exits 2, got %d %s", code, errText)
	}
	s.stop()
	if !fixture.Alive() {
		t.Fatal("stopping the runtime terminated the remote worker")
	}
}

func TestNonLoopbackPlainHTTPBindIsRefusedAtStartup(t *testing.T) {
	bin := binary(t)
	root := t.TempDir()
	for _, bind := range []string{"0.0.0.0", "192.0.2.1", "::"} {
		doc := fmt.Sprintf(`{"port":0,"bind":%q,"directory":%q,"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner","operator":true}],
			"artifact_catalog":{},"worker":{"url":"http://127.0.0.1:9","token_env":"MEDIA_TEST_WORKER_TOKEN"}}`, bind, filepath.Join(root, "state"))
		s, _, failure := serveConfig(t, bin, root, doc)
		if s != nil || !strings.Contains(failure, "Refusing to listen on non-loopback address") {
			t.Fatalf("%s: serve must refuse: %q", bind, failure)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "state")); !os.IsNotExist(err) {
		t.Fatal("a refused configuration created the state directory")
	}
}
