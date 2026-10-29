// Package contracts holds the request, recipe and registration shapes, their strict validation,
// protocol negotiation and the small helpers every other package shares.
package contracts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

const VERSION = "0.7.0"

var PROTOCOLS = []string{"media-worker/1.0", "media-worker/1.1", "media-worker/1.2"}
var PROTOCOL = PROTOCOLS[len(PROTOCOLS)-1]
var BACKENDS = []string{"diffusers-sdxl", "owned-sdxl"}
var TASKS = []string{"text-to-image", "image-to-image", "upscale"}

// FAMILIES are the model families a registration may name, with the tasks each serves.
var FAMILIES = map[string][]string{
	"sdxl-base": {"text-to-image", "image-to-image"},
	"esrgan":    {"upscale"},
}

// TaskFamily is the family whose bundles serve a task.
func TaskFamily(task string) string {
	if task == "upscale" {
		return "esrgan"
	}
	return "sdxl-base"
}

// TaskMinor is the lowest protocol minor a task needs.
func TaskMinor(task string) int {
	switch task {
	case "image-to-image":
		return 1
	case "upscale":
		return 2
	}
	return 0
}

// Minor is the minor number of a negotiated protocol (media-worker/1.N).
func Minor(protocol string) int {
	for i, p := range PROTOCOLS {
		if p == protocol {
			return i
		}
	}
	return 0
}

// FALLBACKS are the fallbacks a caller may authorize.
var FALLBACKS = []string{"cpu", "lower_precision"}

// MaxCount bounds parameters.count before the worker's advertised limit applies.
const MaxCount = 4

const MaxAdapters = 4

var (
	idPattern      = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
	inputIDPattern = regexp.MustCompile(`^input_[a-f0-9]{32}$`)
)

var ID = Str(StrOpts{Pattern: idPattern})
var InputID = Str(StrOpts{Pattern: inputIDPattern})

var parameterFields = []Field{
	{Name: "width", Check: Num(NumOpts{Int: true, Min: F(64), Max: F(2048), MultipleOf: 8})},
	{Name: "height", Check: Num(NumOpts{Int: true, Min: F(64), Max: F(2048), MultipleOf: 8})},
	{Name: "steps", Check: Num(NumOpts{Int: true, Min: F(1), Max: F(150)})},
	{Name: "guidance", Check: Num(NumOpts{Min: F(0), Max: F(30)})},
	{Name: "seed", Check: Num(NumOpts{Int: true, Min: F(0), Max: F(4294967295)})},
	{Name: "sampler", Check: Literal("euler")},
	{Name: "scheduler", Check: Literal("euler")},
	{Name: "strength", Check: Num(NumOpts{GreaterThan: F(0), Max: F(1)}), Optional: true},
	{Name: "count", Check: Num(NumOpts{Int: true, Min: F(1), Max: F(MaxCount)}), Optional: true},
}

var tileFields = []Field{
	{Name: "tile", Check: Num(NumOpts{Int: true, Min: F(0), Max: F(8192)})},
	{Name: "tile_overlap", Check: Num(NumOpts{Int: true, Min: F(0), Max: F(64)})},
}

// Parameters is the complete, resolved parameter set of a diffusion task.
var Parameters = StrictObject(parameterFields...)

// UpscaleParameters is the resolved parameter set of an upscale: the input's size and tiling.
var UpscaleParameters = StrictObject(append(append([]Field{}, parameterFields[:2]...), tileFields...)...)

// PartialParameters is a request's or recipe's parameter overrides for any task.
var PartialParameters = StrictObject(Partial(append(append([]Field{}, parameterFields...), tileFields...))...)

// Prompt is a non-blank prompt; diffusion tasks require one and upscale forbids it.
var Prompt = Str(StrOpts{Min: 1, Max: 10000, Refine: func(s string) bool { return strings.TrimFunc(s, isJSSpace) != "" }})

var Inputs = StrictObject(
	Field{Name: "prompt", Check: Prompt, Optional: true},
	Field{Name: "negative_prompt", Check: Str(StrOpts{Max: 10000}), Optional: true},
	Field{Name: "image", Check: InputID, Optional: true},
)

var loadOptionFields = []Field{
	{Name: "precision", Check: Enum("float16", "float32", "auto")},
	{Name: "attention", Check: Literal("sdpa")},
	{Name: "offload", Check: Literal("none")},
}

var LoadOptions = StrictObject(loadOptionFields...)
var partialLoadOptions = StrictObject(Partial(loadOptionFields)...)

var AdapterUse = StrictObject(
	Field{Name: "adapter_id", Check: ID},
	Field{Name: "scale", Check: Num(NumOpts{Min: F(0), Max: F(2)}), Default: func() any { return 1.0 }},
)

var adapterList = Array(AdapterUse, ArrOpts{Max: MaxAdapters})

var Submission = StrictObject(
	Field{Name: "recipe_id", Check: ID},
	Field{Name: "model_id", Check: ID},
	Field{Name: "inputs", Check: submissionInputs},
	Field{Name: "parameters", Check: PartialParameters, Default: func() any { return jsonv.NewObject() }},
	Field{Name: "adapters", Check: adapterList, Optional: true},
	Field{Name: "backend_id", Check: Enum(BACKENDS...), Optional: true},
	Field{Name: "backend_options", Check: StrictObject(
		Field{Name: "diffusers-sdxl", Check: partialLoadOptions, Optional: true},
		Field{Name: "owned-sdxl", Check: partialLoadOptions, Optional: true},
	), Optional: true},
	Field{Name: "retry_of", Check: ID, Optional: true},
	Field{Name: "authorized_fallbacks", Check: Array(Enum(FALLBACKS...), ArrOpts{Max: len(FALLBACKS)}), Optional: true},
	// Schema-2 recipes: the models of operations after the first, by operation ID.
	Field{Name: "models", Check: keyed(opIDPattern, ID), Optional: true},
	// Pins the job to one configured worker of the pool.
	Field{Name: "worker_id", Check: ID, Optional: true},
)

// recipeSchemaV1 is the schema-1 recipe: one pipeline with its declared inputs and defaults.
var recipeSchemaV1 = Refine(StrictObject(
	Field{Name: "schema_version", Check: Literal(1.0)},
	Field{Name: "recipe_id", Check: ID},
	Field{Name: "revision", Check: Num(NumOpts{Int: true, GreaterThan: F(0)})},
	Field{Name: "pipeline", Check: Enum(TASKS...)},
	Field{Name: "model_family", Check: Enum("sdxl-base", "esrgan")},
	Field{Name: "inputs", Check: StrictObject(
		Field{Name: "prompt", Check: StrictObject(
			Field{Name: "type", Check: Literal("string")},
			Field{Name: "required", Check: Literal(true)},
		), Optional: true},
		Field{Name: "image", Check: StrictObject(
			Field{Name: "type", Check: Literal("image")},
			Field{Name: "required", Check: Literal(true)},
		), Optional: true},
	)},
	Field{Name: "defaults", Check: PartialParameters},
	Field{Name: "adapters", Check: adapterList, Optional: true},
), func(v any) bool {
	r := v.(*jsonv.Object)
	inputs, defaults := r.Obj("inputs"), r.Obj("defaults")
	pipeline := r.Str("pipeline")
	if r.Str("model_family") != TaskFamily(pipeline) || !defaultsServe(pipeline, defaults, r.Has("adapters")) {
		return false
	}
	switch pipeline {
	case "image-to-image":
		return inputs.Has("prompt") && inputs.Has("image")
	case "upscale":
		return inputs.Has("image") && !inputs.Has("prompt")
	}
	return inputs.Has("prompt") && !inputs.Has("image")
}, "inputs")

func hasTile(defaults *jsonv.Object) bool {
	return defaults.Has("tile") || defaults.Has("tile_overlap")
}

// Conversion is the provenance a converted bundle's conversion.json carries.
var Conversion = StrictObject(
	Field{Name: "source_sha256", Check: Str(StrOpts{Pattern: regexp.MustCompile(`^[a-f0-9]{64}$`)})},
	Field{Name: "reference_bundle_digest", Check: Str(StrOpts{Pattern: regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)})},
	Field{Name: "converter", Check: Record(Str(StrOpts{Max: 100}))},
	Field{Name: "mapped_keys", Check: Record(Num(NumOpts{Int: true, Min: F(0)}))},
)

var sourceFields = []Field{
	{Name: "repository", Check: Str(StrOpts{Max: 300}), Optional: true},
	{Name: "revision", Check: Str(StrOpts{Max: 100}), Optional: true},
	{Name: "license", Check: Str(StrOpts{Max: 300}), Optional: true},
}

var sourceSchema = StrictObject(sourceFields...)

var Registration = StrictObject(
	Field{Name: "model_id", Check: ID},
	Field{Name: "artifact_handle", Check: ID},
	Field{Name: "family", Check: Enum("sdxl-base", "esrgan"), Optional: true},
	Field{Name: "source", Check: StrictObject(append(append([]Field{}, sourceFields...),
		Field{Name: "conversion", Check: Conversion, Optional: true})...), Default: func() any { return jsonv.NewObject() }},
)

var AdapterRegistration = StrictObject(
	Field{Name: "adapter_id", Check: ID},
	Field{Name: "artifact_handle", Check: ID},
	Field{Name: "source", Check: sourceSchema, Default: func() any { return jsonv.NewObject() }},
)

// DefaultRecipes are seeded into every state directory.
func DefaultRecipes() []*jsonv.Object {
	return []*jsonv.Object{
		mustParse(RecipeSchema, `{"schema_version":1,"recipe_id":"portrait","revision":1,"pipeline":"text-to-image","model_family":"sdxl-base","inputs":{"prompt":{"type":"string","required":true}},"defaults":{"width":1024,"height":1024,"steps":30,"guidance":6}}`),
		mustParse(RecipeSchema, `{"schema_version":1,"recipe_id":"enlarge","revision":1,"pipeline":"upscale","model_family":"esrgan","inputs":{"image":{"type":"image","required":true}},"defaults":{"tile":0,"tile_overlap":16}}`),
		mustParse(RecipeSchema, `{"schema_version":1,"recipe_id":"variation","revision":1,"pipeline":"image-to-image","model_family":"sdxl-base","inputs":{"prompt":{"type":"string","required":true},"image":{"type":"image","required":true}},"defaults":{"steps":30,"guidance":6,"strength":0.6}}`),
	}
}

func mustParse(check Check, text string) *jsonv.Object {
	raw, err := jsonv.Parse([]byte(text))
	if err != nil {
		panic(err)
	}
	v, err := check(raw, nil)
	if err != nil {
		panic(err)
	}
	return v.(*jsonv.Object)
}

// Validate runs a check from the root of a document.
func Validate(check Check, v any) (*jsonv.Object, error) {
	out, err := check(v, nil)
	if err != nil {
		return nil, err
	}
	return out.(*jsonv.Object), nil
}

// Hash is "sha256:" + hex SHA-256 of the canonical JSON.
func Hash(v any) string {
	sum := sha256.Sum256([]byte(jsonv.Canonical(v)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Seed is a uniformly random 32-bit seed.
func Seed() float64 { return float64(binary.BigEndian.Uint32(randomBytes(4))) }

func NewInputID() string { return "input_" + hex.EncodeToString(randomBytes(16)) }

func RandomHex(n int) string { return hex.EncodeToString(randomBytes(n)) }

// UUID is a random RFC 4122 version 4 identifier.
func UUID() string {
	b := randomBytes(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Now is an ISO-8601 UTC timestamp with millisecond precision (Date#toISOString).
func Now() string { return ISO(time.Now()) }

func ISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ParseISO parses the timestamps this service writes; ok is false for anything else.
func ParseISO(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z07:00", time.RFC3339Nano, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Expired reports whether an optional expiry timestamp is in the past.
func Expired(expires any) bool {
	s, ok := expires.(string)
	if !ok || s == "" {
		return false
	}
	t, ok := ParseISO(s)
	return ok && !t.After(time.Now())
}

// ApiError is the public error shape: an HTTP status, a stable code and an optional field.
type ApiError struct {
	Status  int
	Code    string
	Message string
	Field   string
	Extra   *jsonv.Object
}

func (e *ApiError) Error() string { return e.Message }

func Err(status int, code, message string, field ...string) *ApiError {
	e := &ApiError{Status: status, Code: code, Message: message}
	if len(field) > 0 {
		e.Field = field[0]
	}
	return e
}

func Terminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled"
}

// EffectiveSteps is floor(steps × strength) for image-to-image, steps otherwise.
func EffectiveSteps(p *jsonv.Object) float64 {
	steps, _ := p.Num("steps")
	strength, ok := p.Num("strength")
	if !ok {
		return steps
	}
	return math.Min(math.Floor(steps*strength), steps)
}

// PNGDimensions validates a PNG signature, IHDR and IEND without decoding pixels.
func PNGDimensions(data []byte) (int, int, error) {
	if len(data) < 33 || hex.EncodeToString(data[:8]) != "89504e470d0a1a0a" ||
		string(data[12:16]) != "IHDR" || string(data[len(data)-8:len(data)-4]) != "IEND" {
		return 0, 0, Err(400, "invalid_image", "Expected a complete PNG file")
	}
	width := int(binary.BigEndian.Uint32(data[16:20]))
	height := int(binary.BigEndian.Uint32(data[20:24]))
	depth, color := data[24], data[25]
	if depth != 8 || (color != 0 && color != 2 && color != 4 && color != 6) {
		return 0, 0, Err(400, "unsupported", "Use an 8-bit greyscale or RGB(A) PNG", "image")
	}
	for _, value := range []int{width, height} {
		if value < 64 || value > 2048 || value%8 != 0 {
			return 0, 0, Err(400, "invalid_image", "Image sides must be 64–2048 pixels and multiples of eight")
		}
	}
	return width, height, nil
}

// Negotiate chooses the highest protocol minor both the runtime and the worker support.
func Negotiate(advertised any, fallback any) (string, error) {
	var offered []any
	if list, ok := advertised.([]any); ok {
		offered = list
	} else if s, ok := fallback.(string); ok {
		offered = []any{s}
	}
	for i := len(PROTOCOLS) - 1; i >= 0; i-- {
		for _, o := range offered {
			if o == PROTOCOLS[i] {
				return PROTOCOLS[i], nil
			}
		}
	}
	return "", Err(503, "protocol_mismatch", "Requires one of "+strings.Join(PROTOCOLS, ", ")+" from the worker")
}

func Contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// isJSSpace is the whitespace set String#trim removes.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

// OutputIDs are the outputs a worker produces for a task: the advertised IDs, with each repeated
// output expanded to id, id_2 … id_N for parameters.count = N. Workers before 1.2 produce "image".
func OutputIDs(taskParameters *jsonv.Object, count int) []string {
	if taskParameters == nil {
		return []string{"image"}
	}
	repeated := map[string]bool{}
	for _, id := range taskParameters.Arr("repeated_outputs") {
		if s, ok := id.(string); ok {
			repeated[s] = true
		}
	}
	var out []string
	for _, raw := range taskParameters.Arr("outputs") {
		id, _ := raw.(string)
		out = append(out, id)
		if repeated[id] {
			for k := 2; k <= count; k++ {
				out = append(out, fmt.Sprintf("%s_%d", id, k))
			}
		}
	}
	return out
}

// Extensions maps each verifiable output MIME type to its file extension.
var Extensions = map[string]string{"image/png": "png", "application/zip": "zip", "video/mp4": "mp4", "audio/wav": "wav"}

// Signatures verify an output's bytes against its MIME type and record without decoding
// media. PNG is checked from 0.5.0; the archive, video and audio checks arrive with their tasks.
var Signatures = map[string]func(data []byte, record *jsonv.Object) bool{
	"image/png": func(data []byte, record *jsonv.Object) bool {
		width, _ := record.Num("width")
		height, _ := record.Num("height")
		return len(data) >= 24 && string(data[:8]) == "\x89PNG\r\n\x1a\n" && string(data[12:16]) == "IHDR" &&
			float64(binary.BigEndian.Uint32(data[16:20])) == width && float64(binary.BigEndian.Uint32(data[20:24])) == height
	},
}
