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

// ensureProfiles fails when the PROFILES block parsed no profile entries. The
// profileRE regex is exact-format dependent (4-space closing indent); if
// proxy.py is reformatted and the regex stops matching, the generator would
// otherwise silently write an empty generatedProfiles table with no guard
// firing. A zero-match parse is always a bug, never a valid empty table.
func ensureProfiles(matches [][][]byte) error {
	if len(matches) == 0 {
		return fmt.Errorf("PROFILES block parsed no profile entries — profileRE no longer matches the table's formatting")
	}
	return nil
}

// requireKeys is the generator's format-fragility guard. The regexes are
// exact-format dependent; a key renamed, retyped, or removed in proxy.py would
// otherwise read as its zero value (0, "", false) and ship a silently wrong
// table — the exact failure the old generator had no defense against. Each
// required key is validated against its EXPECTED type's regex, so a string
// where an int belongs is caught, not silently zeroed. Keys whose value is None
// (model/failover) are expected to be absent and are not required.
func requireKeys(name, body string) error {
	// key -> expected field regex (type-scoped)
	for _, k := range []struct {
		key string
		re  *regexp.Regexp
	}{
		{"port", intFieldRE},
		{"max_out", intFieldRE},
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
	return nil
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
