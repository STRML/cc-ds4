package jsonpy

import "testing"

// The expectations here were generated from CPython's json.dumps with its
// defaults, which is the output this codec exists to reproduce. They are not
// hand-written: a hand-written escape table is exactly how a golden gets
// frozen wrong, and this session already had one do so.

func TestNumberFormattingMatchesCPython(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{`int_small`, `{"v": 42}`, `{"v": 42}`},
		{`int_beyond_2_53`, `{"v": 9007199254740993}`, `{"v": 9007199254740993}`},
		{`int_negative`, `{"v": -17}`, `{"v": -17}`},
		{`float_integral`, `{"v": 1.0}`, `{"v": 1.0}`},
		{`float_tiny_exponent`, `{"v": 1.5e-07}`, `{"v": 1.5e-07}`},
		{`float_huge_exponent`, `{"v": 1e+100}`, `{"v": 1e+100}`},
		{`float_negative_exponent`, `{"v": -2.5e-13}`, `{"v": -2.5e-13}`},
		{`float_plain`, `{"v": 0.1}`, `{"v": 0.1}`},
		{`float_repr_edge`, `{"v": 0.30000000000000004}`, `{"v": 0.30000000000000004}`},
		{`zero`, `{"v": 0}`, `{"v": 0}`},
		{`float_zero`, `{"v": 0.0}`, `{"v": 0.0}`},
	}
	runParity(t, cases)
}

// Inputs here carry the raw character; CPython escapes it on output because
// ensure_ascii defaults to True. That asymmetry is the whole point.
func TestStringEscapingMatchesCPython(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{`ascii`, `{"v": "plain"}`, `{"v": "plain"}`},
		{`latin1`, `{"v": "café"}`, `{"v": "caf\u00e9"}`},
		{`cjk`, `{"v": "日本語"}`, `{"v": "\u65e5\u672c\u8a9e"}`},
		{`emoji_surrogate_pair`, `{"v": "😀"}`, `{"v": "\ud83d\ude00"}`},
		{`quote`, `{"v": "he said \"hi\""}`, `{"v": "he said \"hi\""}`},
		{`backslash`, `{"v": "a\\b"}`, `{"v": "a\\b"}`},
		{`forward_slash`, `{"v": "a/b"}`, `{"v": "a/b"}`},
		{`newline_tab`, `{"v": "a\nb\tc"}`, `{"v": "a\nb\tc"}`},
		{`control_char`, `{"v": "a\u0001b"}`, `{"v": "a\u0001b"}`},
		{`html_chars`, `{"v": "<>&"}`, `{"v": "<>&"}`},
		{`mixed`, `{"v": "café 日本語 😀 <>& 1.0"}`, `{"v": "caf\u00e9 \u65e5\u672c\u8a9e \ud83d\ude00 <>& 1.0"}`},
	}
	runParity(t, cases)
}

func runParity(t *testing.T, cases []struct{ name, in, want string }) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Marshal([]byte(c.in), func(*OrderedValue) {})
			if err != nil {
				t.Fatalf("Marshal(%s): %v", c.in, err)
			}
			if string(got) != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestUntouchedRoundTripIsByteIdentical is the property the whole codec exists
// for: parsing and re-emitting without a mutation must return the input
// unchanged, including spacing and key order. Anything else silently rewrites
// bodies the proxy was only supposed to pass through.
func TestUntouchedRoundTrip(t *testing.T) {
	for _, in := range []string{
		`{"a": 1, "b": [1, 2, {"c": null}], "d": {"e": true}}`,
		`{}`,
		`{"empty_obj": {}, "empty_arr": []}`,
		`{"deep": {"a": {"b": {"c": {"d": [1, [2, [3]]]}}}}}`,
		`{"model": "x", "messages": [{"role": "user", "content": "hi"}]}`,
	} {
		got, err := Marshal([]byte(in), func(*OrderedValue) {})
		if err != nil {
			t.Errorf("Marshal(%s): %v", in, err)
			continue
		}
		if string(got) != in {
			t.Errorf("round trip changed bytes\n got %s\nwant %s", got, in)
		}
	}
}

// TestSetUpdatesInPlaceAndAppends pins the ordering rule the proxy depends on.
// rewrite() replaces "model" and adds "reasoning_effort"; the replacement must
// stay where it was and the addition must land at the end, because that is
// where Python's dict insertion order puts it.
func TestSetUpdatesInPlaceAndAppends(t *testing.T) {
	in := `{"model": "old", "max_tokens": 5, "messages": []}`
	got, err := Marshal([]byte(in), func(root *OrderedValue) {
		root.SetString("model", "new")
		root.Set("reasoning_effort", Val("high"))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "new", "max_tokens": 5, "messages": [], "reasoning_effort": "high"}`
	if string(got) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

// TestMalformedInputIsRejected pins that bad JSON produces an error rather
// than a silently truncated or invented body. The proxy forwards whatever
// Marshal returns, so "succeeded with garbage" would reach the upstream.
func TestMalformedInputIsRejected(t *testing.T) {
	for _, in := range []string{
		`{"a": 1`,
		`[1, 2`,
		`{"a": 1,}`,
		`{a: 1}`,
		`{'a': 1}`,
		`{"a": 1} trailing`,
		`{"a": NaN}`,
		`{"a": 01}`,
		``,
		`   `,
	} {
		if _, err := Marshal([]byte(in), func(*OrderedValue) {}); err == nil {
			t.Errorf("Marshal(%q) succeeded; malformed input must error", in)
		}
	}
}

// TestDuplicateKeysCollapseLikeCPython pins what used to be a documented
// divergence and turned out to be a crash.
//
// CPython builds a dict, so a repeated key keeps its FIRST position and its
// LAST value: json.dumps(json.loads('{"a":1,"b":2,"a":3}')) is {"a": 3, "b": 2}.
// This codec used to keep both occurrences and emit the key twice. The old test
// called that "a known divergence rather than a desired behavior" and reasoned
// it was unreachable because nothing Claude Code sends carries duplicate keys.
//
// It was reachable, and it was worse than a wrong byte. Keys held the duplicate
// while vals held one entry, so Delete removed the entry and left a stale key
// behind, and emit then dereferenced the missing value and panicked. Both
// classifier routes rebuild a client-supplied body by deleting every key, so
// any request with a duplicate key crashed its connection — and a request that
// had claimed the failover trial took trialActive down with it, leaving a
// circuit that could never close again.
func TestDuplicateKeysCollapseLikeCPython(t *testing.T) {
	got, err := Marshal([]byte(`{"a": 1, "b": 2, "a": 3}`), func(*OrderedValue) {})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what python3 -c 'import json; print(json.dumps(json.loads(...)))'
	// prints for the same input.
	if want := `{"a": 3, "b": 2}`; string(got) != want {
		t.Errorf("duplicate keys\n got %s\nwant %s", got, want)
	}
}

// TestDeletingADuplicateKeyDoesNotPanic is the crash itself, kept as its own
// case: the parser no longer produces the shape, and this is what proves the
// path that used to reach it is safe.
func TestDeletingADuplicateKeyDoesNotPanic(t *testing.T) {
	got, err := Marshal([]byte(`{"model":"m","provider":{},"provider":{}}`), func(root *OrderedValue) {
		for _, k := range root.Keys() {
			if k != "model" {
				root.Delete(k)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"model": "m"}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestPeekModelMaxTokens covers the cheap pre-parse the classifier detector
// uses. Its ok return is the integer guard: a body with no max_tokens must not
// read as 0, or every such request would look like a classifier call.
func TestPeekModelMaxTokens(t *testing.T) {
	for _, c := range []struct {
		name      string
		in        string
		model     string
		maxTokens int
		ok        bool
	}{
		{"both present", `{"model": "m", "max_tokens": 7}`, "m", 7, true},
		{"no max_tokens", `{"model": "m"}`, "m", 0, false},
		{"no model", `{"max_tokens": 7}`, "", 7, true},
		{"max_tokens is a string", `{"model": "m", "max_tokens": "7"}`, "m", 0, false},
		{"model is not a string", `{"model": 5, "max_tokens": 7}`, "", 7, true},
		{"empty object", `{}`, "", 0, false},
		{"malformed", `{"model":`, "", 0, false},
		{"nested decoys", `{"a": {"model": "inner", "max_tokens": 1}, "model": "outer", "max_tokens": 9}`, "outer", 9, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			model, mt, ok := PeekModelMaxTokens([]byte(c.in))
			if model != c.model || mt != c.maxTokens || ok != c.ok {
				t.Errorf("got (%q, %d, %v), want (%q, %d, %v)",
					model, mt, ok, c.model, c.maxTokens, c.ok)
			}
		})
	}
}
