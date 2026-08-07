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
