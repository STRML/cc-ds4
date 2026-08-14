package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// goldenCase is one frozen (corpus case, profile) rewrite result.
type goldenCase struct {
	Case    string `json:"case"`
	Profile string `json:"profile"`
	Body    string `json:"body"`
	Want    string `json:"want"`
	Note    string `json:"note"`
}

// TestRewriteMatchesPythonGolden replays what the Python proxy actually
// emitted, byte for byte.
//
// This proxy replaced a Python implementation, and while both existed a
// differential harness ran the same corpus through each and compared them. The
// oracle went away with the implementation. testdata/rewrite_golden.json holds
// what that oracle produced on its last run, so the assertions outlived the
// thing that justified them. Each entry is one (case, profile) with the request
// body in and the exact wire bytes out.
//
// tests/diff/dump_golden.py is the recipe that wrote it. It cannot run against
// this tree, since it imports the proxy that was deleted; regenerating means
// restoring src/proxy.py from history first. It is kept because a golden whose
// provenance is unreproducible is a golden nobody can ever justify changing.
//
// Scope, precisely: the Python tree it was dumped from already carried the
// sentinel rename, so this pins Go against Python's rewrite SEMANTICS —
// key order, number formatting, escaping, where reasoning_effort lands — not
// against a build that shipped to users under these names. The byte-level
// serialization is the part that had two independent implementations and is
// what a regression here would be about.
//
// A failure is therefore not automatically a bug, but it does mean the emitted
// bytes changed. That deserves a deliberate decision and a regenerated golden,
// not a quiet edit to make the test pass.
func TestRewriteMatchesPythonGolden(t *testing.T) {
	// Hermetic: profiles.All() expands "~" against HOME, and rewrite consults
	// <profile dir>/effort-override. Against a real home this test fails for
	// anyone who has used /ds4-effort, reporting a drift from Python that is
	// really just the developer's own pinned effort level.
	installProfiles(t, "direct", "openrouter", "nous")

	raw, err := os.ReadFile(filepath.Join("testdata", "rewrite_golden.json"))
	if err != nil {
		t.Fatalf("golden missing: %v", err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("golden unreadable: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("golden is empty; it would assert nothing")
	}

	byName := map[string]profiles.Profile{}
	for _, p := range profiles.All() {
		byName[p.Name] = p
	}

	for _, c := range cases {
		cfg, ok := byName[c.Profile]
		if !ok {
			t.Errorf("%s/%s: unknown profile", c.Case, c.Profile)
			continue
		}
		got, err := rewrite([]byte(c.Body), cfg)
		if err != nil {
			t.Errorf("%s/%s: rewrite: %v", c.Case, c.Profile, err)
			continue
		}
		if string(got) != c.Want {
			t.Errorf("%s/%s drifted from the Python original\n got: %s\nwant: %s",
				c.Case, c.Profile, got, c.Want)
		}
	}
}
