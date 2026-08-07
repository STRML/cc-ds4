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
