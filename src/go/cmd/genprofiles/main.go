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
	"strconv"
	"strings"
)

var (
	// PROFILES block opens at column 0 and closes at the first column-0 "}".
	profilesRE = regexp.MustCompile(`(?sm)^PROFILES = \{(.*?)^\}`)
	// One profile entry: '"name": {' ... closing '    },' (4-space indent).
	// The name class is [\w-]+ (not \w+) so hyphenated profile names parse —
	// a valid Python key like "staging-profile" was silently dropped before.
	profileRE = regexp.MustCompile(`(?sm)"([\w-]+)": \{(.*?)^    \}`)

	// Per-field parsers, applied within one profile's body. A key whose value
	// is None ("model": None, "failover": None, "max_out": None) never matches
	// any of these, so it reads as the zero value — which is what we emit.
	// intFieldRE is ANCHORED to the complete RHS and accepts Python's
	// underscore literals (31_501 == 31501); a bare prefix match (31) would
	// silently emit the wrong value. str/bool are ANCHORED to the complete RHS
	// for the same reason: a Python expression like "upstream": "..." + "/api"
	// or "inject": False or True would pass a bare-prefix check yet emit only
	// the first fragment, routing to the wrong endpoint or dropping thinking
	// injection.
	strFieldRE  = regexp.MustCompile(`(?m)^\s*"([\w-]+)": f?"([^"]*)"\s*(,)?$`)
	intFieldRE  = regexp.MustCompile(`(?m)^\s*"([\w-]+)": (\d+(?:_\d+)*)\s*(,)?$`)
	boolFieldRE = regexp.MustCompile(`(?m)^\s*"([\w-]+)": (True|False)\s*(,)?$`)
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

// intValue returns the int value of one key, or 0 when absent/None. Python
// integer literals may carry underscores (31_501 == 31501); strip them before
// Atoi so the emitted value matches Python's int().
func intValue(body, key string) int {
	for _, m := range intFieldRE.FindAllStringSubmatch(body, -1) {
		if m[1] == key {
			v, _ := strconv.Atoi(strings.ReplaceAll(m[2], "_", ""))
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

// requiredProfiles is the MINIMUM set of profile names PROFILES must define.
// The generated table is the single source the proxy serves from (profiles.go's
// All()/Served()), so a profile dropped by a partial parse is silently lost —
// worse than a total parse failure, because the table still looks populated.
// (2026-08-10 review: the guard must not hardcode an EXACT set, or adding a
// legitimately-formed fourth profile to proxy.py would reject regeneration.
// This is the required floor, not the full universe — extra profiles are fine.)
var requiredProfiles = []string{"direct", "openrouter", "nous"}

// ensureProfiles validates the parse produced every required profile and no
// duplicates. The profileRE regex is exact-format dependent (4-space closing
// indent); if proxy.py is reformatted and entries stop matching, the generator
// would otherwise silently write an incomplete table. A missing required
// profile or a duplicate name is a bug; an EXTRA profile is legitimate (the
// table is the source of truth, so new profiles must flow through).
func ensureProfiles(matches [][][]byte) error {
	if len(matches) == 0 {
		return fmt.Errorf("PROFILES block parsed no profile entries — profileRE no longer matches the table's formatting")
	}
	// A duplicate profile entry would emit two rows with the same name, so
	// All() returns both and ds4-proxy tries to bind the same port twice —
	// one listener fails.
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		name := string(m[1])
		if seen[name] {
			return fmt.Errorf("PROFILES block has duplicate profile %q — would emit two rows with one port", name)
		}
		seen[name] = true
	}
	for _, want := range requiredProfiles {
		if !seen[want] {
			return fmt.Errorf("PROFILES block missing required profile %q — profileRE likely no longer matches its entry's formatting", want)
		}
	}
	return nil
}

// requireKeys is the generator's format-fragility guard. The regexes are
// exact-format dependent; a key renamed, retyped, or removed in proxy.py would
// otherwise read as its zero value (0, "", false) and ship a silently wrong
// table — the exact failure the old generator had no defense against.
//
// Every key the emitted Profile struct needs must be PRESENT, because Python
// reads them with bare key access (`cfg["max_out"]` at proxy.py:499 raises
// KeyError on absence, not .get semantics). A key whose value is None is
// legitimate (model/failover/max_out can all be None) and matches no value
// regexp — so presence is checked against the union of all three regexes, and
// TYPE is checked separately for each key. A present-but-wrong-typed value
// must be caught, not silently zeroed.
func requireKeys(name, body string) error {
	// strOrNone accepts a string literal OR None (None matches no value regexp
	// but is a legal value for these keys).
	strOrNone := func(b, key string) bool {
		return valuePresent(strFieldRE, b, key) || valuePresent(noneFieldRE, b, key)
	}
	for _, k := range []struct {
		key      string
		check    func(body string) bool // returns true when the value is well-typed
		validFor string
	}{
		{"port", func(b string) bool { return valuePresent(intFieldRE, b, "port") }, "int"},
		{"dir", func(b string) bool { return valuePresent(strFieldRE, b, "dir") }, "str"},
		{"upstream", func(b string) bool { return valuePresent(strFieldRE, b, "upstream") }, "str"},
		{"zdr", func(b string) bool { return valuePresent(boolFieldRE, b, "zdr") }, "bool"},
		{"spend", func(b string) bool { return valuePresent(boolFieldRE, b, "spend") }, "bool"},
		{"inject", func(b string) bool { return valuePresent(boolFieldRE, b, "inject") }, "bool"},
		{"model", func(b string) bool { return strOrNone(b, "model") }, "str-or-None"},
		{"failover", func(b string) bool { return strOrNone(b, "failover") }, "str-or-None"},
		{"max_out", func(b string) bool { p, nonInt := hasNonIntMaxOut(b); return p && !nonInt }, "int-or-None"},
	} {
		if !anyFieldPresent(body, k.key) {
			return fmt.Errorf("%s: required key %q not found in profile body", name, k.key)
		}
		if !k.check(body) {
			return fmt.Errorf("%s: key %q has an invalid value (expected %s)", name, k.key, k.validFor)
		}
	}
	return nil
}

// maxOutRE matches a max_out RHS: an integer literal ("65536", underscores
// allowed to match Python), None, or anything else (a float "0.5", a string,
// a bool, a negative). The second group captures the value body so the guard
// can insist it is exactly a bare non-negative integer. (Negative literals are
// never legitimate here — they would clamp max_tokens to a nonsense value —
// and intValue would emit 0 for them anyway, the same silent-zero bug class.)
var maxOutRE = regexp.MustCompile(`(?m)^\s*"max_out":\s*(None|\d+(?:_\d+)*|.*),?$`)

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
	if _, err := strconv.Atoi(strings.ReplaceAll(m[1], "_", "")); err == nil {
		return true, false
	}
	return true, true
}

// noneFieldRE matches a key set to None ("model": None). None is a legitimate
// value (the direct profile's model/failover are None), but it matches none of
// the typed value regexes — so a bare presence check would wrongly treat a
// None-valued key as absent.
var noneFieldRE = regexp.MustCompile(`(?m)^\s*"(\w+)":\s*None`)

// anyFieldPresent reports whether the key appears in the body under any value
// form — a typed literal OR None. Python reads every profile key with bare
// access (cfg["max_out"] raises KeyError on absence), so a key must be present
// even when its value is None. This is the presence check; type is checked
// separately per key.
func anyFieldPresent(body, key string) bool {
	for _, re := range []*regexp.Regexp{strFieldRE, intFieldRE, boolFieldRE, noneFieldRE} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if m[1] == key {
				return true
			}
		}
	}
	return false
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
