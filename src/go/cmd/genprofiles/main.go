// Command genprofiles regenerates internal/profiles/profiles_gen.go from the
// PROFILES table in src/proxy.py, keeping PROFILES the single source of truth
// for profile names and ports (mirroring Python's "Emitting them from here
// keeps PROFILES the only place ports are declared").
//
// Run from the repo root or src/go:
//
//	go run ./cmd/genprofiles
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
)

var (
	// PROFILES block opens at column 0 and closes at the first column-0 "}".
	profilesRE = regexp.MustCompile(`(?sm)^PROFILES = \{(.*?)^\}`)
	// One profile entry: '"name": {' ... closing '    },' (4-space indent).
	profileRE = regexp.MustCompile(`(?sm)"(\w+)": \{(.*?)^    \}`)

	// Per-field parsers, applied within one profile's body. A key whose value
	// is None ("model": None, "failover": None, "max_out": None) never matches
	// any of these, so it reads as the zero value — which is what we emit.
	strFieldRE  = regexp.MustCompile(`(?m)^\s*"(\w+)": f?"([^"]*)"`)
	intFieldRE  = regexp.MustCompile(`(?m)^\s*"(\w+)": (\d+)`)
	boolFieldRE = regexp.MustCompile(`(?m)^\s*"(\w+)": (True|False)`)
	// dir values arrive as f"{HOME}/.claude-ds4"; keep them as "~/.claude-ds4"
	// and expand the "~" at runtime in All().
	homePrefixRE = regexp.MustCompile(`^\{HOME\}/`)
)

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "genprofiles:", err)
	os.Exit(1)
}

// findRepoRoot walks up from cwd until it finds src/proxy.py.
func findRepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "src", "proxy.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	fatal(fmt.Errorf("src/proxy.py not found from %s up to filesystem root", dir))
	return ""
}

// strValue returns the string value of one key in a profile body, or "" when
// the key is absent or None (None matches none of the value regexps).
func strValue(body, key string) string {
	for _, m := range strFieldRE.FindAllStringSubmatch(body, -1) {
		if m[1] == key {
			return m[2]
		}
	}
	return ""
}

// intValue returns the int value of one key, or 0 when absent/None.
func intValue(body, key string) int {
	for _, m := range intFieldRE.FindAllStringSubmatch(body, -1) {
		if m[1] == key {
			v, _ := strconv.Atoi(m[2])
			return v
		}
	}
	return 0
}

// boolValue returns the bool value of one key, or false when absent/None.
func boolValue(body, key string) bool {
	for _, m := range boolFieldRE.FindAllStringSubmatch(body, -1) {
		if m[1] == key {
			return m[2] == "True"
		}
	}
	return false
}

// valuePresent reports whether the key matched the specific field regex
// (str/int/bool). The generator's value regexes are type-scoped, so checking
// against one regex asserts the key is present WITH that type — a wrong-type
// value ("port": "31500" as a string) must not satisfy an int key, or the
// emitted zero value would ship silently (review finding, 2026-08-10).
func valuePresent(re *regexp.Regexp, body, key string) bool {
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if m[1] == key {
			return true
		}
	}
	return false
}

// expectedProfiles is the exact set of profile names PROFILES must define. The
// generated table is the single source the proxy serves from (profiles.go's
// All()/Served()), so a profile dropped by a partial parse is silently lost —
// worse than a total parse failure, because the table still looks populated.
// (2026-08-10 review: ensureProfiles only rejected zero matches, letting a
// single reformatted closing brace drop one profile.)
var expectedProfiles = []string{"direct", "openrouter", "nous"}

// ensureProfiles validates the parse produced exactly the expected profiles.
// The profileRE regex is exact-format dependent (4-space closing indent); if
// proxy.py is reformatted and entries stop matching, the generator would
// otherwise silently write an incomplete table. Any mismatch between the
// parsed set and the expected set is a bug, never a valid state.
func ensureProfiles(matches [][][]byte) error {
	if len(matches) == 0 {
		return fmt.Errorf("PROFILES block parsed no profile entries — profileRE no longer matches the table's formatting")
	}
	// A duplicate profile entry would emit two rows with the same name, so
	// All() returns both and ds4-proxy tries to bind the same port twice —
	// one listener fails. Require the count to match the expected set exactly.
	if len(matches) != len(expectedProfiles) {
		return fmt.Errorf("PROFILES block parsed %d entries, expected %d — a duplicate or missing entry", len(matches), len(expectedProfiles))
	}
	got := make(map[string]bool, len(matches))
	for _, m := range matches {
		got[string(m[1])] = true
	}
	for _, want := range expectedProfiles {
		if !got[want] {
			return fmt.Errorf("PROFILES block missing profile %q — profileRE likely no longer matches its entry's formatting", want)
		}
	}
	for name := range got {
		if !slices.Contains(expectedProfiles, name) {
			return fmt.Errorf("PROFILES block has unexpected profile %q — expected %v", name, expectedProfiles)
		}
	}
	return nil
}

// requireKeys is the generator's format-fragility guard. The regexes are
// exact-format dependent; a key renamed, retyped, or removed in proxy.py would
// otherwise read as its zero value (0, "", false) and ship a silently wrong
// table — the exact failure the old generator had no defense against. Each
// required key is validated against its EXPECTED type's regex, so a string
// where an int belongs is caught, not silently zeroed. Keys whose value is None
// (model/failover) are expected to be absent and are not required. max_out is
// optional-but-typed: Python's clamp is gated on truthiness (proxy.py:499
// `if cfg["max_out"] and ...`), so a profile MAY set it to None (no clamp) —
// but a present value must still be an int, never a string.
func requireKeys(name, body string) error {
	// key -> expected field regex (type-scoped). max_out is intentionally NOT
	// in the required list: absent/None is a valid "no clamp" per Python, while
	// a PRESENT max_out is validated by the type check below.
	for _, k := range []struct {
		key string
		re  *regexp.Regexp
	}{
		{"port", intFieldRE},
		{"dir", strFieldRE},
		{"upstream", strFieldRE},
		{"zdr", boolFieldRE},
		{"spend", boolFieldRE},
		{"inject", boolFieldRE},
	} {
		if !valuePresent(k.re, body, k.key) {
			return fmt.Errorf("%s: required key %q not found with expected type in profile body", name, k.key)
		}
	}
	// If max_out IS present, it must be an integer literal or None. A float
	// like 0.5 matches intFieldRE's \d+ prefix but is not a bare int, and
	// would silently emit MaxOut: 0 while Python applies the float clamp.
	if present, nonInt := hasNonIntMaxOut(body); present && nonInt {
		return fmt.Errorf("%s: max_out must be an int or None, got a non-int value", name)
	}
	return nil
}

// maxOutRE matches a max_out RHS: an integer literal ("65536"), None, or
// anything else (a float "0.5", a string, a bool, a negative). The second
// group captures the value body so the guard can insist it is exactly a bare
// non-negative integer. (Negative literals are never legitimate here — they
// would clamp max_tokens to a nonsense value — and intValue would emit 0 for
// them anyway, the same silent-zero bug class.)
var maxOutRE = regexp.MustCompile(`(?m)^\s*"max_out":\s*(None|\d+|.*),?$`)

// hasNonIntMaxOut reports whether max_out is present (first return) and, if so,
// whether it is anything other than None or a bare integer literal (second
// return). A float like 0.5 matches intFieldRE's \d+ PREFIX (the "0"), so a
// plain intRegex check wrongly accepts it — and intValue would then emit
// MaxOut: 0 while Python applies the float clamp. The complete RHS must be
// an integer literal or None.
func hasNonIntMaxOut(body string) (present, nonInt bool) {
	m := maxOutRE.FindStringSubmatch(body)
	if m == nil {
		return false, false
	}
	if m[1] == "None" {
		return true, false
	}
	if m[1] == "" {
		return true, true
	}
	if _, err := strconv.Atoi(m[1]); err == nil {
		return true, false
	}
	return true, true
}

func main() {
	root := findRepoRoot()
	path := filepath.Join(root, "src", "proxy.py")
	src, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}

	block := profilesRE.FindSubmatch(src)
	if block == nil {
		fatal(fmt.Errorf("PROFILES block not found in %s", path))
	}

	var out []byte
	out = append(out, "// Code generated by cmd/genprofiles; DO NOT EDIT.\n"+
		"// Source: src/proxy.py PROFILES table.\n\n"+
		"package profiles\n\n"+
		"var generatedProfiles = []Profile{\n"...)

	matches := profileRE.FindAllSubmatch(block[1], -1)
	if err := ensureProfiles(matches); err != nil {
		fatal(err)
	}
	for _, m := range matches {
		name := string(m[1])
		body := string(m[2])
		if err := requireKeys(name, body); err != nil {
			fatal(err)
		}

		dir := homePrefixRE.ReplaceAllString(strValue(body, "dir"), "~/")

		out = append(out, fmt.Sprintf("\t{Name: %s, Port: %d, Dir: %s, Upstream: %s, Model: %s, ZDR: %t, Spend: %t, Inject: %t, MaxOut: %d, Failover: %s},\n",
			strconv.Quote(name), intValue(body, "port"), strconv.Quote(dir),
			strconv.Quote(strValue(body, "upstream")), strconv.Quote(strValue(body, "model")),
			boolValue(body, "zdr"), boolValue(body, "spend"), boolValue(body, "inject"),
			intValue(body, "max_out"), strconv.Quote(strValue(body, "failover")))...)
	}
	out = append(out, "}\n"...)

	dst := filepath.Join(root, "src", "go", "internal", "profiles", "profiles_gen.go")
	if err := os.WriteFile(dst, out, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("genprofiles: wrote %s\n", dst)
}
