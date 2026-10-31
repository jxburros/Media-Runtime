package tui

import (
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

func parse(t *testing.T, text string) *jsonv.Object {
	t.Helper()
	v, err := jsonv.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsonv.Object)
}

const running = `{"job_id":"job_11111111-1111-1111-1111-111111111111","state":"running","stage":"sampling","task":"image-to-image","progress":{"stage":"sampling","completed_steps":2,"total_steps":4},"model":{"model_id":"studio-sdxl"},"backend":{"backend_id":"owned-sdxl"},"request":{"parameters":{"seed":42,"width":64,"height":64,"steps":4,"strength":0.6}},"created_at":"2026-10-26T10:00:00.000Z","outputs":[]}`

func TestDashboardRendersQueueStageModelBackendSeedOutputAndErrors(t *testing.T) {
	run := parse(t, running)
	done := run.Clone().Set("job_id", "job_22222222-2222-2222-2222-222222222222").Set("state", "succeeded").Set("stage", nil).
		Set("outputs", []any{jsonv.O("output_id", "image", "expires_at", nil)})
	done.Delete("progress")
	failed := run.Clone().Set("job_id", "job_33333333-3333-3333-3333-333333333333").Set("state", "failed").
		Set("error", jsonv.O("code", "execution_interrupted", "forced_termination", true))
	rt := parse(t, `{"runtime_version":"0.2.0","worker":{"backend_id":"owned-sdxl","ownership":"owned-pipeline","worker_version":"0.2.0","protocol":"media-worker/1.1","device":"cuda:0","device_ready":true,"state":"busy","memory_estimate":{"value_mb":6500,"source":"safetensors_headers"}},"queue":{"running":1,"queued":0,"capacity":32},"models":[{"model_id":"studio-sdxl","loaded":true,"available":true}]}`)
	lines := Render(State{Runtime: rt, Jobs: []*jsonv.Object{run, done, failed}, UpdatedAt: "10:00:00Z"}, 132, 20)
	if len(lines) != 20 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if width(l) > 132 {
			t.Fatalf("line too wide: %q", l)
		}
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{"owned-sdxl (owned-pipeline)", "1 running, 0 queued of 32", "studio-sdxl*", "sampling 2/4", "image-to-image", "output ready", "execution_interrupted (forced)", "strength 0.6", "6500 MiB"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	detail := strings.Join(Render(State{Jobs: []*jsonv.Object{done}}, 200, 12), "\n")
	if !strings.Contains(detail, "/v1/jobs/"+done.Str("job_id")+"/outputs/image") {
		t.Fatal(detail)
	}
	if s := DescribeStage(jsonv.O("state", "queued", "cancel_requested", true)); s != "cancelling" {
		t.Fatal(s)
	}
	if s := OutputLocation(jsonv.O("state", "succeeded", "outputs", []any{jsonv.O("output_id", "image", "expires_at", "2000-01-01T00:00:00Z")})); s != "expired" {
		t.Fatal(s)
	}
}

func TestDashboardMatchesTheTypeScriptLayout(t *testing.T) {
	// Produced by renderDashboard() of the TypeScript 0.3.0 release for the same state.
	lines := Render(State{Jobs: []*jsonv.Object{parse(t, running)}}, 60, 9)
	want := []string{
		"Media Runtime ?    [q] quit  [j/k] select  [c] cancel  [r] …",
		"worker: undefined (undefined) undefined  undefined  device …",
		"queue: unknown running, unknown queued of unknown  models: …",
		strings.Repeat("─", 60),
		"job            stage              task           model     …",
		">11111111-1111 sampling 2/4       image-to-image studio-sdx…",
		strings.Repeat("─", 60),
		"job_11111111-1111-1111-1111-111111111111  owned-sdxl  64×64…",
		strings.Repeat(" ", 60),
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestDashboardShowsEachWorkerOfAPool(t *testing.T) {
	job := parse(t, running).Set("worker_id", "gpu1")
	rt := parse(t, `{"runtime_version":"0.7.0","worker":{"worker_id":"gpu0"},"workers":[
		{"worker_id":"gpu0","backend_id":"diffusers-sdxl","ownership":"reference","worker_version":"0.4.0","protocol":"media-worker/1.2","device":"cuda:0","device_ready":true,"state":"ready"},
		{"worker_id":"gpu1","backend_id":"owned-sdxl","ownership":"owned-pipeline","worker_version":"0.4.0","protocol":"media-worker/1.2","device":"cuda:1","device_ready":true,"state":"busy"}],
		"queue":{"running":1,"queued":0,"capacity":32},"models":[]}`)
	lines := Render(State{Runtime: rt, Jobs: []*jsonv.Object{job}}, 132, 12)
	text := strings.Join(lines, "\n")
	for _, want := range []string{"worker gpu0: diffusers-sdxl (reference)", "worker gpu1: owned-sdxl (owned-pipeline)", "device cuda:1", "image-to-image gpu1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if !strings.HasPrefix(lines[5], "job ") || !strings.Contains(lines[5], "worker") {
		t.Fatalf("no worker column: %q", lines[5])
	}
}

func TestDashboardShowsVideoFramesAndDuration(t *testing.T) {
	decoding := parse(t, `{"job_id":"job_44444444-4444-4444-4444-444444444444","state":"running","stage":"decoding","task":"image-to-video","progress":{"stage":"decoding","scope":"stage","unit":"frames","completed_steps":8,"total_steps":25},"model":{"model_id":"svd-xt"},"request":{"parameters":{"seed":7,"width":1024,"height":576,"steps":25,"frames":25,"fps":7}},"created_at":"2026-10-30T10:00:00.000Z","outputs":[]}`)
	done := decoding.Clone().Set("job_id", "job_55555555-5555-5555-5555-555555555555").Set("state", "succeeded").Set("stage", nil).
		Set("media", jsonv.O("type", "video", "frames", 14.0, "fps", 7.0, "duration_seconds", 2.0)).
		Set("outputs", []any{jsonv.O("output_id", "frames", "expires_at", nil), jsonv.O("output_id", "poster", "expires_at", nil)})
	done.Delete("progress")
	lines := Render(State{Jobs: []*jsonv.Object{decoding, done}}, 160, 12)
	text := strings.Join(lines, "\n")
	if s := DescribeStage(decoding); s != "decoding 8/25 frames" {
		t.Fatal(s)
	}
	for _, want := range []string{"decoding 8/25", "25f 3.57s", "14f 2.00s", "2 outputs ready", "frames 25 at 7 fps"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if !strings.Contains(lines[4], "media") {
		t.Fatalf("no media column: %q", lines[4])
	}
	// Image-only dashboards keep their layout.
	if image := Render(State{Jobs: []*jsonv.Object{parse(t, running)}}, 160, 12); strings.Contains(image[4], "media") {
		t.Fatalf("an image job shows a media column: %q", image[4])
	}
}
