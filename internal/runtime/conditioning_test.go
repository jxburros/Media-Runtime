package runtime

import (
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestConditioningLoadIdentityIncludesEveryUseOption(t *testing.T) {
	op := jsonv.O("conditioning", []any{jsonv.O("component", jsonv.O("conditioning_id", "ti", "kind", "textual-inversion", "digest", "sha256:one"), "token", "<style>", "encoder", "text_encoder")})
	loaded := jsonv.O("conditioning", manifestConditioning(op))
	if !conditioningMatches(loaded, op) {
		t.Fatal("matching identity refused")
	}
	for _, field := range []string{"token", "encoder", "digest"} {
		copy := loaded.Clone()
		copy.Arr("conditioning")[0].(*jsonv.Object).Set(field, "changed")
		if conditioningMatches(copy, op) {
			t.Fatalf("%s omitted from identity", field)
		}
	}
	if conditioningMatches(jsonv.NewObject(), op) {
		t.Fatal("missing conditioning accepted")
	}
	if !conditioningMatches(jsonv.NewObject(), jsonv.NewObject()) {
		t.Fatal("legacy no-conditioning worker stopped matching")
	}
}

func TestClipSkipRequiresAdvertisedReferenceSupport(t *testing.T) {
	r := &Runtime{}
	plan := &operationPlan{op: jsonv.O("task", "text-to-image"), model: jsonv.O("bundle", jsonv.O("family", "sdxl-base")), inputs: jsonv.O("prompt", "x")}
	w := &workerView{minor: 4, caps: jsonv.O("backend_id", "diffusers-sdxl", "clip_skip", []any{1.0, 2.0})}
	if _, _, err := r.resolveConditioning(plan, jsonv.NewObject(), w, jsonv.O("offload", "none"), jsonv.O("clip_skip", 2.0), Caller{}); err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"owned-sdxl", "native-sdxl"} {
		w.caps.Set("backend_id", backend)
		if _, _, err := r.resolveConditioning(plan, jsonv.NewObject(), w, jsonv.O("offload", "none"), jsonv.O("clip_skip", 2.0), Caller{}); err == nil {
			t.Fatalf("unsupported %s accepted", backend)
		}
	}
	plan.inputs.Set("reference_image", "input_00000000000000000000000000000000")
	if _, _, err := r.resolveConditioning(plan, jsonv.NewObject(), w, jsonv.O("offload", "none"), jsonv.NewObject(), Caller{}); err == nil {
		t.Fatal("reference image without IP-Adapter accepted")
	}
}
