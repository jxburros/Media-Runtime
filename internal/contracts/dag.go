package contracts

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

const MaxExpandedOperations = MaxOperations * MaxCount

var operationSchemaV3 = StrictObject(
	Field{Name: "op_id", Check: OpID},
	Field{Name: "task", Check: Enum(TASKS...)},
	Field{Name: "model_family", Check: Enum(FAMILY_NAMES...)},
	Field{Name: "inputs", Check: keyed(inputNamePattern, binding)},
	Field{Name: "defaults", Check: PartialParameters, Default: func() any { return jsonv.NewObject() }},
	Field{Name: "adapters", Check: adapterList, Optional: true},
	Field{Name: "conditioning", Check: ConditioningUses, Optional: true},
	Field{Name: "foreach", Check: StrictObject(Field{Name: "from", Check: Str(StrOpts{Max: 80, Pattern: bindingPattern})}), Optional: true},
)
var recipeSchemaV3 = StrictObject(
	Field{Name: "schema_version", Check: Literal(3.0)},
	Field{Name: "recipe_id", Check: ID},
	Field{Name: "revision", Check: Num(NumOpts{Int: true, GreaterThan: F(0)})},
	Field{Name: "inputs", Check: keyed(inputNamePattern, declaration)},
	Field{Name: "operations", Check: Array(operationSchemaV3, ArrOpts{Min: 1, Max: MaxOperations})},
	Field{Name: "publish", Check: Array(Str(StrOpts{Pattern: publishPattern}), ArrOpts{Min: 1, Max: 32}), Optional: true},
)

// orderedDAG returns a stable topological order. Ties keep declaration order;
// every edge must name an existing producer, and cycles fail before admission.
func orderedDAG(recipe *jsonv.Object) ([]*jsonv.Object, error) {
	byID := map[string]*jsonv.Object{}
	ordered := []*jsonv.Object{}
	for _, raw := range recipe.Arr("operations") {
		op := raw.(*jsonv.Object)
		id := op.Str("op_id")
		if byID[id] != nil {
			return nil, Err(400, "invalid_request", "Duplicate operation ID", "operations."+id)
		}
		byID[id] = op
	}
	dependencies := map[string]map[string]bool{}
	for id, op := range byID {
		deps := map[string]bool{}
		for _, key := range op.Obj("inputs").Keys() {
			kind, from, _ := Binding(op.Obj("inputs").Obj(key))
			if kind == "op" {
				deps[from] = true
			}
		}
		if f := op.Obj("foreach"); f != nil {
			kind, from, output := Binding(f)
			if kind != "op" || output != "image" || !UsesImage(op.Str("task")) || op.Obj("inputs").Has("image") || len(id) > 23 {
				return nil, Err(400, "invalid_request", "foreach binds image from a producer batch and needs an operation ID of at most 23 characters", "operations."+id+".foreach")
			}
			deps[from] = true
		}
		for from := range deps {
			if byID[from] == nil {
				return nil, Err(400, "invalid_request", "Unknown producer "+from, "operations."+id)
			}
		}
		dependencies[id] = deps
	}
	done := map[string]bool{}
	for len(ordered) < len(byID) {
		advanced := false
		for _, raw := range recipe.Arr("operations") {
			op := raw.(*jsonv.Object)
			id := op.Str("op_id")
			if done[id] {
				continue
			}
			ready := true
			for from := range dependencies[id] {
				if !done[from] {
					ready = false
					break
				}
			}
			if ready {
				done[id] = true
				ordered = append(ordered, op)
				advanced = true
			}
		}
		if !advanced {
			return nil, Err(400, "invalid_request", "Recipe contains a dependency cycle", "operations")
		}
	}
	return ordered, nil
}
func checkDAG(recipe *jsonv.Object, path []string) error {
	ordered, err := orderedDAG(recipe)
	if err != nil {
		return err
	}
	operations := []any{}
	for _, op := range ordered {
		copy := op.Clone()
		if f := copy.Obj("foreach"); f != nil {
			copy.Obj("inputs").Set("image", f)
			copy.Delete("foreach")
		}
		operations = append(operations, copy)
	}
	return checkLinear(recipe.Clone().Set("operations", operations), path)
}

// RecipeOutput accepts numbered image outputs only in schema 3. Their actual
// existence is checked during expansion, after count bindings are resolved.
func RecipeOutput(task, id string, dag bool) bool {
	if Contains(TaskOutputs[task], id) {
		return true
	}
	if !dag || task != "text-to-image" || !strings.HasPrefix(id, "image_") {
		return false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, "image_"))
	return err == nil && n >= 2 && n <= MaxCount
}
func OutputType(id string) string {
	if strings.HasPrefix(id, "image_") {
		return "image/png"
	}
	return OutputMIME[id]
}

// ExpandDAG turns declarative foreach into explicit operations before admission.
// Every output binding and count is fixed up front; expansion never runs code.
// A foreach group can be published as a group, but consuming its aggregate again
// is refused: callers must name an explicit operation for the next stage.
func ExpandDAG(recipe, request *jsonv.Object) (*jsonv.Object, error) {
	if recipe.Get("schema_version") != 3.0 {
		return recipe, nil
	}
	ordered, err := orderedDAG(recipe)
	if err != nil {
		return nil, err
	}
	expanded := []any{}
	expandedIDs := map[string]bool{}
	groups := map[string][]string{}
	counts := map[string]int{}
	tasks := map[string]string{}
	for pos, source := range ordered {
		id := source.Str("op_id")
		task := source.Str("task")
		count := 1
		if task == "text-to-image" {
			if n, ok := source.Obj("defaults").Num("count"); ok {
				count = int(n)
			}
			if b := source.Obj("inputs").Obj("count"); b != nil {
				kind, name, _ := Binding(b)
				if kind == "input" {
					if n, ok := request.Obj("inputs").Num(name); ok {
						count = int(n)
					}
				}
			}
			if pos == 0 {
				if n, ok := request.Obj("parameters").Num("count"); ok {
					count = int(n)
				}
			}
			if count < 1 || count > MaxCount {
				return nil, Err(400, "invalid_request", "Batch count is outside supported range", "operations."+id+".count")
			}
		}
		copies := 1
		from := ""
		if f := source.Obj("foreach"); f != nil {
			_, from, _ = Binding(f)
			if tasks[from] != "text-to-image" || len(groups[from]) != 1 {
				return nil, Err(400, "invalid_request", "foreach needs one text-to-image batch producer", "operations."+id+".foreach")
			}
			copies = counts[from]
		}
		for index := 1; index <= copies; index++ {
			op := source.Clone()
			op.Delete("foreach")
			op.Set("source_op_id", id)
			expandedID := id
			if from != "" {
				expandedID = fmt.Sprintf("%s_item_%d", id, index)
				output := "image"
				if index > 1 {
					output = fmt.Sprintf("image_%d", index)
				}
				op.Obj("inputs").Set("image", jsonv.O("from", "op:"+from+"/"+output))
			}
			if expandedIDs[expandedID] {
				return nil, Err(400, "invalid_request", "Expanded operation ID collides with another operation", "operations."+id)
			}
			expandedIDs[expandedID] = true
			op.Set("op_id", expandedID)
			for _, name := range op.Obj("inputs").Keys() {
				kind, producer, output := Binding(op.Obj("inputs").Obj(name))
				if kind != "op" {
					continue
				}
				if len(groups[producer]) != 1 {
					return nil, Err(400, "invalid_request", "A foreach group is not a single output; publish it or name a single producer", "operations."+id+".inputs."+name)
				}
				if !RecipeOutput(tasks[producer], output, true) {
					return nil, Err(400, "invalid_request", "Unknown producer output", "operations."+id+".inputs."+name)
				}
				if strings.HasPrefix(output, "image_") {
					n, _ := strconv.Atoi(strings.TrimPrefix(output, "image_"))
					if n > counts[producer] {
						return nil, Err(400, "invalid_request", "Producer batch does not contain the requested output", "operations."+id+".inputs."+name)
					}
				}
				op.Obj("inputs").Set(name, jsonv.O("from", "op:"+groups[producer][0]+"/"+output))
			}
			expanded = append(expanded, op)
			groups[id] = append(groups[id], expandedID)
		}
		counts[id] = count
		tasks[id] = task
		if len(expanded) > MaxExpandedOperations {
			return nil, Err(400, "invalid_request", "Expanded recipe is too large", "operations")
		}
	}
	publish := recipe.Arr("publish")
	if len(publish) == 0 {
		last := ordered[len(ordered)-1]
		for _, output := range TaskOutputs[last.Str("task")] {
			publish = append(publish, last.Str("op_id")+"/"+output)
		}
	}
	expandedPublish := []any{}
	for _, raw := range publish {
		entry := raw.(string)
		parts := strings.SplitN(entry, "/", 2)
		if len(parts) != 2 || len(groups[parts[0]]) == 0 {
			return nil, Err(400, "invalid_request", "Unknown publish operation", "publish")
		}
		if strings.HasPrefix(parts[1], "image_") {
			n, _ := strconv.Atoi(strings.TrimPrefix(parts[1], "image_"))
			if n > counts[parts[0]] {
				return nil, Err(400, "invalid_request", "Published output exceeds producer count", "publish")
			}
		}
		for _, id := range groups[parts[0]] {
			expandedPublish = append(expandedPublish, id+"/"+parts[1])
		}
	}
	return recipe.Clone().Set("source_operations", recipe.Get("operations")).Set("operations", expanded).Set("publish", expandedPublish), nil
}
