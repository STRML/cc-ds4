package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAllNames asserts the table holds exactly the three profiles, so an edit
// that drops or renames one fails here instead of only surfacing as a missing
// line in --ports output. All() is asserted rather than Ports()/Served()
// because those filter on config-directory existence; the tests below drive
// that filtering with a synthetic HOME.
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

// TestFamilyModelsAreDeclaredForEveryProfile pins that every profile can
// resolve both sentinel families. A missing key would fall through to Model,
// which on direct is empty — the sentinel would then reach the upstream
// verbatim and 400.
func TestFamilyModelsAreDeclaredForEveryProfile(t *testing.T) {
	for _, p := range All() {
		for _, fam := range []string{"pro", "flash"} {
			if p.FamilyModels[fam] == "" {
				t.Fatalf("%s: no model for the %q family", p.Name, fam)
			}
		}
	}
}

// TestOnlyDirectServesPro pins the asymmetry that keeps biting: OpenRouter has
// no working host for pro-0813 and Nous lists no deepseek pro at all, so both
// point the pro family at their flash id. Only direct actually serves pro.
func TestOnlyDirectServesPro(t *testing.T) {
	for _, p := range All() {
		pro, flash := p.FamilyModels["pro"], p.FamilyModels["flash"]
		if p.Name == "direct" {
			if pro == flash {
				t.Fatalf("direct: pro and flash resolve to the same id %q", pro)
			}
			continue
		}
		if pro != flash {
			t.Fatalf("%s: pro = %q, flash = %q; neither upstream serves pro",
				p.Name, pro, flash)
		}
	}
}

// TestNousFailsOverToOpenRouter pins the target. Failing over to direct bills
// per-token on api.deepseek.com and is what drained the balance; openrouter's
// flash is the cheap target.
func TestNousFailsOverToOpenRouter(t *testing.T) {
	for _, p := range All() {
		switch p.Name {
		case "nous":
			if p.Failover != "openrouter" {
				t.Fatalf("nous failover = %q, want openrouter", p.Failover)
			}
		default:
			if p.Failover != "" {
				t.Fatalf("%s failover = %q, want none", p.Name, p.Failover)
			}
		}
	}
}

// TestAllReturnsIndependentCopies pins the defensive copy in All(). Profile is
// passed by value, which reads as immutable, but FamilyModels and
// ZDRSkipModels are reference types — a caller writing through either would
// repoint every later request on that profile.
func TestAllReturnsIndependentCopies(t *testing.T) {
	first := All()
	for i := range first {
		first[i].FamilyModels["pro"] = "poisoned"
		first[i].ZDRSkipModels = append(first[i].ZDRSkipModels, "poisoned")
	}
	for _, p := range All() {
		if p.FamilyModels["pro"] == "poisoned" {
			t.Fatalf("%s: FamilyModels leaked a mutation from a prior All()", p.Name)
		}
		for _, s := range p.ZDRSkipModels {
			if s == "poisoned" {
				t.Fatalf("%s: ZDRSkipModels leaked a mutation", p.Name)
			}
		}
	}
}

// TestServedFiltersOnDirectoryExistence pins the rule that decides which ports
// get bound at all. A profile whose config dir is absent is not installed here,
// and binding its port anyway would lie to anyone checking with nc.
func TestServedFiltersOnDirectoryExistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude-nous"), 0o755); err != nil {
		t.Fatal(err)
	}

	served := Served()
	if len(served) != 1 || served[0].Name != "nous" {
		var names []string
		for _, p := range served {
			names = append(names, p.Name)
		}
		t.Fatalf("Served() = %v, want just nous", names)
	}
}

// TestServedEmptyWhenNothingInstalled pins that Served is genuinely filtering
// rather than always returning the table.
func TestServedEmptyWhenNothingInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := Served(); len(got) != 0 {
		t.Fatalf("Served() returned %d profiles with no config dirs present", len(got))
	}
}

// TestPortsRendersTheInstallScriptFormat pins the exact shape install.sh
// parses to build the launchd plist: one "name port" line per served profile.
// A change here writes a malformed plist rather than failing loudly.
func TestPortsRendersTheInstallScriptFormat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{".claude-ds4", ".claude-nous"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := Ports()
	want := "direct 31500\nnous 31502\n"
	if got != want {
		t.Fatalf("Ports() = %q, want %q", got, want)
	}
}

// TestPortsHonorsThePortOverride pins that a DS4_PORT_* override reaches the
// plist. Without it launchd binds the table port while settings.json points at
// the override, and Claude talks to a port nothing listens on.
func TestPortsHonorsThePortOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DS4_PORT_NOUS", "31599")
	if err := os.MkdirAll(filepath.Join(home, ".claude-nous"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := Ports(), "nous 31599\n"; got != want {
		t.Fatalf("Ports() = %q, want %q", got, want)
	}
}

// TestExpandHome covers the tilde forms the table can hold. A bare "~" and a
// "~/..." path expand; anything else is already absolute and must be left
// alone, including a path that merely starts with the same letters.
func TestExpandHome(t *testing.T) {
	for _, tc := range []struct{ in, home, want string }{
		{"~", "/Users/x", "/Users/x"},
		{"~/.claude-nous", "/Users/x", "/Users/x/.claude-nous"},
		{"/absolute/path", "/Users/x", "/absolute/path"},
		{"~notahome/dir", "/Users/x", "~notahome/dir"},
		{"", "/Users/x", ""},
	} {
		if got := expandHome(tc.in, tc.home); got != tc.want {
			t.Errorf("expandHome(%q, %q) = %q, want %q", tc.in, tc.home, got, tc.want)
		}
	}
}
