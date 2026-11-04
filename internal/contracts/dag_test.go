package contracts

import (
	"github.com/jxburros/media-runtime/internal/jsonv"
	"testing"
)

func dagFixture(t *testing.T) *jsonv.Object {
	t.Helper()
	v, e := jsonv.Parse([]byte(`{"schema_version":3,"recipe_id":"fanout","revision":1,"inputs":{"prompt":{"type":"string","required":true}},"operations":[{"op_id":"refine","task":"image-to-image","model_family":"sdxl-base","inputs":{"prompt":{"from":"input:prompt"}},"foreach":{"from":"op:generate/image"},"defaults":{"steps":2,"strength":0.5}},{"op_id":"generate","task":"text-to-image","model_family":"sdxl-base","inputs":{"prompt":{"from":"input:prompt"}},"defaults":{"count":2,"width":64,"height":64,"steps":2}}],"publish":["refine/image"]}`))
	if e != nil {
		t.Fatal(e)
	}
	return v.(*jsonv.Object)
}
func TestDAGExpandsBatchesInStableTopologicalOrder(t *testing.T) {
	recipe, e := Validate(RecipeSchema, dagFixture(t))
	if e != nil {
		t.Fatal(e)
	}
	request := jsonv.O("inputs", jsonv.O("prompt", "test"), "parameters", jsonv.O("count", 3.0))
	expanded, e := ExpandDAG(recipe, request)
	if e != nil {
		t.Fatal(e)
	}
	operations := expanded.Arr("operations")
	if len(operations) != 4 {
		t.Fatal(len(operations))
	}
	if operations[0].(*jsonv.Object).Str("op_id") != "generate" {
		t.Fatal("not topologically ordered")
	}
	if got := operations[3].(*jsonv.Object).Obj("inputs").Obj("image").Str("from"); got != "op:generate/image_3" {
		t.Fatal(got)
	}
	if len(expanded.Arr("publish")) != 3 || expanded.Arr("publish")[2] != "refine_item_3/image" {
		t.Fatal(expanded.Get("publish"))
	}
	if len(recipe.Arr("operations")) != 2 || recipe.Arr("operations")[0].(*jsonv.Object).Str("op_id") != "refine" {
		t.Fatal("expanded recipe mutated registration")
	}
}
func TestDAGRefusesCyclesAndMissingProducers(t *testing.T) {
	for _, from := range []string{"op:refine/image", "op:missing/image"} {
		r := dagFixture(t)
		r.Arr("operations")[0].(*jsonv.Object).Obj("foreach").Set("from", from)
		if _, e := Validate(RecipeSchema, r); e == nil {
			t.Fatal("accepted", from)
		}
	}
}
func TestDAGAllowsSeveralConsumersAndRefusesMissingBatchOutput(t *testing.T) {
	r := dagFixture(t)
	first := r.Arr("operations")[0].(*jsonv.Object)
	first.Delete("foreach")
	first.Obj("inputs").Set("image", jsonv.O("from", "op:generate/image_2"))
	copy := first.Clone().Set("op_id", "second")
	r.Set("operations", append(r.Arr("operations"), copy)).Set("publish", []any{"refine/image", "second/image"})
	recipe, e := Validate(RecipeSchema, r)
	if e != nil {
		t.Fatal(e)
	}
	request := jsonv.O("inputs", jsonv.O("prompt", "test"), "parameters", jsonv.O("count", 2.0))
	if _, e = ExpandDAG(recipe, request); e != nil {
		t.Fatal(e)
	}
	request.Obj("parameters").Set("count", 1.0)
	if _, e = ExpandDAG(recipe, request); e == nil {
		t.Fatal("accepted nonexistent image_2")
	}
}
func TestSchemaTwoStillRejectsForwardBindings(t *testing.T) {
	r := dagFixture(t)
	r.Set("schema_version", 2.0)
	first := r.Arr("operations")[0].(*jsonv.Object)
	first.Delete("foreach")
	first.Obj("inputs").Set("image", jsonv.O("from", "op:generate/image"))
	if _, e := Validate(RecipeSchema, r); e == nil {
		t.Fatal("legacy recipe changed semantics")
	}
}
