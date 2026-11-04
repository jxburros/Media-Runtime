package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestFileAdapterIgnoresNeighboringModels(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "style.safetensors")
	os.WriteFile(path, []byte("fixture weights"), 0600)
	os.WriteFile(filepath.Join(root, "unsafe.pth"), []byte("not part of handle"), 0600)
	adapter, err := BuildAdapter(jsonv.O("adapter_id", "style", "artifact_handle", "style"), jsonv.O("style", path))
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Obj("files").Len() != 1 || !adapter.Obj("files").Has("style.safetensors") {
		t.Fatal("adapter included neighboring files")
	}
}
func TestFileUpscalerUsesOnlyExplicitCompanion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "upscale.safetensors")
	os.WriteFile(path, []byte("fixture weights"), 0600)
	os.WriteFile(path+".json", []byte(`{"architecture":"rrdbnet","scale":2}`), 0600)
	os.WriteFile(filepath.Join(root, "neighbor.pth"), []byte("ignore"), 0600)
	model, err := BuildBundle(jsonv.O("model_id", "upscale", "artifact_handle", "upscale", "family", "esrgan"), jsonv.O("upscale", path))
	if err != nil {
		t.Fatal(err)
	}
	files := model.Obj("bundle").Obj("configuration").Obj("files")
	if files.Len() != 2 || !files.Has("config.json") {
		t.Fatal(files)
	}
}

func TestOnlyContentAddressedCacheLinksAreApproved(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "models--org--model")
	root := filepath.Join(repository, "snapshots", "0123456789012345678901234567890123456789")
	blobs := filepath.Join(repository, "blobs")
	os.MkdirAll(root, 0700)
	os.MkdirAll(blobs, 0700)
	blob := filepath.Join(blobs, "hash")
	os.WriteFile(blob, []byte("weights"), 0600)
	link := filepath.Join(root, "weights.safetensors")
	os.Symlink(blob, link)
	if !approvedSnapshotLink(root, link) {
		t.Fatal("safe snapshot link refused")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("private"), 0600)
	os.Remove(link)
	os.Symlink(outside, link)
	if approvedSnapshotLink(root, link) {
		t.Fatal("snapshot escape accepted")
	}
}
