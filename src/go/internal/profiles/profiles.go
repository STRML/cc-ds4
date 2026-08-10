// Package profiles mirrors the PROFILES table in src/proxy.py: name, port,
// config dir, upstream, and the per-profile rewriting flags. The table values
// are generated into profiles_gen.go; this file holds the types and the
// runtime pieces that resolve them against the host (home dir, directory
// existence).
package profiles

import (
	"os"
	"strconv"
	"strings"
)

// Profile is one entry in the PROFILES table. Python's None values become Go
// zero values: empty strings, false, and 0 for max_out.
type Profile struct {
	Name     string
	Port     int
	Dir      string
	Upstream string
	Model    string
	ZDR      bool
	Spend    bool
	Inject   bool
	MaxOut   int
	Failover string
	// FailoverTarget marks a profile being used as a failover target (its
	// requests get the FAILOVER_MODEL remap, not the profile's own rewrite).
	FailoverTarget bool
}

// All returns every profile with a leading "~" in Dir expanded to the user's
// home directory. src/proxy.py sets HOME = os.path.expanduser("~") once and
// the dir fields embed it via f-strings; we keep "~" in the generated table
// and expand here so the table stays host-independent.
func All() []Profile {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}
	out := make([]Profile, len(generatedProfiles))
	for i, p := range generatedProfiles {
		p.Dir = expandHome(p.Dir, home)
		out[i] = p
	}
	return out
}

func expandHome(dir, home string) string {
	switch {
	case dir == "~":
		return home
	case strings.HasPrefix(dir, "~/"):
		return home + dir[1:]
	default:
		return dir
	}
}

// Served returns the profiles whose config directory exists, mirroring
// served = {n: c for n, c in PROFILES.items() if os.path.isdir(c["dir"])} in
// src/proxy.py main(). An absent directory means that profile is not installed
// here; binding its port anyway would be a lie to anyone checking with nc.
func Served() []Profile {
	all := All()
	out := make([]Profile, 0, len(all))
	for _, p := range all {
		if info, err := os.Stat(p.Dir); err == nil && info.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// Ports renders the served profiles as "name port" lines in table order, the
// format install.sh consumes from `proxy.py --ports`.
func Ports() string {
	var sb strings.Builder
	for _, p := range Served() {
		sb.WriteString(p.Name)
		sb.WriteByte(' ')
		sb.WriteString(strconv.Itoa(effectivePort(p)))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// effectivePort returns the port a profile serves on, honoring a
// DS4_PORT_<NAME> environment override. src/proxy.py resolves it with
// int(os.environ.get(f"DS4_PORT_{name.upper()}", cfg["port"])), so an override
// is the name uppercased and the value must parse as an int. Python would
// crash on a junk value; Go falls back to the table port instead. An empty
// override also falls back, matching Python's default handling.
func effectivePort(p Profile) int {
	raw := os.Getenv("DS4_PORT_" + strings.ToUpper(p.Name))
	if raw == "" {
		return p.Port
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	return p.Port
}
