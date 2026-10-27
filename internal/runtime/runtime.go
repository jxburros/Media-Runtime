// Package runtime is the service core: request resolution, durable admission, the FIFO
// scheduler that drives the worker, output commitment, diagnostics and retention cleanup.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/store"
	"github.com/jxburros/media-runtime/internal/worker"
)

type Runtime struct {
	Config *config.Config
	Store  *store.Store
	Worker *worker.Client

	// mu serializes every read-modify-write of a job between the scheduler and API handlers,
	// which the single-threaded TypeScript service got from its event loop.
	mu       sync.Mutex
	tickMu   sync.Mutex
	stopping atomic.Bool
	quit     chan struct{}
	loop     sync.WaitGroup
}

func New(cfg *config.Config) (*Runtime, error) {
	s, err := store.Open(cfg.Directory)
	if err != nil {
		return nil, err
	}
	return &Runtime{Config: cfg, Store: s, Worker: worker.New(cfg), quit: make(chan struct{})}, nil
}

// Start launches or checks the worker, then runs the scheduler every poll interval.
func (r *Runtime) Start() error {
	if err := r.Worker.Start(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.Config.Directory, "inputs"), 0o700); err != nil {
		return err
	}
	r.loop.Add(1)
	go func() {
		defer r.loop.Done()
		ticker := time.NewTicker(time.Duration(r.Config.PollIntervalMS) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.quit:
				return
			case <-ticker.C:
				r.Tick()
			}
		}
	}()
	r.Tick()
	return nil
}

// Stop waits for the current tick, stops an owned worker and closes the store.
func (r *Runtime) Stop() {
	if r.stopping.Swap(true) {
		return
	}
	close(r.quit)
	r.loop.Wait()
	r.tickMu.Lock()
	r.tickMu.Unlock()
	r.Worker.Stop()
	r.Store.Close()
}

func (r *Runtime) Owned(jobID, namespace string) (*jsonv.Object, error) {
	job := r.Store.Get(jobID, namespace)
	if job == nil {
		return nil, c.Err(404, "not_found", "Job not found")
	}
	return job, nil
}

func (r *Runtime) InputPath(inputID string) string {
	return filepath.Join(r.Config.Directory, "inputs", inputID+".png")
}

func (r *Runtime) expiry() any {
	days := r.Config.OutputRetentionDays
	if days == nil || *days == 0 {
		return nil
	}
	return c.ISO(time.Now().Add(time.Duration(*days*86400000) * time.Millisecond))
}

// StoreInput keeps a durable, namespace-owned PNG input; the worker only ever receives bytes
// the runtime verified.
func (r *Runtime) StoreInput(bytes []byte, namespace string) (*jsonv.Object, error) {
	if len(bytes) > r.Config.MaxInputBytes {
		return nil, c.Err(413, "input_too_large", "Input exceeds max_input_bytes")
	}
	width, height, err := c.PNGDimensions(bytes)
	if err != nil {
		return nil, err
	}
	record := jsonv.O(
		"input_id", c.NewInputID(),
		"namespace", namespace,
		"mime_type", "image/png",
		"size", float64(len(bytes)),
		"sha256", c.SHA256Hex(bytes),
		"width", float64(width),
		"height", float64(height),
		"created_at", c.Now(),
		"expires_at", r.expiry(),
	)
	if err := atomicFile(r.InputPath(record.Str("input_id")), bytes); err != nil {
		return nil, err
	}
	r.Store.InsertInput(record)
	return record, nil
}

func (r *Runtime) OwnedInput(inputID, namespace string) (*jsonv.Object, error) {
	record := r.Store.Input(inputID, namespace)
	if record == nil {
		return nil, c.Err(404, "not_found", "Input not found")
	}
	return record, nil
}

func (r *Runtime) referenced(inputID string) bool {
	for _, j := range r.Store.All("") {
		if !c.Terminal(j.Str("state")) && j.Obj("input").Str("input_id") == inputID {
			return true
		}
	}
	return false
}

func (r *Runtime) DeleteInput(inputID, namespace string) error {
	if _, err := r.OwnedInput(inputID, namespace); err != nil {
		return err
	}
	if r.referenced(inputID) {
		return c.Err(409, "input_in_use", "An active job references this input")
	}
	if err := os.Remove(r.InputPath(inputID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	r.Store.DeleteInput(inputID)
	return nil
}

// nullish is JavaScript's `a ?? b` over decoded JSON values.
func nullish(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func strList(v any) []string {
	var out []string
	for _, e := range asArray(v) {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func asArray(v any) []any { a, _ := v.([]any); return a }

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	}
	return true
}

// Caller is the authenticated identity a request runs under.
type Caller struct {
	Namespace      string
	AllowFallbacks bool
}

func forbidden(field, message string) error {
	return c.Err(400, "invalid_request", message, field)
}

// Resolve validates a submission against the registries and the live worker. The result is
// the job's task, request, input, adapters, model, recipe and backend, in that order.
func (r *Runtime) Resolve(body any, caller Caller) (*jsonv.Object, error) {
	namespace := caller.Namespace
	request, err := c.Validate(c.Submission, body)
	if err != nil {
		return nil, err
	}
	model := r.Store.Registry("models", request.Str("model_id"))
	recipe := r.Store.Registry("recipes", request.Str("recipe_id"))
	if model == nil {
		return nil, c.Err(400, "unknown_model", "Register the selected model before submitting", "model_id")
	}
	if recipe == nil {
		return nil, c.Err(400, "unknown_recipe", "Recipe not found", "recipe_id")
	}
	if retry := request.Str("retry_of"); retry != "" {
		if _, err := r.Owned(retry, namespace); err != nil {
			return nil, err
		}
	}
	task := recipe.Str("pipeline")
	p, err := r.Worker.Probe()
	if err != nil {
		return nil, err
	}
	info, caps, protocol := p.Info, p.Caps, p.Protocol
	minor := c.Minor(protocol)
	if !truthy(info.Get("service_ready")) || !(truthy(caps.Get("ready")) || cpuFallbackReady(caps, protocol, request)) {
		return nil, c.Err(503, "backend_unavailable", "Worker dependencies or selected device are not ready")
	}
	if b := request.Str("backend_id"); b != "" && b != caps.Str("backend_id") {
		return nil, c.Err(400, "unsupported_backend", "Selected backend is not available", "backend_id")
	}
	tasks := []string{"text-to-image"}
	if _, ok := caps.Get("tasks").([]any); ok {
		tasks = strList(caps.Get("tasks"))
	}
	if !c.Contains(tasks, task) || minor < c.TaskMinor(task) {
		return nil, c.Err(400, "unsupported", "The connected worker does not advertise "+task, "recipe_id")
	}
	// The model's family must serve the recipe, and from 1.2 the worker must load that family.
	bundle := model.Obj("bundle")
	family := bundle.Str("family")
	if family != recipe.Str("model_family") {
		return nil, c.Err(400, "unsupported", "Model "+model.Str("model_id")+" is a "+family+" bundle; recipe "+recipe.Str("recipe_id")+" needs "+recipe.Str("model_family"), "model_id")
	}
	if minor >= 2 && !loads(caps, family) {
		return nil, c.Err(400, "unsupported", "The connected worker does not load the "+family+" family", "model_id")
	}
	var taskParameters *jsonv.Object
	if minor >= 2 {
		taskParameters = caps.Obj("task_parameters").Obj(task)
	}
	inputs := request.Obj("inputs")
	params := request.Obj("parameters")
	usesImage := task == "image-to-image" || task == "upscale"
	if task == "upscale" {
		if inputs.Has("prompt") || inputs.Has("negative_prompt") {
			field := "inputs.prompt"
			if !inputs.Has("prompt") {
				field = "inputs.negative_prompt"
			}
			return nil, forbidden(field, "An upscale takes an image and no prompt")
		}
		for _, k := range []string{"seed", "steps", "guidance", "sampler", "scheduler", "strength", "count"} {
			if params.Has(k) {
				return nil, forbidden("parameters."+k, "An upscale has no "+k+"; it enlarges by the model's fixed factor")
			}
		}
		if request.Has("adapters") {
			return nil, forbidden("adapters", "Adapters do not apply to an upscale")
		}
	} else if !inputs.Has("prompt") {
		return nil, c.Invalid([]string{"inputs", "prompt"})
	} else if params.Has("tile") || params.Has("tile_overlap") {
		field := "parameters.tile"
		if !params.Has("tile") {
			field = "parameters.tile_overlap"
		}
		return nil, forbidden(field, "Tiling applies to upscale recipes only")
	}
	if params.Has("count") {
		if task != "text-to-image" {
			return nil, forbidden("parameters.count", "count applies to text-to-image recipes only")
		}
		if taskParameters == nil || !c.Contains(strList(taskParameters.Get("parameters")), "count") {
			return nil, c.Err(400, "unsupported", "The connected worker does not advertise multiple outputs", "parameters.count")
		}
		count, _ := params.Num("count")
		if limit := asArray(caps.Obj("limits").Get("count")); len(limit) == 2 {
			if high, _ := limit[1].(float64); count > high {
				return nil, c.Err(400, "unsupported", "count exceeds the worker's limit", "parameters.count")
			}
		}
	}
	var input *jsonv.Object
	imageDefaults := jsonv.NewObject()
	if usesImage {
		if !inputs.Has("image") {
			return nil, c.Err(400, "invalid_request", "This recipe requires an uploaded image input", "inputs.image")
		}
		if input, err = r.OwnedInput(inputs.Str("image"), namespace); err != nil {
			return nil, err
		}
		if c.Expired(input.Get("expires_at")) {
			return nil, c.Err(410, "input_expired", "Input is no longer on local storage", "inputs.image")
		}
		imageDefaults.Set("width", input.Get("width")).Set("height", input.Get("height"))
	} else if inputs.Has("image") || params.Has("strength") {
		field := "parameters.strength"
		if inputs.Has("image") {
			field = "inputs.image"
		}
		return nil, c.Err(400, "invalid_request", "Image and strength apply to image-to-image recipes only", field)
	}
	defaults := recipe.Obj("defaults")
	var effective *jsonv.Object
	if task == "upscale" {
		merged := jsonv.NewObject().Assign(caps.Obj("task_defaults").Obj(task)).Assign(taskParameters.Obj("defaults"))
		merged.Assign(defaults).Assign(imageDefaults).Assign(params)
		if effective, err = c.Validate(c.UpscaleParameters, merged); err != nil {
			return nil, err
		}
		if sizes := asArray(caps.Obj("limits").Get("tile_sizes")); len(sizes) > 0 {
			tile, _ := effective.Num("tile")
			if tile != 0 && !containsNumber(sizes, tile) {
				return nil, c.Err(400, "unsupported", "tile must be 0 or an advertised tile size", "parameters.tile")
			}
		}
	} else {
		merged := jsonv.NewObject().Assign(caps.Obj("defaults"))
		if task == "image-to-image" {
			merged.Assign(caps.Obj("task_defaults").Obj(task))
		}
		merged.Assign(defaults).Assign(imageDefaults).Assign(params)
		merged.Set("seed", nullish(params.Get("seed"), defaults.Get("seed"), c.Seed()))
		if effective, err = c.Validate(c.Parameters, merged); err != nil {
			return nil, err
		}
	}
	if input != nil {
		if !jsonEqual(effective.Get("width"), input.Get("width")) || !jsonEqual(effective.Get("height"), input.Get("height")) {
			field := "parameters.height"
			if !jsonEqual(effective.Get("width"), input.Get("width")) {
				field = "parameters.width"
			}
			return nil, c.Err(400, "invalid_request", "Width and height must equal the uploaded image size; nothing is resized", field)
		}
	}
	if task == "image-to-image" {
		if !effective.Has("strength") {
			return nil, c.Err(400, "invalid_request", "Strength is required for image-to-image", "parameters.strength")
		}
		if c.EffectiveSteps(effective) < 1 {
			return nil, c.Err(400, "invalid_request", "steps × strength must denoise for at least one step", "parameters.strength")
		}
	}
	if task == "upscale" {
		scale, _ := model.Num("scale")
		side, ok := caps.Obj("limits").Num("output_side")
		width, _ := effective.Num("width")
		height, _ := effective.Num("height")
		if scale < 1 {
			return nil, c.Err(400, "unsupported", "Re-register this upscaler so its scale is known", "model_id")
		}
		if ok && math.Max(width, height)*scale > side {
			return nil, c.Err(400, "invalid_request", "The upscaled image would exceed the worker's output_side limit", "output_side")
		}
	}
	uses := asArray(nullish(request.Get("adapters"), recipe.Get("adapters"), []any{}))
	adapters := []any{}
	seen := map[string]bool{}
	for _, raw := range uses {
		use := raw.(*jsonv.Object)
		adapter := r.Store.Registry("adapters", use.Str("adapter_id"))
		if adapter == nil {
			return nil, c.Err(400, "unknown_adapter", "Register adapter "+use.Str("adapter_id")+" before using it", "adapters")
		}
		adapters = append(adapters, jsonv.O("adapter", adapter, "scale", use.Get("scale")))
		seen[use.Str("adapter_id")] = true
	}
	if len(seen) != len(uses) {
		return nil, c.Err(400, "invalid_request", "Adapters must be distinct", "adapters")
	}
	capAdapters := caps.Obj("adapters")
	if len(adapters) > 0 && (!truthy(caps.Get("adapters")) || protocol == c.PROTOCOLS[0]) {
		return nil, c.Err(400, "unsupported", "The connected worker does not advertise LoRA adapters", "adapters")
	}
	maxAdapters, _ := capAdapters.Num("max_adapters")
	if float64(len(adapters)) > maxAdapters {
		return nil, c.Err(400, "unsupported", "Too many adapters for this worker", "adapters")
	}
	backendID := caps.Str("backend_id")
	backendOptions := request.Obj("backend_options")
	options, err := c.Validate(c.LoadOptions, jsonv.NewObject().Assign(caps.Obj("load_defaults")).Assign(backendOptions.Obj(backendID)))
	if err != nil {
		return nil, err
	}
	for _, other := range backendOptions.Keys() {
		if other != backendID && truthy(backendOptions.Get(other)) {
			return nil, c.Err(400, "unsupported_backend", "Options for "+other+" do not apply to the "+backendID+" worker", "backend_options")
		}
	}
	if options.Str("precision") == "auto" {
		if minor < 2 || !c.Contains(strList(caps.Get("auto_options")), "precision") {
			return nil, c.Err(400, "unsupported", "The connected worker does not resolve automatic precision", "precision")
		}
	} else if !c.Contains(strList(caps.Get("precisions")), options.Str("precision")) {
		return nil, c.Err(400, "unsupported", "Requested precision is unavailable", "precision")
	}
	fallbacks := strList(request.Get("authorized_fallbacks"))
	if len(fallbacks) > 0 {
		if !caller.AllowFallbacks {
			return nil, c.Err(403, "fallbacks_not_allowed", "This credential may not authorize fallbacks", "authorized_fallbacks")
		}
		distinct := map[string]bool{}
		for _, f := range fallbacks {
			if minor < 2 || !c.Contains(strList(caps.Get("fallbacks")), f) {
				return nil, c.Err(400, "unsupported", "The connected worker does not advertise the "+f+" fallback", "authorized_fallbacks")
			}
			distinct[f] = true
		}
		if len(distinct) != len(fallbacks) {
			return nil, c.Err(400, "invalid_request", "Fallbacks must be distinct", "authorized_fallbacks")
		}
	}
	validateBody := jsonv.O("protocol", protocol, "bundle", bundle, "options", options)
	if len(adapters) > 0 {
		validateBody.Set("adapters", adapters)
	}
	if len(fallbacks) > 0 {
		validateBody.Set("authorized_fallbacks", request.Get("authorized_fallbacks"))
	}
	validation, err := r.Worker.Obj("/v1/validate", "POST", validateBody)
	if err != nil {
		return nil, err
	}
	resolvedRequest := request.Clone().
		Set("parameters", effective).
		Set("adapters", uses).
		Set("backend_id", backendID).
		Set("backend_options", jsonv.O(backendID, options))
	ownership := nullish(caps.Get("ownership"), "reference")
	backend := jsonv.O(
		"backend_id", backendID,
		"ownership", ownership,
		"worker_version", jsonv.Maybe(caps.Get("worker_version")),
		"protocol", protocol,
		"device", jsonv.Maybe(caps.Get("device")),
		"options", options,
		"memory_budget_mb", jsonv.Maybe(caps.Get("memory_budget_mb")),
		"memory_estimate", nullish(validation.Get("memory_estimate")),
	)
	if minor >= 2 {
		// The worker identity and the capability facts this job was admitted against.
		backend.Set("device_identity", jsonv.Maybe(info.Get("device_identity")))
		backend.Set("loader", jsonv.Maybe(loader(caps, family)))
		backend.Set("auto_options", nullish(caps.Get("auto_options"), []any{}))
		backend.Set("fallbacks", nullish(caps.Get("fallbacks"), []any{}))
		backend.Set("task_parameters", jsonv.Maybe(taskParameters))
		backend.Set("resolved_options", jsonv.Maybe(validation.Get("resolved_options")))
		backend.Set("resolutions", nullish(validation.Get("resolutions"), []any{}))
	}
	return jsonv.O(
		"task", task,
		"request", resolvedRequest,
		"input", jsonv.Maybe(input),
		"adapters", adapters,
		"model", model,
		"recipe", recipe,
		"backend", backend,
	), nil
}

// cpuFallbackReady is a worker whose selected device is unavailable but which can run the request
// on its host CPU because the request authorizes that fallback. Nothing else relaxes readiness.
func cpuFallbackReady(caps *jsonv.Object, protocol string, request *jsonv.Object) bool {
	return c.Minor(protocol) >= 2 && caps.Get("device_available") == false && len(caps.Arr("missing_dependencies")) == 0 &&
		c.Contains(strList(caps.Get("fallbacks")), "cpu") && c.Contains(strList(request.Get("authorized_fallbacks")), "cpu")
}

// loader is the worker's loader entry for a family, or nil.
func loader(caps *jsonv.Object, family string) *jsonv.Object {
	for _, raw := range caps.Arr("loaders") {
		if l, ok := raw.(*jsonv.Object); ok && c.Contains(strList(l.Get("families")), family) {
			return l
		}
	}
	return nil
}

// loads reports whether a 1.2 worker advertises a loader for the family.
func loads(caps *jsonv.Object, family string) bool { return loader(caps, family) != nil }

func containsNumber(list []any, n float64) bool {
	for _, v := range list {
		if f, ok := v.(float64); ok && f == n {
			return true
		}
	}
	return false
}

func jsonEqual(a, b any) bool { return string(jsonv.MustMarshal(a)) == string(jsonv.MustMarshal(b)) }

// Submit admits a job durably; a repeated idempotency key returns the original job.
func (r *Runtime) Submit(body any, caller Caller, key *string) (*jsonv.Object, error) {
	namespace := caller.Namespace
	parsed, err := c.Validate(c.Submission, body)
	if err != nil {
		return nil, err
	}
	digest := c.Hash(parsed)
	if key != nil && (len(*key) == 0 || utf16Len(*key) > 200) {
		return nil, c.Err(400, "invalid_idempotency_key", "Idempotency-Key must contain 1–200 characters")
	}
	existing := func(s *store.Store) (*jsonv.Object, error) {
		if key == nil {
			return nil, nil
		}
		stored, jobID, ok := s.Idempotency(namespace, *key)
		if !ok {
			return nil, nil
		}
		if stored != digest {
			return nil, c.Err(409, "idempotency_conflict", "Idempotency key was already used for a different request")
		}
		return s.Get(jobID, ""), nil
	}
	if retry, err := existing(r.Store); err != nil || retry != nil {
		return retry, err
	}
	resolved, err := r.Resolve(parsed, caller)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var job *jsonv.Object
	err = r.Store.Transaction(func(tx *store.Store) error {
		raced, err := existing(tx)
		if err != nil || raced != nil {
			job = raced
			return err
		}
		if len(tx.All("queued")) >= r.Config.QueueCapacity {
			return c.Err(429, "queue_full", "Queue is full; retry later")
		}
		now := c.Now()
		job = jsonv.O(
			"job_id", "job_"+c.UUID(),
			"namespace", namespace,
			"state", "queued",
			"stage", nil,
			"cancel_requested", false,
			"created_at", now,
			"updated_at", now,
		).Assign(resolved).
			Set("worker_sequence", 0.0).
			Set("outputs", []any{}).
			Set("retry_of", jsonv.Maybe(parsed.Get("retry_of")))
		tx.Insert(job)
		tx.Event(job, "job_state", jsonv.O("state", "queued"))
		if key != nil {
			tx.InsertIdempotency(namespace, *key, digest, job.Str("job_id"))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Cancel records cancellation intent; a queued job is cancelled immediately.
func (r *Runtime) Cancel(jobID, namespace string) (*jsonv.Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err := r.Owned(jobID, namespace)
	if err != nil {
		return nil, err
	}
	if c.Terminal(job.Str("state")) {
		return job, nil
	}
	job.Set("cancel_requested", true)
	if job.Get("cancel_at") == nil {
		job.Set("cancel_at", float64(time.Now().UnixMilli()))
	}
	err = r.Store.Transaction(func(tx *store.Store) error {
		tx.Save(job)
		tx.Event(job, "job_state", jsonv.O("state", job.Str("state"), "cancel_requested", true))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if job.Str("state") == "queued" {
		r.Store.Finish(job, "cancelled", nil)
	}
	return r.Store.Get(jobID, ""), nil
}

// refresh reloads cancellation intent, the only job field API handlers change while it runs.
func (r *Runtime) refresh(job *jsonv.Object) {
	latest := r.Store.Get(job.Str("job_id"), "")
	if latest == nil {
		return
	}
	job.Set("cancel_requested", latest.Get("cancel_requested"))
	job.Set("cancel_at", jsonv.Maybe(latest.Get("cancel_at")))
}

// save writes the scheduler's copy of a job (plus any events) without losing a cancellation.
func (r *Runtime) save(job *jsonv.Object, events func(tx *store.Store) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(job)
	return r.Store.Transaction(func(tx *store.Store) error {
		if events != nil {
			if err := events(tx); err != nil {
				return err
			}
		}
		tx.Save(job)
		return nil
	})
}

func (r *Runtime) finish(job *jsonv.Object, state string, errorRecord *jsonv.Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(job)
	r.Store.Finish(job, state, errorRecord)
}

func (r *Runtime) firstRunning() *jsonv.Object {
	if running := r.Store.All("running"); len(running) > 0 {
		return running[0]
	}
	return nil
}

// Tick advances the scheduler by one step; overlapping calls return immediately.
func (r *Runtime) Tick() {
	if !r.tickMu.TryLock() {
		return
	}
	defer r.tickMu.Unlock()
	if r.stopping.Load() {
		return
	}
	err := r.tick()
	var api *c.ApiError
	if err == nil || !errors.As(err, &api) {
		return
	}
	// Transient transport errors preserve dispatch identity for reconciliation; never regenerate.
	if job := r.firstRunning(); job != nil && api.Code != "backend_unavailable" {
		code, message := api.Code, api.Message
		if api.Status == 404 {
			code, message = "execution_interrupted", "Worker record is no longer available"
		}
		r.finish(job, "failed", jsonv.O("code", code, "message", message))
	}
}

func (r *Runtime) tick() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("scheduler: %v", p)
		}
	}()
	if child := r.Worker.Child(); child != nil && child.Exited() {
		if interrupted := r.firstRunning(); interrupted != nil {
			r.finish(interrupted, "failed", jsonv.O("code", "execution_interrupted", "message", "Owned worker exited; this job was not regenerated"))
		}
		r.Worker.ClearChild()
		if err := r.Worker.Start(); err != nil {
			return err
		}
	}
	p, err := r.Worker.Probe()
	if err != nil {
		return err
	}
	info, caps, protocol := p.Info, p.Caps, p.Protocol
	session := info.Get("session_id")
	// Release only after durable terminal commit. Retry collection acknowledgements after disconnects.
	for _, done := range r.Store.All("") {
		if !c.Terminal(done.Str("state")) || truthy(done.Get("released")) ||
			!(truthy(done.Get("execution_id")) || truthy(done.Get("worker_input_id"))) || !jsonEqual(done.Get("session_id"), session) {
			continue
		}
		err := func() error {
			if id := done.Str("execution_id"); id != "" {
				if _, err := r.Worker.JSON("/v1/executions/"+id, "DELETE", nil, ""); err != nil {
					return err
				}
			}
			if id := done.Str("worker_input_id"); id != "" {
				if _, err := r.Worker.JSON("/v1/inputs/"+id, "DELETE", nil, ""); err != nil {
					var api *c.ApiError
					if !errors.As(err, &api) || api.Status != 404 {
						return err
					}
				}
			}
			return nil
		}()
		var api *c.ApiError
		if err == nil || (errors.As(err, &api) && api.Status == 404) {
			done.Set("released", true)
			if err := r.save(done, nil); err != nil {
				return err
			}
		}
	}
	job := r.firstRunning()
	if job == nil {
		if info.Str("state") != "ready" {
			return nil
		}
		r.mu.Lock()
		queued := r.Store.All("queued")
		if len(queued) == 0 || !(truthy(caps.Get("ready")) || cpuFallbackReady(caps, protocol, queued[0].Obj("request"))) {
			r.mu.Unlock()
			return nil
		}
		job = queued[0]
		err := r.Store.Transaction(func(tx *store.Store) error {
			job.Set("state", "running")
			job.Obj("backend").Set("worker_version", jsonv.Maybe(caps.Get("worker_version")))
			job.Set("stage", nil)
			job.Set("attempt_id", "attempt_"+c.UUID())
			job.Set("session_id", jsonv.Maybe(session))
			job.Set("dispatch_phase", "loading")
			tx.Save(job)
			tx.Event(job, "job_state", jsonv.O("state", "running"))
			return nil
		})
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if !jsonEqual(job.Get("session_id"), session) {
		r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Worker session ended; this job was not regenerated"))
		return nil
	}
	r.refresh(job)
	grace := time.Duration(r.Config.CancellationGraceMS) * time.Millisecond
	if truthy(job.Get("cancel_requested")) && r.Worker.Child() != nil {
		at, ok := job.Num("cancel_at")
		if !ok || at == 0 {
			at = float64(time.Now().UnixMilli())
		}
		if time.Since(time.UnixMilli(int64(at))) > grace {
			r.Worker.Stop()
			r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Owned worker was forcibly stopped after the cancellation deadline", "forced_termination", true))
			return r.Worker.Start()
		}
	}
	if job.Str("dispatch_phase") == "loading" {
		if opID := job.Str("load_operation_id"); opID != "" {
			operation, err := r.Worker.Obj("/v1/operations/"+opID, "GET", nil)
			if err != nil {
				return err
			}
			if operation.Str("state") == "running" {
				return nil
			}
			if _, err := r.Worker.JSON("/v1/operations/"+opID, "DELETE", nil, ""); err != nil {
				return err
			}
			job.Delete("load_operation_id")
			if operation.Str("state") == "failed" {
				code := operation.Obj("error").Str("code")
				if code == "" {
					code = "model_load_failed"
				}
				r.finish(job, "failed", jsonv.O("code", code, "message", "Selected model could not be loaded"))
				return nil
			}
			if estimate := operation.Get("memory_estimate"); truthy(estimate) {
				job.Obj("backend").Set("memory_estimate", estimate)
			}
			for _, k := range []string{"resolved_options", "resolutions"} {
				if v := operation.Get(k); v != nil {
					job.Obj("backend").Set(k, v)
				}
			}
			return r.save(job, nil)
		}
		if s := info.Str("state"); s == "loading" || s == "unloading" {
			return nil
		}
		r.refresh(job)
		if truthy(job.Get("cancel_requested")) {
			r.finish(job, "cancelled", nil)
			return nil
		}
		desired := job.Obj("request").Obj("backend_options").Get(job.Obj("backend").Str("backend_id"))
		identity := []any{}
		for _, raw := range asArray(job.Get("adapters")) {
			a := raw.(*jsonv.Object)
			adapter := a.Obj("adapter")
			identity = append(identity, jsonv.O("adapter_id", adapter.Get("adapter_id"), "digest", adapter.Get("digest"), "scale", a.Get("scale")))
		}
		loaded := info.Obj("loaded_bundle")
		loadedIdentity := []any{}
		for _, raw := range asArray(loaded.Get("adapters")) {
			a, _ := raw.(*jsonv.Object)
			loadedIdentity = append(loadedIdentity, jsonv.O("adapter_id", jsonv.Maybe(a.Get("adapter_id")), "digest", jsonv.Maybe(a.Get("digest")), "scale", jsonv.Maybe(a.Get("scale"))))
		}
		if loaded == nil || !jsonEqual(loaded.Get("bundle_digest"), job.Obj("model").Obj("bundle").Get("bundle_digest")) ||
			c.Hash(loaded.Get("options")) != c.Hash(desired) || c.Hash(loadedIdentity) != c.Hash(identity) {
			body := jsonv.O("protocol", protocol, "bundle", job.Obj("model").Get("bundle"), "options", desired)
			if len(identity) > 0 {
				body.Set("adapters", job.Get("adapters"))
			}
			if fallbacks := job.Obj("request").Arr("authorized_fallbacks"); len(fallbacks) > 0 {
				body.Set("authorized_fallbacks", fallbacks)
			}
			op, err := r.Worker.Obj("/v1/model/load", "POST", body)
			if err != nil {
				return err
			}
			job.Set("load_operation_id", jsonv.Maybe(op.Get("operation_id")))
			job.Set("stage", "loading")
			return r.save(job, func(tx *store.Store) error {
				tx.Event(job, "stage", jsonv.O("stage", "loading"))
				return nil
			})
		}
		job.Set("dispatch_phase", "executing")
		// Record any fallback the loaded bundle runs on; it was authorized by this or an earlier job.
		var applied []any
		if c.Minor(protocol) >= 2 {
			applied = asArray(nullish(loaded.Get("applied_fallbacks"), []any{}))
			job.Set("applied_fallbacks", applied)
			job.Obj("backend").Set("applied_fallbacks", applied)
		}
		if err := r.save(job, func(tx *store.Store) error {
			if len(applied) > 0 {
				tx.Event(job, "stage", jsonv.O("stage", "loading", "fallback", applied))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	var snapshot *jsonv.Object
	if job.Str("execution_id") == "" {
		// Query the same session before resending an ambiguous acceptance. A retry uses the SAME attempt.
		snapshot, err = r.Worker.Obj("/v1/executions?attempt_id="+url.QueryEscape(job.Str("attempt_id")), "GET", nil)
		if err != nil {
			var api *c.ApiError
			if !errors.As(err, &api) || api.Status != 404 {
				return err
			}
			r.refresh(job)
			if truthy(job.Get("cancel_requested")) {
				r.finish(job, "cancelled", nil)
				return nil
			}
			inputs := job.Obj("request").Obj("inputs").Clone()
			if task := job.Str("task"); task == "image-to-image" || task == "upscale" {
				if job.Str("worker_input_id") == "" {
					// Hand the worker the verified bytes under a session-scoped handle, never a path.
					record := job.Obj("input")
					bytes, err := os.ReadFile(r.InputPath(record.Str("input_id")))
					if err != nil || c.SHA256Hex(bytes) != record.Str("sha256") {
						return c.Err(410, "input_unavailable", "Input bytes changed or disappeared before dispatch")
					}
					uploaded, err := r.Worker.JSON("/v1/inputs", "POST", bytes, "image/png")
					if err != nil {
						return err
					}
					up, _ := uploaded.(*jsonv.Object)
					if up.Str("sha256") != record.Str("sha256") {
						return c.Err(503, "protocol_mismatch", "Worker input digest mismatch")
					}
					job.Set("worker_input_id", jsonv.Maybe(up.Get("input_id")))
					if err := r.save(job, nil); err != nil {
						return err
					}
				}
				inputs.Set("image", job.Get("worker_input_id"))
			}
			snapshot, err = r.Worker.Obj("/v1/executions", "POST", jsonv.O(
				"protocol", protocol,
				"attempt_id", job.Get("attempt_id"),
				"bundle_digest", job.Obj("model").Obj("bundle").Get("bundle_digest"),
				"task", job.Get("task"),
				"inputs", inputs,
				"parameters", job.Obj("request").Get("parameters"),
			))
			if err != nil {
				return err
			}
		}
		if !jsonEqual(snapshot.Get("session_id"), job.Get("session_id")) || !jsonEqual(snapshot.Get("attempt_id"), job.Get("attempt_id")) {
			return c.Err(503, "protocol_mismatch", "Worker execution identity mismatch")
		}
		job.Set("execution_id", jsonv.Maybe(snapshot.Get("execution_id")))
		if err := r.save(job, nil); err != nil {
			return err
		}
	} else if snapshot, err = r.Worker.Obj("/v1/executions/"+job.Str("execution_id"), "GET", nil); err != nil {
		return err
	}
	r.refresh(job)
	if truthy(job.Get("cancel_requested")) && !c.Terminal(snapshot.Str("state")) {
		if snapshot, err = r.Worker.Obj("/v1/executions/"+job.Str("execution_id")+"/cancel", "POST", jsonv.NewObject()); err != nil {
			return err
		}
	}
	err = r.save(job, func(tx *store.Store) error {
		for _, raw := range snapshot.Arr("events") {
			event, _ := raw.(*jsonv.Object)
			sequence, _ := event.Num("sequence")
			current, _ := job.Num("worker_sequence")
			if sequence <= current {
				continue
			}
			if sequence != current+1 {
				return c.Err(503, "event_gap", "Worker event history has a gap")
			}
			data := event.Get("data")
			switch event.Str("type") {
			case "stage":
				job.Set("stage", jsonv.Maybe(event.Obj("data").Get("stage")))
				job.Delete("progress")
				tx.Event(job, "stage", data)
			case "progress":
				job.Set("progress", jsonv.Maybe(data))
				tx.Event(job, "progress", data)
			}
			job.Set("worker_sequence", sequence)
		}
		return nil
	})
	if err != nil {
		return err
	}
	switch snapshot.Str("state") {
	case "succeeded":
		return r.commitOutputs(job, snapshot)
	case "failed":
		code := snapshot.Obj("error").Str("code")
		if code == "" {
			code = "execution_failed"
		}
		r.finish(job, "failed", jsonv.O("code", code, "message", "Worker execution failed", "restart_required", truthy(snapshot.Obj("error").Get("restart_required"))))
	case "cancelled":
		r.finish(job, "cancelled", nil)
	}
	return nil
}

// atomicFile writes a temporary file, fsyncs it and renames it into place.
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

// OutputFile is the stored path of one committed output.
func (r *Runtime) OutputFile(jobID string, output *jsonv.Object) string {
	ext := c.Extensions[output.Str("mime_type")]
	if ext == "" {
		ext = "png"
	}
	return filepath.Join(r.OutputDirectory(jobID), output.Str("output_id")+"."+ext)
}

// commitOutputs verifies every output the worker advertised for the task, then writes their
// bytes, the manifest and the database in that order; success is recorded only after
// everything is durable, and a partial set is output_unavailable.
func (r *Runtime) commitOutputs(job, snapshot *jsonv.Object) error {
	directory := r.OutputDirectory(job.Str("job_id"))
	err := func() error {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		backend := job.Obj("backend")
		params := job.Obj("request").Obj("parameters")
		task := job.Str("task")
		count, ok := params.Num("count")
		if !ok {
			count = 1
		}
		taskParameters := backend.Obj("task_parameters")
		expected := c.OutputIDs(taskParameters, int(count))
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
			scale, _ = job.Obj("model").Num("scale")
			width, height = width*scale, height*scale
		}
		seed, seeded := params.Num("seed")
		committed := []any{}
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
			res, err := r.Worker.Response("/v1/executions/"+job.Str("execution_id")+"/outputs/"+id, "GET", nil, "")
			if err != nil {
				return err
			}
			bytes, err := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
			res.Body.Close()
			if err != nil {
				return err
			}
			if len(bytes) > limit {
				return errors.New("output too large")
			}
			if float64(len(bytes)) != size || c.SHA256Hex(bytes) != output.Str("sha256") || !check(bytes, output) {
				return errors.New("output integrity failure")
			}
			if mime == "image/png" {
				if w, _ := output.Num("width"); w != width {
					return errors.New("output dimensions changed")
				}
				if h, _ := output.Num("height"); h != height {
					return errors.New("output dimensions changed")
				}
			}
			if err := atomicFile(r.OutputFile(job.Str("job_id"), output), bytes); err != nil {
				return err
			}
			record := output.Clone()
			if seeded && task == "text-to-image" {
				// Output k of a batch used seed + k − 1, so any one image can be reproduced alone.
				record.Set("seed", seed+float64(k))
			}
			committed = append(committed, record.Set("expires_at", r.expiry()))
		}
		job.Set("outputs", committed)
		input := job.Obj("input")
		inputs := []any{}
		if input != nil {
			inputs = append(inputs, jsonv.O(
				"input_id", input.Get("input_id"),
				"mime_type", input.Get("mime_type"),
				"size", input.Get("size"),
				"sha256", input.Get("sha256"),
				"width", input.Get("width"),
				"height", input.Get("height"),
			))
		}
		adapters := []any{}
		for _, raw := range asArray(job.Get("adapters")) {
			a := raw.(*jsonv.Object)
			adapter := a.Obj("adapter")
			adapters = append(adapters, jsonv.O(
				"adapter_id", adapter.Get("adapter_id"),
				"digest", adapter.Get("digest"),
				"scale", a.Get("scale"),
				"source", jsonv.Maybe(adapter.Get("source")),
			))
		}
		var scaleValue any = jsonv.Absent
		if task == "upscale" {
			scaleValue = scale
		}
		manifest := jsonv.O(
			"schema_version", 3.0,
			"runtime_version", c.VERSION,
			"job_id", job.Get("job_id"),
			"task", job.Get("task"),
			"family", job.Obj("model").Obj("bundle").Get("family"),
			"scale", scaleValue,
			"recipe", job.Get("recipe"),
			"model", job.Obj("model").Get("bundle"),
			"request", job.Get("request"),
			"inputs", inputs,
			"adapters", adapters,
			"backend", job.Get("backend"),
			"device_identity", nullish(backend.Get("device_identity")),
			"resolved_options", nullish(backend.Get("resolved_options"), backend.Get("options")),
			"resolutions", nullish(backend.Get("resolutions"), []any{}),
			"applied_fallbacks", nullish(job.Get("applied_fallbacks"), []any{}),
			"worker", jsonv.Maybe(snapshot.Get("implementation")),
			"effective_settings", jsonv.Maybe(snapshot.Get("effective_settings")),
			"measurements", jsonv.Maybe(snapshot.Get("measurements")),
			"attempt_id", job.Get("attempt_id"),
			"session_id", job.Get("session_id"),
			"execution_id", job.Get("execution_id"),
			"outputs", committed,
			"determinism", "Seed reproducibility depends on hardware, software and dependency versions; matching bytes are not guaranteed.",
		)
		job.Set("manifest", manifest)
		if err := atomicFile(filepath.Join(directory, "manifest.json"), jsonv.Indent(manifest)); err != nil {
			return err
		}
		for _, path := range []string{directory, filepath.Join(r.Config.Directory, "outputs"), r.Config.Directory} {
			if err := syncDir(path); err != nil {
				return err
			}
		}
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
		if errors.As(err, &api) && api.Code == "backend_unavailable" {
			return err
		}
		r.finish(job, "failed", jsonv.O("code", "output_unavailable", "message", "Output could not be verified and committed to durable storage"))
	}
	return nil
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

// Doctor is the diagnostic report behind `doctor` and GET /v1/runtime.
func (r *Runtime) Doctor() (*jsonv.Object, error) {
	var w *jsonv.Object
	p, err := r.Worker.Probe()
	if err == nil {
		info, caps := p.Info, p.Caps
		w = jsonv.O(
			"protocol", p.Protocol,
			"supported_protocols", nullish(info.Get("supported_protocols"), []any{info.Get("protocol")}),
			"worker_version", jsonv.Maybe(info.Get("worker_version")),
			"backend_id", caps.Get("backend_id"),
			"ownership", nullish(caps.Get("ownership"), "reference"),
			"owned_process", len(r.Config.Worker.Command) > 0,
			"session_id", jsonv.Maybe(info.Get("session_id")),
			"state", jsonv.Maybe(info.Get("state")),
			"service_ready", jsonv.Maybe(info.Get("service_ready")),
			"device", jsonv.Maybe(caps.Get("device")),
			"device_ready", jsonv.Maybe(caps.Get("ready")),
			"missing_dependencies", nullish(caps.Get("missing_dependencies"), []any{}),
			"dependencies", nullish(caps.Get("dependencies"), jsonv.NewObject()),
			"tasks", nullish(caps.Get("tasks"), []any{"text-to-image"}),
			"adapters", nullish(caps.Get("adapters")),
			"precisions", jsonv.Maybe(caps.Get("precisions")),
			"load_defaults", jsonv.Maybe(caps.Get("load_defaults")),
			"memory_budget_mb", nullish(caps.Get("memory_budget_mb")),
			"memory_estimate", nullish(caps.Get("memory_estimate")),
			"loaded_bundle_digest", nullish(info.Obj("loaded_bundle").Get("bundle_digest")),
		)
		if c.Minor(p.Protocol) >= 2 {
			w.Set("device_identity", nullish(info.Get("device_identity")))
			w.Set("loaders", nullish(caps.Get("loaders"), []any{}))
			w.Set("auto_options", nullish(caps.Get("auto_options"), []any{}))
			w.Set("fallbacks", nullish(caps.Get("fallbacks"), []any{}))
			w.Set("applied_fallbacks", nullish(info.Obj("loaded_bundle").Get("applied_fallbacks"), []any{}))
		}
	} else {
		code, message := "backend_unavailable", "Worker is unavailable"
		var api *c.ApiError
		if errors.As(err, &api) {
			code, message = api.Code, api.Message
		}
		w = jsonv.O("error", code, "message", message)
	}
	available, err := statfs(r.Config.Directory)
	if err != nil {
		return nil, err
	}
	writable := syscall.Access(r.Config.Directory, 2) == nil
	models := []any{}
	for _, model := range r.Store.RegistryAll("models") {
		root := r.Config.Catalog(model.Str("artifact_handle"))
		missing := []string{}
		seen := map[string]bool{}
		bundle := model.Obj("bundle")
		for _, relative := range bundle.Obj("configuration").Obj("files").Keys() {
			if root == "" || !exists(filepath.Join(root, relative)) {
				role := strings.Split(relative, "/")[0]
				if !seen[role] {
					seen[role] = true
					missing = append(missing, role)
				}
			}
		}
		roles := make([]any, len(missing))
		for i, m := range missing {
			roles[i] = m
		}
		models = append(models, jsonv.O(
			"model_id", model.Get("model_id"),
			"artifact_handle", model.Get("artifact_handle"),
			"family", nullish(bundle.Get("family"), "sdxl-base"),
			"scale", jsonv.Maybe(model.Get("scale")),
			"conversion", jsonv.Maybe(bundle.Obj("source").Get("conversion")),
			"bundle_digest", bundle.Get("bundle_digest"),
			"available", len(missing) == 0,
			"missing_roles", roles,
			"loaded", jsonEqual(w.Get("loaded_bundle_digest"), bundle.Get("bundle_digest")) && w.Has("loaded_bundle_digest"),
		))
	}
	adapters := []any{}
	for _, adapter := range r.Store.RegistryAll("adapters") {
		root := r.Config.Catalog(adapter.Str("artifact_handle"))
		ok := true
		for _, relative := range adapter.Obj("files").Keys() {
			if root == "" || !exists(filepath.Join(root, relative)) {
				ok = false
			}
		}
		adapters = append(adapters, jsonv.O("adapter_id", adapter.Get("adapter_id"), "digest", adapter.Get("digest"), "available", ok))
	}
	recipes := []any{}
	for _, recipe := range r.Store.RegistryAll("recipes") {
		recipes = append(recipes, jsonv.O("recipe_id", recipe.Get("recipe_id"), "pipeline", recipe.Get("pipeline")))
	}
	queued, running := 0, 0
	for _, j := range r.Store.All("") {
		switch j.Str("state") {
		case "queued":
			queued++
		case "running":
			running++
		}
	}
	var retention any
	if r.Config.OutputRetentionDays != nil {
		retention = *r.Config.OutputRetentionDays
	}
	protocols := make([]any, len(c.PROTOCOLS))
	for i, p := range c.PROTOCOLS {
		protocols[i] = p
	}
	return jsonv.O(
		"runtime_version", c.VERSION,
		"protocols", protocols,
		"worker", w,
		"models", models,
		"adapters", adapters,
		"recipes", recipes,
		"storage", jsonv.O("writable", writable, "available_bytes", float64(available), "output_retention_days", retention),
		"queue", jsonv.O("capacity", float64(r.Config.QueueCapacity), "queued", float64(queued), "running", float64(running)),
		"queue_capacity", float64(r.Config.QueueCapacity),
	), nil
}

// Cleanup removes expired outputs and expired inputs no active job references.
func (r *Runtime) Cleanup() error {
	for _, job := range r.Store.All("succeeded") {
		for _, raw := range job.Arr("outputs") {
			output, _ := raw.(*jsonv.Object)
			if c.Expired(output.Get("expires_at")) {
				path := r.OutputFile(job.Str("job_id"), output)
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	for _, input := range r.Store.Inputs("") {
		if c.Expired(input.Get("expires_at")) && !r.referenced(input.Str("input_id")) {
			if err := os.Remove(r.InputPath(input.Str("input_id"))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
