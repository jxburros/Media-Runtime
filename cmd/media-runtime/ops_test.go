package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jxburros/media-runtime/internal/integration"
)

func TestManagedLifecycleClosesOnStdinEOF(t *testing.T) {
	bin := binary(t)
	root := t.TempDir()
	fixture := integration.StartFixture(t, root)
	configPath := filepath.Join(root, "config.json")
	document := fmt.Sprintf(`{"directory":%q,"credentials":[{"token_env":"MEDIA_RUNTIME_TOKEN","namespace":"owner","operator":true}],"artifact_catalog":{},"worker":{"url":%q,"token_env":"MEDIA_TEST_WORKER_TOKEN"}}`, filepath.Join(root, "state"), fixture.URL)
	if err := os.WriteFile(configPath, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, bin, "serve", "--managed", "--port", "0", "--config", configPath)
	command.Env = append(os.Environ(), "MEDIA_RUNTIME_TOKEN="+token, "MEDIA_TEST_WORKER_TOKEN="+integration.WorkerToken)
	stdin, _ := command.StdinPipe()
	stdout, _ := command.StdoutPipe()
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	reader := bufio.NewReader(stdout)
	read := func() map[string]any {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err = json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	ready := read()
	if ready["type"] != "ready" || ready["port"].(float64) < 1 {
		t.Fatal(ready)
	}
	stdin.Close()
	if stopping := read(); stopping["type"] != "stopping" {
		t.Fatal(stopping)
	}
	if stopped := read(); stopped["type"] != "stopped" {
		t.Fatal(stopped)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if !fixture.Alive() {
		t.Fatal("managed shutdown terminated an external worker")
	}
}

func TestFoldersCommandDoesNotCopyOrDeleteModels(t *testing.T) {
	bin := binary(t)
	root := t.TempDir()
	fixture := integration.StartFixture(t, root)
	service := start(t, bin, root, filepath.Join(root, "state"), fixture)
	watched := filepath.Join(root, "comfy", "loras")
	os.MkdirAll(watched, 0700)
	model := filepath.Join(watched, "style.safetensors")
	os.WriteFile(model, []byte("fixture"), 0600)
	output, stderr, status := service.cli(t, "folders", "add", filepath.Dir(watched), "--json")
	if status != 0 {
		t.Fatal(output, stderr, status)
	}
	folder := lastJSON(t, output)
	id := folder.Str("folder_id")
	if id == "" {
		t.Fatal(output)
	}
	if out, err, status := service.cli(t, "folders", "list", "--json"); status != 0 {
		t.Fatal(out, err, status)
	}
	if out, err, status := service.cli(t, "folders", "remove", id, "--json"); status != 0 {
		t.Fatal(out, err, status)
	}
	data, err := os.ReadFile(model)
	if err != nil || string(data) != "fixture" {
		t.Fatal("source changed", err)
	}
}
