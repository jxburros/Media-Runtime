package catalog

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"os"
	"path/filepath"
	"testing"
)

func TestControlNetPinsExactFilesAndRejectsExtraOrLinkedWeights(t *testing.T) {
	root := t.TempDir()
	weights := filepath.Join(root, "diffusion_pytorch_model.safetensors")
	os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"_class_name":"ControlNetModel"}`), 0600)
	os.WriteFile(weights, []byte("weights"), 0600)
	registration := jsonv.O("controlnet_id", "edge", "artifact_handle", "edge")
	catalog := jsonv.O("edge", root)
	first, err := BuildControlNet(registration, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if first.Obj("files").Len() != 2 || first.Str("family") != "sdxl-base" {
		t.Fatal(first)
	}
	unhashed := first.Clone()
	unhashed.Delete("digest")
	if first.Str("digest") != c.Hash(unhashed) {
		t.Fatal("wrong descriptor digest")
	}
	os.WriteFile(weights, []byte("changed weights"), 0600)
	second, err := BuildControlNet(registration, catalog)
	if err != nil || first.Str("digest") == second.Str("digest") {
		t.Fatal("changed weights not pinned", err)
	}
	os.WriteFile(filepath.Join(root, "unsafe.py"), nil, 0600)
	if _, err := BuildControlNet(registration, catalog); err == nil {
		t.Fatal("extra code accepted")
	}
	os.Remove(filepath.Join(root, "unsafe.py"))
	os.Remove(weights)
	outside := filepath.Join(t.TempDir(), "weights")
	os.WriteFile(outside, nil, 0600)
	os.Symlink(outside, weights)
	if _, err := BuildControlNet(registration, catalog); err == nil {
		t.Fatal("linked weights accepted")
	}
}
