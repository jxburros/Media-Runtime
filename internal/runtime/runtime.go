// Package runtime is the service core: request resolution, durable admission, the FIFO
// scheduler that drives the worker pool, output commitment, diagnostics and retention cleanup.
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
	"github.com/jxburros/media-runtime/internal/pool"
	"github.com/jxburros/media-runtime/internal/store"
	"github.com/jxburros/media-runtime/internal/worker"
)

type Runtime struct {
	Config *config.Config
	Store  *store.Store
	// Pool is every configured worker. Worker is the first of them, the only one of a legacy
	// single-worker configuration.
	Pool   *pool.Pool
	Worker *worker.Client

	// mu serializes every read-modify-write of a job between the worker loops and API handlers,
	// which the single-threaded TypeScript service got from its event loop. Dispatch decisions
	// are taken under it, so two workers never take the same job.
	mu sync.Mutex
	// lanes run at most one scheduler step at a time for each worker.
	lanes    map[string]*sync.Mutex
	stopping atomic.Bool
	quit     chan struct{}
	loop     sync.WaitGroup
}

func New(cfg *config.Config) (*Runtime, error) {
	s, err := store.Open(cfg.Directory)
	if err != nil {
		return nil, err
	}
	p := pool.New(cfg)
	lanes := map[string]*sync.Mutex{}
	for _, m := range p.Members {
		lanes[m.ID] = &sync.Mutex{}
		// A remote worker's recorded certificate fingerprint lives in the state directory.
		m.Client.SetTrust(s)
	}
	return &Runtime{Config: cfg, Store: s, Pool: p, Worker: p.Members[0].Client, lanes: lanes, quit: make(chan struct{})}, nil
}

// Start launches or checks every worker, then runs one scheduler loop per worker, each every
// poll interval.
func (r *Runtime) Start() error {
	if err := r.Pool.Start(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.Config.Directory, "inputs"), 0o700); err != nil {
		return err
	}
	r.unconfigured()
	for _, m := range r.Pool.Members {
		r.loop.Add(1)
		go func(m *pool.Member) {
			defer r.loop.Done()
			ticker := time.NewTicker(time.Duration(r.Config.PollIntervalMS) * time.Millisecond)
			defer ticker.Stop()
			r.Tick(m)
			for {
				select {
				case <-r.quit:
					return
				case <-ticker.C:
					r.Tick(m)
				}
			}
		}(m)
	}
	return nil
}

// unconfigured fails running jobs whose worker is no longer in the configuration: nothing can
// reconcile their attempt.
func (r *Runtime) unconfigured() {
	for _, job := range r.Store.All("running") {
		if r.Pool.Get(job.Str("worker_id")) == nil {
			r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Worker "+job.Str("worker_id")+" is no longer configured; this job was not regenerated"))
		}
	}
}

// Stop waits for every worker's current step, stops owned workers and closes the store.
func (r *Runtime) Stop() {
	if r.stopping.Swap(true) {
		return
	}
	close(r.quit)
	r.loop.Wait()
	for _, lane := range r.lanes {
		lane.Lock()
		lane.Unlock()
	}
	r.Pool.Stop()
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
		if c.Terminal(j.Str("state")) {
			continue
		}
		if j.Obj("input").Str("input_id") == inputID {
			return true
		}
		for _, op := range Operations(j) {
			if op.Obj("input").Str("input_id") == inputID {
				return true
			}
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

// Resolve validates a submission against the registries and the worker pool. The result is
// the job's task, request, input, adapters, model, recipe and backend, in that order, followed by
// every operation resolved up front so admission is all-or-nothing, and the candidate workers.
// The top-level task, input, adapters, model and backend are those of the first operation.
//
// Every probed worker (or only the pinned one) is checked against the request's tasks, families,
// protocol minor and options; the workers that pass are the job's candidates. The first candidate
// in configuration order resolves the job, including the worker's bundle validation. With no
// candidate the first worker's refusal is returned, exactly as a pool of one refuses.
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
	members := r.Pool.Members
	if pin := request.Str("worker_id"); pin != "" {
		m := r.Pool.Get(pin)
		if m == nil {
			return nil, c.Err(400, "unknown_worker", "Worker "+pin+" is not configured", "worker_id")
		}
		members = []*pool.Member{m}
	}
	probes := make([]*worker.Probe, len(members))
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Add(1)
		go func(i int, m *pool.Member) { defer wg.Done(); probes[i], errs[i] = r.Pool.Probe(m) }(i, m)
	}
	wg.Wait()
	var first error
	candidates := []any{}
	primary := -1
	for i, m := range members {
		err := errs[i]
		if err == nil {
			_, err = r.resolveOn(m, probes[i], request, recipe, model, caller, false)
		}
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		candidates = append(candidates, jsonv.O("worker_id", m.ID, "protocol", probes[i].Protocol))
		if primary < 0 {
			primary = i
		}
	}
	if primary < 0 {
		return nil, first
	}
	resolved, err := r.resolveOn(members[primary], probes[primary], request, recipe, model, caller, true)
	if err != nil {
		return nil, err
	}
	return resolved.Set("candidates", candidates), nil
}

// resolveOn resolves a submission against one probed worker; validate also asks the worker to
// validate every operation's bundle, which hashes it, so only the resolving worker does that.
func (r *Runtime) resolveOn(m *pool.Member, p *worker.Probe, request, recipe, model *jsonv.Object, caller Caller, validate bool) (*jsonv.Object, error) {
	w := &workerView{client: m.Client, validate: validate, info: p.Info, caps: p.Caps, protocol: p.Protocol, minor: c.Minor(p.Protocol)}
	if !truthy(w.info.Get("service_ready")) || !(truthy(w.caps.Get("ready")) || cpuFallbackReady(w.caps, w.protocol, request)) {
		return nil, c.Err(503, "backend_unavailable", "Worker dependencies or selected device are not ready")
	}
	if b := request.Str("backend_id"); b != "" && b != w.caps.Str("backend_id") {
		return nil, c.Err(400, "unsupported_backend", "Selected backend is not available", "backend_id")
	}
	inputs := request.Obj("inputs")
	params := request.Obj("parameters")
	var plans []*operationPlan
	var err error
	if !c.IsLinear(recipe) {
		if request.Has("models") {
			return nil, forbidden("models", "models selects later operations of schema-2 recipes")
		}
		if _, err := c.Inputs(inputs, []string{"inputs"}); err != nil {
			return nil, err
		}
		plans = []*operationPlan{{
			op:     c.NormalizeRecipe(recipe).Arr("operations")[0].(*jsonv.Object),
			model:  model,
			inputs: inputs,
			params: params,
			uses:   asArray(nullish(request.Get("adapters"), recipe.Get("adapters"), []any{})),
			first:  true,
		}}
	} else if plans, err = r.linearPlans(recipe, request, model); err != nil {
		return nil, err
	}
	resolved := []*jsonv.Object{}
	var first *resolvedOperation
	for i, plan := range plans {
		if from := plan.from; from != nil {
			// The bound image's size is the producing operation's output size.
			producer := resolved[int(from.index)]
			width, _ := producer.Obj("parameters").Num("width")
			height, _ := producer.Obj("parameters").Num("height")
			if producer.Str("task") == "upscale" {
				scale, _ := producer.Obj("model").Num("scale")
				width, height = width*scale, height*scale
			}
			plan.fromSize = [2]float64{width, height}
		}
		op, err := r.resolveOperation(plan, request, w, caller)
		if err != nil {
			var job jobLevel
			if errors.As(err, &job) {
				return nil, job.err
			}
			if c.IsLinear(recipe) {
				return nil, inOperation(err, plan.op.Str("op_id"))
			}
			return nil, err
		}
		if i == 0 {
			first = op
		}
		resolved = append(resolved, op.operation)
	}
	resolvedRequest := request.Clone().
		Set("parameters", first.operation.Get("parameters")).
		Set("adapters", first.uses).
		Set("backend_id", first.backendID).
		Set("backend_options", jsonv.O(first.backendID, first.options))
	operations := make([]any, len(resolved))
	for i, op := range resolved {
		operations[i] = op
	}
	return jsonv.O(
		"task", first.operation.Get("task"),
		"request", resolvedRequest,
		"input", jsonv.Maybe(first.operation.Get("input")),
		"adapters", first.operation.Get("adapters"),
		"model", model,
		"recipe", recipe,
		"backend", first.operation.Get("backend"),
		"operations", operations,
	), nil
}

// workerView is the probed worker a submission is resolved against.
type workerView struct {
	client     *worker.Client
	validate   bool
	info, caps *jsonv.Object
	protocol   string
	minor      int
}

// operationPlan is one operation of a submission before resolution: its recipe operation, the
// model selected for it, its task inputs, its parameter overrides and its adapters.
type operationPlan struct {
	op       *jsonv.Object
	model    *jsonv.Object
	inputs   *jsonv.Object
	from     *binding
	fromSize [2]float64
	params   *jsonv.Object
	uses     []any
	// first is the operation the submission's model_id, parameters and adapters select.
	first bool
}

// binding is an image bound from an earlier operation's output.
type binding struct {
	index        float64
	opID, output string
}

type resolvedOperation struct {
	operation *jsonv.Object
	uses      []any
	backendID string
	options   *jsonv.Object
}

// jobLevel marks an error about the whole submission (load options, fallbacks) rather than one
// operation, so it is reported without an operation prefix.
type jobLevel struct{ err error }

func (j jobLevel) Error() string { return j.err.Error() }

// inOperation names the operation in an error's field: operations.<op_id>.<field>.
func inOperation(err error, opID string) error {
	var api *c.ApiError
	if !errors.As(err, &api) {
		return err
	}
	named := *api
	named.Field = "operations." + opID
	if api.Field != "" {
		named.Field += "." + api.Field
	}
	return &named
}

// linearPlans binds a schema-2 submission to the recipe's operations: declared inputs are
// checked, every operation gets its model (model_id for the first, models[op_id] for the rest),
// and the first operation takes the submission's parameters and adapters.
func (r *Runtime) linearPlans(recipe, request, model *jsonv.Object) ([]*operationPlan, error) {
	normalized := c.NormalizeRecipe(recipe)
	declared := normalized.Obj("inputs")
	submitted := request.Obj("inputs")
	if err := c.SubmittedInputs(declared, submitted); err != nil {
		return nil, err
	}
	ops := normalized.Arr("operations")
	index := map[string]int{}
	for i, raw := range ops {
		index[raw.(*jsonv.Object).Str("op_id")] = i
	}
	models := request.Obj("models")
	for _, opID := range models.Keys() {
		if i, ok := index[opID]; !ok || i == 0 {
			return nil, c.Err(400, "invalid_request", "models names operations after the first; model_id selects the first", "models."+opID)
		}
	}
	var plans []*operationPlan
	for i, raw := range ops {
		op := raw.(*jsonv.Object)
		opID := op.Str("op_id")
		plan := &operationPlan{op: op, model: model, inputs: jsonv.NewObject(), params: jsonv.NewObject()}
		if i == 0 {
			plan.first = true
			plan.params = request.Obj("parameters").Clone()
			plan.uses = asArray(nullish(request.Get("adapters"), op.Get("adapters"), []any{}))
		} else {
			plan.uses = asArray(nullish(op.Get("adapters"), []any{}))
			if !models.Has(opID) {
				return nil, c.Err(400, "invalid_request", "Select a model for operation "+opID, "models."+opID)
			}
			if plan.model = r.Store.Registry("models", models.Str(opID)); plan.model == nil {
				return nil, c.Err(400, "unknown_model", "Register the selected model before submitting", "models."+opID)
			}
		}
		bound := op.Obj("inputs")
		for _, key := range bound.Keys() {
			kind, name, output := c.Binding(bound.Obj(key))
			if kind == "op" {
				plan.from = &binding{index: float64(index[name]), opID: name, output: output}
				continue
			}
			if !submitted.Has(name) {
				continue
			}
			value := submitted.Get(name)
			if declared.Obj(name).Str("type") == "number" {
				// A bound number is a request override of that parameter.
				if plan.params.Has(key) && !jsonEqual(plan.params.Get(key), value) {
					return nil, c.Err(400, "invalid_request", key+" is bound to input "+name, "parameters."+key)
				}
				plan.params.Set(key, value)
				continue
			}
			plan.inputs.Set(key, value)
		}
		if _, err := c.PartialParameters(plan.params, []string{"parameters"}); err != nil {
			return nil, inOperation(err, opID)
		}
		if _, err := c.Inputs(plan.inputs, []string{"inputs"}); err != nil {
			return nil, inOperation(err, opID)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// resolveOperation resolves one operation against the live worker: task support, the model's
// family and loader, inputs, effective parameters, adapters and load options, then asks the
// worker to validate the bundle. The checks and their order are those of a schema-1 recipe.
func (r *Runtime) resolveOperation(plan *operationPlan, request *jsonv.Object, w *workerView, caller Caller) (*resolvedOperation, error) {
	namespace := caller.Namespace
	info, caps, protocol, minor := w.info, w.caps, w.protocol, w.minor
	op, model, inputs, params := plan.op, plan.model, plan.inputs, plan.params
	task := op.Str("task")
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
	if family != op.Str("model_family") {
		return nil, c.Err(400, "unsupported", "Model "+model.Str("model_id")+" is a "+family+" bundle; recipe "+request.Str("recipe_id")+" needs "+op.Str("model_family"), "model_id")
	}
	if minor >= 2 && !loads(caps, family) {
		return nil, c.Err(400, "unsupported", "The connected worker does not load the "+family+" family", "model_id")
	}
	var taskParameters *jsonv.Object
	if minor >= 2 {
		taskParameters = caps.Obj("task_parameters").Obj(task)
	}
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
		if (plan.first && request.Has("adapters")) || op.Has("adapters") {
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
	var err error
	imageDefaults := jsonv.NewObject()
	if usesImage && plan.from != nil {
		imageDefaults.Set("width", plan.fromSize[0]).Set("height", plan.fromSize[1])
	} else if usesImage {
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
	defaults := op.Obj("defaults")
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
	if usesImage {
		size := []any{imageDefaults.Get("width"), imageDefaults.Get("height")}
		if !jsonEqual(effective.Get("width"), size[0]) || !jsonEqual(effective.Get("height"), size[1]) {
			field := "parameters.height"
			if !jsonEqual(effective.Get("width"), size[0]) {
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
	uses := plan.uses
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
	backendID, options, fallbacks, err := loadPlan(request, w, caller)
	if err != nil {
		return nil, jobLevel{err}
	}
	validateBody := jsonv.O("protocol", protocol, "bundle", bundle, "options", options)
	if len(adapters) > 0 {
		validateBody.Set("adapters", adapters)
	}
	if len(fallbacks) > 0 {
		validateBody.Set("authorized_fallbacks", request.Get("authorized_fallbacks"))
	}
	validation := jsonv.NewObject()
	if w.validate {
		if validation, err = w.client.Obj("/v1/validate", "POST", validateBody); err != nil {
			return nil, err
		}
	}
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
	var from any = jsonv.Absent
	if plan.from != nil {
		from = jsonv.O("op_id", plan.from.opID, "output_id", plan.from.output)
	}
	operation := jsonv.O(
		"op_id", op.Get("op_id"),
		"task", task,
		"state", "pending",
		"model", model,
		"input", jsonv.Maybe(input),
		"input_from", from,
		"inputs", inputs,
		"adapters", adapters,
		"parameters", effective,
		"backend", backend,
		"worker_sequence", 0.0,
		"outputs", []any{},
	)
	return &resolvedOperation{operation: operation, uses: uses, backendID: backendID, options: options}, nil
}

// loadPlan resolves the job's backend, load options and authorized fallbacks; every operation
// loads with the same options.
func loadPlan(request *jsonv.Object, w *workerView, caller Caller) (string, *jsonv.Object, []string, error) {
	caps, minor := w.caps, w.minor
	backendID := caps.Str("backend_id")
	backendOptions := request.Obj("backend_options")
	options, err := c.Validate(c.LoadOptions, jsonv.NewObject().Assign(caps.Obj("load_defaults")).Assign(backendOptions.Obj(backendID)))
	if err != nil {
		return "", nil, nil, err
	}
	for _, other := range backendOptions.Keys() {
		if other != backendID && truthy(backendOptions.Get(other)) {
			return "", nil, nil, c.Err(400, "unsupported_backend", "Options for "+other+" do not apply to the "+backendID+" worker", "backend_options")
		}
	}
	if options.Str("precision") == "auto" {
		if minor < 2 || !c.Contains(strList(caps.Get("auto_options")), "precision") {
			return "", nil, nil, c.Err(400, "unsupported", "The connected worker does not resolve automatic precision", "precision")
		}
	} else if !c.Contains(strList(caps.Get("precisions")), options.Str("precision")) {
		return "", nil, nil, c.Err(400, "unsupported", "Requested precision is unavailable", "precision")
	}
	fallbacks := strList(request.Get("authorized_fallbacks"))
	if len(fallbacks) > 0 {
		if !caller.AllowFallbacks {
			return "", nil, nil, c.Err(403, "fallbacks_not_allowed", "This credential may not authorize fallbacks", "authorized_fallbacks")
		}
		distinct := map[string]bool{}
		for _, f := range fallbacks {
			if minor < 2 || !c.Contains(strList(caps.Get("fallbacks")), f) {
				return "", nil, nil, c.Err(400, "unsupported", "The connected worker does not advertise the "+f+" fallback", "authorized_fallbacks")
			}
			distinct[f] = true
		}
		if len(distinct) != len(fallbacks) {
			return "", nil, nil, c.Err(400, "invalid_request", "Fallbacks must be distinct", "authorized_fallbacks")
		}
	}
	return backendID, options, fallbacks, nil
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
			Set("current_operation", 0.0).
			Set("worker_id", nil).
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
	mirror(job)
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

// finish settles a job and its current operation; a linear job's error names the operation.
func (r *Runtime) finish(job *jsonv.Object, state string, errorRecord *jsonv.Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(job)
	if op, _ := current(job); op != nil && !c.Terminal(op.Str("state")) {
		op.Set("state", state)
		if errorRecord != nil {
			op.Set("error", errorRecord.Clone())
			if c.IsLinear(job.Obj("recipe")) {
				errorRecord.Set("operation", op.Get("op_id"))
			}
		}
	}
	mirror(job)
	r.Store.Finish(job, state, errorRecord)
}

// Operations returns a job's operation records; they are shared with the job, so changes to
// them are saved with it.
func Operations(job *jsonv.Object) []*jsonv.Object {
	var out []*jsonv.Object
	for _, raw := range job.Arr("operations") {
		if op, ok := raw.(*jsonv.Object); ok {
			out = append(out, op)
		}
	}
	return out
}

// current is the operation the scheduler is driving and its index.
func current(job *jsonv.Object) (*jsonv.Object, int) {
	ops := Operations(job)
	index, _ := job.Num("current_operation")
	if int(index) < 0 || int(index) >= len(ops) {
		return nil, 0
	}
	return ops[int(index)], int(index)
}

// mirrored are the current operation's worker identities, also kept at the top level of the
// job where single-operation jobs have always recorded them.
var mirrored = []string{"attempt_id", "session_id", "execution_id", "worker_input_id", "load_operation_id", "dispatch_phase", "worker_sequence", "applied_fallbacks", "released"}

func mirror(job *jsonv.Object) {
	op, index := current(job)
	if op == nil {
		return
	}
	for _, k := range mirrored {
		if op.Has(k) {
			job.Set(k, op.Get(k))
		} else {
			job.Delete(k)
		}
	}
	if index == 0 {
		job.Set("backend", op.Get("backend"))
	}
}

// annotate adds a linear job's operation to the data of a stage or progress event; schema-1
// jobs keep the worker's data unchanged.
func annotate(job, op *jsonv.Object, index int, data any) any {
	o, ok := data.(*jsonv.Object)
	if !ok || !c.IsLinear(job.Obj("recipe")) {
		return data
	}
	return o.Clone().Set("op_id", op.Get("op_id")).Set("operation_index", float64(index)).Set("operation_count", float64(len(Operations(job))))
}

// runningOn is the job running on a worker; each worker runs at most one.
func (r *Runtime) runningOn(workerID string) *jsonv.Object {
	for _, job := range r.Store.All("running") {
		if job.Str("worker_id") == workerID {
			return job
		}
	}
	return nil
}

// Tick advances one worker's scheduler by one step; overlapping calls return immediately.
func (r *Runtime) Tick(m *pool.Member) {
	lane := r.lanes[m.ID]
	if !lane.TryLock() {
		return
	}
	defer lane.Unlock()
	if r.stopping.Load() {
		return
	}
	err := r.tick(m)
	var api *c.ApiError
	if err == nil || !errors.As(err, &api) {
		return
	}
	// Transient transport errors preserve dispatch identity for reconciliation; never regenerate.
	// A refused certificate is one: nothing was said to the worker, and the job resumes from its
	// snapshot once the operator restores the expected certificate or pin.
	if job := r.runningOn(m.ID); job != nil && !transient(api) {
		code, message := api.Code, api.Message
		if api.Status == 404 {
			code, message = "execution_interrupted", "Worker record is no longer available"
		}
		r.finish(job, "failed", jsonv.O("code", code, "message", message))
	}
}

// transient is a transport failure that leaves the job's dispatch identity for reconciliation:
// an unreachable worker, or a remote worker whose certificate was refused before anything was
// said to it.
func transient(api *c.ApiError) bool {
	return api.Code == "backend_unavailable" || api.Code == "certificate_mismatch"
}

func (r *Runtime) tick(m *pool.Member) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("scheduler: %v", p)
		}
	}()
	client := m.Client
	if child := client.Child(); child != nil && child.Exited() {
		// A lost worker interrupts only its own job.
		if interrupted := r.runningOn(m.ID); interrupted != nil {
			r.finish(interrupted, "failed", jsonv.O("code", "execution_interrupted", "message", "Owned worker exited; this job was not regenerated"))
		}
		client.ClearChild()
		if err := client.Start(); err != nil {
			return err
		}
	}
	p, err := r.Pool.Probe(m)
	if err != nil {
		return err
	}
	info, caps, protocol := p.Info, p.Caps, p.Protocol
	session := info.Get("session_id")
	// Release an operation's worker records only after its durable terminal commit: every
	// operation of a finished job, and each committed operation of a running linear job. Retry
	// collection acknowledgements after disconnects. Session IDs are per worker process, so each
	// worker releases only what it holds.
	for _, done := range r.Store.All("") {
		changed := false
		for _, op := range Operations(done) {
			if !c.Terminal(op.Str("state")) || truthy(op.Get("released")) ||
				!(truthy(op.Get("execution_id")) || truthy(op.Get("worker_input_id"))) || !jsonEqual(op.Get("session_id"), session) {
				continue
			}
			err := func() error {
				if id := op.Str("execution_id"); id != "" {
					if _, err := client.JSON("/v1/executions/"+id, "DELETE", nil, ""); err != nil {
						return err
					}
				}
				if id := op.Str("worker_input_id"); id != "" {
					if _, err := client.JSON("/v1/inputs/"+id, "DELETE", nil, ""); err != nil {
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
				op.Set("released", true)
				changed = true
			}
		}
		if changed {
			if err := r.save(done, nil); err != nil {
				return err
			}
		}
	}
	job := r.runningOn(m.ID)
	if job == nil {
		if info.Str("state") != "ready" {
			return nil
		}
		r.mu.Lock()
		job = r.next(m)
		if job == nil {
			r.mu.Unlock()
			return nil
		}
		err := r.Store.Transaction(func(tx *store.Store) error {
			job.Set("state", "running")
			job.Set("stage", nil)
			attribute(job, m.ID, p)
			op, _ := current(job)
			dispatch(op, caps, session)
			mirror(job)
			tx.Save(job)
			data := jsonv.O("state", "running", "worker_id", m.ID)
			if c.Minor(protocol) >= 2 {
				data.Set("device_identity", jsonv.Maybe(info.Get("device_identity")))
			}
			tx.Event(job, "job_state", data)
			return nil
		})
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
	op, index := current(job)
	if op.Str("attempt_id") == "" {
		// The next operation of a linear job: its predecessor's outputs are already committed.
		dispatch(op, caps, session)
		if err := r.save(job, nil); err != nil {
			return err
		}
	}
	if !jsonEqual(op.Get("session_id"), session) {
		r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Worker session ended; this job was not regenerated"))
		return nil
	}
	r.refresh(job)
	grace := time.Duration(r.Config.CancellationGraceMS) * time.Millisecond
	// overdue is a remote worker's cancellation past the deadline. A remote worker is never
	// owned, so nothing is terminated: the job settles only when the worker itself reports a
	// terminal state (or its session changes, handled above).
	overdue := false
	if truthy(job.Get("cancel_requested")) {
		at, ok := job.Num("cancel_at")
		if !ok || at == 0 {
			at = float64(time.Now().UnixMilli())
		}
		late := time.Since(time.UnixMilli(int64(at))) > grace
		switch {
		case late && m.Config.Remote:
			overdue = true
		case late && client.Child() != nil:
			client.Stop()
			r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Owned worker was forcibly stopped after the cancellation deadline", "forced_termination", true))
			return client.Start()
		}
	}
	backend := op.Obj("backend")
	if op.Str("dispatch_phase") == "loading" {
		if opID := op.Str("load_operation_id"); opID != "" {
			operation, err := client.Obj("/v1/operations/"+opID, "GET", nil)
			if err != nil {
				return err
			}
			if operation.Str("state") == "running" {
				return nil
			}
			if _, err := client.JSON("/v1/operations/"+opID, "DELETE", nil, ""); err != nil {
				return err
			}
			op.Delete("load_operation_id")
			if operation.Str("state") == "failed" {
				code := operation.Obj("error").Str("code")
				if code == "" {
					code = "model_load_failed"
				}
				r.finish(job, "failed", jsonv.O("code", code, "message", "Selected model could not be loaded"))
				return nil
			}
			if estimate := operation.Get("memory_estimate"); truthy(estimate) {
				backend.Set("memory_estimate", estimate)
			}
			for _, k := range []string{"resolved_options", "resolutions"} {
				if v := operation.Get(k); v != nil {
					backend.Set(k, v)
				}
			}
			return r.save(job, nil)
		}
		if s := info.Str("state"); s == "loading" || s == "unloading" {
			return nil
		}
		r.refresh(job)
		// Cancellation is honored before every dispatch, including between operations.
		if truthy(job.Get("cancel_requested")) {
			r.finish(job, "cancelled", nil)
			return nil
		}
		desired := job.Obj("request").Obj("backend_options").Get(backend.Str("backend_id"))
		loaded := info.Obj("loaded_bundle")
		model := op.Obj("model")
		if !loadMatches(info, job, op) {
			body := jsonv.O("protocol", protocol, "bundle", model.Get("bundle"), "options", desired)
			if len(asArray(op.Get("adapters"))) > 0 {
				body.Set("adapters", op.Get("adapters"))
			}
			if fallbacks := job.Obj("request").Arr("authorized_fallbacks"); len(fallbacks) > 0 {
				body.Set("authorized_fallbacks", fallbacks)
			}
			load, err := client.Obj("/v1/model/load", "POST", body)
			if err != nil {
				return err
			}
			op.Set("load_operation_id", jsonv.Maybe(load.Get("operation_id")))
			job.Set("stage", "loading")
			job.Delete("progress")
			return r.save(job, func(tx *store.Store) error {
				tx.Event(job, "stage", annotate(job, op, index, jsonv.O("stage", "loading")))
				return nil
			})
		}
		op.Set("dispatch_phase", "executing")
		// Record any fallback the loaded bundle runs on; it was authorized by this or an earlier job.
		var applied []any
		if c.Minor(protocol) >= 2 {
			applied = asArray(nullish(loaded.Get("applied_fallbacks"), []any{}))
			op.Set("applied_fallbacks", applied)
			backend.Set("applied_fallbacks", applied)
		}
		if err := r.save(job, func(tx *store.Store) error {
			if len(applied) > 0 {
				tx.Event(job, "stage", annotate(job, op, index, jsonv.O("stage", "loading", "fallback", applied)))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	var snapshot *jsonv.Object
	if op.Str("execution_id") == "" {
		// Query the same session before resending an ambiguous acceptance. A retry uses the SAME attempt.
		snapshot, err = client.Obj("/v1/executions?attempt_id="+url.QueryEscape(op.Str("attempt_id")), "GET", nil)
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
			inputs := op.Obj("inputs").Clone()
			if task := op.Str("task"); task == "image-to-image" || task == "upscale" {
				if op.Str("worker_input_id") == "" {
					// Hand the worker the verified bytes under a session-scoped handle, never a path.
					bytes, digest, err := r.operationInput(job, op)
					if err != nil {
						return err
					}
					uploaded, err := client.JSON("/v1/inputs", "POST", bytes, "image/png")
					if err != nil {
						return err
					}
					up, _ := uploaded.(*jsonv.Object)
					if up.Str("sha256") != digest {
						return c.Err(503, "protocol_mismatch", "Worker input digest mismatch")
					}
					op.Set("worker_input_id", jsonv.Maybe(up.Get("input_id")))
					if err := r.save(job, nil); err != nil {
						return err
					}
				}
				inputs.Set("image", op.Get("worker_input_id"))
			}
			snapshot, err = client.Obj("/v1/executions", "POST", jsonv.O(
				"protocol", protocol,
				"attempt_id", op.Get("attempt_id"),
				"bundle_digest", op.Obj("model").Obj("bundle").Get("bundle_digest"),
				"task", op.Get("task"),
				"inputs", inputs,
				"parameters", op.Get("parameters"),
			))
			if err != nil {
				return err
			}
		}
		if !jsonEqual(snapshot.Get("session_id"), op.Get("session_id")) || !jsonEqual(snapshot.Get("attempt_id"), op.Get("attempt_id")) {
			return c.Err(503, "protocol_mismatch", "Worker execution identity mismatch")
		}
		op.Set("execution_id", jsonv.Maybe(snapshot.Get("execution_id")))
		if err := r.save(job, nil); err != nil {
			return err
		}
	} else if snapshot, err = client.Obj("/v1/executions/"+op.Str("execution_id"), "GET", nil); err != nil {
		return err
	}
	r.refresh(job)
	if truthy(job.Get("cancel_requested")) && !c.Terminal(snapshot.Str("state")) {
		if snapshot, err = client.Obj("/v1/executions/"+op.Str("execution_id")+"/cancel", "POST", jsonv.NewObject()); err != nil {
			return err
		}
	}
	err = r.save(job, func(tx *store.Store) error {
		for _, raw := range snapshot.Arr("events") {
			event, _ := raw.(*jsonv.Object)
			sequence, _ := event.Num("sequence")
			current, _ := op.Num("worker_sequence")
			if sequence <= current {
				continue
			}
			if sequence != current+1 {
				return c.Err(503, "event_gap", "Worker event history has a gap")
			}
			data := annotate(job, op, index, event.Get("data"))
			switch event.Str("type") {
			case "stage":
				job.Set("stage", jsonv.Maybe(event.Obj("data").Get("stage")))
				job.Delete("progress")
				tx.Event(job, "stage", data)
			case "progress":
				job.Set("progress", jsonv.Maybe(data))
				tx.Event(job, "progress", data)
			}
			op.Set("worker_sequence", sequence)
			mirror(job)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if state := snapshot.Str("state"); overdue && (state == "succeeded" || state == "failed") {
		// The remote worker finished after the cancellation deadline instead of confirming the
		// cancellation: the job is interrupted, and nothing it produced is committed.
		r.finish(job, "failed", jsonv.O("code", "execution_interrupted",
			"message", "Remote worker reported "+state+" after the cancellation deadline; nothing was committed and the worker was not terminated",
			"forced_termination", false))
		return nil
	}
	switch snapshot.Str("state") {
	case "succeeded":
		return r.commitOutputs(client, job, snapshot)
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

// dispatch persists an operation's dispatch intent: a unique attempt bound to the worker session.
func dispatch(op, caps *jsonv.Object, session any) {
	op.Set("state", "running")
	op.Obj("backend").Set("worker_version", jsonv.Maybe(caps.Get("worker_version")))
	op.Set("attempt_id", "attempt_"+c.UUID())
	op.Set("session_id", jsonv.Maybe(session))
	op.Set("dispatch_phase", "loading")
}

// operationInput is the verified image an operation hands the worker: the submitted input, or
// an earlier operation's committed output, read back from durable storage.
func (r *Runtime) operationInput(job, op *jsonv.Object) ([]byte, string, error) {
	var path, digest string
	if from := op.Obj("input_from"); from != nil {
		output := operationOutput(job, from.Str("op_id"), from.Str("output_id"))
		if output == nil {
			return nil, "", c.Err(410, "input_unavailable", "The bound output of operation "+from.Str("op_id")+" is not committed")
		}
		path, digest = r.OutputFile(job.Str("job_id"), output), output.Str("sha256")
	} else {
		record := op.Obj("input")
		path, digest = r.InputPath(record.Str("input_id")), record.Str("sha256")
	}
	bytes, err := os.ReadFile(path)
	if err != nil || c.SHA256Hex(bytes) != digest {
		return nil, "", c.Err(410, "input_unavailable", "Input bytes changed or disappeared before dispatch")
	}
	return bytes, digest, nil
}

// operationOutput is the committed output of an operation by the worker's output ID.
func operationOutput(job *jsonv.Object, opID, outputID string) *jsonv.Object {
	for _, op := range Operations(job) {
		if op.Str("op_id") != opID {
			continue
		}
		for _, raw := range op.Arr("outputs") {
			if o, ok := raw.(*jsonv.Object); ok && nullish(o.Get("operation_output_id"), o.Get("output_id")) == outputID {
				return o
			}
		}
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
	return publish[opID+"/"+base]
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
			res, err := client.Response("/v1/executions/"+op.Str("execution_id")+"/outputs/"+id, "GET", nil, "")
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
			if err := atomicFile(file, bytes); err != nil {
				return err
			}
			if seeded && task == "text-to-image" {
				// Output k of a batch used seed + k − 1, so any one image can be reproduced alone.
				record.Set("seed", seed+float64(k))
			}
			committed = append(committed, record.Set("expires_at", r.expiry()))
		}
		op.Set("outputs", committed)
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
		if linear {
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
			manifest = r.linearManifest(job)
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

// linearManifest is the schema-4 manifest of a linear job: every operation with its model,
// inputs, settings, worker identity and outputs, then the published and intermediate outputs.
func (r *Runtime) linearManifest(job *jsonv.Object) *jsonv.Object {
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
		var scale any = jsonv.Absent
		if op.Str("task") == "upscale" {
			scale = op.Obj("model").Get("scale")
		}
		operations = append(operations, jsonv.O(
			"op_id", op.Get("op_id"),
			"task", op.Get("task"),
			"family", op.Obj("model").Obj("bundle").Get("family"),
			"scale", scale,
			"model_id", op.Obj("model").Get("model_id"),
			"model", op.Obj("model").Get("bundle"),
			"input", input,
			"input_from", jsonv.Maybe(op.Get("input_from")),
			"adapters", manifestAdapters(op),
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
		))
	}
	return jsonv.O(
		"schema_version", 4.0,
		"runtime_version", c.VERSION,
		"job_id", job.Get("job_id"),
		"recipe", job.Get("recipe"),
		"request", job.Get("request"),
		"worker_id", nullish(job.Get("worker_id")),
		"device_identity", nullish(job.Get("device_identity")),
		"inputs", inputs,
		"operations", operations,
		"outputs", job.Get("outputs"),
		"intermediate_outputs", job.Get("intermediate_outputs"),
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
func workerReport(view pool.View) *jsonv.Object {
	m := view.Member
	var w *jsonv.Object
	if view.Ready() {
		p := view.Probe
		info, caps := p.Info, p.Caps
		w = jsonv.O(
			"worker_id", m.ID,
			"protocol", p.Protocol,
			"supported_protocols", nullish(info.Get("supported_protocols"), []any{info.Get("protocol")}),
			"worker_version", jsonv.Maybe(info.Get("worker_version")),
			"backend_id", caps.Get("backend_id"),
			"ownership", nullish(caps.Get("ownership"), "reference"),
			"owned_process", len(m.Config.Command) > 0,
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
		w.Set("ready", truthy(info.Get("service_ready")) && truthy(caps.Get("ready")))
	} else {
		code, message := "backend_unavailable", "Worker is unavailable"
		var api *c.ApiError
		if errors.As(view.Err, &api) {
			code, message = api.Code, api.Message
		}
		w = jsonv.O("worker_id", m.ID, "error", code, "message", message, "owned_process", len(m.Config.Command) > 0, "ready", false)
		if view.Conflict != "" {
			w.Set("duplicate_of", view.Conflict)
		}
	}
	// How the worker is reached: loopback HTTP, or mutual TLS to a remote worker with the
	// fingerprint it must present (pinned or recorded on first contact) and the one it presented.
	t := m.Client.Transport()
	w.Set("transport", map[bool]string{true: "mtls", false: "http"}[t.Remote])
	w.Set("remote", t.Remote)
	var expected, presented, latency any
	if t.Expected != "" {
		expected = t.Expected
	}
	if t.Presented != "" {
		presented = t.Presented
	}
	if t.Latency > 0 {
		latency = math.Round(float64(t.Latency.Microseconds())/10) / 100
	}
	if t.Remote {
		source := "first_contact"
		if t.Pinned {
			source = "pinned"
		}
		w.Set("certificate_sha256", expected)
		w.Set("certificate_source", source)
		w.Set("presented_sha256", presented)
		if view.Ready() {
			w.Set("ownership_available", nullish(view.Probe.Info.Get("ownership_available")))
		}
	}
	w.Set("latency_ms", latency)
	// The operator's statement of intent, beside what the worker reports.
	w.Set("configured_device", nullish(m.Config.Device))
	var budget any
	if m.Config.MemoryBudgetMB > 0 {
		budget = float64(m.Config.MemoryBudgetMB)
	}
	w.Set("configured_memory_budget_mb", budget)
	return w
}

// Doctor is the diagnostic report behind `doctor` and GET /v1/runtime: `worker` is the first
// configured worker (the only one of a legacy configuration) and `workers` lists every one.
func (r *Runtime) Doctor() (*jsonv.Object, error) {
	views := r.Pool.ProbeAll()
	reports := []any{}
	loadedOn := map[string][]any{}
	for _, view := range views {
		report := workerReport(view)
		reports = append(reports, report)
		if digest, ok := report.Get("loaded_bundle_digest").(string); ok {
			loadedOn[digest] = append(loadedOn[digest], view.Member.ID)
		}
	}
	w := reports[0].(*jsonv.Object)
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
			"loaded", len(loadedOn[bundle.Str("bundle_digest")]) > 0,
			"loaded_on", append([]any{}, loadedOn[bundle.Str("bundle_digest")]...),
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
		entry := jsonv.O("recipe_id", recipe.Get("recipe_id"), "pipeline", recipe.Get("pipeline"))
		if c.IsLinear(recipe) {
			// A linear recipe's pipeline is its operations' tasks in order.
			tasks := []string{}
			for _, raw := range recipe.Arr("operations") {
				tasks = append(tasks, raw.(*jsonv.Object).Str("task"))
			}
			entry.Set("pipeline", strings.Join(tasks, " > ")).Set("operations", float64(len(tasks)))
		}
		recipes = append(recipes, entry)
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
		"workers", reports,
		"models", models,
		"adapters", adapters,
		"recipes", recipes,
		"storage", jsonv.O("writable", writable, "available_bytes", float64(available), "output_retention_days", retention),
		"queue", jsonv.O("capacity", float64(r.Config.QueueCapacity), "queued", float64(queued), "running", float64(running)),
		"queue_capacity", float64(r.Config.QueueCapacity),
	), nil
}

// Workers is GET /v1/workers: every configured worker with its device identity, readiness and
// loaded identity, whether it is running a job, and that job's ID only when the job belongs to
// the caller's namespace.
func (r *Runtime) Workers(namespace string) []any {
	views := r.Pool.ProbeAll()
	running := map[string]*jsonv.Object{}
	for _, job := range r.Store.All("running") {
		running[job.Str("worker_id")] = job
	}
	models := r.Store.RegistryAll("models")
	list := []any{}
	for _, view := range views {
		m := view.Member
		report := workerReport(view)
		entry := jsonv.O(
			"worker_id", m.ID,
			"ready", report.Get("ready"),
			"state", nullish(report.Get("state")),
			"protocol", nullish(report.Get("protocol")),
			"backend_id", nullish(report.Get("backend_id")),
			"device", nullish(report.Get("device")),
			"device_identity", nullish(report.Get("device_identity")),
			"configured_device", report.Get("configured_device"),
			"configured_memory_budget_mb", report.Get("configured_memory_budget_mb"),
			"owned_process", report.Get("owned_process"),
			"transport", report.Get("transport"),
			"remote", report.Get("remote"),
			"certificate_sha256", nullish(report.Get("certificate_sha256")),
			"latency_ms", report.Get("latency_ms"),
		)
		var loaded any
		if view.Ready() {
			if bundle := view.Probe.Info.Obj("loaded_bundle"); bundle != nil {
				ids := []any{}
				for _, model := range models {
					if jsonEqual(model.Obj("bundle").Get("bundle_digest"), bundle.Get("bundle_digest")) {
						ids = append(ids, model.Get("model_id"))
					}
				}
				adapters := []any{}
				for _, raw := range asArray(bundle.Get("adapters")) {
					if a, ok := raw.(*jsonv.Object); ok {
						adapters = append(adapters, jsonv.O("adapter_id", nullish(a.Get("adapter_id")), "digest", nullish(a.Get("digest")), "scale", nullish(a.Get("scale"))))
					}
				}
				loaded = jsonv.O("bundle_digest", nullish(bundle.Get("bundle_digest")), "model_ids", ids, "options", nullish(bundle.Get("options")), "adapters", adapters)
			}
		}
		entry.Set("loaded", loaded)
		job := running[m.ID]
		entry.Set("busy", job != nil)
		var jobID any
		if job != nil && job.Str("namespace") == namespace {
			jobID = job.Get("job_id")
		}
		entry.Set("running_job_id", jobID)
		if report.Has("error") {
			entry.Set("error", report.Get("error")).Set("message", report.Get("message"))
		}
		list = append(list, entry)
	}
	return list
}

// Cleanup removes expired outputs, including a linear job's intermediates and the committed
// operation outputs of a job that failed later, and expired inputs no active job references.
func (r *Runtime) Cleanup() error {
	for _, job := range r.Store.All("") {
		if !c.Terminal(job.Str("state")) {
			continue
		}
		records := append(append([]any{}, job.Arr("outputs")...), job.Arr("intermediate_outputs")...)
		for _, op := range Operations(job) {
			records = append(records, op.Arr("outputs")...)
		}
		removed := map[string]bool{}
		for _, raw := range records {
			output, _ := raw.(*jsonv.Object)
			path := r.OutputFile(job.Str("job_id"), output)
			if c.Expired(output.Get("expires_at")) && !removed[path] {
				removed[path] = true
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
