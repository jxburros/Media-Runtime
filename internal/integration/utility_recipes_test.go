package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Exercise the shipped recipe graphs through admission, real pixel operations,
// durable intermediate commits, and final manifest publication.
func TestExplicitHiresFixAndOutpaintRecipes(t *testing.T) {
	for _, name := range []string{"hires-fix", "outpaint"} {
		t.Run(name, func(t *testing.T) {
			e := bootLinear(t)
			data, err := os.ReadFile(filepath.Join("..", "..", "examples", "recipe."+name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonv.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			recipe := value.(*jsonv.Object)
			models := jsonv.NewObject()
			for i, raw := range recipe.Arr("operations") {
				op := raw.(*jsonv.Object)
				defaults := op.Obj("defaults")
				if defaults.Has("steps") {
					defaults.Set("steps", 2.0)
				}
				if defaults.Has("width") {
					defaults.Set("width", 128.0).Set("height", 128.0)
				}
				if name == "hires-fix" && i == 0 {
					defaults.Set("width", 64.0).Set("height", 64.0)
				}
				if defaults.Str("operation") == "pad" {
					defaults.Set("x", 32.0).Set("y", 32.0)
				}
				if i > 0 {
					models.Set(op.Str("op_id"), "fixture")
				}
			}
			must[any](t)(e.client.JSON("/v1/recipes", "POST", recipe))
			inputs := jsonv.O("prompt", "extend the landscape")
			if name == "outpaint" {
				input := must[*jsonv.Object](t)(e.client.UploadInput(MakePNG(64, 64)))
				mask := must[*jsonv.Object](t)(e.client.UploadInput(MakePNG(128, 128)))
				inputs.Set("image", input.Get("input_id")).Set("mask", mask.Get("input_id"))
			}
			request := jsonv.O("recipe_id", name, "model_id", "fixture", "models", models, "inputs", inputs)
			job := e.wait(must[*jsonv.Object](t)(e.client.Submit(request, "")).Str("job_id"), nil)
			equal(t, job.Str("state"), "succeeded", job.Get("error"))
			outputs := job.Arr("outputs")
			equal(t, len(outputs), 1)
			output := outputs[0].(*jsonv.Object)
			equal(t, output.Get("width"), 128.0)
			equal(t, output.Get("height"), 128.0)
			manifest := e.rt.Store.Get(job.Str("job_id"), "").Obj("manifest")
			equal(t, len(manifest.Arr("operations")), len(recipe.Arr("operations")))
		})
	}
}
