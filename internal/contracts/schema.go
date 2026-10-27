package contracts

import (
	"math"
	"regexp"
	"strings"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Check validates one decoded JSON value and returns its normalized form. Validation is strict:
// unknown object members are errors, and the first failing path is reported the way the
// TypeScript releases reported zod's first issue.
type Check func(v any, path []string) (any, error)

// Field is one member of a strict object.
type Field struct {
	Name     string
	Check    Check
	Optional bool
	Default  func() any
}

// Invalid is the error for a value that does not satisfy its schema.
func Invalid(path []string) *ApiError {
	return &ApiError{Status: 400, Code: "invalid_request", Message: "Invalid or unsupported request field", Field: strings.Join(path, ".")}
}

func at(path []string, name string) []string {
	return append(append([]string(nil), path...), name)
}

// StrictObject checks members in declaration order, then refuses unknown members.
func StrictObject(fields ...Field) Check {
	return func(v any, path []string) (any, error) {
		in, ok := v.(*jsonv.Object)
		if !ok {
			return nil, Invalid(path)
		}
		out := jsonv.NewObject()
		known := map[string]bool{}
		for _, f := range fields {
			known[f.Name] = true
			if !in.Has(f.Name) {
				if f.Default != nil {
					out.Set(f.Name, f.Default())
					continue
				}
				if f.Optional {
					continue
				}
				return nil, Invalid(at(path, f.Name))
			}
			value, err := f.Check(in.Get(f.Name), at(path, f.Name))
			if err != nil {
				return nil, err
			}
			out.Set(f.Name, value)
		}
		for _, k := range in.Keys() {
			if !known[k] {
				return nil, Invalid(path)
			}
		}
		return out, nil
	}
}

// Partial makes every field optional (zod's .partial()).
func Partial(fields []Field) []Field {
	out := make([]Field, len(fields))
	for i, f := range fields {
		f.Optional = true
		f.Default = nil
		out[i] = f
	}
	return out
}

// Refine applies a whole-value predicate after the base check succeeds.
func Refine(base Check, ok func(any) bool, field ...string) Check {
	return func(v any, path []string) (any, error) {
		out, err := base(v, path)
		if err != nil {
			return nil, err
		}
		if !ok(out) {
			return nil, Invalid(append(append([]string(nil), path...), field...))
		}
		return out, nil
	}
}

type StrOpts struct {
	Min, Max int
	Pattern  *regexp.Regexp
	Refine   func(string) bool
}

func Str(o StrOpts) Check {
	return func(v any, path []string) (any, error) {
		s, ok := v.(string)
		// zod measures string length in UTF-16 code units.
		n := utf16Len(s)
		if !ok || n < o.Min || (o.Max > 0 && n > o.Max) || (o.Pattern != nil && !o.Pattern.MatchString(s)) || (o.Refine != nil && !o.Refine(s)) {
			return nil, Invalid(path)
		}
		return s, nil
	}
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

type NumOpts struct {
	Int         bool
	Min, Max    *float64
	GreaterThan *float64
	MultipleOf  float64
}

func F(f float64) *float64 { return &f }

func Num(o NumOpts) Check {
	return func(v any, path []string) (any, error) {
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || (o.Int && (f != math.Trunc(f) || math.IsInf(f, 0))) ||
			(o.Min != nil && f < *o.Min) || (o.Max != nil && f > *o.Max) ||
			(o.GreaterThan != nil && f <= *o.GreaterThan) ||
			(o.MultipleOf != 0 && math.Mod(f, o.MultipleOf) != 0) {
			return nil, Invalid(path)
		}
		return f, nil
	}
}

func Bool() Check {
	return func(v any, path []string) (any, error) {
		b, ok := v.(bool)
		if !ok {
			return nil, Invalid(path)
		}
		return b, nil
	}
}

// Literal accepts exactly one value.
func Literal(want any) Check {
	return func(v any, path []string) (any, error) {
		if v != want {
			return nil, Invalid(path)
		}
		return v, nil
	}
}

func Enum(values ...string) Check {
	return func(v any, path []string) (any, error) {
		s, ok := v.(string)
		for _, allowed := range values {
			if ok && s == allowed {
				return s, nil
			}
		}
		return nil, Invalid(path)
	}
}

type ArrOpts struct{ Min, Max int }

func Array(elem Check, o ArrOpts) Check {
	return func(v any, path []string) (any, error) {
		a, ok := v.([]any)
		if !ok {
			return nil, Invalid(path)
		}
		if len(a) < o.Min || (o.Max > 0 && len(a) > o.Max) {
			return nil, Invalid(path)
		}
		out := make([]any, 0, len(a))
		for i, e := range a {
			value, err := elem(e, at(path, itoa(i)))
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	}
}

// Record checks every member value of an object with arbitrary keys.
func Record(value Check) Check {
	return func(v any, path []string) (any, error) {
		in, ok := v.(*jsonv.Object)
		if !ok {
			return nil, Invalid(path)
		}
		out := jsonv.NewObject()
		for _, k := range in.Keys() {
			checked, err := value(in.Get(k), at(path, k))
			if err != nil {
				return nil, err
			}
			out.Set(k, checked)
		}
		return out, nil
	}
}

func Nullable(base Check) Check {
	return func(v any, path []string) (any, error) {
		if v == nil {
			return nil, nil
		}
		return base(v, path)
	}
}

func itoa(i int) string {
	return jsonv.Number(float64(i))
}
