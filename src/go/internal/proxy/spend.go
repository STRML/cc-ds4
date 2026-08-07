package proxy

import (
	"encoding/json"
	"io"
	"net/http"
)

// spend serves GET /__spend for one profile: 200 with a JSON shape when the
// profile has Spend enabled, otherwise 404 matching the not-found contract
// (mirroring Python's do_GET/__spend gate).
//
// Phase A ships status + JSON shape only — NOT byte parity. The real handler
// is stateful filesystem/time logic (usage ledger, remaining budget); detailed
// pricing is a separate Go unit-tested implementation, later. Here we return
// the shape the statusline reads (remaining/usage/model/profile).
func (h *Handler) spend(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Spend {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error": {"message": "not found"}}`)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Shape-parity with Python's spend() (proxy.py): the base body is always
	// {"model", "zdr"}; pricing/remaining are added only when the upstream
	// credits/pricing calls resolve (which they 404 against the harness fake).
	out := map[string]any{
		"model": h.cfg.Model,
		"zdr":   h.cfg.ZDR,
	}
	json.NewEncoder(w).Encode(out)
}
