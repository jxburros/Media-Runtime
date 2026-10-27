package contracts

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Recipe schema 2 is a linear sequence of typed operations. Schema 1 recipes stay valid and are
// normalized to one operation in memory, so every downstream path handles one shape.

// MaxOperations bounds a schema-2 recipe; each operation is its own worker execution.
const MaxOperations = 8

var (
	// Operation IDs never contain "__", which separates them from an output ID in the public
	// ID of an intermediate output.
	opIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)
	inputNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

var OpID = Str(StrOpts{Max: 32, Pattern: opIDPattern})

// TaskOutputs are the base output IDs each task produces; repeated outputs expand to id_2 …
// at execution time from the worker's task_parameters.
var TaskOutputs = map[string][]string{
	"text-to-image":  {"image"},
	"image-to-image": {"image"},
	"upscale":        {"image"},
}

// TaskParameterInputs are the parameters a typed number input may bind to, per task.
var TaskParameterInputs = map[string][]string{
	"text-to-image":  {"width", "height", "steps", "guidance", "seed", "count"},
	"image-to-image": {"steps", "guidance", "seed", "strength"},
	"upscale":        {"tile", "tile_overlap"},
}

// declaration is one typed recipe input: a string, an uploaded image or a bounded number.
func declaration(v any, path []string) (any, error) {
	in, ok := v.(*jsonv.Object)
	if !ok {
		return nil, Invalid(path)
	}
	required := Field{Name: "required", Check: Bool(), Default: func() any { return true }}
	switch in.Str("type") {
	case "string":
		return StrictObject(Field{Name: "type", Check: Literal("string")}, required,
			Field{Name: "max_length", Check: Num(NumOpts{Int: true, Min: F(1), Max: F(10000)}), Optional: true})(v, path)
	case "image":
		return StrictObject(Field{Name: "type", Check: Literal("image")}, required)(v, path)
	case "number":
		out, err := StrictObject(Field{Name: "type", Check: Literal("number")}, required,
			Field{Name: "minimum", Check: Num(NumOpts{}), Optional: true},
			Field{Name: "maximum", Check: Num(NumOpts{}), Optional: true})(v, path)
		if err != nil {
			return nil, err
		}
		o := out.(*jsonv.Object)
		low, lok := o.Num("minimum")
		high, hok := o.Num("maximum")
		if lok && hok && low > high {
			return nil, Invalid(at(path, "maximum"))
		}
		return o, nil
	}
	return nil, Invalid(at(path, "type"))
}

// keyed is a Record whose keys must match a pattern.
func keyed(pattern *regexp.Regexp, value Check) Check {
	record := Record(value)
	return func(v any, path []string) (any, error) {
		if in, ok := v.(*jsonv.Object); ok {
			for _, k := range in.Keys() {
				if !pattern.MatchString(k) {
					return nil, Invalid(at(path, k))
				}
			}
		}
		return record(v, path)
	}
}

var bindingPattern = regexp.MustCompile(`^(?:input:([a-z][a-z0-9_]{0,31})|op:([a-z][a-z0-9_]{0,31})/([a-z][a-z0-9_]{0,31}))$`)

var binding = StrictObject(Field{Name: "from", Check: Str(StrOpts{Max: 80, Pattern: bindingPattern})})

var operationSchema = StrictObject(
	Field{Name: "op_id", Check: OpID},
	Field{Name: "task", Check: Enum(TASKS...)},
	Field{Name: "model_family", Check: Enum("sdxl-base", "esrgan")},
	Field{Name: "inputs", Check: keyed(inputNamePattern, binding)},
	Field{Name: "defaults", Check: PartialParameters, Default: func() any { return jsonv.NewObject() }},
	Field{Name: "adapters", Check: adapterList, Optional: true},
)

var publishPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}/[a-z][a-z0-9_]{0,31}$`)

var recipeSchemaV2 = StrictObject(
	Field{Name: "schema_version", Check: Literal(2.0)},
	Field{Name: "recipe_id", Check: ID},
	Field{Name: "revision", Check: Num(NumOpts{Int: true, GreaterThan: F(0)})},
	Field{Name: "inputs", Check: keyed(inputNamePattern, declaration)},
	Field{Name: "operations", Check: Array(operationSchema, ArrOpts{Min: 1, Max: MaxOperations})},
	Field{Name: "publish", Check: Array(Str(StrOpts{Pattern: publishPattern}), ArrOpts{Min: 1, Max: 16}), Optional: true},
)

// RecipeSchema accepts schema 1 exactly as before and schema 2 linear recipes.
func RecipeSchema(v any, path []string) (any, error) {
	if in, ok := v.(*jsonv.Object); ok && in.Get("schema_version") == 2.0 {
		out, err := recipeSchemaV2(v, path)
		if err != nil {
			return nil, err
		}
		if err := checkLinear(out.(*jsonv.Object), path); err != nil {
			return nil, err
		}
		return out, nil
	}
	return recipeSchemaV1(v, path)
}

// Binding splits a binding's source: ("input", name, "") or ("op", op_id, output_id).
func Binding(b *jsonv.Object) (kind, name, output string) {
	m := bindingPattern.FindStringSubmatch(b.Str("from"))
	if m == nil {
		return "", "", ""
	}
	if m[1] != "" {
		return "input", m[1], ""
	}
	return "op", m[2], m[3]
}

// taskInputType is the declaration type a task input key consumes, or "" when the task has no
// such input. Number inputs bind to the task's parameters.
func taskInputType(task, key string) string {
	switch key {
	case "prompt", "negative_prompt":
		if task != "upscale" {
			return "string"
		}
	case "image":
		if task != "text-to-image" {
			return "image"
		}
	default:
		if Contains(TaskParameterInputs[task], key) {
			return "number"
		}
	}
	return ""
}

// checkLinear validates what a schema cannot express: distinct operation IDs, families that serve
// their tasks, bindings without forward references whose types match the consuming task, and
// publish entries naming outputs that exist.
func checkLinear(r *jsonv.Object, path []string) error {
	declared := r.Obj("inputs")
	for _, name := range declared.Keys() {
		if want, reserved := reservedInputs[name]; reserved && declared.Obj(name).Str("type") != want {
			return Invalid(at(at(at(path, "inputs"), name), "type"))
		}
	}
	used := map[string]bool{}
	ops := map[string]string{}
	for i, raw := range r.Arr("operations") {
		op := raw.(*jsonv.Object)
		here := at(at(path, "operations"), strconv.Itoa(i))
		id, task := op.Str("op_id"), op.Str("task")
		if _, dup := ops[id]; dup {
			return Invalid(at(here, "op_id"))
		}
		if op.Str("model_family") != TaskFamily(task) {
			return Invalid(at(here, "model_family"))
		}
		if !defaultsServe(task, op.Obj("defaults"), op.Has("adapters")) {
			return Invalid(at(here, "defaults"))
		}
		inputs := op.Obj("inputs")
		for _, key := range inputs.Keys() {
			want := taskInputType(task, key)
			if want == "" {
				return Invalid(at(at(here, "inputs"), key))
			}
			kind, name, output := Binding(inputs.Obj(key))
			switch kind {
			case "input":
				if declared.Obj(name).Str("type") != want {
					return Invalid(at(at(at(here, "inputs"), key), "from"))
				}
				used[name] = true
			case "op":
				producer, earlier := ops[name]
				if !earlier || want != "image" || !Contains(TaskOutputs[producer], output) {
					return Invalid(at(at(at(here, "inputs"), key), "from"))
				}
			}
		}
		// A task's required input must always be present: bound to a required declaration or to
		// an earlier operation's output.
		for _, need := range requiredInputs(task) {
			b := inputs.Obj(need)
			if b == nil {
				return Invalid(at(at(here, "inputs"), need))
			}
			if kind, name, _ := Binding(b); kind == "input" && !declared.Obj(name).Bool("required") {
				return Invalid(at(at(at(here, "inputs"), need), "from"))
			}
		}
		ops[id] = task
	}
	for _, name := range declared.Keys() {
		if !used[name] {
			return Invalid(at(at(path, "inputs"), name))
		}
	}
	seen := map[string]bool{}
	for i, raw := range r.Arr("publish") {
		entry := raw.(string)
		parts := strings.SplitN(entry, "/", 2)
		task, ok := ops[parts[0]]
		if !ok || !Contains(TaskOutputs[task], parts[1]) || seen[entry] {
			return Invalid(at(at(path, "publish"), strconv.Itoa(i)))
		}
		seen[entry] = true
	}
	return nil
}

func requiredInputs(task string) []string {
	switch task {
	case "image-to-image":
		return []string{"prompt", "image"}
	case "upscale":
		return []string{"image"}
	}
	return []string{"prompt"}
}

// defaultsServe is the schema-1 rule for which defaults a pipeline accepts.
func defaultsServe(pipeline string, defaults *jsonv.Object, adapters bool) bool {
	switch pipeline {
	case "image-to-image":
		return !defaults.Has("count") && !hasTile(defaults)
	case "upscale":
		// An upscale takes one image; only tiling has defaults and nothing adapts the network.
		for _, k := range defaults.Keys() {
			if k != "tile" && k != "tile_overlap" {
				return false
			}
		}
		return !adapters
	}
	return !defaults.Has("strength") && !hasTile(defaults)
}

// NormalizeRecipe returns a recipe as schema 2. A schema-1 recipe becomes one operation named
// after the recipe whose inputs keep their names; its declarations are the inputs schema 1
// accepted (negative_prompt was always optional on diffusion pipelines).
func NormalizeRecipe(recipe *jsonv.Object) *jsonv.Object {
	if recipe.Get("schema_version") == 2.0 {
		out := recipe.Clone()
		if !out.Has("publish") {
			last := out.Arr("operations")[len(out.Arr("operations"))-1].(*jsonv.Object)
			publish := []any{}
			for _, id := range TaskOutputs[last.Str("task")] {
				publish = append(publish, last.Str("op_id")+"/"+id)
			}
			out.Set("publish", publish)
		}
		return out
	}
	task := recipe.Str("pipeline")
	declared, bindings := jsonv.NewObject(), jsonv.NewObject()
	for _, name := range []string{"prompt", "negative_prompt", "image"} {
		switch {
		case recipe.Obj("inputs").Has(name):
			declared.Set(name, recipe.Obj("inputs").Get(name))
		case name == "negative_prompt" && task != "upscale":
			declared.Set(name, jsonv.O("type", "string", "required", false))
		default:
			continue
		}
		bindings.Set(name, jsonv.O("from", "input:"+name))
	}
	op := jsonv.O(
		"op_id", OpName(recipe.Str("recipe_id")),
		"task", task,
		"model_family", recipe.Get("model_family"),
		"inputs", bindings,
		"defaults", recipe.Get("defaults"),
		"adapters", jsonv.Maybe(recipe.Get("adapters")),
	)
	out := jsonv.O(
		"schema_version", 2.0,
		"recipe_id", recipe.Get("recipe_id"),
		"revision", recipe.Get("revision"),
		"inputs", declared,
		"operations", []any{op},
	)
	publish := []any{}
	for _, id := range TaskOutputs[task] {
		publish = append(publish, op.Str("op_id")+"/"+id)
	}
	return out.Set("publish", publish)
}

// OpName is a schema-1 recipe ID as an operation ID where it is one, else its pipeline's name.
func OpName(recipeID string) string {
	if len(recipeID) <= 32 && opIDPattern.MatchString(recipeID) {
		return recipeID
	}
	return "operation"
}

// SubmittedInputs validates a schema-2 submission's inputs against the recipe's declarations.
func SubmittedInputs(declared, inputs *jsonv.Object) error {
	for _, name := range inputs.Keys() {
		d := declared.Obj(name)
		field := []string{"inputs", name}
		if d == nil {
			return Err(400, "invalid_request", "Recipe declares no input "+name, "inputs."+name)
		}
		value := inputs.Get(name)
		switch d.Str("type") {
		case "string":
			s, ok := value.(string)
			max, hasMax := d.Num("max_length")
			if !ok || (hasMax && float64(utf16Len(s)) > max) {
				return Invalid(field)
			}
		case "image":
			if _, err := InputID(value, field); err != nil {
				return err
			}
		case "number":
			f, ok := value.(float64)
			low, lok := d.Num("minimum")
			high, hok := d.Num("maximum")
			if !ok || math.IsInf(f, 0) || (lok && f < low) || (hok && f > high) {
				return Invalid(field)
			}
		}
	}
	for _, name := range declared.Keys() {
		if declared.Obj(name).Bool("required") && !inputs.Has(name) {
			return Invalid([]string{"inputs", name})
		}
	}
	return nil
}

// namedInputs are a schema-2 submission's inputs: declared names with string or number values,
// checked against the recipe's declarations at resolution.
var namedInputs = keyed(inputNamePattern, func(v any, path []string) (any, error) {
	switch t := v.(type) {
	case string:
		if utf16Len(t) <= 10000 {
			return t, nil
		}
	case float64:
		if !math.IsNaN(t) && !math.IsInf(t, 0) {
			return t, nil
		}
	}
	return nil, Invalid(path)
})

// reservedInputs keep their schema-1 types in every recipe, so a submission using only these
// names is validated exactly as before.
var reservedInputs = map[string]string{"prompt": "string", "negative_prompt": "string", "image": "image"}

// submissionInputs accepts the schema-1 inputs exactly as before, else named schema-2 inputs
// when the submission uses a name schema 1 does not know; otherwise the schema-1 error stands.
func submissionInputs(v any, path []string) (any, error) {
	out, err := Inputs(v, path)
	if err == nil {
		return out, nil
	}
	in, _ := v.(*jsonv.Object)
	for _, k := range in.Keys() {
		if _, reserved := reservedInputs[k]; !reserved {
			if named, nerr := namedInputs(v, path); nerr == nil {
				return named, nil
			}
			break
		}
	}
	return nil, err
}

// IsLinear reports whether a recipe snapshot is schema 2.
func IsLinear(recipe *jsonv.Object) bool { return recipe.Get("schema_version") == 2.0 }
