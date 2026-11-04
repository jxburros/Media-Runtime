package runtime

import (
	"os"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/worker"
)

func (r *Runtime) resolveConditioning(plan *operationPlan, request *jsonv.Object, w *workerView, options, effective *jsonv.Object, caller Caller) ([]any, *jsonv.Object, error) {
	uses := plan.op.Arr("conditioning")
	if plan.first && request.Has("conditioning") {
		uses = request.Arr("conditioning")
	}
	task, family := plan.op.Str("task"), plan.model.Obj("bundle").Str("family")
	supported := w.minor >= 4 && w.caps.Str("backend_id") == "diffusers-sdxl" && family == "sdxl-base"
	if effective.Has("clip_skip") {
		skip, _ := effective.Num("clip_skip")
		found := false
		for _, raw := range w.caps.Arr("clip_skip") {
			if raw == skip {
				found = true
			}
		}
		if !supported || !c.Contains([]string{"text-to-image", "image-to-image", "inpaint"}, task) || !found {
			return nil, nil, c.Err(400, "unsupported", "Worker does not support CLIP skip for this operation", "parameters.clip_skip")
		}
	}
	if len(uses) == 0 {
		if plan.inputs.Has("reference_image") {
			return nil, nil, c.Err(400, "invalid_request", "reference_image requires IP-Adapter conditioning", "inputs.reference_image")
		}
		return []any{}, nil, nil
	}
	caps := w.caps.Obj("conditioning")
	max, _ := caps.Num("max_components")
	if !supported || caps == nil || float64(len(uses)) > max || options.Str("offload") != "none" {
		return nil, nil, c.Err(400, "unsupported", "Conditioning requires advertised reference SDXL support with offload none", "conditioning")
	}
	for _, key := range []string{"quantization", "compile"} {
		if value := options.Get(key); value != nil && value != "none" && value != false {
			return nil, nil, c.Err(400, "unsupported", "Conditioning requires default quantization and compilation options", "conditioning")
		}
	}
	selected := []any{}
	seen := map[string]bool{}
	kinds := map[string]bool{}
	hasIP := false
	for _, raw := range uses {
		use := raw.(*jsonv.Object)
		component := r.Store.Registry("conditioning", use.Str("conditioning_id"))
		if component == nil {
			return nil, nil, c.Err(400, "unknown_conditioning", "Register conditioning before using it", "conditioning")
		}
		kind, id := component.Str("kind"), component.Str("conditioning_id")
		if seen[id] || !c.Contains(strList(caps.Get("kinds")), kind) {
			return nil, nil, c.Err(400, "unsupported", "Conditioning IDs must be distinct and kinds advertised", "conditioning")
		}
		seen[id] = true
		key := kind
		switch kind {
		case "vae":
			if use.Has("scale") || use.Has("token") || use.Has("encoder") {
				return nil, nil, c.Err(400, "invalid_request", "VAE replacement takes no use options", "conditioning")
			}
		case "textual-inversion":
			if !use.Has("token") || !use.Has("encoder") || use.Has("scale") {
				return nil, nil, c.Err(400, "invalid_request", "Textual inversion requires token and encoder only", "conditioning")
			}
			key += "/" + use.Str("encoder")
		case "ip-adapter":
			if !use.Has("scale") || use.Has("token") || use.Has("encoder") || task != "text-to-image" {
				return nil, nil, c.Err(400, "invalid_request", "IP-Adapter requires scale and text-to-image", "conditioning")
			}
			if len(plan.op.Arr("controlnets")) > 0 || (plan.first && len(request.Arr("controlnets")) > 0) {
				return nil, nil, c.Err(400, "unsupported", "IP-Adapter cannot combine with ControlNet", "conditioning")
			}
			hasIP = true
		default:
			return nil, nil, c.Err(400, "unsupported", "Unknown conditioning kind", "conditioning")
		}
		if kinds[key] {
			return nil, nil, c.Err(400, "invalid_request", "Only one component of each kind/encoder may be loaded", "conditioning")
		}
		kinds[key] = true
		entry := use.Clone()
		entry.Delete("conditioning_id")
		entry.Set("component", component)
		selected = append(selected, entry)
	}
	var reference *jsonv.Object
	if hasIP {
		if !plan.inputs.Has("reference_image") {
			return nil, nil, c.Err(400, "invalid_request", "IP-Adapter requires an uploaded reference image", "inputs.reference_image")
		}
		var err error
		reference, err = r.OwnedInput(plan.inputs.Str("reference_image"), caller.Namespace)
		if err != nil {
			return nil, nil, err
		}
		if c.Expired(reference.Get("expires_at")) {
			return nil, nil, c.Err(410, "input_expired", "Reference image expired", "inputs.reference_image")
		}
	} else if plan.inputs.Has("reference_image") {
		return nil, nil, c.Err(400, "invalid_request", "reference_image requires IP-Adapter", "inputs.reference_image")
	}
	return selected, reference, nil
}

// manifestConditioning is also the worker's compact loaded identity.
func manifestConditioning(op *jsonv.Object) []any {
	out := []any{}
	for _, raw := range op.Arr("conditioning") {
		use := raw.(*jsonv.Object)
		component := use.Obj("component")
		entry := jsonv.O("conditioning_id", component.Get("conditioning_id"), "kind", component.Get("kind"), "digest", component.Get("digest"))
		for _, name := range []string{"scale", "token", "encoder"} {
			if use.Has(name) {
				entry.Set(name, use.Get(name))
			}
		}
		out = append(out, entry)
	}
	return out
}
func conditioningMatches(loaded, op *jsonv.Object) bool {
	return c.Hash(nullish(loaded.Get("conditioning"), []any{})) == c.Hash(manifestConditioning(op))
}
func (r *Runtime) uploadReference(client *worker.Client, job, op, inputs *jsonv.Object) error {
	reference := op.Obj("reference_image")
	if reference == nil {
		return nil
	}
	if op.Str("worker_reference_id") == "" {
		data, err := os.ReadFile(r.InputPath(reference.Str("input_id")))
		if err != nil || c.SHA256Hex(data) != reference.Str("sha256") {
			return c.Err(410, "input_unavailable", "Reference image changed before dispatch")
		}
		raw, err := client.JSON("/v1/inputs", "POST", data, "image/png")
		if err != nil {
			return err
		}
		uploaded, _ := raw.(*jsonv.Object)
		if uploaded.Str("sha256") != reference.Str("sha256") {
			return c.Err(503, "protocol_mismatch", "Worker reference image digest mismatch")
		}
		op.Set("worker_reference_id", uploaded.Get("input_id"))
		if err := r.save(job, nil); err != nil {
			return err
		}
	}
	inputs.Set("reference_image", op.Get("worker_reference_id"))
	return nil
}
