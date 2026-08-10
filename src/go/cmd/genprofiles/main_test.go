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
}
