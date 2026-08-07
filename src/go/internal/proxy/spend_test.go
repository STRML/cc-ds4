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
// shape matches Python's spend() base (model/zdr). Real pricing/ledger is a
// separate, Go unit-tested implementation (later).
func TestSpendShape(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous", Model: "deepseek/deepseek-v4-flash-0731", ZDR: true, Spend: true}, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__spend", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	// shape: Python's spend() always emits model + zdr.
	if _, ok := m["model"]; !ok {
		t.Errorf("missing 'model' in %s", rec.Body.String())
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

// TestSpendNonSpendPath404 pins the GET-routing parity: a GET to any path
// other than /__spend 404s even on a Spend profile, exactly as Python's
// do_GET does (`if self.path != "/__spend"`). This is the byte-parity edge
// the statusline-only /__spend client never exercises but a misbehaving
// client would.
func TestSpendNonSpendPath404(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous", Spend: true}, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/other/path", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for non-/__spend GET", rec.Code)
	}
}
