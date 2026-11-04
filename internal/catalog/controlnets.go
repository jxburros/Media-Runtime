package catalog

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"os"
	"path/filepath"
)

// BuildControlNet hashes only a flat, approved SDXL ControlNet export. The worker
// independently checks its architecture and tensors before registration/admission.
func BuildControlNet(raw any, catalog *jsonv.Object) (*jsonv.Object, error) {
	data, err := c.Validate(c.ControlNetRegistration, raw)
	if err != nil {
		return nil, err
	}
	configured := catalog.Str(data.Str("artifact_handle"))
	if configured == "" {
		return nil, c.Err(400, "missing_component", "ControlNet handle is not approved", "artifact_handle")
	}
	root, err := realpath(configured)
	if err != nil {
		return nil, c.Err(400, "missing_component", "Approved ControlNet is unavailable")
	}
	names, err := readdir(root)
	if err != nil {
		return nil, c.Err(400, "missing_component", "Approved ControlNet directory is unavailable")
	}
	if len(names) != 2 {
		return nil, c.Err(400, "unsupported", "ControlNet requires exactly config.json and diffusion_pytorch_model.safetensors")
	}
	files := jsonv.NewObject()
	for _, name := range names {
		if name != "config.json" && name != "diffusion_pytorch_model.safetensors" {
			return nil, c.Err(400, "unsupported", "ControlNet contains an unsupported file")
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, c.Err(400, "unsupported", "ControlNet files must be regular files, without symlinks")
		}
		digest, err := fileDigest(path)
		if err != nil {
			return nil, c.Err(400, "missing_component", "Cannot read approved ControlNet")
		}
		files.Set(name, digest)
	}
	record := jsonv.O("schema_version", 1.0, "controlnet_id", data.Get("controlnet_id"), "artifact_handle", data.Get("artifact_handle"), "family", "sdxl-base", "files", files, "source", data.Get("source"))
	return record.Clone().Set("digest", c.Hash(record)), nil
}
