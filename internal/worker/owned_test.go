package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jxburros/media-runtime/internal/config"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestOwnedCatalogComesOnlyFromRuntime(t *testing.T) {
	cfg := &config.Config{Directory: t.TempDir(), ArtifactCatalog: jsonv.O("approved", "/models/approved")}
	client := New(cfg, config.Worker{WorkerID: "gpu0"})
	path, err := client.writeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"approved":"/models/approved"}` {
		t.Fatal(string(data))
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("catalog is not private")
	}
	cfg.AddArtifacts(map[string]string{"approved": "/other", "new": "/models/new"})
	client.writeCatalog()
	data, _ = os.ReadFile(path)
	if string(data) != `{"approved":"/models/approved","new":"/models/new"}` {
		t.Fatal("configured catalog identity changed", string(data))
	}
}
func TestReaperSignalsOnlyMatchingProcessIdentity(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	defer child.Wait()
	cfg := &config.Config{Directory: t.TempDir()}
	client := New(cfg, config.Worker{WorkerID: "gpu0"})
	if err := client.recordProcess(child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(client.pidFile())
	var record processIdentity
	json.Unmarshal(data, &record)
	record.Start = "wrong-start-time"
	data, _ = json.Marshal(record)
	os.WriteFile(client.pidFile(), data, 0600)
	if err := client.reapOrphan(); err != nil {
		t.Fatal(err)
	}
	if _, err := processInfo(child.Process.Pid); err != nil {
		t.Fatal("reaper killed mismatched PID", err)
	}
	if err := client.recordProcess(child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := client.reapOrphan(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Directory, "workers", "gpu0.pid.json")); !os.IsNotExist(err) {
		t.Fatal("PID record retained", err)
	}
}
