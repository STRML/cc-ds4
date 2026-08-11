package main

import (
	"strings"
	"testing"
)

// TestRequireKeys pins the generator's format-fragility guard: a profile body
// missing a required key must fail loudly instead of emitting a zero value.
// The generator's value regexes are exact-format dependent, so a key renamed
// or removed in proxy.py would otherwise read as 0/""/false and ship a wrong
// table. Keys whose value is None (model/failover) legitimately match no value
// regexp and must NOT trip the guard.
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

	for _, missing := range []string{"port", "dir", "upstream", "zdr", "spend", "inject", "max_out"} {
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
		{"port", `"port": "31500"`},     // string where int belongs
		{"max_out", `"max_out": "65536"`}, // string where int belongs
		{"zdr", `"zdr": "False"`},        // string where bool belongs
		{"inject", `"inject": 1`},        // int where bool belongs
		{"dir", `"dir": 123`},            // int where str belongs
	} {
		// Replace the key's correct-typed line with the wrong-typed one.
		bad := strings.Replace(good, `"`+typed.key+`":`, typed.wrong+`,`, 1)
		if err := requireKeys("direct", bad); err == nil {
			t.Errorf("type-mismatch for %q accepted (would emit zero value)", typed.key)
		}
	}
}

// TestEnsureProfiles pins the zero-parse guard: if the PROFILES block yields no
// profile entries (e.g. proxy.py's indent changes and profileRE stops
// matching), the generator must fail loudly rather than write an empty table.
func TestEnsureProfiles(t *testing.T) {
	if err := ensureProfiles([][][]byte{{[]byte("x")}}); err != nil {
		t.Fatalf("non-empty match rejected: %v", err)
	}
	if err := ensureProfiles(nil); err == nil {
		t.Fatal("zero matches accepted — would silently write an empty table")
	}
}
