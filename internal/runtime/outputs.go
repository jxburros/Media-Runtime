package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/store"
	"github.com/jxburros/media-runtime/internal/worker"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func atomicFile(path string, data []byte) error {
	temp := path + ".tmp"
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := file.Write(data)
	serr := file.Sync()
	cerr := file.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (r *Runtime) OutputDirectory(jobID string) string {
	return filepath.Join(r.Config.Directory, "outputs", jobID)
}

func isInt(v any) bool {
	f, ok := v.(float64)
	return ok && f == float64(int64(f))
}

// OutputFile is the stored path of one committed output. A linear job's intermediate output
// lives under its operation's directory; every other output sits beside the manifest.
func (r *Runtime) OutputFile(jobID string, output *jsonv.Object) string {
	ext := c.Extensions[output.Str("mime_type")]
	if ext == "" {
		ext = "png"
	}
	if output.Get("published") == false {
		return filepath.Join(r.OutputDirectory(jobID), output.Str("op_id"), output.Str("operation_output_id")+"."+ext)
	}
	return filepath.Join(r.OutputDirectory(jobID), output.Str("output_id")+"."+ext)
}

// outputName is the job-level identity of one operation output.
type outputName struct {
	id        string
	published bool
}

// outputNames maps every operation's outputs to their job-level IDs. Schema-1 jobs publish the
// worker's IDs unchanged. A linear job publishes the outputs its recipe lists (an entry naming a
// repeated output publishes each image of the batch) under the worker's ID, prefixed with
// "<op_id>__" when two published outputs share it; unpublished intermediates are always prefixed.
func outputNames(job *jsonv.Object) []map[string]outputName {
	ops := Operations(job)
	linear := c.IsLinear(job.Obj("recipe"))
	publish := map[string]bool{}
	for _, entry := range strList(c.NormalizeRecipe(job.Obj("recipe")).Get("publish")) {
		publish[entry] = true
	}
	expected := make([][]string, len(ops))
	uses := map[string]int{}
	for i, op := range ops {
		count, ok := op.Obj("parameters").Num("count")
		if !ok {
			count = 1
		}
		expected[i] = c.OutputIDs(op.Obj("backend").Obj("task_parameters"), int(count))
		for _, id := range expected[i] {
			if published(publish, op.Str("op_id"), id, linear) {
				uses[id]++
			}
		}
	}
	names := make([]map[string]outputName, len(ops))
	for i, op := range ops {
		names[i] = map[string]outputName{}
		for _, id := range expected[i] {
			name := outputName{id: id, published: published(publish, op.Str("op_id"), id, linear)}
			if linear && (!name.published || uses[id] > 1) {
				name.id = op.Str("op_id") + "__" + id
			}
			names[i][id] = name
		}
	}
	return names
}

func published(publish map[string]bool, opID, outputID string, linear bool) bool {
	if !linear {
		return true
	}
	base := outputID
	if i := strings.LastIndex(outputID, "_"); i > 0 && isDigits(outputID[i+1:]) {
		base = outputID[:i]
	}
	return publish[opID+"/"+outputID] || publish[opID+"/"+base]
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// commitOutputs verifies every output the worker advertised for the current operation's task,
// then writes their bytes. An intermediate operation's outputs are fsynced and recorded before
// the next operation is dispatched; after the last operation the manifest and the database are
// written in that order, success is recorded only after everything is durable, and a partial set
// is output_unavailable.
func (r *Runtime) commitOutputs(client *worker.Client, job, snapshot *jsonv.Object) error {
	directory := r.OutputDirectory(job.Str("job_id"))
	op, index := current(job)
	linear := c.IsLinear(job.Obj("recipe"))
	ops := Operations(job)
	err := func() error {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		backend := op.Obj("backend")
		params := op.Obj("parameters")
		task := op.Str("task")
		count, ok := params.Num("count")
		if !ok {
			count = 1
		}
		taskParameters := backend.Obj("task_parameters")
		expected := c.OutputIDs(taskParameters, int(count))
		names := outputNames(job)[index]
		formats := taskParameters.Obj("output_formats")
		records := map[string]*jsonv.Object{}
		for _, raw := range snapshot.Arr("outputs") {
			record, _ := raw.(*jsonv.Object)
			if record == nil || records[record.Str("output_id")] != nil {
				return errors.New("invalid outputs")
			}
			records[record.Str("output_id")] = record
		}
		if len(records) != len(expected) {
			return errors.New("output set differs from the advertised outputs")
		}
		width, _ := params.Num("width")
		height, _ := params.Num("height")
		var scale float64
		if task == "upscale" {
			scale, _ = op.Obj("model").Num("scale")
			width, height = width*scale, height*scale
		}
		seed, seeded := params.Num("seed")
		committed := []any{}
		synced := map[string]bool{directory: true}
		for k, id := range expected {
			output := records[id]
			if output == nil {
				return errors.New("missing advertised output " + id)
			}
			mime := output.Str("mime_type")
			if want := formats.Str(id); (want != "" && mime != want) || (formats == nil && mime != "image/png") {
				return errors.New("output type differs from the advertised type")
			}
			limit := r.Config.OutputLimit(mime)
			check := c.Signatures[mime]
			size, _ := output.Num("size")
			if check == nil || limit == 0 || !isInt(output.Get("size")) || size < 1 || size > float64(limit) {
				return errors.New("invalid output")
			}
			if mime == "image/png" {
				if w, _ := output.Num("width"); w != width {
					return errors.New("output dimensions changed")
				}
				if h, _ := output.Num("height"); h != height {
					return errors.New("output dimensions changed")
				}
			}
			if mime == "audio/wav" {
				// The clip is the bundle's format and covers the request, rounded up by less than
				// one latent frame; it is never shorter than asked.
				if err := audioMatches(output, params, op.Obj("model")); err != nil {
					return err
				}
			}
			if mime == "application/zip" || mime == "video/mp4" {
				// The archive holds exactly the requested frames at the requested rate and size;
				// frames are never dropped or re-timed.
				for _, k := range []string{"width", "height", "frames", "fps"} {
					if !jsonEqual(output.Get(k), params.Get(k)) {
						return errors.New("video output differs from the request")
					}
				}
			}
			record := output.Clone()
			if linear {
				name := names[id]
				record.Set("output_id", name.id).Set("op_id", op.Get("op_id")).Set("operation_output_id", id).Set("published", name.published)
			}
			file := r.OutputFile(job.Str("job_id"), record)
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				return err
			}
			synced[filepath.Dir(file)] = true
			var receiveErr error
			if op.Obj("cache") != nil {
				receiveErr = r.receiveCached(job, op, output, file, int64(limit), check)
			} else {
				receiveErr = receive(client, "/v1/executions/"+op.Str("execution_id")+"/outputs/"+id, file, int64(limit), output, check)
			}
			if err := receiveErr; err != nil {
				return err
			}
			if seeded && task == "text-to-image" {
				// Output k of a batch used seed + k − 1, so any one image can be reproduced alone.
				record.Set("seed", seed+float64(k))
			}
			committed = append(committed, record.Set("expires_at", r.expiry()))
		}
		op.Set("outputs", committed)
		if media := mediaFacts(task, committed); media != nil {
			op.Set("media", media)
			job.Set("media", media)
		}
		op.Set("worker", jsonv.Maybe(snapshot.Get("implementation")))
		op.Set("effective_settings", jsonv.Maybe(snapshot.Get("effective_settings")))
		op.Set("measurements", jsonv.Maybe(snapshot.Get("measurements")))
		for path := range synced {
			if path != directory {
				if err := syncDir(path); err != nil {
					return err
				}
			}
		}
		if index < len(ops)-1 {
			// The operation's outputs are durable before the next operation is dispatched.
			for _, path := range []string{directory, filepath.Join(r.Config.Directory, "outputs"), r.Config.Directory} {
				if err := syncDir(path); err != nil {
					return err
				}
			}
			op.Set("state", "succeeded")
			job.Set("current_operation", float64(index+1))
			job.Set("stage", nil)
			job.Delete("progress")
			err := r.save(job, func(tx *store.Store) error {
				for _, output := range committed {
					tx.Event(job, "output", output)
				}
				return nil
			})
			if err != nil {
				op.Set("state", "running")
				job.Set("current_operation", float64(index))
			}
			return err
		}
		var manifest *jsonv.Object
		if !linear && hasMedia(job) {
			// Schema 5: the operations layout with media facts, for any job with a video or audio
			// operation.
			job.Set("outputs", committed)
			manifest = r.linearManifest(job, 5)
		} else if linear {
			outputs, intermediates := []any{}, []any{}
			for _, o := range ops {
				for _, raw := range o.Arr("outputs") {
					if raw.(*jsonv.Object).Get("published") == false {
						intermediates = append(intermediates, raw)
					} else {
						outputs = append(outputs, raw)
					}
				}
			}
			job.Set("outputs", outputs)
			job.Set("intermediate_outputs", intermediates)
			schema := 4.0
			if hasMedia(job) {
				schema = 5
			}
			if job.Obj("recipe").Get("schema_version") == 3.0 || job.Obj("request").Bool("reuse_intermediates") {
				schema = 6
			}
			manifest = r.linearManifest(job, schema)
		} else {
			job.Set("outputs", committed)
			manifest = r.manifest(job, op, snapshot, committed, scale)
		}
		job.Set("manifest", manifest)
		if err := atomicFile(filepath.Join(directory, "manifest.json"), jsonv.Indent(manifest)); err != nil {
			return err
		}
		for _, path := range []string{directory, filepath.Join(r.Config.Directory, "outputs"), r.Config.Directory} {
			if err := syncDir(path); err != nil {
				return err
			}
		}
		op.Set("state", "succeeded")
		job.Set("state", "succeeded")
		job.Set("stage", nil)
		return r.save(job, func(tx *store.Store) error {
			for _, output := range committed {
				tx.Event(job, "output", output)
			}
			tx.Event(job, "job_state", jsonv.O("state", "succeeded"))
			return nil
		})
	}()
	if err != nil {
		var api *c.ApiError
		job.Set("state", "running")
		op.Set("state", "running")
		if errors.As(err, &api) && transient(api) {
			return err
		}
		r.finish(job, "failed", jsonv.O("code", "output_unavailable", "message", "Output could not be verified and committed to durable storage"))
	}
	return nil
}

// receive streams one worker output into a temporary file beside its final path, hashing it on
// the way, so an output is never held in memory whatever its size. The file must be exactly the
// recorded size (at most limit bytes), carry the recorded SHA-256 and pass its MIME type's
// signature check; only then is it fsynced and renamed into place.
func receive(client *worker.Client, path, file string, limit int64, output *jsonv.Object, check c.Signature) error {
	res, err := client.Response(path, "GET", nil, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return receiveBody(res.Body, file, limit, output, check)
}

func receiveBody(body io.Reader, file string, limit int64, output *jsonv.Object, check c.Signature) error {
	temp := file + ".tmp"
	f, err := os.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			f.Close()
			os.Remove(temp)
		}
	}()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, digest), io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	size, _ := output.Num("size")
	if n > limit {
		return errors.New("output too large")
	}
	if float64(n) != size || hex.EncodeToString(digest.Sum(nil)) != output.Str("sha256") || !check(f, n, output) {
		return errors.New("output integrity failure")
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	committed = true
	return os.Rename(temp, file)
}

// manifestInput is the provenance of a submitted input, without its owner or expiry.
func manifestInput(input *jsonv.Object) *jsonv.Object {
	return jsonv.O(
		"input_id", input.Get("input_id"),
		"mime_type", input.Get("mime_type"),
		"size", input.Get("size"),
		"sha256", input.Get("sha256"),
		"width", input.Get("width"),
		"height", input.Get("height"),
	)
}

func manifestAdapters(op *jsonv.Object) []any {
	adapters := []any{}
	for _, raw := range asArray(op.Get("adapters")) {
		a := raw.(*jsonv.Object)
		adapter := a.Obj("adapter")
		adapters = append(adapters, jsonv.O(
			"adapter_id", adapter.Get("adapter_id"),
			"digest", adapter.Get("digest"),
			"scale", a.Get("scale"),
			"source", jsonv.Maybe(adapter.Get("source")),
		))
	}
	return adapters
}

// manifest is the schema-3 manifest of a single-operation job.
func (r *Runtime) manifest(job, op, snapshot *jsonv.Object, committed []any, scale float64) *jsonv.Object {
	backend := op.Obj("backend")
	inputs := []any{}
	if input := op.Obj("input"); input != nil {
		inputs = append(inputs, manifestInput(input))
	}
	if mask := op.Obj("mask"); mask != nil {
		inputs = append(inputs, manifestInput(mask).Set("role", "mask"))
	}
	if overlay := op.Obj("overlay"); overlay != nil {
		inputs = append(inputs, manifestInput(overlay).Set("role", "overlay"))
	}
	for _, role := range []string{"control_image", "reference_image"} {
		if input := op.Obj(role); input != nil {
			inputs = append(inputs, manifestInput(input).Set("role", role))
		}
	}
	var scaleValue any = jsonv.Absent
	if op.Str("task") == "upscale" {
		scaleValue = scale
	}
	return jsonv.O(
		"schema_version", 3.0,
		"runtime_version", c.VERSION,
		"job_id", job.Get("job_id"),
		"task", op.Get("task"),
		"family", op.Obj("model").Obj("bundle").Get("family"),
		"scale", scaleValue,
		"recipe", job.Get("recipe"),
		"model", op.Obj("model").Get("bundle"),
		"request", job.Get("request"),
		"inputs", inputs,
		"adapters", manifestAdapters(op),
		"controlnets", manifestControlNets(op),
		"control_image_from", jsonv.Maybe(op.Get("control_image_from")),
		"conditioning", manifestConditioning(op),
		"backend", backend,
		"worker_id", nullish(job.Get("worker_id")),
		"device_identity", nullish(backend.Get("device_identity")),
		"resolved_options", nullish(backend.Get("resolved_options"), backend.Get("options")),
		"resolutions", nullish(backend.Get("resolutions"), []any{}),
		"applied_fallbacks", nullish(op.Get("applied_fallbacks"), []any{}),
		"worker", jsonv.Maybe(snapshot.Get("implementation")),
		"effective_settings", jsonv.Maybe(snapshot.Get("effective_settings")),
		"measurements", jsonv.Maybe(snapshot.Get("measurements")),
		"attempt_id", op.Get("attempt_id"),
		"session_id", op.Get("session_id"),
		"execution_id", op.Get("execution_id"),
		"outputs", committed,
		"determinism", determinism,
	)
}

const determinism = "Seed reproducibility depends on hardware, software and dependency versions; matching bytes are not guaranteed."

// audioMatches checks a WAV output record against the request and the model's recorded facts:
// the bundle's sample rate and channels, and a duration at least the requested one and short of
// it by less than one latent frame (hop_length samples) when the hop is known.
func audioMatches(output, params, model *jsonv.Object) error {
	for _, k := range []string{"sample_rate", "channels"} {
		if model.Has(k) && !jsonEqual(output.Get(k), model.Get(k)) {
			return errors.New("audio format differs from the model")
		}
	}
	requested, _ := params.Num("duration_seconds")
	duration, _ := output.Num("duration_seconds")
	const epsilon = 1e-9
	if duration+epsilon < requested {
		return errors.New("audio is shorter than requested")
	}
	hop, hok := model.Num("hop_length")
	rate, rok := output.Num("sample_rate")
	if hok && rok && rate > 0 && duration > requested+hop/rate+epsilon {
		return errors.New("audio is longer than the latent granularity allows")
	}
	return nil
}

// mediaTasks produce video or audio; a job with one of them writes manifest schema 5.
var mediaTasks = map[string]bool{"image-to-video": true, "text-to-audio": true}

func hasMedia(job *jsonv.Object) bool {
	for _, op := range Operations(job) {
		if mediaTasks[op.Str("task")] {
			return true
		}
	}
	return false
}

// mediaFacts summarizes a video or audio operation's committed outputs from the worker's
// verified output records: frame count, rate, duration and size for video (and whether an
// encoded video was committed); sample rate, channels, samples and duration for audio.
func mediaFacts(task string, committed []any) *jsonv.Object {
	if task == "text-to-audio" {
		for _, raw := range committed {
			if o := raw.(*jsonv.Object); o.Str("mime_type") == "audio/wav" {
				return jsonv.O(
					"type", "audio",
					"sample_rate", o.Get("sample_rate"),
					"channels", o.Get("channels"),
					"samples", o.Get("samples"),
					"duration_seconds", o.Get("duration_seconds"),
				)
			}
		}
		return nil
	}
	if task != "image-to-video" {
		return nil
	}
	var frames *jsonv.Object
	encoded := false
	for _, raw := range committed {
		o := raw.(*jsonv.Object)
		switch o.Str("mime_type") {
		case "application/zip":
			frames = o
		case "video/mp4":
			encoded = true
		}
	}
	if frames == nil {
		return nil
	}
	return jsonv.O(
		"type", "video",
		"frames", frames.Get("frames"),
		"fps", frames.Get("fps"),
		"duration_seconds", frames.Get("duration_seconds"),
		"width", frames.Get("width"),
		"height", frames.Get("height"),
		"encoded_video", encoded,
	)
}

// linearManifest is the operations manifest: every operation with its model, inputs, settings,
// worker identity and outputs, then the published and intermediate outputs. Schema 4 is a linear
// job's; schema 5 adds each operation's media facts and is written for every job that runs a
// video or audio operation, linear or not.
func (r *Runtime) linearManifest(job *jsonv.Object, schema float64) *jsonv.Object {
	inputs, operations := []any{}, []any{}
	seen := map[string]bool{}
	for _, op := range Operations(job) {
		backend := op.Obj("backend")
		var input any = jsonv.Absent
		if record := op.Obj("input"); record != nil {
			input = manifestInput(record)
			if !seen[record.Str("input_id")] {
				seen[record.Str("input_id")] = true
				inputs = append(inputs, input)
			}
		}
		var mask any = jsonv.Absent
		if record := op.Obj("mask"); record != nil {
			mask = manifestInput(record).Set("role", "mask")
			if !seen[record.Str("input_id")] {
				seen[record.Str("input_id")] = true
				inputs = append(inputs, mask)
			}
		}
		var overlay any = jsonv.Absent
		if record := op.Obj("overlay"); record != nil {
			overlay = manifestInput(record).Set("role", "overlay")
			if !seen[record.Str("input_id")] {
				seen[record.Str("input_id")] = true
				inputs = append(inputs, overlay)
			}
		}
		for _, role := range []string{"control_image", "reference_image"} {
			if input := op.Obj(role); input != nil && !seen[input.Str("input_id")] {
				seen[input.Str("input_id")] = true
				inputs = append(inputs, manifestInput(input).Set("role", role))
			}
		}
		var scale any = jsonv.Absent
		if op.Str("task") == "upscale" {
			scale = op.Obj("model").Get("scale")
		}
		entry := jsonv.O(
			"op_id", op.Get("op_id"),
			"task", op.Get("task"),
			"family", op.Obj("model").Obj("bundle").Get("family"),
			"scale", scale,
			"model_id", op.Obj("model").Get("model_id"),
			"model", op.Obj("model").Get("bundle"),
			"input", input,
			"mask", mask,
			"overlay", overlay,
			"input_from", jsonv.Maybe(op.Get("input_from")),
			"adapters", manifestAdapters(op),
			"controlnets", manifestControlNets(op),
			"control_image_from", jsonv.Maybe(op.Get("control_image_from")),
			"conditioning", manifestConditioning(op),
			"parameters", op.Get("parameters"),
			"backend", backend,
			"worker_id", nullish(op.Get("worker_id"), job.Get("worker_id")),
			"device_identity", nullish(backend.Get("device_identity")),
			"resolved_options", nullish(backend.Get("resolved_options"), backend.Get("options")),
			"resolutions", nullish(backend.Get("resolutions"), []any{}),
			"applied_fallbacks", nullish(op.Get("applied_fallbacks"), []any{}),
			"worker", jsonv.Maybe(op.Get("worker")),
			"effective_settings", jsonv.Maybe(op.Get("effective_settings")),
			"measurements", jsonv.Maybe(op.Get("measurements")),
			"attempt_id", op.Get("attempt_id"),
			"session_id", op.Get("session_id"),
			"execution_id", op.Get("execution_id"),
			"outputs", op.Get("outputs"),
		)
		for _, role := range []string{"control_image", "reference_image"} {
			if input := op.Obj(role); input != nil {
				entry.Set(role, manifestInput(input).Set("role", role))
			}
		}
		if schema >= 6 {
			entry.Set("source_op_id", jsonv.Maybe(op.Get("source_op_id")))
			entry.Set("cache", jsonv.Maybe(op.Get("cache")))
		}
		if schema >= 5 {
			entry.Set("media", nullish(op.Get("media")))
		}
		operations = append(operations, entry)
	}
	return jsonv.O(
		"schema_version", schema,
		"runtime_version", c.VERSION,
		"job_id", job.Get("job_id"),
		"recipe", job.Get("recipe"),
		"request", job.Get("request"),
		"worker_id", nullish(job.Get("worker_id")),
		"device_identity", nullish(job.Get("device_identity")),
		"inputs", inputs,
		"operations", operations,
		"outputs", job.Get("outputs"),
		"intermediate_outputs", nullish(job.Get("intermediate_outputs"), []any{}),
		"determinism", determinism,
	)
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func statfs(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// workerReport is one worker's section of the diagnostic report.
