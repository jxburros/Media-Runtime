package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestConditioningCatalogClosedFilesAndDigest(t *testing.T) {
	for kind, names := range conditioningFiles {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range names {
				file := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("fixture-component"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			request := jsonv.O("conditioning_id", "component", "kind", kind, "artifact_handle", "approved")
			catalog := jsonv.O("approved", root)
			original, err := BuildConditioning(request, catalog)
			if err != nil {
				t.Fatal(err)
			}
			if len(original.Obj("files").Keys()) != len(names) || original.Str("digest") == "" {
				t.Fatal("missing component provenance")
			}
			if err := os.WriteFile(filepath.Join(root, names[0]), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			changed, err := BuildConditioning(request, catalog)
			if err != nil || changed.Str("digest") == original.Str("digest") {
				t.Fatal("changed content kept its digest", err)
			}
			if err := os.WriteFile(filepath.Join(root, "code.py"), []byte("untrusted"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := BuildConditioning(request, catalog); err == nil {
				t.Fatal("unexpected code file accepted")
			}
		})
	}
}
func TestConditioningCatalogRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "embedding.safetensors")
	if err := os.WriteFile(outside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "embedding.safetensors")); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildConditioning(jsonv.O("conditioning_id", "ti", "kind", "textual-inversion", "artifact_handle", "approved"), jsonv.O("approved", root)); err == nil {
		t.Fatal("symlink accepted")
	}
}
