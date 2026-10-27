// Package jsonv is an ordered JSON value model. Objects keep insertion order and numbers are
// float64, so encoding matches JavaScript's JSON.stringify for the records the TypeScript
// releases wrote: stored jobs, manifests, API bodies and the canonical digests over them.
package jsonv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"unicode/utf8"
)

// Object is a JSON object that remembers key order. Setting an existing key keeps its position,
// exactly like assigning to a JavaScript object property.
type Object struct {
	keys []string
	vals map[string]any
}

func NewObject() *Object { return &Object{vals: map[string]any{}} }

// O builds an object from alternating key/value pairs; a nil Absent value omits the key.
func O(pairs ...any) *Object {
	o := NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		if _, absent := pairs[i+1].(absent); absent {
			continue
		}
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

type absent struct{}

// Absent marks a value that JavaScript would leave undefined (and JSON.stringify would drop).
var Absent any = absent{}

// Maybe returns Absent for a nil pointer-like value and the value otherwise.
func Maybe(v any) any {
	if v == nil {
		return Absent
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
		if rv.IsNil() {
			return Absent
		}
	}
	return v
}

func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}
func (o *Object) Has(k string) bool {
	if o == nil {
		return false
	}
	_, ok := o.vals[k]
	return ok
}
func (o *Object) Get(k string) any {
	if o == nil {
		return nil
	}
	return o.vals[k]
}
func (o *Object) Set(k string, v any) *Object {
	if _, isAbsent := v.(absent); isAbsent {
		o.Delete(k)
		return o
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}
func (o *Object) Delete(k string) {
	if o == nil {
		return
	}
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Assign copies every key of src into o in src's order (JavaScript object spread).
func (o *Object) Assign(src *Object) *Object {
	if src == nil {
		return o
	}
	for _, k := range src.keys {
		o.Set(k, src.vals[k])
	}
	return o
}

// Clone is a deep copy.
func (o *Object) Clone() *Object {
	if o == nil {
		return nil
	}
	return Clone(o).(*Object)
}

func Clone(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return (*Object)(nil)
		}
		c := NewObject()
		for _, k := range t.keys {
			c.Set(k, Clone(t.vals[k]))
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, e := range t {
			c[i] = Clone(e)
		}
		return c
	}
	return v
}

// Typed accessors return zero values for missing or differently typed members.
func (o *Object) Str(k string) string { s, _ := o.Get(k).(string); return s }
func (o *Object) Obj(k string) *Object {
	v, _ := o.Get(k).(*Object)
	return v
}
func (o *Object) Arr(k string) []any { a, _ := o.Get(k).([]any); return a }
func (o *Object) Bool(k string) bool { b, _ := o.Get(k).(bool); return b }
func (o *Object) Num(k string) (float64, bool) {
	f, ok := o.Get(k).(float64)
	return f, ok
}

func (o *Object) MarshalJSON() ([]byte, error) { return Marshal(o) }
func (o *Object) UnmarshalJSON(b []byte) error {
	v, err := Parse(b)
	if err != nil {
		return err
	}
	obj, ok := v.(*Object)
	if !ok {
		return errors.New("expected a JSON object")
	}
	*o = *obj
	return nil
}

// Parse decodes JSON into nil, bool, float64, string, []any and *Object values.
func Parse(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := decode(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after JSON value")
	}
	return v, nil
}

func decode(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch v := t.(type) {
	case json.Delim:
		switch v {
		case '{':
			o := NewObject()
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				val, err := decode(d)
				if err != nil {
					return nil, err
				}
				o.Set(k, val)
			}
			_, err := d.Token()
			return o, err
		case '[':
			a := []any{}
			for d.More() {
				val, err := decode(d)
				if err != nil {
					return nil, err
				}
				a = append(a, val)
			}
			_, err := d.Token()
			return a, err
		}
		return nil, errors.New("unexpected delimiter")
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, err
		}
		return f, nil
	default:
		return v, nil
	}
}

// From converts any Go value (structs with json tags included) into the ordered model.
func From(v any) any {
	switch t := v.(type) {
	case nil, bool, float64, string, *Object, []any:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out, err := Parse(b)
	if err != nil {
		panic(err)
	}
	return out
}

// Marshal encodes like JSON.stringify(value).
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := write(&buf, v, false); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MustMarshal panics on unsupported values; every value built by this module is encodable.
func MustMarshal(v any) []byte {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Indent encodes like JSON.stringify(value, null, 2).
func Indent(v any) []byte {
	var out bytes.Buffer
	if err := json.Indent(&out, MustMarshal(v), "", "  "); err != nil {
		panic(err)
	}
	return out.Bytes()
}

// Canonical is the TypeScript canonical(): sorted object keys, no whitespace.
func Canonical(v any) string {
	var buf bytes.Buffer
	if err := write(&buf, From(v), true); err != nil {
		panic(err)
	}
	return buf.String()
}

func write(buf *bytes.Buffer, v any, sorted bool) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case absent:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64:
		buf.WriteString(Number(t))
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case string:
		String(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if _, ok := e.(absent); ok {
				e = nil
			}
			if err := write(buf, e, sorted); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case *Object:
		if t == nil {
			buf.WriteString("null")
			return nil
		}
		keys := t.keys
		if sorted {
			keys = append([]string(nil), keys...)
			sort.Strings(keys)
		}
		buf.WriteByte('{')
		first := true
		for _, k := range keys {
			val := t.vals[k]
			if _, ok := val.(absent); ok {
				continue
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			String(buf, k)
			buf.WriteByte(':')
			if err := write(buf, val, sorted); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return write(buf, From(v), sorted)
	}
	return nil
}

// Number formats like JavaScript's Number#toString for finite values (NaN/Infinity become null).
func Number(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	// Go writes e-07/e+21; JavaScript writes e-7/e+21.
	if i := bytes.IndexByte([]byte(s), 'e'); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		sign := exp[0]
		digits := exp[1:]
		for len(digits) > 1 && digits[0] == '0' {
			digits = digits[1:]
		}
		s = mant + "e" + string(sign) + digits
	}
	return s
}

// String writes a JSON string exactly as JSON.stringify does (no HTML or U+2028 escaping).
func String(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			buf.WriteString(`\"`)
		case r == '\\':
			buf.WriteString(`\\`)
		case r == '\b':
			buf.WriteString(`\b`)
		case r == '\f':
			buf.WriteString(`\f`)
		case r == '\n':
			buf.WriteString(`\n`)
		case r == '\r':
			buf.WriteString(`\r`)
		case r == '\t':
			buf.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(buf, `\u%04x`, r)
		case r == utf8.RuneError && size == 1:
			buf.WriteString(`�`)
		default:
			buf.WriteString(s[i : i+size])
		}
		i += size
	}
	buf.WriteByte('"')
}
