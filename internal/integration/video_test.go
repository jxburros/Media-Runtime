package integration

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

// bootVideo starts a runtime against a 1.3 fixture with the sdxl and svd models registered. It
// skips against a fixture without image-to-video (Engine before 0.6.0).
func bootVideo(t *testing.T, args ...string) *env {
	t.Helper()
	e := boot12(t, args...)
	if !e.cfg.ArtifactCatalog.Has("video") {
		t.Skip("the Engine fixture predates image-to-video (Engine 0.6.0)")
	}
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	e.register(t, jsonv.O("model_id", "svd", "artifact_handle", "video", "family", "svd"))
	return e
}

func animate(input string) *jsonv.Object {
	return jsonv.O("recipe_id", "animate", "model_id", "svd", "inputs", jsonv.O("image", input), "parameters", jsonv.O("steps", 2.0))
}

func download(t *testing.T, e *env, jobID, outputID string) []byte {
	t.Helper()
	res, err := e.client.Output(jobID, outputID)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("Content-Length %q for %d bytes", res.Header.Get("Content-Length"), len(data))
	}
	return data
}

func TestVideoAnimatesAnUploadedImage(t *testing.T) {
	e := bootVideo(t)
	ok := must[*jsonv.Object](t)
	equal(t, get(ok(e.client.Health()), "worker", "protocol"), "media-worker/1.3")
	detail := must[any](t)(e.client.Model("svd", false)).(*jsonv.Object)
	equal(t, detail.Get("family"), "svd")
	equal(t, detail.Get("tasks"), []any{"image-to-video"})
	// The bundle's facts, read by the worker from video.json and the unet configuration.
	equal(t, detail.Get("video_sizes"), []any{[]any{64.0, 64.0}, []any{1024.0, 576.0}, []any{576.0, 1024.0}})
	equal(t, detail.Get("default_frames"), 2.0)
	// The runtime builds the same bundle the fixture describes for its own registration.
	registrations := mustObject(string(must[[]byte](t)(os.ReadFile(filepath.Join(e.root, "registrations.json")))))
	built := mustObject(string(must[[]byte](t)(os.ReadFile(filepath.Join(e.root, "video", "model_index.json")))))
	equal(t, built.Get("_class_name"), "StableVideoDiffusionPipeline")
	video := registrations.Obj("video")
	same := e.register(t, jsonv.O("model_id", video.Str("bundle_id"), "artifact_handle", "video", "family", "svd", "source", video.Get("source")))
	equal(t, same.Get("bundle_digest"), video.Get("bundle_digest"))

	record := ok(e.client.UploadInput(MakePNG(64, 64)))
	done := e.wait(ok(e.client.Submit(animate(record.Str("input_id")), "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	equal(t, done.Str("task"), "image-to-video")
	// Every effective value is resolved and recorded before dispatch: frames from the bundle,
	// fps, motion, noise_augmentation and decode_chunk from the worker, steps from the request.
	params := done.Obj("request").Obj("parameters")
	equal(t, params.Get("width"), 64.0)
	equal(t, params.Get("frames"), 2.0)
	equal(t, params.Get("fps"), 7.0)
	equal(t, params.Get("motion"), 127.0)
	equal(t, params.Get("noise_augmentation"), 0.02)
	equal(t, params.Get("decode_chunk"), 8.0)
	equal(t, params.Get("steps"), 2.0)
	if params.Get("seed") == nil || params.Has("guidance") {
		t.Fatalf("parameters %s", jsonv.MustMarshal(params))
	}
	outputs := done.Arr("outputs")
	equal(t, len(outputs), 2)
	frames, poster := outputs[0].(*jsonv.Object), outputs[1].(*jsonv.Object)
	equal(t, frames.Str("output_id"), "frames")
	equal(t, frames.Str("mime_type"), "application/zip")
	equal(t, frames.Get("frames"), 2.0)
	equal(t, frames.Get("fps"), 7.0)
	equal(t, frames.Get("duration_seconds"), 2.0/7)
	equal(t, poster.Str("output_id"), "poster")
	equal(t, poster.Str("mime_type"), "image/png")
	equal(t, done.Obj("media"), jsonv.O("type", "video", "frames", 2.0, "fps", 7.0, "duration_seconds", 2.0/7, "width", 64.0, "height", 64.0, "encoded_video", false))

	archive := download(t, e, done.Str("job_id"), "frames")
	equal(t, c.SHA256Hex(archive), frames.Str("sha256"))
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	members := map[string][]byte{}
	for _, f := range reader.File {
		names = append(names, f.Name)
		stream, _ := f.Open()
		members[f.Name], _ = io.ReadAll(stream)
		stream.Close()
	}
	equal(t, names, []string{"frames.json", "000001.png", "000002.png"})
	equal(t, mustObject(string(members["frames.json"])), jsonv.O("fps", 7.0, "frames", 2.0, "height", 64.0, "width", 64.0))
	// The poster is frame 1, byte for byte.
	equal(t, c.SHA256Hex(download(t, e, done.Str("job_id"), "poster")), c.SHA256Hex(members["000001.png"]))

	events := e.events(t, done.Str("job_id"))
	for _, want := range []string{`"stage":"encoding"`, `"stage":"decoding","scope":"stage","unit":"frames"`, `"stage":"packaging"`} {
		if !strings.Contains(events, want) {
			t.Fatalf("missing %s in %s", want, events)
		}
	}
	equal(t, strings.Count(events, "event: output\n"), 2)
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("schema_version"), 5.0)
	equal(t, len(manifest.Arr("operations")), 1)
	op := manifest.Arr("operations")[0].(*jsonv.Object)
	equal(t, op.Get("task"), "image-to-video")
	equal(t, op.Get("family"), "svd")
	equal(t, op.Get("media"), done.Get("media"))
	equal(t, get(op, "effective_settings", "frames"), 2.0)
	equal(t, len(manifest.Arr("outputs")), 2)
	equal(t, manifest.Get("intermediate_outputs"), []any{})
	equal(t, len(manifest.Arr("inputs")), 1)

	t.Run("the request is refused before admission when it does not fit", func(t *testing.T) {
		id := record.Str("input_id")
		cases := []struct {
			change func(*jsonv.Object)
			status int
			code   string
			field  string
		}{
			{func(s *jsonv.Object) { s.Obj("inputs").Set("prompt", "x") }, 400, "invalid_request", "inputs.prompt"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("guidance", 3.0) }, 400, "invalid_request", "parameters.guidance"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("count", 2.0) }, 400, "invalid_request", "parameters.count"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("tile", 256.0) }, 400, "invalid_request", "parameters.tile"},
			{func(s *jsonv.Object) { s.Set("adapters", []any{jsonv.O("adapter_id", "x")}) }, 400, "invalid_request", "adapters"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("width", 128.0) }, 400, "invalid_request", "parameters.width"},
			{func(s *jsonv.Object) { s.Obj("parameters").Set("frames", 65.0) }, 400, "invalid_request", "parameters.frames"},
			// Within the protocol's step range but beyond the worker's video_steps limit.
			{func(s *jsonv.Object) { s.Obj("parameters").Set("steps", 60.0) }, 400, "invalid_request", "parameters.steps"},
			{func(s *jsonv.Object) { s.Set("model_id", "fixture") }, 400, "unsupported", "model_id"},
		}
		for _, k := range cases {
			s := animate(id)
			k.change(s)
			_, err := e.client.Submit(s, "")
			rejects(t, err, k.status, k.code, k.field)
		}
		// A text-to-image recipe cannot select a video model.
		_, err := e.client.Submit(submit().Set("model_id", "svd"), "")
		rejects(t, err, 400, "unsupported", "model_id")
		// A size the bundle does not serve: the input is never resized.
		large := ok(e.client.UploadInput(MakePNG(128, 128)))
		_, err = e.client.Submit(animate(large.Str("input_id")), "")
		rejects(t, err, 400, "invalid_request", "video_size")
	})

	t.Run("an archive that differs from its record or request fails the job", func(t *testing.T) {
		// A record re-timed against the request, and an archive whose frames.json disagrees
		// with its record (bytes injected, digest and size consistent).
		forged := frameArchive(t, 3, 7)
		sum := sha256.Sum256(forged)
		for _, forge := range []bool{false, true} {
			e.rt.Worker.SetHooks(func(path, method string, body, result any) (any, error) {
				if o, isObj := result.(*jsonv.Object); isObj && strings.HasPrefix(path, "/v1/executions/") && o.Str("state") == "succeeded" {
					for _, raw := range o.Arr("outputs") {
						if out := raw.(*jsonv.Object); out.Str("output_id") == "frames" && forge {
							out.Set("size", float64(len(forged))).Set("sha256", hex.EncodeToString(sum[:]))
						} else if out.Str("output_id") == "frames" {
							out.Set("fps", 8.0).Set("duration_seconds", 2.0/8)
						}
					}
				}
				return result, nil
			}, nil)
			e.rt.Worker.SetResponseHook(func(path, method string) *http.Response {
				if forge && strings.HasSuffix(path, "/outputs/frames") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(forged))}
				}
				return nil
			})
			failed := e.wait(ok(e.client.Submit(animate(record.Str("input_id")), "")).Str("job_id"), nil)
			e.rt.Worker.SetHooks(nil, nil)
			e.rt.Worker.SetResponseHook(nil)
			equal(t, failed.Str("state"), "failed", forge)
			equal(t, get(failed, "error", "code"), "output_unavailable")
		}
	})

	t.Run("an advertised video output is required", func(t *testing.T) {
		e.rt.Worker.SetHooks(advertiseVideo(nil), nil)
		defer e.rt.Worker.SetHooks(nil, nil)
		failed := e.wait(ok(e.client.Submit(animate(record.Str("input_id")), "")).Str("job_id"), nil)
		equal(t, failed.Str("state"), "failed")
		equal(t, get(failed, "error", "code"), "output_unavailable")
	})

	t.Run("an advertised video output is committed", func(t *testing.T) {
		// Engine ships no encoder, so its fixture never produces video; the bytes are injected
		// here to prove the optional path, not produced by the worker.
		mp4 := fakeMP4()
		e.rt.Worker.SetHooks(advertiseVideo(mp4), nil)
		e.rt.Worker.SetResponseHook(func(path, method string) *http.Response {
			if strings.HasSuffix(path, "/outputs/video") {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(bytes.NewReader(mp4))}
			}
			return nil
		})
		defer e.rt.Worker.SetHooks(nil, nil)
		defer e.rt.Worker.SetResponseHook(nil)
		done := e.wait(ok(e.client.Submit(animate(record.Str("input_id")), "")).Str("job_id"), nil)
		equal(t, done.Str("state"), "succeeded", done.Get("error"))
		equal(t, len(done.Arr("outputs")), 3)
		equal(t, get(done, "media", "encoded_video"), true)
		equal(t, c.SHA256Hex(download(t, e, done.Str("job_id"), "video")), c.SHA256Hex(mp4))
		manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
		equal(t, len(manifest.Arr("outputs")), 3)
	})
}

// advertiseVideo makes the worker advertise an encoded video/mp4 output for image-to-video and,
// when bytes are given, report a matching output record on the finished execution.
func advertiseVideo(mp4 []byte) func(path, method string, body, result any) (any, error) {
	return func(path, method string, body, result any) (any, error) {
		o, isObj := result.(*jsonv.Object)
		if !isObj {
			return result, nil
		}
		if path == "/v1/capabilities" {
			tp := o.Obj("task_parameters").Obj("image-to-video")
			tp.Set("outputs", append(tp.Arr("outputs"), "video"))
			tp.Obj("output_formats").Set("video", "video/mp4")
		}
		if mp4 != nil && strings.HasPrefix(path, "/v1/executions/") && o.Str("state") == "succeeded" {
			sum := sha256.Sum256(mp4)
			o.Set("outputs", append(o.Arr("outputs"), jsonv.O("output_id", "video", "mime_type", "video/mp4", "size", float64(len(mp4)), "sha256", hex.EncodeToString(sum[:]))))
		}
		return result, nil
	}
}

// frameArchive is a stored ZIP of frames 64×64 PNG frames with frames.json naming fps.
func frameArchive(t *testing.T, frames, fps int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add := func(name string, data []byte) {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
	}
	add("frames.json", jsonv.MustMarshal(jsonv.O("fps", float64(fps), "frames", float64(frames), "height", 64.0, "width", 64.0)))
	for k := 1; k <= frames; k++ {
		add(fmt.Sprintf("%06d.png", k), MakePNG(64, 64))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeMP4 is the smallest ISO base media file the signature check accepts: an ftyp box and an
// empty mdat box. It is not a playable video.
func fakeMP4() []byte {
	return append([]byte("\x00\x00\x00\x14ftypisom\x00\x00\x02\x00isom"), []byte("\x00\x00\x00\x08mdat")...)
}

func TestVideoOutputLimitIsEnforced(t *testing.T) {
	e := bootVideo(t)
	ok := must[*jsonv.Object](t)
	limits := config.DefaultOutputLimits()
	limits["application/zip"] = 100 // the fixture's two-frame archive is larger
	e.rt.Config.MaxOutputBytes = limits
	record := ok(e.client.UploadInput(MakePNG(64, 64)))
	failed := e.wait(ok(e.client.Submit(animate(record.Str("input_id")), "")).Str("job_id"), nil)
	equal(t, failed.Str("state"), "failed")
	equal(t, get(failed, "error", "code"), "output_unavailable")
	if _, err := os.Stat(filepath.Join(e.rt.OutputDirectory(failed.Str("job_id")), "frames.zip")); err == nil {
		t.Fatal("an oversized archive was committed")
	}
}

// animated generates an image and animates it, publishing the frames and poster.
func animated() *jsonv.Object {
	return mustObject(`{
	  "schema_version": 2, "recipe_id": "animated", "revision": 1,
	  "inputs": { "prompt": { "type": "string", "required": true }, "frames": { "type": "number", "required": false } },
	  "operations": [
	    { "op_id": "generate", "task": "text-to-image", "model_family": "sdxl-base",
	      "inputs": { "prompt": { "from": "input:prompt" } },
	      "defaults": { "width": 64, "height": 64, "steps": 2, "guidance": 6 } },
	    { "op_id": "animate", "task": "image-to-video", "model_family": "svd",
	      "inputs": { "image": { "from": "op:generate/image" }, "frames": { "from": "input:frames" } },
	      "defaults": { "steps": 2, "fps": 6 } }
	  ],
	  "publish": ["animate/frames", "animate/poster"]
	}`)
}

func TestVideoRunsAsALinearOperation(t *testing.T) {
	e := bootVideo(t)
	ok := must[*jsonv.Object](t)
	if _, err := e.client.JSON("/v1/recipes", "POST", animated()); err != nil {
		t.Fatal(err)
	}
	// Only a PNG output can feed an image input: a frame archive cannot be upscaled.
	bad := animated()
	bad.Set("recipe_id", "bad")
	bad.Set("operations", append(bad.Arr("operations"), mustObject(`{"op_id":"enlarge","task":"upscale","model_family":"esrgan","inputs":{"image":{"from":"op:animate/frames"}}}`)))
	_, err := e.client.JSON("/v1/recipes", "POST", bad)
	rejects(t, err, 400, "invalid_request", "operations.2.inputs.image.from")
	s := jsonv.O("recipe_id", "animated", "model_id", "fixture", "models", jsonv.O("animate", "svd"), "inputs", jsonv.O("prompt", "a lighthouse", "frames", 3.0))
	done := e.wait(ok(e.client.Submit(s, "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	ids := []string{}
	for _, raw := range done.Arr("outputs") {
		ids = append(ids, raw.(*jsonv.Object).Str("output_id"))
	}
	equal(t, ids, []string{"frames", "poster"})
	equal(t, get(done, "media", "frames"), 3.0)
	equal(t, get(done, "media", "fps"), 6.0)
	equal(t, len(done.Arr("intermediate_outputs")), 1)
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("schema_version"), 5.0)
	equal(t, get(manifest.Arr("operations")[0].(*jsonv.Object), "media"), nil)
	equal(t, get(manifest.Arr("operations")[1].(*jsonv.Object), "media", "frames"), 3.0)
}

func TestProtocol12WorkerRefusesVideo(t *testing.T) {
	e := boot12(t, "--protocol", "media-worker/1.2")
	if !e.cfg.ArtifactCatalog.Has("video") {
		t.Skip("the Engine fixture predates image-to-video (Engine 0.6.0)")
	}
	ok := must[*jsonv.Object](t)
	equal(t, get(ok(e.client.Health()), "worker", "protocol"), "media-worker/1.2")
	_, err := e.client.JSON("/v1/models", "POST", jsonv.O("model_id", "svd", "artifact_handle", "video", "family", "svd"))
	rejects(t, err, 503, "unsupported", "family")
	e.register(t, jsonv.O("model_id", "fixture", "artifact_handle", "fixture"))
	record := ok(e.client.UploadInput(MakePNG(64, 64)))
	_, err = e.client.Submit(animate(record.Str("input_id")).Set("model_id", "fixture"), "")
	rejects(t, err, 400, "unsupported", "recipe_id")
	done := e.wait(ok(e.client.Submit(submit(), "")).Str("job_id"), nil)
	equal(t, done.Str("state"), "succeeded", done.Get("error"))
	manifest := must[any](t)(e.client.Manifest(done.Str("job_id"))).(*jsonv.Object)
	equal(t, manifest.Get("schema_version"), 3.0)
}
