package contracts

import (
	"os/exec"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Exercise the translator's emitted documents against the real admission schemas, so changes
// to publish expansion, bounds, or sampler names cannot silently produce unusable imports.
func TestComfyUIImportMatchesContracts(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	cmd := exec.Command(python, "-c", `import json,sys
sys.path.insert(0,'../../tools')
from comfyui_import import translate
from test_comfyui_import import graph
source=graph()
source['4']['inputs']['batch_size']=4
print(json.dumps(translate(source,{'sdxl.safetensors':'sdxl'},'imported')))
`)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("translator: %v: %s", err, data)
	}
	value, err := jsonv.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	result := value.(*jsonv.Object)
	recipe, err := Validate(RecipeSchema, result.Get("recipe"))
	if err != nil {
		t.Fatalf("recipe contract: %v", err)
	}
	request, err := Validate(Submission, result.Get("request"))
	if err != nil {
		t.Fatalf("submission contract: %v", err)
	}
	if err := SubmittedInputs(recipe.Obj("inputs"), request.Obj("inputs")); err != nil {
		t.Fatal(err)
	}
}
