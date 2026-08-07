package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadKeyFromDir pins the settings.json credential read that mirrors
// Python's api_key() (proxy.py:505-512): the key is env.ANTHROPIC_AUTH_TOKEN
// inside the profile dir's settings.json. Empty dir or missing key -> "".
func TestReadKeyFromDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"env": {"ANTHROPIC_AUTH_TOKEN": "sk-test-123"}}`), 0600)
	if got := readKeyFromDir(dir); got != "sk-test-123" {
		t.Fatalf("readKeyFromDir = %q, want sk-test-123", got)
	}

	// A dir with settings.json but no key -> empty.
	noKey := t.TempDir()
	os.WriteFile(filepath.Join(noKey, "settings.json"), []byte(`{"env": {}}`), 0600)
	if got := readKeyFromDir(noKey); got != "" {
		t.Fatalf("readKeyFromDir(no key) = %q, want empty", got)
	}

	// A dir with no settings.json -> empty.
	if got := readKeyFromDir(t.TempDir()); got != "" {
		t.Fatalf("readKeyFromDir(missing) = %q, want empty", got)
	}

	// Empty dir string -> empty (no path to read).
	if got := readKeyFromDir(""); got != "" {
		t.Fatalf("readKeyFromDir(\"\") = %q, want empty", got)
	}
}
