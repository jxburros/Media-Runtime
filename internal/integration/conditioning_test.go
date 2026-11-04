package integration

import (
	"path/filepath"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestConditioningRegistrationAdmissionAndProvenance(t *testing.T) {
	e := boot12(t)
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	handles := map[string]string{}
	for _, name := range []string{"conditioning-vae", "conditioning-ti", "conditioning-ip"} {
		handles[name] = filepath.Join(e.root, name)
	}
	e.cfg.AddArtifacts(handles)
	for _, entry := range []struct{ id, kind string }{{"conditioning-vae", "vae"}, {"conditioning-ti", "textual-inversion"}, {"conditioning-ip", "ip-adapter"}} {
		registered := must[any](t)(e.client.JSON("/v1/conditioning", "POST", jsonv.O("conditioning_id", entry.id, "kind", entry.kind, "artifact_handle", entry.id))).(*jsonv.Object)
		equal(t, registered.Str("kind"), entry.kind)
		if registered.Str("digest") == "" {
			t.Fatal("missing component digest")
		}
	}
	reference := must[*jsonv.Object](t)(e.client.UploadInput(MakePNG(64, 64)))
	request := submit().Set("conditioning", []any{
		jsonv.O("conditioning_id", "conditioning-vae"),
		jsonv.O("conditioning_id", "conditioning-ti", "token", "<style>", "encoder", "text_encoder"),
		jsonv.O("conditioning_id", "conditioning-ip", "scale", 0.6),
	})
	request.Obj("inputs").Set("reference_image", reference.Get("input_id"))
	request.Obj("parameters").Set("clip_skip", 2.0)
	job := e.wait(must[*jsonv.Object](t)(e.client.Submit(request, "")).Str("job_id"), nil)
	equal(t, job.Str("state"), "succeeded", job.Get("error"))
	equal(t, len(operation(job, 0).Arr("conditioning")), 3)
	if operation(job, 0).Obj("reference_image").Has("namespace") {
		t.Fatal("reference image exposes namespace")
	}
	if operation(job, 0).Arr("conditioning")[0].(*jsonv.Object).Has("component") {
		t.Fatal("public job exposes full conditioning descriptor")
	}
	manifest := e.rt.Store.Get(job.Str("job_id"), "").Obj("manifest")
	equal(t, len(manifest.Arr("conditioning")), 3)
	equal(t, manifest.Obj("effective_settings").Get("clip_skip"), 2.0)
	found := false
	for _, raw := range manifest.Arr("inputs") {
		if raw.(*jsonv.Object).Str("role") == "reference_image" {
			found = true
		}
	}
	if !found {
		t.Fatal("reference input missing from provenance")
	}
	// Clearing conditioning is a load identity change; no component leaks into the next job.
	plain := e.wait(must[*jsonv.Object](t)(e.client.Submit(submit(), "")).Str("job_id"), nil)
	equal(t, plain.Str("state"), "succeeded", plain.Get("error"))
	equal(t, len(operation(plain, 0).Arr("conditioning")), 0)
	for _, bad := range []*jsonv.Object{
		request.Clone().Set("conditioning", []any{jsonv.O("conditioning_id", "conditioning-vae", "scale", 1.0)}),
		request.Clone().Set("conditioning", []any{jsonv.O("conditioning_id", "conditioning-ti", "token", "<style>")}),
		request.Clone().Set("conditioning", []any{jsonv.O("conditioning_id", "conditioning-ip")}),
		request.Clone().Set("conditioning", []any{jsonv.O("conditioning_id", "conditioning-vae"), jsonv.O("conditioning_id", "conditioning-vae")}),
	} {
		if _, err := e.client.Validate(bad); err == nil {
			t.Fatal("invalid conditioning use accepted")
		}
	}
	missing := request.Clone()
	missing.Obj("inputs").Delete("reference_image")
	_, err := e.client.Validate(missing)
	rejects(t, err, 400, "invalid_request", "inputs.reference_image")
	foreign := e.as(token, "other")
	_, err = foreign.Submit(request, "")
	rejects(t, err, 404)
}
