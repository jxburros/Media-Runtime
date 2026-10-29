package runtime

import (
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/worker"
)

func parsed(t *testing.T, text string) *jsonv.Object {
	t.Helper()
	v, err := jsonv.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsonv.Object)
}

// lane is an idle, ready 1.2 worker with an optional loaded bundle digest.
func lane(t *testing.T, id, loaded string) Lane {
	info := parsed(t, `{"state":"ready","service_ready":true,"loaded_bundle":null}`)
	if loaded != "" {
		info.Set("loaded_bundle", parsed(t, `{"bundle_digest":"`+loaded+`","options":{"precision":"float32"},"adapters":[]}`))
	}
	caps := parsed(t, `{"ready":true,"backend_id":"diffusers-sdxl","tasks":["text-to-image","image-to-image","upscale"],
		"loaders":[{"families":["sdxl-base"]},{"families":["esrgan"]}]}`)
	return Lane{ID: id, Idle: true, Probe: &worker.Probe{Info: info, Caps: caps, Protocol: "media-worker/1.2"}}
}

// queued is a queued text-to-image job for a bundle digest, optionally pinned and with candidates.
func queued(t *testing.T, id, digest, pin string, candidates ...string) *jsonv.Object {
	job := parsed(t, `{"job_id":"`+id+`","state":"queued","current_operation":0,
		"request":{"backend_options":{"diffusers-sdxl":{"precision":"float32"}}},
		"operations":[{"op_id":"portrait","task":"text-to-image","adapters":[],
			"model":{"bundle":{"bundle_digest":"`+digest+`","family":"sdxl-base"}},"backend":{"backend_id":"diffusers-sdxl"}}]}`)
	if pin != "" {
		job.Obj("request").Set("worker_id", pin)
	}
	if len(candidates) > 0 {
		list := []any{}
		for _, c := range candidates {
			list = append(list, jsonv.O("worker_id", c, "protocol", "media-worker/1.2"))
		}
		job.Set("candidates", list)
	}
	return job
}

func plan(assignments []Assignment) string {
	parts := []string{}
	for _, a := range assignments {
		parts = append(parts, a.Job.Str("job_id")+"→"+a.WorkerID)
	}
	return strings.Join(parts, " ")
}

func TestSchedulingKeepsFIFOAndPrefersTheLoadedIdentity(t *testing.T) {
	cases := []struct {
		name    string
		queued  []*jsonv.Object
		lanes   []Lane
		reorder bool
		want    string
	}{
		{"two queued jobs run on two idle workers in order",
			[]*jsonv.Object{queued(t, "j1", "x", ""), queued(t, "j2", "x", "")},
			[]Lane{lane(t, "a", ""), lane(t, "b", "")}, false, "j1→a j2→b"},
		{"affinity: a worker with the job's loaded identity wins over configuration order",
			[]*jsonv.Object{queued(t, "j1", "x", ""), queued(t, "j2", "y", "")},
			[]Lane{lane(t, "a", "y"), lane(t, "b", "x")}, false, "j1→b j2→a"},
		{"ties between matching workers go to configuration order",
			[]*jsonv.Object{queued(t, "j1", "x", "")},
			[]Lane{lane(t, "a", "x"), lane(t, "b", "x")}, false, "j1→a"},
		{"a pinned head waits for its busy worker and holds the queue",
			[]*jsonv.Object{queued(t, "j1", "x", "a"), queued(t, "j2", "x", "")},
			[]Lane{func() Lane { l := lane(t, "a", ""); l.Idle = false; return l }(), lane(t, "b", "")}, false, ""},
		{"allow_reordering lets a later job use the worker the head cannot",
			[]*jsonv.Object{queued(t, "j1", "x", "a"), queued(t, "j2", "x", "")},
			[]Lane{func() Lane { l := lane(t, "a", ""); l.Idle = false; return l }(), lane(t, "b", "")}, true, "j2→b"},
		{"a pinned job goes only to its worker even when another matches",
			[]*jsonv.Object{queued(t, "j1", "x", "b")},
			[]Lane{lane(t, "a", "x"), lane(t, "b", "")}, false, "j1→b"},
		{"candidates recorded at admission limit the workers",
			[]*jsonv.Object{queued(t, "j1", "x", "", "b")},
			[]Lane{lane(t, "a", ""), lane(t, "b", "")}, false, "j1→b"},
		{"an unavailable worker takes nothing",
			[]*jsonv.Object{queued(t, "j1", "x", "")},
			[]Lane{{ID: "a", Idle: true}, lane(t, "b", "")}, false, "j1→b"},
		{"a loading worker is not ready",
			[]*jsonv.Object{queued(t, "j1", "x", "")},
			[]Lane{func() Lane { l := lane(t, "a", ""); l.Probe.Info.Set("state", "loading"); return l }()}, false, ""},
	}
	for _, tc := range cases {
		if got := plan(Assign(tc.queued, tc.lanes, tc.reorder)); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEligibilityFollowsTasksFamiliesMinorAndBackend(t *testing.T) {
	job := queued(t, "j1", "x", "", "a")
	upscale := parsed(t, `{"op_id":"enlarge","task":"upscale","adapters":[],"model":{"bundle":{"bundle_digest":"u","family":"esrgan"}},"backend":{"backend_id":"diffusers-sdxl"}}`)
	job.Set("operations", append(job.Arr("operations"), upscale))
	l := lane(t, "a", "")
	if !Eligible(job, l) {
		t.Fatal("a worker serving every operation is eligible")
	}
	// A worker that restarted at a lower minor than it was admitted at is not.
	l.Probe.Protocol = "media-worker/1.1"
	if Eligible(job, l) {
		t.Fatal("a worker below the admitted minor is not eligible")
	}
	l = lane(t, "a", "")
	l.Probe.Caps.Set("loaders", []any{jsonv.O("families", []any{"sdxl-base"})})
	if Eligible(job, l) {
		t.Fatal("a worker without the esrgan loader cannot run a linear job's upscale")
	}
	l = lane(t, "a", "")
	l.Probe.Caps.Set("backend_id", "owned-sdxl")
	if Eligible(job, l) {
		t.Fatal("a job resolved for one backend does not run on another")
	}
	l = lane(t, "a", "")
	l.Probe.Caps.Set("ready", false)
	if Eligible(job, l) {
		t.Fatal("a worker whose device is not ready is not eligible")
	}
}
