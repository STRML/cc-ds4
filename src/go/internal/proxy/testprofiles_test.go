package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// Fixtures for the three real profiles. Tests used to build Profile literals
// inline, which stopped working once a sentinel needed FamilyModels and Effort
// to resolve: an incomplete literal does not fail, it silently forwards the
// sentinel to the upstream. Building fixtures here means a new field is added
// in one place and every test sees it.

const (
	nousModel   = "deepseek/deepseek-v4-flash-0731"
	orModel     = "deepseek/deepseek-v4-flash-0731:nitro"
	directPro   = "deepseek-v4-pro"
	directFlash = "deepseek-v4-flash"
)

// testNous mirrors the nous row: both families on flash, effort honored.
func testNous() profiles.Profile {
	return profiles.Profile{
		Name:   "nous",
		Model:  nousModel,
		Effort: true,
		FamilyModels: map[string]string{
			"pro":   nousModel,
			"flash": nousModel,
		},
		MaxOut:   65536,
		Failover: "openrouter",
	}
}

// testOpenRouter mirrors the or-ds4 row: ZDR on, :nitro ids, pro-0813 skipped
// for ZDR because its only host rejects the block.
func testOpenRouter() profiles.Profile {
	return profiles.Profile{
		Name:   "openrouter",
		Model:  orModel,
		Effort: true,
		FamilyModels: map[string]string{
			"pro":   orModel,
			"flash": orModel,
		},
		ZDR:           true,
		ZDRSkipModels: []string{"deepseek/deepseek-v4-pro-0813"},
		MaxOut:        65536,
	}
}

// testDirect mirrors the direct row: bare model ids, no effort injection, and
// the only profile where the pro family serves pro.
func testDirect() profiles.Profile {
	return profiles.Profile{
		Name:   "direct",
		Model:  "",
		Effort: false,
		FamilyModels: map[string]string{
			"pro":   directPro,
			"flash": directFlash,
		},
		MaxOut: 65536,
		Inject: true,
	}
}

// withUpstream returns cfg pointed at url, for tests that stand up a fake.
func withUpstream(cfg profiles.Profile, url string) profiles.Profile {
	cfg.Upstream = url
	return cfg
}

// installProfiles points HOME at a scratch directory and creates the named
// profile dirs inside it.
//
// profiles.All() expands "~" against HOME, and several code paths now check
// that a profile's directory exists before using it (failover will not route
// to an uninstalled target, and the or-ds4 classifier route will not either).
// Without this a test reads whatever profiles happen to be installed on the
// machine running it: green here, red on a fresh checkout or in CI, and
// sensitive to the developer's own /ds4-effort state.
func installProfiles(t *testing.T, names ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dirs := map[string]string{
		"direct":     ".claude-ds4",
		"openrouter": ".claude-or-ds4",
		"nous":       ".claude-nous",
	}
	for _, n := range names {
		d, ok := dirs[n]
		if !ok {
			t.Fatalf("unknown profile %q", n)
		}
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}
