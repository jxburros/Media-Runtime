package runtime

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/store"
	"testing"
)

func TestControlNetResolutionPinsNamespaceDimensionsAndCapability(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Register("controlnets", "edge", jsonv.O("controlnet_id", "edge", "digest", "sha256:weights"))
	db.InsertInput(jsonv.O("input_id", "input_control", "namespace", "owner", "width", 512.0, "height", 512.0, "expires_at", nil))
	r := &Runtime{Store: db}
	plan := &operationPlan{first: true, op: jsonv.O("task", "text-to-image"), model: jsonv.O("bundle", jsonv.O("family", "sdxl-base")), inputs: jsonv.O("control_image", "input_control")}
	req := jsonv.O("controlnets", []any{jsonv.O("controlnet_id", "edge", "scale", 0.5)})
	w := &workerView{minor: 4, caps: jsonv.O("controlnets", jsonv.O("families", []any{"sdxl-base"}, "tasks", []any{"text-to-image"}, "max_controlnets", 1.0))}
	opts := jsonv.O("offload", "none")
	effective := jsonv.O("width", 512.0, "height", 512.0)
	controls, input, err := r.resolveControlNets(plan, req, w, opts, effective, Caller{Namespace: "owner"})
	if err != nil || len(controls) != 1 || input == nil {
		t.Fatal(controls, input, err)
	}
	if _, _, err := r.resolveControlNets(plan, req, w, opts, effective, Caller{Namespace: "other"}); err == nil {
		t.Fatal("cross namespace input accepted")
	}
	if _, _, err := r.resolveControlNets(plan, req, w, opts, effective.Clone().Set("width", 1024.0), Caller{Namespace: "owner"}); err == nil {
		t.Fatal("mismatched dimensions accepted")
	}
	w.minor = 3
	if _, _, err := r.resolveControlNets(plan, req, w, opts, effective, Caller{Namespace: "owner"}); err == nil {
		t.Fatal("old protocol accepted")
	}
}
func TestControlNetLoadedIdentityIncludesWeightsAndScale(t *testing.T) {
	descriptor := jsonv.O("controlnet_id", "edge", "digest", c.Hash("weights"))
	op := jsonv.O("controlnets", []any{jsonv.O("controlnet", descriptor, "scale", 0.5)})
	summary := jsonv.O("controlnet_id", "edge", "digest", descriptor.Get("digest"), "scale", 0.5)
	loaded := jsonv.O("controlnets", []any{summary})
	if !controlNetsMatch(loaded, op) {
		t.Fatal("matching identity refused")
	}
	summary.Set("scale", 1.0)
	if controlNetsMatch(loaded, op) {
		t.Fatal("scale ignored")
	}
	summary.Set("scale", 0.5).Set("digest", c.Hash("different"))
	if controlNetsMatch(loaded, op) {
		t.Fatal("weights ignored")
	}
	if controlNetsMatch(jsonv.NewObject(), op) {
		t.Fatal("missing controls matched")
	}
}
