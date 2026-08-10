package profiles

import (
	"testing"
)

// TestAllNames asserts the generated table holds exactly the three profiles,
// so a generator regression that drops or renames one fails here instead of
// only surfacing as a missing line in --ports output. All() is asserted rather
// than Ports()/Served() because the latter filter on config-directory
// existence, which varies by host (CI has none).
func TestAllNames(t *testing.T) {
	got := make(map[string]bool)
	for _, p := range All() {
		got[p.Name] = true
	}
	want := map[string]bool{"direct": true, "openrouter": true, "nous": true}
	for name := range want {
		if !got[name] {
			t.Errorf("All() missing profile %q", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("All() has unexpected profile %q", name)
		}
	}
}

func TestPortsMatchesPython(t *testing.T) {
	// Assert the three static ports directly from the table, independent of
	// Served()'s config-directory filtering, which varies by host.
	for _, p := range All() {
		switch p.Name {
		case "direct":
			if p.Port != 31500 {
				t.Fatalf("direct port = %d", p.Port)
			}
		case "openrouter":
			if p.Port != 31501 {
				t.Fatalf("openrouter port = %d", p.Port)
			}
		case "nous":
			if p.Port != 31502 {
				t.Fatalf("nous port = %d", p.Port)
			}
		}
	}
}

// TestPortsEnvOverride pins the DS4_PORT_<NAME> override that src/proxy.py
// honors: the environment value (name uppercased, int-parsed) wins over the
// static table port. Asserted at the effectivePort level, not through Ports(),
// because Ports() filters via Served() (config-dir existence), which varies by
// host — the same reason the sibling tests assert against All().
func TestPortsEnvOverride(t *testing.T) {
	t.Setenv("DS4_PORT_DIRECT", "31999")
	for _, p := range All() {
		if p.Name == "direct" {
			if got := effectivePort(p); got != 31999 {
				t.Fatalf("effectivePort(direct) = %d, want 31999", got)
			}
		}
	}
}

// TestEffectivePortJunkOverride pins the Go-specific fallback: a non-numeric
// override must not crash or leak into the plist; it falls back to the table
// port (Python would crash on this, Go does not).
func TestEffectivePortJunkOverride(t *testing.T) {
	t.Setenv("DS4_PORT_NOUS", "not-a-port")
	for _, p := range All() {
		if p.Name == "nous" {
			if got := effectivePort(p); got != 31502 {
				t.Fatalf("effectivePort(nous) = %d, want 31502", got)
			}
		}
	}
}
