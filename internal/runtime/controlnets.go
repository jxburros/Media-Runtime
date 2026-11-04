package runtime

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/worker"
	"os"
)

func (r *Runtime) resolveControlNets(plan *operationPlan, request *jsonv.Object, w *workerView, options, effective *jsonv.Object, caller Caller) ([]any, *jsonv.Object, error) {
	raw := plan.op.Get("controlnets")
	if plan.first && request.Has("controlnets") {
		raw = request.Get("controlnets")
	}
	uses := asArray(raw)
	result := []any{}
	if len(uses) == 0 {
		if plan.inputs.Has("control_image") || plan.controlFrom != nil {
			return nil, nil, forbidden("inputs.control_image", "Control image requires a ControlNet")
		}
		return result, nil, nil
	}
	caps := w.caps.Obj("controlnets")
	max, _ := caps.Num("max_controlnets")
	if w.minor < 4 || plan.op.Str("task") != "text-to-image" || plan.model.Obj("bundle").Str("family") != "sdxl-base" || options.Str("offload") != "none" || !c.Contains(strList(caps.Get("tasks")), "text-to-image") || !c.Contains(strList(caps.Get("families")), "sdxl-base") || float64(len(uses)) > max {
		return nil, nil, c.Err(400, "unsupported", "Worker does not support this ControlNet task, family or offload mode", "controlnets")
	}
	for _, raw := range uses {
		use := raw.(*jsonv.Object)
		descriptor := r.Store.Registry("controlnets", use.Str("controlnet_id"))
		if descriptor == nil {
			return nil, nil, c.Err(400, "unknown_controlnet", "Register ControlNet before submitting", "controlnets")
		}
		result = append(result, jsonv.O("controlnet", descriptor, "scale", use.Get("scale")))
	}
	if plan.controlFrom != nil {
		width, _ := effective.Num("width")
		height, _ := effective.Num("height")
		if plan.controlSize != [2]float64{width, height} {
			return nil, nil, c.Err(400, "invalid_request", "Bound control image dimensions must equal output dimensions", "inputs.control_image")
		}
		return result, nil, nil
	}
	if !plan.inputs.Has("control_image") {
		return nil, nil, c.Err(400, "invalid_request", "ControlNet requires an uploaded control image", "inputs.control_image")
	}
	input, err := r.OwnedInput(plan.inputs.Str("control_image"), caller.Namespace)
	if err != nil {
		return nil, nil, err
	}
	if c.Expired(input.Get("expires_at")) {
		return nil, nil, c.Err(410, "input_expired", "Control image expired", "inputs.control_image")
	}
	if !jsonEqual(input.Get("width"), effective.Get("width")) || !jsonEqual(input.Get("height"), effective.Get("height")) {
		return nil, nil, c.Err(400, "invalid_request", "Control image dimensions must equal output dimensions", "inputs.control_image")
	}
	return result, input, nil
}

func manifestControlNets(op *jsonv.Object) []any {
	result := []any{}
	for _, raw := range op.Arr("controlnets") {
		use := raw.(*jsonv.Object)
		d := use.Obj("controlnet")
		result = append(result, jsonv.O("controlnet_id", d.Get("controlnet_id"), "digest", d.Get("digest"), "scale", use.Get("scale"), "source", jsonv.Maybe(d.Get("source"))))
	}
	return result
}
func controlNetsMatch(loaded, op *jsonv.Object) bool {
	desired := []any{}
	actual := []any{}
	for _, raw := range op.Arr("controlnets") {
		use := raw.(*jsonv.Object)
		d := use.Obj("controlnet")
		desired = append(desired, jsonv.O("controlnet_id", d.Get("controlnet_id"), "digest", d.Get("digest"), "scale", use.Get("scale")))
	}
	for _, raw := range loaded.Arr("controlnets") {
		d, _ := raw.(*jsonv.Object)
		actual = append(actual, jsonv.O("controlnet_id", d.Get("controlnet_id"), "digest", d.Get("digest"), "scale", d.Get("scale")))
	}
	return c.Hash(desired) == c.Hash(actual)
}

func (r *Runtime) uploadControlImage(client *worker.Client, job, op, inputs *jsonv.Object) error {
	record := op.Obj("control_image")
	from := op.Obj("control_image_from")
	if record == nil && from == nil {
		return nil
	}
	if op.Str("worker_control_id") == "" {
		var path, digest string
		if from != nil {
			output := operationOutput(job, from.Str("op_id"), from.Str("output_id"))
			if output == nil {
				return c.Err(410, "input_unavailable", "Control image producer has no committed output")
			}
			path, digest = r.OutputFile(job.Str("job_id"), output), output.Str("sha256")
		} else {
			path, digest = r.InputPath(record.Str("input_id")), record.Str("sha256")
		}
		data, err := os.ReadFile(path)
		if err != nil || c.SHA256Hex(data) != digest {
			return c.Err(410, "input_unavailable", "Control image changed or disappeared")
		}
		raw, err := client.JSON("/v1/inputs", "POST", data, "image/png")
		if err != nil {
			return err
		}
		uploaded, ok := raw.(*jsonv.Object)
		if !ok || uploaded.Str("sha256") != digest {
			return c.Err(503, "protocol_mismatch", "Worker control image digest mismatch")
		}
		op.Set("worker_control_id", uploaded.Get("input_id"))
		if err := r.save(job, nil); err != nil {
			return err
		}
	}
	inputs.Set("control_image", op.Get("worker_control_id"))
	return nil
}
