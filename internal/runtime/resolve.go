package runtime

import (
	"errors"
	"fmt"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/pool"
	"github.com/jxburros/media-runtime/internal/worker"
	"math"
	"strings"
	"sync"
)

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
	if err := caller.CheckModels(body); err != nil {
		return nil, err
	}
	if err := caller.CheckModels(body); err != nil {
		return nil, err
	}
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
	if recipe, err = c.ExpandDAG(recipe, request); err != nil {
		return nil, err
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
		"mask", jsonv.Maybe(first.operation.Get("mask")),
		"overlay", jsonv.Maybe(first.operation.Get("overlay")),
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
		op := raw.(*jsonv.Object)
		index[op.Str("op_id")] = i
		if source := op.Str("source_op_id"); source != "" {
			if _, exists := index[source]; !exists {
				index[source] = i
			}
		}
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
		modelKey := opID
		if source := op.Str("source_op_id"); source != "" {
			modelKey = source
		}
		plan := &operationPlan{op: op, model: model, inputs: jsonv.NewObject(), params: jsonv.NewObject()}
		if i == 0 {
			plan.first = true
			plan.params = request.Obj("parameters").Clone()
			plan.uses = asArray(nullish(request.Get("adapters"), op.Get("adapters"), []any{}))
		} else {
			plan.uses = asArray(nullish(op.Get("adapters"), []any{}))
			if !models.Has(modelKey) {
				return nil, c.Err(400, "invalid_request", "Select a model for operation "+opID, "models."+opID)
			}
			if plan.model = r.Store.Registry("models", models.Str(modelKey)); plan.model == nil {
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
	usesImage := c.UsesImage(task)
	// A video or audio parameter on a task that does not take it is refused by name, as are image
	// steps beyond the diffusion range (the request schema admits the wider audio range).
	for _, k := range params.Keys() {
		if c.MediaParameter(k) && !c.Contains(c.TaskParameterNames[task], k) {
			return nil, forbidden("parameters."+k, "A "+task+" operation has no "+k)
		}
	}
	if steps, ok := params.Num("steps"); ok && task != "text-to-audio" && steps > c.MaxDiffusionSteps {
		return nil, c.Invalid([]string{"parameters", "steps"})
	}
	if task == "image-to-video" {
		if err := videoRequest(plan, request, inputs, params, taskParameters); err != nil {
			return nil, err
		}
	} else if task == "text-to-audio" {
		if err := audioRequest(plan, request, inputs, params, taskParameters); err != nil {
			return nil, err
		}
	} else if task == "upscale" || task == "image-transform" {
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
	var mask *jsonv.Object
	if task == "inpaint" {
		if !inputs.Has("mask") {
			return nil, c.Err(400, "invalid_request", "Inpaint requires a durable mask input", "inputs.mask")
		}
		mask, err = r.OwnedInput(inputs.Str("mask"), namespace)
		if err != nil {
			return nil, err
		}
		if c.Expired(mask.Get("expires_at")) {
			return nil, c.Err(410, "input_expired", "Mask input expired", "inputs.mask")
		}
		if !jsonEqual(mask.Get("width"), imageDefaults.Get("width")) || !jsonEqual(mask.Get("height"), imageDefaults.Get("height")) {
			return nil, c.Err(400, "invalid_request", "Mask dimensions must match the image", "inputs.mask")
		}
	} else if inputs.Has("mask") {
		return nil, c.Err(400, "invalid_request", "Only inpaint takes a mask", "inputs.mask")
	}
	var overlay *jsonv.Object
	if inputs.Has("overlay") {
		if task != "image-transform" {
			return nil, forbidden("inputs.overlay", "Only composite accepts an overlay")
		}
		overlay, err = r.OwnedInput(inputs.Str("overlay"), namespace)
		if err != nil {
			return nil, err
		}
		if c.Expired(overlay.Get("expires_at")) {
			return nil, c.Err(410, "input_expired", "Overlay expired", "inputs.overlay")
		}
	}
	defaults := op.Obj("defaults")
	var effective *jsonv.Object
	if task == "image-transform" {
		merged := jsonv.NewObject().Assign(imageDefaults).Assign(defaults).Assign(params)
		effective, err = c.Validate(c.TransformParameters, merged)
		if err != nil {
			return nil, err
		}
	} else if task == "upscale" {
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
	} else if task == "image-to-video" {
		// The worker's video defaults (fps, motion, noise_augmentation, decode_chunk), then the
		// bundle's frame count recorded at registration, then the recipe, the image and the request.
		merged := jsonv.NewObject().Assign(caps.Obj("task_defaults").Obj(task)).Assign(taskParameters.Obj("defaults"))
		if frames, ok := model.Num("default_frames"); ok {
			merged.Set("frames", frames)
		}
		merged.Assign(defaults).Assign(imageDefaults).Assign(params)
		merged.Set("seed", nullish(params.Get("seed"), defaults.Get("seed"), c.Seed()))
		if effective, err = c.Validate(c.VideoParameters, merged); err != nil {
			return nil, err
		}
		if err := withinLimits(effective, caps.Obj("limits"), videoLimits); err != nil {
			return nil, err
		}
	} else if task == "text-to-audio" {
		// The worker's audio defaults (guidance), then the recipe and the request; steps and the
		// duration have no worker default, so a recipe or the request must give them.
		merged := jsonv.NewObject().Assign(caps.Obj("task_defaults").Obj(task)).Assign(taskParameters.Obj("defaults"))
		merged.Assign(defaults).Assign(params)
		merged.Set("seed", nullish(params.Get("seed"), defaults.Get("seed"), c.Seed()))
		if effective, err = c.Validate(c.AudioParameters, merged); err != nil {
			return nil, err
		}
		if err := withinLimits(effective, caps.Obj("limits"), audioLimits); err != nil {
			return nil, err
		}
		// Only a duration the bundle serves; a longer clip is refused, never trimmed.
		span := asArray(model.Get("duration_range"))
		if len(span) != 2 {
			span = asArray(caps.Obj("limits").Get("duration_seconds"))
		}
		duration, _ := effective.Num("duration_seconds")
		if len(span) == 2 {
			low, _ := span[0].(float64)
			high, _ := span[1].(float64)
			if duration < low || duration > high {
				return nil, c.Err(400, "invalid_request", fmt.Sprintf("duration_seconds must be within the model's range %s–%s s; a clip is never trimmed", jsonv.Number(low), jsonv.Number(high)), "parameters.duration_seconds")
			}
		}
	} else {
		merged := jsonv.NewObject().Assign(caps.Obj("defaults"))
		if task == "image-to-image" || task == "inpaint" {
			merged.Assign(caps.Obj("task_defaults").Obj(task))
		}
		merged.Assign(defaults).Assign(imageDefaults).Assign(params)
		merged.Set("seed", nullish(params.Get("seed"), defaults.Get("seed"), c.Seed()))
		if effective, err = c.Validate(c.Parameters, merged); err != nil {
			return nil, err
		}
	}
	if effective.Has("preview_interval") && (minor < 4 || caps.Obj("previews") == nil) {
		return nil, c.Err(400, "unsupported", "The worker does not advertise previews", "parameters.preview_interval")
	}
	if task == "text-to-image" || task == "image-to-image" || task == "inpaint" {
		pair := jsonv.O("sampler", effective.Get("sampler"), "scheduler", effective.Get("scheduler"))
		supported := effective.Str("sampler") == "euler" && effective.Str("scheduler") == "euler"
		if minor >= 4 {
			for _, offered := range caps.Arr("sampling") {
				if jsonEqual(offered, pair) {
					supported = true
					break
				}
			}
		}
		if !supported {
			return nil, c.Err(400, "unsupported", "The worker does not advertise this sampler/scheduler pair", "parameters.scheduler")
		}
	}
	if task == "image-transform" {
		iw, _ := imageDefaults.Num("width")
		ih, _ := imageDefaults.Num("height")
		ow, _ := effective.Num("width")
		oh, _ := effective.Num("height")
		x, _ := effective.Num("x")
		y, _ := effective.Num("y")
		switch effective.Str("operation") {
		case "resize":
			if !effective.Has("filter") || effective.Has("x") || effective.Has("y") || effective.Has("background") || effective.Has("opacity") {
				return nil, forbidden("parameters", "Resize needs an explicit filter and no other operation settings")
			}
		case "crop":
			if x+ow > iw || y+oh > ih || effective.Has("filter") || effective.Has("background") || effective.Has("opacity") {
				return nil, forbidden("parameters", "Crop must lie inside the source image")
			}
		case "pad":
			if x+iw > ow || y+ih > oh || effective.Has("filter") || effective.Has("opacity") {
				return nil, forbidden("parameters", "Pad must contain the entire source image")
			}
		case "composite":
			if overlay == nil || ow != iw || oh != ih || effective.Has("filter") || effective.Has("background") {
				return nil, forbidden("parameters", "Composite requires an overlay and preserves base dimensions")
			}
			fw, _ := overlay.Num("width")
			fh, _ := overlay.Num("height")
			if x+fw > iw || y+fh > ih {
				return nil, forbidden("inputs.overlay", "Overlay must fit inside the base image")
			}
		}
		if effective.Str("operation") != "composite" && overlay != nil {
			return nil, forbidden("inputs.overlay", "Only composite accepts an overlay")
		}
	}
	if usesImage && task != "image-transform" {
		size := []any{imageDefaults.Get("width"), imageDefaults.Get("height")}
		if !jsonEqual(effective.Get("width"), size[0]) || !jsonEqual(effective.Get("height"), size[1]) {
			field := "parameters.height"
			if !jsonEqual(effective.Get("width"), size[0]) {
				field = "parameters.width"
			}
			return nil, c.Err(400, "invalid_request", "Width and height must equal the uploaded image size; nothing is resized", field)
		}
	}
	if task == "image-to-image" || task == "inpaint" {
		if !effective.Has("strength") {
			return nil, c.Err(400, "invalid_request", "Strength is required for image-to-image", "parameters.strength")
		}
		if c.EffectiveSteps(effective) < 1 {
			return nil, c.Err(400, "invalid_request", "steps × strength must denoise for at least one step", "parameters.strength")
		}
	}
	if task == "image-to-video" {
		// Only a size the bundle serves; the input is never resized or cropped to fit one.
		sizes := asArray(model.Get("video_sizes"))
		if len(sizes) == 0 {
			sizes = asArray(caps.Obj("limits").Get("video_sizes"))
		}
		size := []any{effective.Get("width"), effective.Get("height")}
		if !containsJSON(sizes, size) {
			return nil, c.Err(400, "invalid_request", "The input's size is not one the model serves ("+sizeList(sizes)+"); nothing is resized", "video_size")
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
	if task == "image-to-video" && effective.Has("encode_video") {
		if minor < 4 || !c.Contains(strList(taskParameters.Get("parameters")), "encode_video") {
			return nil, c.Err(400, "unsupported", "The worker does not advertise MP4 encoding", "parameters.encode_video")
		}
		if effective.Bool("encode_video") {
			taskParameters = taskParameters.Clone()
			taskParameters.Set("outputs", append(append([]any{}, taskParameters.Arr("outputs")...), "video"))
		}
	}
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
		"mask", jsonv.Maybe(mask),
		"overlay", jsonv.Maybe(overlay),
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

// videoRequest refuses what an image-to-video operation does not take: a prompt, image-generation
// parameters (guidance, sampler, scheduler, strength, count), tiling and adapters. A video
// parameter the worker does not advertise is unsupported.
func videoRequest(plan *operationPlan, request, inputs, params, taskParameters *jsonv.Object) error {
	for _, k := range []string{"prompt", "negative_prompt"} {
		if inputs.Has(k) {
			return forbidden("inputs."+k, "An image-to-video operation is conditioned on its image alone and takes no prompt")
		}
	}
	for _, k := range []string{"guidance", "sampler", "scheduler", "strength", "count", "tile", "tile_overlap"} {
		if params.Has(k) {
			return forbidden("parameters."+k, "An image-to-video operation has no "+k)
		}
	}
	if (plan.first && request.Has("adapters")) || plan.op.Has("adapters") {
		return forbidden("adapters", "Adapters do not apply to image-to-video")
	}
	advertised := strList(taskParameters.Get("parameters"))
	for _, k := range []string{"frames", "fps", "motion", "noise_augmentation", "decode_chunk"} {
		if (params.Has(k) || plan.op.Obj("defaults").Has(k)) && !c.Contains(advertised, k) {
			return c.Err(400, "unsupported", "The connected worker does not advertise the video parameter "+k, "parameters."+k)
		}
	}
	return nil
}

// audioRequest refuses what a text-to-audio operation does not take: an image, image-generation
// parameters (width, height, sampler, scheduler, strength, count), tiling and adapters, and
// requires a prompt. An audio parameter the worker does not advertise is unsupported.
func audioRequest(plan *operationPlan, request, inputs, params, taskParameters *jsonv.Object) error {
	if inputs.Has("image") {
		return forbidden("inputs.image", "A text-to-audio operation is conditioned on text alone and takes no image")
	}
	if !inputs.Has("prompt") {
		return c.Invalid([]string{"inputs", "prompt"})
	}
	for _, k := range []string{"width", "height", "sampler", "scheduler", "strength", "count", "tile", "tile_overlap"} {
		if params.Has(k) {
			return forbidden("parameters."+k, "A text-to-audio operation has no "+k)
		}
	}
	if (plan.first && request.Has("adapters")) || plan.op.Has("adapters") {
		return forbidden("adapters", "Adapters do not apply to text-to-audio")
	}
	advertised := strList(taskParameters.Get("parameters"))
	for _, k := range []string{"guidance", "duration_seconds"} {
		if (params.Has(k) || plan.op.Obj("defaults").Has(k)) && !c.Contains(advertised, k) {
			return c.Err(400, "unsupported", "The connected worker does not advertise the audio parameter "+k, "parameters."+k)
		}
	}
	return nil
}

// audioLimits maps each audio parameter to the worker limit that bounds it.
var audioLimits = [][2]string{{"steps", "audio_steps"}, {"guidance", "guidance"}}

// videoLimits maps each video parameter to the worker limit that bounds it.
var videoLimits = [][2]string{{"steps", "video_steps"}, {"frames", "frames"}, {"fps", "fps"}, {"motion", "motion"}, {"noise_augmentation", "noise_augmentation"}, {"decode_chunk", "decode_chunk"}}

// withinLimits checks resolved parameters against the worker's advertised [low, high] ranges; a
// value outside one is refused, never clamped.
func withinLimits(effective, limits *jsonv.Object, pairs [][2]string) error {
	for _, pair := range pairs {
		value, ok := effective.Num(pair[0])
		bounds := asArray(limits.Get(pair[1]))
		if !ok || len(bounds) != 2 {
			continue
		}
		low, lok := bounds[0].(float64)
		high, hok := bounds[1].(float64)
		if lok && hok && (value < low || value > high) {
			return c.Err(400, "invalid_request", fmt.Sprintf("%s must be within the worker's range %s–%s", pair[0], jsonv.Number(low), jsonv.Number(high)), "parameters."+pair[0])
		}
	}
	return nil
}

func containsJSON(list []any, v any) bool {
	for _, e := range list {
		if jsonEqual(e, v) {
			return true
		}
	}
	return false
}

// sizeList formats [[w, h], …] as "w×h, …".
func sizeList(sizes []any) string {
	parts := []string{}
	for _, raw := range sizes {
		if pair := asArray(raw); len(pair) == 2 {
			w, _ := pair[0].(float64)
			h, _ := pair[1].(float64)
			parts = append(parts, jsonv.Number(w)+"×"+jsonv.Number(h))
		}
	}
	return strings.Join(parts, ", ")
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
	if options.Str("offload") != "none" && (minor < 4 || !c.Contains(strList(caps.Get("offload_modes")), options.Str("offload"))) {
		return "", nil, nil, c.Err(400, "unsupported", "The connected worker does not advertise this offload mode", "options.offload")
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
