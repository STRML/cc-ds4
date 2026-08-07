// Package jsonpy is an order-preserving JSON serializer whose output matches
// CPython's json.dumps(..., ensure_ascii=True) with default separators
// (", " and ": ") byte-for-byte.
//
// It exists to re-emit the exact bytes Claude Code sends to the API so the
// proxy can rewrite a subset of fields (see the OrderedValue mutation API)
// without disturbing key order or number spelling.
package jsonpy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// OrderedValue is one JSON value in the tree. Key order is preserved in Keys,
// and numbers keep their raw literal spelling so a big integer does not round
// through float64. Fields are unexported; Task 4 mutates through the exported
// accessors (Get/Set/SetString/SetInt) and package-internal code may touch the
// fields directly.
type OrderedValue struct {
	obj   bool
	arr   bool
	isStr bool
	str   string
	num   string // raw literal (number, "true", "false", "null")
	keys  []string
	vals  map[string]*OrderedValue
	items []*OrderedValue
}

// parseOrdered parses data into an OrderedValue tree, preserving key order and
// raw number literals. json.Number keeps 9007199254740993 intact rather than
// mangling it through float64.
func parseOrdered(data []byte) (*OrderedValue, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return parseValue(dec)
}

func parseValue(dec *json.Decoder) (*OrderedValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &OrderedValue{obj: true, vals: map[string]*OrderedValue{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, fmt.Errorf("jsonpy: expected string object key, got %T", keyTok)
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.keys = append(o.keys, key)
				o.vals[key] = v
			}
			if _, err := dec.Token(); err != nil { // closing '}'
				return nil, err
			}
			return o, nil
		case '[':
			a := &OrderedValue{arr: true}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				a.items = append(a.items, v)
			}
			if _, err := dec.Token(); err != nil { // closing ']'
				return nil, err
			}
			return a, nil
		}
	case string:
		return &OrderedValue{str: t, isStr: true}, nil
	case json.Number:
		return &OrderedValue{num: t.String()}, nil
	case bool:
		b := "false"
		if t {
			b = "true"
		}
		return &OrderedValue{num: b}, nil
	case nil:
		return &OrderedValue{num: "null"}, nil
	}
	return nil, fmt.Errorf("jsonpy: unexpected token %T", tok)
}

// escapeAscii mirrors CPython's string encoder: runes in [0x20, 0x7E] pass
// through except " and \, the JSON control characters use their short escapes,
// and everything else non-ASCII (including DEL) is emitted as \uXXXX with
// astral runes as UTF-16 surrogate pairs.
func escapeAscii(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r < 0x20 || r > 0x7E:
				if r < 0x10000 {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else {
					r1 := 0xD800 + (r-0x10000)>>10
					r2 := 0xDC00 + (r-0x10000)&0x3FF
					fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func (o *OrderedValue) emit(b *bytes.Buffer) {
	switch {
	case o.obj:
		b.WriteByte('{')
		for i, k := range o.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('"')
			b.WriteString(escapeAscii(k))
			b.WriteString(`": `)
			o.vals[k].emit(b)
		}
		b.WriteByte('}')
	case o.arr:
		b.WriteByte('[')
		for i, it := range o.items {
			if i > 0 {
				b.WriteString(", ")
			}
			it.emit(b)
		}
		b.WriteByte(']')
	case o.isStr:
		b.WriteByte('"')
		b.WriteString(escapeAscii(o.str))
		b.WriteByte('"')
	default:
		b.WriteString(o.num)
	}
}

// Marshal parses data order-preserving and re-emits it with Python's
// separators and ensure_ascii escaping. rewrite, when non-nil, is called on the
// root object after parse and before emit; it may mutate the tree through the
// OrderedValue accessors.
func Marshal(data []byte, rewrite func(root *OrderedValue)) ([]byte, error) {
	root, err := parseOrdered(data)
	if err != nil {
		return nil, err
	}
	if rewrite != nil {
		rewrite(root)
	}
	var b bytes.Buffer
	root.emit(&b)
	return b.Bytes(), nil
}

// Get returns the child stored under key, or nil if o is not an object or the
// key is absent.
func (o *OrderedValue) Get(key string) *OrderedValue {
	if o == nil || !o.obj {
		return nil
	}
	return o.vals[key]
}

// Has reports whether key is present on this object.
func (o *OrderedValue) Has(key string) bool {
	if o == nil || !o.obj {
		return false
	}
	_, ok := o.vals[key]
	return ok
}

// IsString reports whether o holds a JSON string.
func (o *OrderedValue) IsString() bool {
	return o != nil && o.isStr
}

// String returns the value's raw text: the string content for a JSON string,
// or the raw literal for a number/bool/null. Objects and arrays return "".
func (o *OrderedValue) String() string {
	if o == nil {
		return ""
	}
	if o.isStr {
		return o.str
	}
	return o.num
}

// Raw re-emits this value as JSON bytes with Python's separators, letting a
// caller compare an entire subtree against a canonical spelling (e.g. the
// {"type": "disabled"} thinking sentinel). The bytes share the Marshal
// encoder, so a value round-trips byte-for-byte. A nil value emits "null",
// which is never equal to any object sentinel — the Python analogue is
// None != DISABLED.
func (o *OrderedValue) Raw() string {
	if o == nil {
		return "null"
	}
	var b bytes.Buffer
	o.emit(&b)
	return b.String()
}

// Set stores v under key, appending key to the object's order if it is new.
func (o *OrderedValue) Set(key string, v *OrderedValue) {
	if o == nil || !o.obj {
		return
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// SetString stores a JSON string under key.
func (o *OrderedValue) SetString(key, val string) {
	o.Set(key, &OrderedValue{str: val, isStr: true})
}

// SetInt stores a JSON number under key using its decimal spelling.
func (o *OrderedValue) SetInt(key string, val int) {
	o.Set(key, &OrderedValue{num: strconv.Itoa(val)})
}

// IsObject reports whether o holds a JSON object.
func (o *OrderedValue) IsObject() bool {
	return o != nil && o.obj
}

// IsArray reports whether o holds a JSON array.
func (o *OrderedValue) IsArray() bool {
	return o != nil && o.arr
}

// Items returns the array's elements, or nil if o is not an array. The slice
// is the array's own backing store; callers may read it but must not append.
func (o *OrderedValue) Items() []*OrderedValue {
	if o == nil || !o.arr {
		return nil
	}
	return o.items
}

// Insert places v at index i in an array, shifting later elements right.
// Indices out of range clamp to the ends; no-op on a non-array.
func (o *OrderedValue) Insert(i int, v *OrderedValue) {
	if o == nil || !o.arr {
		return
	}
	if i < 0 {
		i = 0
	}
	if i > len(o.items) {
		i = len(o.items)
	}
	o.items = append(o.items, nil)
	copy(o.items[i+1:], o.items[i:])
	o.items[i] = v
}

// GetInt returns the integer stored under key, or 0 when key is absent or the
// value is not a JSON integer literal (a string, float, object, or array).
// Mirrors Python's isinstance(want, int) guard in proxy.py rewrite(): only an
// integer literal is a candidate for clamping or thinking-disable.
func (o *OrderedValue) GetInt(key string) int {
	v, ok := o.AsInt(key)
	if !ok {
		return 0
	}
	return v
}

// AsInt returns the integer stored under key and whether the value is a JSON
// integer literal. The second return distinguishes an absent or non-integer
// value (ok=false) from a present integer (ok=true), which GetInt collapses.
func (o *OrderedValue) AsInt(key string) (int, bool) {
	v := o.Get(key)
	if v == nil || v.isStr || v.obj || v.arr {
		return 0, false
	}
	n, err := strconv.ParseInt(v.num, 10, 64)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// SetBefore stores v under key, inserting key immediately before the existing
// key before. When key already exists it is updated in place (position kept);
// when before is absent the key is appended, like Set.
func (o *OrderedValue) SetBefore(key string, v *OrderedValue, before string) {
	if o == nil || !o.obj {
		return
	}
	if _, ok := o.vals[key]; ok {
		o.vals[key] = v
		return
	}
	idx := -1
	for i, k := range o.keys {
		if k == before {
			idx = i
			break
		}
	}
	if idx < 0 {
		o.Set(key, v)
		return
	}
	o.keys = append(o.keys, "")
	copy(o.keys[idx+1:], o.keys[idx:])
	o.keys[idx] = key
	o.vals[key] = v
}

// PeekModelMaxTokens reads the top-level "model" string and "max_tokens"
// integer without re-emitting the body. The second return is whether
// max_tokens was PRESENT as a JSON integer literal — mirroring Python's
// isinstance(payload.get("max_tokens"), int) guard, where an absent key is
// None and therefore not an int. A caller that needs "0 when absent" (e.g.
// rewrite's thinking decision) should use GetInt instead; a caller that needs
// to distinguish "no max_tokens" from "max_tokens: 0" (e.g. the classifier
// detector) needs the presence flag.
func PeekModelMaxTokens(data []byte) (model string, maxTokens int, ok bool) {
	root, err := parseOrdered(data)
	if err != nil {
		return "", 0, false
	}
	if m := root.Get("model"); m != nil && m.IsString() {
		model = m.String()
	}
	mt, present := root.AsInt("max_tokens")
	return model, mt, present
}

// Val converts a Go value into an OrderedValue leaf. Supported types: string,
// bool, int, int64, float64, *OrderedValue (passed through), and nil. Unknown
// types become JSON null.
func Val(v any) *OrderedValue {
	switch t := v.(type) {
	case string:
		return &OrderedValue{str: t, isStr: true}
	case bool:
		if t {
			return &OrderedValue{num: "true"}
		}
		return &OrderedValue{num: "false"}
	case int:
		return &OrderedValue{num: strconv.Itoa(t)}
	case int64:
		return &OrderedValue{num: strconv.FormatInt(t, 10)}
	case float64:
		return &OrderedValue{num: strconv.FormatFloat(t, 'f', -1, 64)}
	case *OrderedValue:
		return t
	case nil:
		return &OrderedValue{num: "null"}
	}
	return &OrderedValue{num: "null"}
}

// MustObj builds an object from alternating key/value pairs, mirroring the
// dict(...) literals in proxy.py. Non-string keys are skipped; values go
// through Val.
func MustObj(pairs ...any) *OrderedValue {
	o := &OrderedValue{obj: true, vals: map[string]*OrderedValue{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			continue
		}
		o.Set(key, Val(pairs[i+1]))
	}
	return o
}

// MustArr builds an array from the given items, each through Val.
func MustArr(items ...any) *OrderedValue {
	a := &OrderedValue{arr: true}
	for _, it := range items {
		a.items = append(a.items, Val(it))
	}
	return a
}
