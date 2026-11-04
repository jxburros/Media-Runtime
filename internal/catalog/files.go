package catalog

import (
	"os"
	"strings"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func inspectFile(handle, path string, info os.FileInfo) *jsonv.Object {
	family, kind, complete := "sdxl-base", "single-file-checkpoint", false
	problems := []any{"Single-file checkpoint: convert it with media-engine convert before registering"}
	registration := jsonv.O("model_id", handle, "artifact_handle", handle)
	if strings.Contains(strings.ToLower(path), "lora") {
		family, kind, complete = "sdxl-base", "lora-safetensors", true
		registration = jsonv.O("adapter_id", handle, "artifact_handle", handle)
		problems = []any{}
	}
	if _, _, ok := networkConfig(path); ok {
		family, kind, complete = "esrgan", "single-file-safetensors", true
		registration.Set("family", "esrgan")
		problems = []any{}
	}
	if !strings.HasSuffix(path, ".safetensors") {
		complete = false
		problems = []any{"File handles must name .safetensors weights"}
	}
	var doc any
	if complete {
		doc = registration
	}
	return jsonv.O("artifact_handle", handle, "present", true, "family", family, "format", kind, "complete", complete, "file_count", 1.0, "total_bytes", float64(info.Size()), "problems", problems, "registration", doc)
}
