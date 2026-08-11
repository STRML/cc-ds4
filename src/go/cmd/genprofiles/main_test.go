package main

import (
	"strings"
	"testing"
)

// TestRequireKeys pins the generator's format-fragility guard: a profile body
// missing a required key must fail loudly instead of emitting a zero value.
// The generator's value regexes are exact-format dependent, so a key renamed
// or removed in proxy.py would otherwise read as 0/""/false and ship a wrong
// table. Keys whose value is None (model/failover/max_out) legitimately match
// no value regexp and must NOT trip the guard.
func TestRequireKeys(t *testing.T) {
	good := `        "port": 31500,
        "dir": f"{HOME}/.claude-ds4",
        "upstream": "https://api.deepseek.com/anthropic",
        "model": None,
        "zdr": False,
        "spend": False,
        "max_out": 65536,
        "inject": True,
        "failover": None,
`
	if err := requireKeys("direct", good); err != nil {
		t.Fatalf("well-formed body rejected: %v", err)
	}

	for _, missing := range []string{"port", "dir", "upstream", "zdr", "spend", "inject"} {
		// Keys are quoted in the table, so rename the quoted key ("port" ->
		// "portx"); an unquoted "port:" never occurs and would be a silent no-op.
		bad := strings.Replace(good, `"`+missing+`"`, `"`+missing+`x"`, 1)
		if err := requireKeys("direct", bad); err == nil {
			t.Errorf("body missing %q accepted", missing)
		}
	}

	// Type mismatches: a key present but with the WRONG type must be caught, or
	// it would emit a silent zero value (e.g. "port": "31500" as a string ->
	// intValue returns 0 -> the direct profile loses its max_tokens clamp).
	for _, typed := range []struct{ key, wrong string }{
		{"port", `"port": "31500"`},       // string where int belongs
		{"zdr", `"zdr": "False"`},          // string where bool belongs
		{"inject", `"inject": 1`},          // int where bool belongs
		{"dir", `"dir": 123`},              // int where str belongs
		{"max_out", `"max_out": "65536"`},  // string where int belongs (present-but-wrong)
	} {
		// Replace the key's correct-typed line with the wrong-typed one.
		bad := strings.Replace(good, `"`+typed.key+`":`, typed.wrong+`,`, 1)
		if err := requireKeys("direct", bad); err == nil {
			t.Errorf("type-mismatch for %q accepted (would emit zero value)", typed.key)
		}
	}

	// max_out may legitimately be absent/None (Python's clamp is truthiness-
	// gated, proxy.py:499), so a body WITHOUT max_out must still pass.
	noMaxOut := strings.Replace(good, `        "max_out": 65536,
`, "", 1)
	if err := requireKeys("direct", noMaxOut); err != nil {
		t.Fatalf("max_out absent/None should be accepted, got %v", err)
	}
}

// TestEnsureProfiles pins the coverage guard: the parse must yield EXACTLY the
// expected profile set. A zero-match parse (indent reformat), a partial parse
// (one profile's closing brace reformatted), or an unexpected extra profile are
// all bugs that would silently ship a wrong generated table.
func TestEnsureProfiles(t *testing.T) {
	// FindAllSubmatch returns [fullMatch, name] per match (capture group 1 is
	// the profile name); mirror that shape so ensureProfiles reads m[1] as name.
	full := func() [][][]byte {
		out := make([][][]byte, 0, len(expectedProfiles))
		for _, name := range expectedProfiles {
			out = append(out, [][]byte{[]byte("fullmatch"), []byte(name)})
		}
		return out
	}

	if err := ensureProfiles(full()); err != nil {
		t.Fatalf("complete set rejected: %v", err)
	}
	if err := ensureProfiles(nil); err == nil {
		t.Fatal("zero matches accepted — would silently write an empty table")
	}
	if err := ensureProfiles(full()[:len(full())-1]); err == nil {
		t.Fatal("partial match (one profile dropped) accepted — would silently omit it")
	}
	if err := ensureProfiles(append(full(), [][][]byte{{[]byte("fullmatch"), []byte("extra")}}...)); err == nil {
		t.Fatal("unexpected profile accepted")
	}
}
