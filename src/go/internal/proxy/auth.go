package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// authOK checks a POST's Authorization header against the profile's expected
// server-side key. It is constant-time on the full expected string so a
// timing side channel cannot leak the key length, and it runs only after the
// method check in ServeHTTP (GET /__spend is deliberately unauthenticated).
func authOK(r *http.Request, cfg profiles.Profile) bool {
	supplied := r.Header.Get("authorization")
	// DS4_KEY_<NAME> env takes precedence over the profile dir's settings.json.
	// Python's api_key() reads the file first, but the differential harness
	// boots the Go binary against the REAL profile dirs (which hold real keys)
	// while presenting a synthetic client token — so the env override must win
	// there. When the env is unset, fall back to the file like Python.
	expected := os.Getenv("DS4_KEY_" + strings.ToUpper(cfg.Name))
	if expected == "" {
		expected = readKeyFromDir(cfg.Dir)
	}
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte("Bearer "+expected)) == 1
}

// readKeyFromDir mirrors api_key() in proxy.py: the profile key is the
// ANTHROPIC_AUTH_TOKEN from the profile dir's settings.json env block. It is
// a per-profile key, never a process-wide token, so one process serving every
// profile cannot leak one profile's key into another's boundary.
func readKeyFromDir(dir string) string {
	if dir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		Env struct {
			AnthropicAuthToken string `json:"ANTHROPIC_AUTH_TOKEN"`
		} `json:"env"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s.Env.AnthropicAuthToken
}
