package integration

import (
	"github.com/jxburros/media-runtime/internal/jsonv"
	"path/filepath"
	"testing"
)

func TestControlNetRegistrationExecutionAndBoundPreprocessing(t *testing.T) {
	e := boot12(t, "--protocol", "media-worker/1.4")
	e.cfg.AddArtifacts(map[string]string{"fixture-controlnet": filepath.Join(e.root, "controlnet")})
	obj := must[*jsonv.Object](t)
	anyOK := must[any](t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	anyOK(e.client.JSON("/v1/controlnets", "POST", jsonv.O("controlnet_id", "edges", "artifact_handle", "fixture-controlnet")))
	input := obj(e.client.UploadInput(MakePNG(64, 64)))
	body := submit().Set("controlnets", []any{jsonv.O("controlnet_id", "edges", "scale", 0.5)})
	body.Obj("inputs").Set("control_image", input.Get("input_id"))
	job := obj(e.client.Submit(body, "controlnet-retry"))
	done := e.wait(job.Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	manifest := anyOK(e.client.Manifest(job.Str("job_id"))).(*jsonv.Object)
	equal(t, len(manifest.Arr("controlnets")), 1)
	equal(t, manifest.Arr("inputs")[0].(*jsonv.Object).Str("role"), "control_image")
	// The prep stage remains explicit, and its committed PNG feeds the next operation.
	recipe := mustObject(`{"schema_version":2,"recipe_id":"controlled","revision":1,"inputs":{"source":{"type":"image","required":true},"prompt":{"type":"string","required":true}},"operations":[{"op_id":"edges","task":"image-transform","model_family":"sdxl-base","inputs":{"image":{"from":"input:source"}},"defaults":{"operation":"canny","threshold_low":10,"threshold_high":20}},{"op_id":"generate","task":"text-to-image","model_family":"sdxl-base","inputs":{"prompt":{"from":"input:prompt"},"control_image":{"from":"op:edges/image"}},"defaults":{"width":64,"height":64,"steps":3},"controlnets":[{"controlnet_id":"edges","scale":1}]}]}`)
	anyOK(e.client.JSON("/v1/recipes", "POST", recipe))
	request := jsonv.O("recipe_id", "controlled", "model_id", "fixture", "models", jsonv.O("generate", "fixture"), "inputs", jsonv.O("source", input.Get("input_id"), "prompt", "bridge"))
	job = obj(e.client.Submit(request, ""))
	done = e.wait(job.Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	manifest = anyOK(e.client.Manifest(job.Str("job_id"))).(*jsonv.Object)
	final := manifest.Arr("operations")[1].(*jsonv.Object)
	equal(t, final.Obj("control_image_from").Str("op_id"), "edges")
	equal(t, len(final.Arr("controlnets")), 1)
}
