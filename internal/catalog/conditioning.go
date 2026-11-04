package catalog

import (
	"io/fs"
	"os"
	"path/filepath"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

var conditioningFiles = map[string][]string{
	"vae":               {"config.json", "diffusion_pytorch_model.safetensors"},
	"textual-inversion": {"embedding.safetensors"},
	"ip-adapter":        {"ip_adapter.safetensors", "image_encoder/config.json", "image_encoder/model.safetensors", "preprocessor_config.json"},
}

// BuildConditioning hashes a closed set of non-executable regular files under an
// operator-approved directory. Engine validates their architecture and tensors.
func BuildConditioning(raw any, catalog *jsonv.Object) (*jsonv.Object, error) {
	data, err := c.Validate(c.ConditioningRegistration, raw)
	if err != nil {
		return nil, err
	}
	configured := catalog.Str(data.Str("artifact_handle"))
	if configured == "" {
		return nil, c.Err(400, "missing_component", "Artifact handle is not in the operator catalog", "artifact_handle")
	}
	root, err := realpath(configured)
	if err != nil {
		return nil, c.Err(400, "missing_component", "Approved conditioning directory is unavailable")
	}
	allowed := map[string]bool{}
	for _, name := range conditioningFiles[data.Str("kind")] {
		allowed[name] = true
	}
	files := jsonv.NewObject()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return c.Err(400, "unsupported", "Conditioning symlinks are not supported")
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !allowed[relative] || !info.Mode().IsRegular() {
			return c.Err(400, "unsupported", "Conditioning directory contains unsupported files")
		}
		digest, err := fileDigest(path)
		if err != nil {
			return err
		}
		files.Set(relative, digest)
		return nil
	})
	if err != nil {
		if api, ok := err.(*c.ApiError); ok {
			return nil, api
		}
		return nil, c.Err(400, "missing_component", "Cannot read approved conditioning directory")
	}
	if len(files.Keys()) != len(allowed) {
		return nil, c.Err(400, "missing_component", "Conditioning directory is missing required files")
	}
	record := jsonv.O("schema_version", 1.0, "conditioning_id", data.Get("conditioning_id"), "kind", data.Get("kind"), "artifact_handle", data.Get("artifact_handle"), "files", files, "source", data.Get("source"))
	return record.Clone().Set("digest", c.Hash(record)), nil
}
