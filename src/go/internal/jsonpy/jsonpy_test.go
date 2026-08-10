package jsonpy

import "testing"

func TestMarshalMatchesPythonDumps(t *testing.T) {
	cases := []struct{ in, want string }{
		// non-ASCII escaping (ensure_ascii), order preserved, integral float.
		// Expected output verified against real CPython:
		//   python3 -c "import json; print(json.dumps({'café':1.0,'emoji':'😀','html':'<>&'}, ensure_ascii=True))"
		//   -> {"caf\u00e9": 1.0, "emoji": "\ud83d\ude00", "html": "<>&"}
		{`{"café": 1.0, "emoji": "😀", "html": "<>&"}`, `{"caf\u00e9": 1.0, "emoji": "\ud83d\ude00", "html": "<>&"}`},
		// big int preserved (UseNumber), not float64-mangled
		{`{"big": 9007199254740993}`, `{"big": 9007199254740993}`},
		// Python separators: ", " and ": "
		{`{"a": 1, "b": [2, 3]}`, `{"a": 1, "b": [2, 3]}`},
		// astral runes -> UTF-16 surrogate pairs (CPython ensure_ascii).
		// Verified: python3 -c "import json; print(json.dumps({'c':'🦀'}, ensure_ascii=True))"
		{`{"c": "\ud83e\udd80"}`, `{"c": "\ud83e\udd80"}`},
		// BMP non-ASCII and U+2028/U+2029 (CPython escapes these too).
		{`{"c": "\u00e9\u4e2d\u6587\u2028\u2029"}`, `{"c": "\u00e9\u4e2d\u6587\u2028\u2029"}`},
		// negative big int and exponent spelling preserved verbatim (UseNumber).
		{`{"n": -9007199254740993, "e": 1e100}`, `{"n": -9007199254740993, "e": 1e100}`},
		// integral float keeps the trailing .0 (Python float repr), not Go's 1.
		{`{"f": 1.0, "z": -0.0}`, `{"f": 1.0, "z": -0.0}`},
		// empty string stays a string even after an empty-string parse (isStr).
		{`{"s": "", "o": {}, "a": []}`, `{"s": "", "o": {}, "a": []}`},
		// characters CPython escapes beyond ensure_ascii: quote, backslash,
		// short-escape control chars, and \uXXXX for the rest (incl. DEL).
		// The JSON-escaped input round-trips to identical output.
		{`{"s": "a\"b\\c\nd\te\u0000f\u007fg"}`, `{"s": "a\"b\\c\nd\te\u0000f\u007fg"}`},
	}
	for _, c := range cases {
		got, err := Marshal([]byte(c.in), nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestMarshalMutatesTree(t *testing.T) {
	// Task 4 consumes Marshal via the exported OrderedValue API;
	// pin the mutation helpers now so a signature change fails here.
	root, err := parseOrdered([]byte(`{"model": "claude-sonnet-4-20250514", "max_tokens": 4096}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := root.Get("model").String(); got != "claude-sonnet-4-20250514" {
		t.Errorf("Get(model).String() = %s", got)
	}
	root.SetString("model", "claude-opus-4-20250514")
	if got := root.Get("model").String(); got != "claude-opus-4-20250514" {
		t.Errorf("SetString: Get(model).String() = %s", got)
	}
	root.SetInt("max_tokens", 8192)
	if got := root.Get("max_tokens").String(); got != "8192" {
		t.Errorf("SetInt: Get(max_tokens).String() = %s", got)
	}
	got, err := Marshal([]byte(`{"model": "claude-sonnet-4-20250514", "max_tokens": 4096}`), func(r *OrderedValue) {
		r.SetString("model", "claude-opus-4-20250514")
		r.SetInt("max_tokens", 8192)
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"model": "claude-opus-4-20250514", "max_tokens": 8192}` {
		t.Errorf("Marshal rewrite = %s", got)
	}
}
