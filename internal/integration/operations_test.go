package integration

import (
	"testing"
	"time"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestIdleUnloadHonorsExplicitKeepLoaded(t *testing.T) {
	e := boot12(t)
	e.shutdown()
	e.cfg.Worker.IdleUnloadMinutes = 0.002
	e.boot()
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	obj := must[*jsonv.Object](t)
	kept := obj(e.client.Submit(submit().Set("keep_loaded", true), ""))
	done := e.wait(kept.Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	time.Sleep(250 * time.Millisecond)
	info := obj(e.rt.Worker.Obj("/v1/info", "GET", nil))
	if info.Get("loaded_bundle") == nil {
		t.Fatal("explicit keep_loaded was ignored")
	}
	released := obj(e.client.Submit(submit().Set("keep_loaded", false), ""))
	done = e.wait(released.Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info = obj(e.rt.Worker.Obj("/v1/info", "GET", nil))
		if info.Get("loaded_bundle") == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("idle model was not unloaded")
}
