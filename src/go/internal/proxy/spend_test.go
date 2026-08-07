package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// TestSpendShape pins the Phase A contract for /__spend: status + JSON shape
// only, not byte parity. The GET path must return 200 with a JSON body whose
// shape includes the keys the statusline reads (remaining/usage/model/profile).
// Real pricing/ledger is a separate, Go unit-tested implementation (later).
func TestSpendShape(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous", Model: "deepseek/deepseek-v4-flash-0731", Spend: true}, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__spend", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	// shape: has keys like remaining/usage — assert a known one is present
	if _, ok := m["remaining"]; !ok {
		t.Errorf("missing 'remaining' in %s", rec.Body.String())
	}
}

// TestSpendDisabled404 pins the other side of the Spend gate: a profile with
// Spend unset gets a 404 with an Anthropic-shaped error, matching the
// /__spend "not found" contract for non-spend profiles.
func TestSpendDisabled404(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous"}, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__spend", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if _, ok := m["error"]; !ok {
		t.Errorf("missing 'error' in %s", rec.Body.String())
	}
}
