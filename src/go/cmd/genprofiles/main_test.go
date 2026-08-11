package main

import (
	"strings"
	"testing"
)

// TestRequireKeys pins the generator's format-fragility guard: a profile body
// missing a required key must fail loudly instead of emitting a zero value.
// Every key the emitted Profile struct needs must be PRESENT — Python reads
// them with bare key access (cfg["max_out"] raises KeyError on absence) — and
// a present-but-wrong-typed value must be caught, not silently zeroed. Keys
// whose value is None (model/failover/max_out) are legal but must still be
// present.
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

	// Every required key must be present, including the None-valued ones.
	for _, missing := range []string{"port", "dir", "upstream", "zdr", "spend", "inject", "model", "failover", "max_out"} {
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
		{"max_out", `"max_out": "65536"`},  // string where int belongs
		{"model", `"model": 123`},          // int where str-or-None belongs
		{"failover", `"failover": 123`},    // int where str-or-None belongs
	} {
		bad := strings.Replace(good, `"`+typed.key+`":`, typed.wrong+`,`, 1)
		if err := requireKeys("direct", bad); err == nil {
			t.Errorf("type-mismatch for %q accepted (would emit zero value)", typed.key)
		}
	}

	// A float max_out (e.g. 0.5) must be rejected: it matches intFieldRE's
	// \d+ prefix but is not a bare int, and would emit MaxOut: 0 while Python
	// applies the float clamp.
	for _, bad := range []string{
		`"max_out": 0.5`,
		`"max_out": 65536.0`,
		`"max_out": -3`,
		`"max_out": "65536"`,
	} {
		replaced := strings.Replace(good, `        "max_out": 65536,
`, "        "+bad+",\n", 1)
		if err := requireKeys("direct", replaced); err == nil {
			t.Errorf("non-int max_out %q accepted (would emit zero value)", bad)
		}
	}

	// max_out: None remains valid (Python: cfg["max_out"] present but falsy).
	if err := requireKeys("direct", strings.Replace(good, `        "max_out": 65536,
`, `        "max_out": None,
`, 1)); err != nil {
		t.Fatalf("max_out: None should be accepted, got %v", err)
	}
	// model/failover: None remain valid too.
	if err := requireKeys("direct", strings.Replace(good, `        "model": None,
`, `        "model": None,
`, 1)); err != nil {
		t.Fatalf("model: None should be accepted, got %v", err)
	}
}

// TestIntValueUnderscores pins that Python underscore integer literals parse
// correctly: 31_501 == 31501, and the value is not truncated at the prefix.
func TestIntValueUnderscores(t *testing.T) {
	body := `        "port": 31_501,
`
	if got := intValue(body, "port"); got != 31501 {
		t.Fatalf("intValue(31_501) = %d, want 31501 (underscore literal must match Python)", got)
	}
}

// TestProfileREHyphen pins that a hyphenated profile name parses — a valid
// Python key like "staging-profile" must not be silently dropped by the
// generator (the name class is [\w-]+, not \w+).
func TestProfileREHyphen(t *testing.T) {
	block := `PROFILES = {
    "staging-profile": {
        "port": 31600,
        "dir": f"{HOME}/.claude-staging",
        "upstream": "https://example.com",
        "model": None,
        "zdr": False,
        "spend": False,
        "max_out": None,
        "inject": False,
        "failover": None,
    },
}`
	matches := profileRE.FindAllSubmatch([]byte(block), -1)
	if len(matches) != 1 || string(matches[0][1]) != "staging-profile" {
		t.Fatalf("hyphenated profile not parsed: %d matches, want [staging-profile]", len(matches))
	}
}

// TestEnsureProfiles pins the coverage guard: the parse must contain every
// required profile with no duplicates. A zero-match parse (indent reformat), a
// partial parse (one required profile's closing brace reformatted), or a
// duplicate name are bugs that would silently ship a wrong generated table. An
// EXTRA profile is legitimate — the table is the source of truth and new
// profiles must flow through.
func TestEnsureProfiles(t *testing.T) {
	full := func() [][][]byte {
		out := make([][][]byte, 0, len(requiredProfiles))
		for _, name := range requiredProfiles {
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
		t.Fatal("partial match (one required profile dropped) accepted — would silently omit it")
	}
	// Extra profile is fine (source of truth allows growth).
	if err := ensureProfiles(append(full(), [][][]byte{{[]byte("fullmatch"), []byte("new-profile")}}...)); err != nil {
		t.Fatalf("extra legitimate profile rejected: %v", err)
	}
	// Duplicate name: all required names present but one appears twice — the
	// generator would emit two rows with the same name and ds4-proxy would
	// double-bind the port.
	dupe := full()
	dupe[1] = dupe[0]
	if err := ensureProfiles(dupe); err == nil {
		t.Fatal("duplicate profile accepted — would emit two rows with one port")
	}
}
