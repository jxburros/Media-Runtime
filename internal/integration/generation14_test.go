package integration

import (
	"github.com/jxburros/media-runtime/internal/jsonv"
	"testing"
)

func TestDiTFamiliesRegisterAndExecuteThroughWorker(t *testing.T) {
	e := boot12(t)
	ok := must[*jsonv.Object](t)
	for _, family := range []string{"flux-schnell", "sd35"} {
		t.Run(family, func(t *testing.T) {
			if !e.cfg.ArtifactCatalog.Has(family) {
				t.Skip("Engine fixture predates DiT family integration")
			}
			e.register(t, jsonv.O("model_id", family, "artifact_handle", family, "family", family))
			body := jsonv.O("recipe_id", family, "model_id", family, "inputs", jsonv.O("prompt", "test"), "parameters", jsonv.O("width", 64.0, "height", 64.0, "steps", 2.0, "seed", 7.0))
			bad := body.Clone()
			bad.Set("parameters", body.Obj("parameters").Clone().Set("width", 72.0).Set("height", 72.0))
			if _, err := e.client.Validate(bad); err == nil {
				t.Fatal("DiT accepted dimensions that the real model would round")
			}
			done := e.wait(ok(e.client.Submit(body, "")).Str("job_id"), nil)
			equal(t, done.Str("state"), "succeeded", done.Get("error"))
			equal(t, done.Obj("request").Obj("parameters").Str("sampler"), "flow-euler")
			equal(t, len(done.Arr("outputs")), 1)
			image := ok(e.client.UploadInput(MakePNG(64, 64)))
			mask := ok(e.client.UploadInput(MakePNG(64, 64)))
			recipe := jsonv.O("schema_version", 1.0, "recipe_id", family+"-inpaint", "revision", 1.0, "pipeline", "inpaint", "model_family", family,
				"inputs", jsonv.O("prompt", jsonv.O("type", "string", "required", true), "image", jsonv.O("type", "image", "required", true), "mask", jsonv.O("type", "image", "required", true)),
				"defaults", jsonv.O("steps", 2.0, "strength", 1.0, "guidance", 0.0, "sampler", "flow-euler", "scheduler", "flow-match"))
			if _, err := e.client.JSON("/v1/recipes", "POST", recipe); err != nil {
				t.Fatal(err)
			}
			body.Set("recipe_id", family+"-inpaint").Set("inputs", jsonv.O("prompt", "test", "image", image.Get("input_id"), "mask", mask.Get("input_id")))
			done = e.wait(ok(e.client.Submit(body, "")).Str("job_id"), nil)
			equal(t, done.Str("state"), "succeeded", done.Get("error"))
			equal(t, operation(done, 0).Obj("mask").Str("input_id"), mask.Str("input_id"))
		})
	}
}

func TestExplicitPixelResizeAndCannyThroughWorker(t *testing.T) {
	e := boot12(t)
	ok := must[*jsonv.Object](t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	input := ok(e.client.UploadInput(MakePNG(64, 64)))
	for _, parameters := range []*jsonv.Object{
		jsonv.O("operation", "resize", "filter", "nearest", "width", 128.0, "height", 128.0),
		jsonv.O("operation", "canny", "threshold_low", 0.1, "threshold_high", 0.3),
	} {
		// An explicit recipe avoids carrying resize-specific defaults into a different operation.
		recipe := jsonv.O("schema_version", 1.0, "recipe_id", "pixels-"+parameters.Str("operation"), "revision", 1.0, "pipeline", "image-transform", "model_family", "sdxl-base",
			"inputs", jsonv.O("image", jsonv.O("type", "image", "required", true)), "defaults", parameters)
		if _, err := e.client.JSON("/v1/recipes", "POST", recipe); err != nil {
			t.Fatal(err)
		}
		done := e.wait(ok(e.client.Submit(jsonv.O("recipe_id", recipe.Get("recipe_id"), "model_id", "fixture", "inputs", jsonv.O("image", input.Get("input_id"))), "")).Str("job_id"), nil)
		equal(t, done.Str("state"), "succeeded", done.Get("error"))
		equal(t, len(done.Arr("outputs")), 1)
	}
}
