package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

// bootAudio starts a runtime against a fixture with text-to-audio and registers the sdxl and
// stable-audio-open models. It skips against a fixture without the audio bundle (before 0.7.0).
func bootAudio(t *testing.T, args ...string) *env {
	t.Helper()
	e := boot12(t, args...)
	if !e.cfg.ArtifactCatalog.Has("audio") {
		t.Skip("the Engine fixture predates text-to-audio (Engine 0.7.0)")
	}
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	e.register(t, jsonv.O("model_id", "stable-audio", "artifact_handle", "audio", "family", "stable-audio-open"))
	return e
}

func soundscape(duration float64) *jsonv.Object {
	return jsonv.O("recipe_id", "soundscape", "model_id", "stable-audio", "inputs", jsonv.O("prompt", "rain on a tin roof"),
		"parameters", jsonv.O("steps", 2.0, "duration_seconds", duration))
}

// wav is a canonical 16-bit PCM WAV of silence.
func wav(rate, channels, samples int) []byte {
	data := samples * channels * 2
	head := make([]byte, 44)
	copy(head, "RIFF")
	binary.LittleEndian.PutUint32(head[4:], uint32(36+data))
	copy(head[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(head[16:], 16)
	binary.LittleEndian.PutUint16(head[20:], 1)
	binary.LittleEndian.PutUint16(head[22:], uint16(channels))
	binary.LittleEndian.PutUint32(head[24:], uint32(rate))
	binary.LittleEndian.PutUint32(head[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(head[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(head[34:], 16)
	copy(head[36:], "data")
	binary.LittleEndian.PutUint32(head[40:], uint32(data))
	return append(head, make([]byte, data)...)
}

func TestAudioGeneratesAClipFromAPrompt(t *testing.T) {
	e := bootAudio(t)
	ok := must[*jsonv.Object](t)
	equal(t, get(ok(e.client.Health()), "worker", "protocol"), "media-worker/1.3")
	detail := must[any](t)(e.client.Model("stable-audio", false)).(*jsonv.Object)
	equal(t, detail.Get("family"), "stable-audio-open")
	equal(t, detail.Get("tasks"), []any{"text-to-audio"})
	// The bundle's facts, read by the worker from its VAE and transformer configurations.
	equal(t, detail.Get("sample_rate"), 8000.0)
	equal(t, detail.Get("channels"), 2.0)
	equal(t, detail.Get("hop_length"), 256.0)
	equal(t, detail.Get("duration_range"), []any{1.0, 47.0})
	// The runtime builds the same bundle the fixture describes for its own registration.
	registrations := mustObject(string(must[[]byte](t)(os.ReadFile(filepath.Join(e.root, "registrations.json")))))
	audio := registrations.Obj("audio")
	same := e.register(t, jsonv.O("model_id", audio.Str("bundle_id"), "artifact_handle", "audio", "family", "stable-audio-open", "source", audio.Get("source")))
	equal(t, same.Get("bundle_digest"), audio.Get("bundle_digest"))

	// 2.5 s at 8 kHz with 256-sample latent frames is 79 frames: 20,224 samples, 2.528 s.
	done := e.wait(ok(e.client.Submit(soundscape(2.5), "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	equal(t, done.Str("task"), "text-to-audio")
	params := done.Obj("request").Obj("parameters")
	equal(t, params.Get("duration_seconds"), 2.5)
	equal(t, params.Get("guidance"), 7.0)
	if params.Get("seed") == nil || params.Has("width") || params.Has("sampler") {
		t.Fatalf("parameters %s", jsonv.MustMarshal(params))
	}
	outputs := done.Arr("outputs")
	equal(t, len(outputs), 1)
	clip := outputs[0].(*jsonv.Object)
	equal(t, clip.Str("output_id"), "audio")
	equal(t, clip.Str("mime_type"), "audio/wav")
	equal(t, clip.Get("sample_rate"), 8000.0)
	equal(t, clip.Get("channels"), 2.0)
	equal(t, clip.Get("samples"), 20224.0)
	equal(t, clip.Get("duration_seconds"), 2.528)
	equal(t, done.Obj("media"), jsonv.O("type", "audio", "sample_rate", 8000.0, "channels", 2.0, "samples", 20224.0, "duration_seconds", 2.528))
	data := download(t, e, done.Str("job_id"), "audio")
	equal(t, c.SHA256Hex(data), clip.Str("sha256"))
	equal(t, string(data[:4])+string(data[8:12]), "RIFFWAVE")
	equal(t, len(data), 44+20224*2*2)

	events := e.events(t, done.Str("job_id"))
	for _, want := range []string{`"stage":"encoding"`, `"stage":"sampling"`, `"stage":"decoding"`, `"stage":"saving"`} {
		if !strings.Contains(events, want) {
			t.Fatalf("missing %s in %s", want, events)
		}
	}
	if strings.Contains(events, "rain on a tin roof") {
		t.Fatal("the prompt reached the event stream")
	}
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("schema_version"), 5.0)
	op := manifest.Arr("operations")[0].(*jsonv.Object)
	equal(t, op.Get("task"), "text-to-audio")
	equal(t, op.Get("family"), "stable-audio-open")
	equal(t, op.Get("media"), done.Get("media"))
	equal(t, get(op, "effective_settings", "effective_duration_seconds"), 2.528)
	equal(t, manifest.Get("inputs"), []any{})

	t.Run("the request is refused before admission when it does not fit", func(t *testing.T) {
		record := ok(e.client.UploadInput(MakePNG(64, 64)))
		cases := []struct {
			change func(*jsonv.Object)
			status int
			code   string
			field  string
		}{
			{func(s *jsonv.Object) { s.Obj("inputs").Set("image", record.Str("input_id")) }, 400, "invalid_request", "inputs.image"},
			{func(s *jsonv.Object) { s.Set("inputs", jsonv.O("negative_prompt", "noise")) }, 400, "invalid_request", "inputs.prompt"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("width", 64.0) }, 400, "invalid_request", "parameters.width"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("sampler", "euler") }, 400, "invalid_request", "parameters.sampler"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("count", 2.0) }, 400, "invalid_request", "parameters.count"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("frames", 2.0) }, 400, "invalid_request", "parameters.frames"},
			{func(s *jsonv.Object) { s.Set("adapters", []any{jsonv.O("adapter_id", "x")}) }, 400, "invalid_request", "adapters"},
			// Longer than the bundle serves: refused, never trimmed.
			{func(s *jsonv.Object) { s.Obj("parameters").Set("duration_seconds", 50.0) }, 400, "invalid_request", "parameters.duration_seconds"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("duration_seconds", 0.0) }, 400, "invalid_request", "parameters.duration_seconds"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("steps", 201.0) }, 400, "invalid_request", "parameters.steps"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("guidance", 31.0) }, 400, "invalid_request", "parameters.guidance"},
			{func(s *jsonv.Object) { s.Set("model_id", "fixture") }, 400, "unsupported", "model_id"},
		}
		for _, k := range cases {
			s := soundscape(2)
			k.change(s)
			_, err := e.client.Submit(s, "")
			rejects(t, err, k.status, k.code, k.field)
		}
		// A text-to-image recipe cannot select an audio model, nor take audio parameters, and its
		// steps stay within the diffusion range even though audio admits more.
		_, err := e.client.Submit(submit().Set("model_id", "stable-audio"), "")
		rejects(t, err, 400, "unsupported", "model_id")
		s := submit()
		s.Obj("parameters").Set("duration_seconds", 3.0)
		_, err = e.client.Submit(s, "")
		rejects(t, err, 400, "invalid_request", "parameters.duration_seconds")
		s = submit()
		s.Obj("parameters").Set("frames", 2.0)
		_, err = e.client.Submit(s, "")
		rejects(t, err, 400, "invalid_request", "parameters.frames")
		_, err = e.client.Submit(withSteps(175), "")
		rejects(t, err, 400, "invalid_request", "parameters.steps")
		long := soundscape(47)
		long.Obj("parameters").Set("steps", 175.0)
		if _, err := e.client.JSON("/v1/jobs/validate", "POST", long); err != nil {
			t.Fatalf("175 steps within the audio range: %v", err)
		}
	})

	t.Run("a clip that differs from its record or request fails the job", func(t *testing.T) {
		// A record whose sample rate differs from the header, and a consistent clip shorter than
		// the request (bytes injected, digest and size consistent).
		short := wav(8000, 2, 8000)
		sum := sha256.Sum256(short)
		for _, forge := range []bool{false, true} {
			e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
				if o, isObj := result.(*jsonv.Object); isObj && strings.HasPrefix(path, "/v1/executions/") && o.Str("state") == "succeeded" {
					out := o.Arr("outputs")[0].(*jsonv.Object)
					if forge {
						out.Set("size", float64(len(short))).Set("sha256", hex.EncodeToString(sum[:])).Set("samples", 8000.0).Set("duration_seconds", 1.0)
					} else {
						out.Set("sample_rate", 16000.0)
					}
				}
				return result, nil
			}, nil)
			e.rt.Worker.SetResponseHook(func(path, method string) *http.Response {
				if forge && strings.HasSuffix(path, "/outputs/audio") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(short))}
				}
				return nil
			})
			failed := e.wait(ok(e.client.Submit(soundscape(2), "")).Str("job_id"), nil)
			e.rt.Worker.SetHooks(nil, nil)
			e.rt.Worker.SetResponseHook(nil)
			equal(t, failed.Str("state"), "failed", forge)
			equal(t, get(failed, "error", "code"), "output_unavailable")
		}
	})
}

func TestAudioOutputLimitIsEnforced(t *testing.T) {
	e := bootAudio(t)
	ok := must[*jsonv.Object](t)
	limits := config.DefaultOutputLimits()
	limits["audio/wav"] = 1024 // a one-second fixture clip is about 32 KiB
	e.rt.Config.MaxOutputBytes = limits
	failed := e.wait(ok(e.client.Submit(soundscape(1), "")).Str("job_id"), nil)
	equal(t, failed.Str("state"), "failed")
	equal(t, get(failed, "error", "code"), "output_unavailable")
	if _, err := os.Stat(filepath.Join(e.rt.OutputDirectory(failed.Str("job_id")), "audio.wav")); err == nil {
		t.Fatal("an oversized clip was committed")
	}
}

func TestProtocol12WorkerRefusesAudio(t *testing.T) {
	e := boot12(t, "--protocol", "media-worker/1.2")
	if !e.cfg.ArtifactCatalog.Has("audio") {
		t.Skip("the Engine fixture predates text-to-audio (Engine 0.7.0)")
	}
	ok := must[*jsonv.Object](t)
	equal(t, get(ok(e.client.Health()), "worker", "protocol"), "media-worker/1.2")
	_, err := e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "stable-audio", "artifact_handle", "audio", "family", "stable-audio-open"))
	rejects(t, err, 503, "unsupported", "family")
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	_, err = e.client.Submit(soundscape(2).Set("model_id", "fixture"), "")
	rejects(t, err, 400, "unsupported", "recipe_id")
}

// TestVideoWorkerWithoutAudioRefusesIt is a 1.3 worker that does not list text-to-audio, as
// Engine 0.6.0 and a reference backend without the audio extra are: the task, not the minor,
// decides.
func TestVideoWorkerWithoutAudioRefusesIt(t *testing.T) {
	e := bootAudio(t)
	e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
		if o, isObj := result.(*jsonv.Object); isObj && path == "/v1/capabilities" {
			tasks := []any{}
			for _, task := range o.Arr("tasks") {
				if task != "text-to-audio" {
					tasks = append(tasks, task)
				}
			}
			o.Set("tasks", tasks)
		}
		return result, nil
	}, nil)
	defer e.rt.Worker.SetHooks(nil, nil)
	_, err := e.client.Submit(soundscape(2), "")
	rejects(t, err, 400, "unsupported", "recipe_id")
}
