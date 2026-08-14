package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// modelOf pulls the rewritten model id out of a body, for tests that care
// about the resolution and not the byte layout.
func modelOf(t *testing.T, body []byte) string {
	t.Helper()
	return modelFromJSON(body)
}

// TestSentinelFamilySelectsTheModel pins the family half of the sentinel. On
// direct the two families resolve to different models, which is the only place
// the split is observable.
func TestSentinelFamilySelectsTheModel(t *testing.T) {
	cfg := testDirect()
	cases := map[string]string{
		"ds4-pro-xhigh":    directPro,
		"ds4-pro-medium":   directPro,
		"ds4-flash-xhigh":  directFlash,
		"ds4-flash-medium": directFlash,
	}
	for tier, want := range cases {
		got, err := rewrite([]byte(`{"model": "`+tier+`", "max_tokens": 32000, "messages": []}`), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if m := modelOf(t, got); m != want {
			t.Errorf("%s resolved to %q, want %q", tier, m, want)
		}
	}
}

// TestSentinelEffortSetsTheDefault pins the effort half. The two halves are
// independent: a -medium sentinel must not inherit the -xhigh default.
func TestSentinelEffortSetsTheDefault(t *testing.T) {
	cfg := testOpenRouter()
	cases := map[string]string{
		"ds4-pro-xhigh":    "xhigh",
		"ds4-pro-medium":   "medium",
		"ds4-flash-xhigh":  "xhigh",
		"ds4-flash-medium": "medium",
	}
	for tier, want := range cases {
		got, err := rewrite([]byte(`{"model": "`+tier+`", "max_tokens": 32000, "messages": []}`), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), `"reasoning_effort": "`+want+`"`) {
			t.Errorf("%s: want effort %q, got %s", tier, want, got)
		}
	}
}

// TestClientEffortWinsOverSentinelDefault pins the /effort contract: a
// reasoning_effort already in the body is the user asking for a specific
// thinking budget, and it beats the default the sentinel carries.
func TestClientEffortWinsOverSentinelDefault(t *testing.T) {
	cfg := testOpenRouter()
	body := []byte(`{"model": "ds4-flash-medium", "reasoning_effort": "max", "max_tokens": 32000, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"reasoning_effort": "max"`) {
		t.Errorf("client effort was clobbered: %s", got)
	}
	if strings.Contains(string(got), `"reasoning_effort": "medium"`) {
		t.Errorf("sentinel default leaked in: %s", got)
	}
	if m := modelOf(t, got); m != orModel {
		t.Errorf("model = %q, want %q", m, orModel)
	}
}

// TestInvalidClientEffortFallsBackToDefault pins that a junk level is not
// forwarded. OpenRouter accepts the parameter and DeepSeek drops unknown
// values silently, so a typo has to be caught here or it vanishes.
func TestInvalidClientEffortFallsBackToDefault(t *testing.T) {
	cfg := testOpenRouter()
	body := []byte(`{"model": "ds4-flash-medium", "reasoning_effort": "banana", "max_tokens": 32000, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"reasoning_effort": "medium"`) {
		t.Errorf("want the sentinel default, got %s", got)
	}
	if strings.Contains(string(got), "banana") {
		t.Errorf("invalid level reached the upstream body: %s", got)
	}
}

// TestDirectInjectsNoEffort pins that a profile whose upstream ignores
// reasoning_effort never has one added. api.deepseek.com drops unknown values
// without error, so an injected level would look like it worked.
func TestDirectInjectsNoEffort(t *testing.T) {
	cfg := testDirect()
	got, err := rewrite([]byte(`{"model": "ds4-pro-xhigh", "max_tokens": 32000, "messages": []}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "reasoning_effort") {
		t.Errorf("direct got a reasoning_effort: %s", got)
	}
}

// TestZDRSkipModels pins the per-profile escape hatch: a model with no
// ZDR-capable host gets no provider block, while every other model on the same
// profile keeps one.
func TestZDRSkipModels(t *testing.T) {
	cfg := testOpenRouter()
	cfg.FamilyModels["pro"] = "deepseek/deepseek-v4-pro-0813"

	skipped, err := rewrite([]byte(`{"model": "ds4-pro-xhigh", "max_tokens": 32000, "messages": []}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(skipped), `"provider"`) {
		t.Errorf("pro-0813 got a ZDR block, which its only host rejects: %s", skipped)
	}

	kept, err := rewrite([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 32000, "messages": []}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kept), `"zdr": true`) {
		t.Errorf("flash lost its ZDR block: %s", kept)
	}
}

// TestEffortOverrideFile pins the /ds4-effort pin: a valid level in
// <profile-dir>/effort-override beats the sentinel default, junk reads as
// absent, and a rewrite is picked up without a restart.
func TestEffortOverrideFile(t *testing.T) {
	dir := t.TempDir()
	cfg := testOpenRouter()
	cfg.Dir = dir
	path := filepath.Join(dir, "effort-override")

	// Absent file: the sentinel default stands.
	got, err := rewrite([]byte(`{"model": "ds4-flash-medium", "max_tokens": 32000, "messages": []}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"reasoning_effort": "medium"`) {
		t.Fatalf("no override should leave the default: %s", got)
	}

	write := func(text string) {
		t.Helper()
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		// Atomic replace, matching what the slash command does — and what the
		// cache's identity check has to survive.
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ file, want string }{
		{"high\n", "high"},
		{"  low  \n", "low"},
		{"banana\n", "medium"}, // invalid -> sentinel default
		{"", "medium"},         // empty -> sentinel default
		{"max\n", "max"},       // picked up without a restart
	} {
		write(tc.file)
		got, err := rewrite([]byte(`{"model": "ds4-flash-medium", "max_tokens": 32000, "messages": []}`), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), `"reasoning_effort": "`+tc.want+`"`) {
			t.Errorf("override %q: want %q, got %s", tc.file, tc.want, got)
		}
	}
}

// TestEffortOverrideIsPerProfile pins that the cache is keyed by path. One
// process serves every profile, so a pin on one must not bleed onto another.
func TestEffortOverrideIsPerProfile(t *testing.T) {
	pinned := testOpenRouter()
	pinned.Dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(pinned.Dir, "effort-override"), []byte("low\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := testNous()
	other.Dir = t.TempDir()

	got, err := rewrite([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 32000, "messages": []}`), other)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"reasoning_effort": "xhigh"`) {
		t.Errorf("a pin on another profile leaked in: %s", got)
	}
}

// TestClassifierTiersAreFlashSentinels pins the detector's tier set against
// the sentinel table. A rename that misses classifier.go disables the gate
// silently: the classifier stops being detected and every tool call is judged
// on ds4 instead of the trusted route.
func TestClassifierTiersAreFlashSentinels(t *testing.T) {
	if len(classifierTiers) == 0 {
		t.Fatal("classifierTiers is empty; the gate can never fire")
	}
	for tier := range classifierTiers {
		sen, ok := sentinelTable[tier]
		if !ok {
			t.Errorf("%q is not a sentinel; the detector can never match it", tier)
			continue
		}
		if sen.family != "flash" {
			t.Errorf("%q is family %q; the classifier only rides flash", tier, sen.family)
		}
	}
}
