package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newAuthedHandler returns a handler plus the bearer value a client must send.
func newAuthedHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	t.Setenv("DS4_KEY_NOUS", "client-secret")
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Upstream = "http://unused.invalid"
	return NewHandler(cfg, time.Minute), "Bearer client-secret"
}

// TestServeHTTPRejectsWrongCredential pins the local auth gate. The proxy holds
// real upstream keys, so anything else on the loopback port that can reach it
// must not be able to spend them.
func TestServeHTTPRejectsWrongCredential(t *testing.T) {
	h, good := newAuthedHandler(t)
	for _, tc := range []struct{ name, auth string }{
		{"absent", ""},
		{"wrong value", "Bearer nope"},
		{"missing bearer prefix", "client-secret"},
		{"prefix of the real key", "Bearer client-secre"},
		{"real key plus suffix", "Bearer client-secretX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			if tc.auth != "" {
				req.Header.Set("authorization", tc.auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != 401 {
				t.Errorf("status = %d, want 401", rr.Code)
			}
			if ct := rr.Header().Get("content-type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}
		})
	}
	// The good credential must not 401, or the test above proves nothing.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("authorization", good)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 401 {
		t.Fatal("the correct credential was rejected; the 401 cases above are meaningless")
	}
}

// TestServeHTTPMethodHandling pins the method surface: POST relays, GET is
// only /__spend, everything else is 501.
func TestServeHTTPMethodHandling(t *testing.T) {
	h, _ := newAuthedHandler(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead} {
		req := httptest.NewRequest(m, "/v1/messages", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 501 {
			t.Errorf("%s: status = %d, want 501", m, rr.Code)
		}
	}
}

// TestServeHTTPGetOnlyServesSpend pins that GET does not become an open
// read surface: any path other than /__spend is a 404, not a relay.
func TestServeHTTPGetOnlyServesSpend(t *testing.T) {
	h, _ := newAuthedHandler(t)
	for _, path := range []string{"/", "/v1/messages", "/__spend/../etc", "/__spendx"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 404 {
			t.Errorf("GET %s: status = %d, want 404", path, rr.Code)
		}
	}
}

// TestSpendNeedsNoClientCredential pins that /__spend is readable without the
// client key. The status line renders on every prompt and does not carry one;
// requiring it would blank the cost display rather than fail loudly.
func TestSpendNeedsNoClientCredential(t *testing.T) {
	h, _ := newAuthedHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/__spend", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 401 {
		t.Fatal("/__spend demanded a client credential the status line does not send")
	}
}

// TestServeHTTPCountsTrafficForIdleWatch pins the wiring between the handler
// and the idle watch. Every request must register, including the status line's
// GET: a proxy that exits under a live status line looks like a dead endpoint
// on the next render.
func TestServeHTTPCountsTrafficForIdleWatch(t *testing.T) {
	h, good := newAuthedHandler(t)
	before := DefaultTraffic.LastSeen()
	// A timestamp only moves if the clock ticked, so force a gap.
	time.Sleep(2 * time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/__spend", nil)
	req.Header.Set("authorization", good)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !DefaultTraffic.LastSeen().After(before) {
		t.Error("a served request did not register as activity")
	}
	if n := DefaultTraffic.InFlight(); n != 0 {
		t.Errorf("InFlight = %d after the request completed, want 0", n)
	}
}

// TestTrafficInFlightIsConcurrencySafe pins the counter under parallel load.
// A lost decrement leaves InFlight permanently positive, which disables idle
// exit silently: the proxy just never reclaims its memory and nobody notices.
func TestTrafficInFlightIsConcurrencySafe(t *testing.T) {
	tr := NewTraffic()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := tr.begin()
			done()
		}()
	}
	wg.Wait()
	if n := tr.InFlight(); n != 0 {
		t.Fatalf("InFlight = %d after all requests finished, want 0", n)
	}
}

// TestTrafficActivityExposesBothSignals pins the adapter WatchIdle consumes. A
// nil field there would panic the watcher on its first tick.
func TestTrafficActivityExposesBothSignals(t *testing.T) {
	tr := NewTraffic()
	act := tr.Activity()
	if act.LastSeen == nil || act.InFlight == nil {
		t.Fatal("Activity left a signal nil; WatchIdle would panic on it")
	}
	done := tr.begin()
	defer done()
	if act.InFlight() != 1 {
		t.Errorf("Activity().InFlight() = %d during a request, want 1", act.InFlight())
	}
}

// TestClaudeRunningProcScansEnviron covers the Linux liveness scan on any
// platform by pointing it at a fake proc tree. The real thing only runs on
// Linux, which is exactly why it shipped broken: nothing here exercised it.
func TestClaudeRunningProcScansEnviron(t *testing.T) {
	root := t.TempDir()
	writeProc := func(pid, environ string) {
		t.Helper()
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// environ is NUL-separated, and the real thing has a trailing NUL.
	writeProc("101", "PATH=/usr/bin\x00CLAUDE_CONFIG_DIR=/home/u/.claude-nous\x00")
	writeProc("102", "PATH=/usr/bin\x00")
	// A non-numeric entry (/proc has plenty) must be skipped, not parsed.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !claudeRunningProcIn(root, "/home/u/.claude-nous") {
		t.Error("did not find a live process holding the config dir")
	}
	if claudeRunningProcIn(root, "/home/u/.claude-or-ds4") {
		t.Error("matched a config dir no process is using")
	}
	// The prefix case: a -backup profile must not pin the plain one up. This is
	// what the BSD path got from its trailing word-boundary match, and the
	// NUL-separated compare is what preserves it here.
	writeProc("103", "CLAUDE_CONFIG_DIR=/home/u/.claude-nous-backup\x00")
	if claudeRunningProcIn(root, "/home/u/.claude-nous-backup") != true {
		t.Error("did not match the backup dir itself")
	}
	writeProc("101", "PATH=/usr/bin\x00") // drop the exact match
	if claudeRunningProcIn(root, "/home/u/.claude-nous") {
		t.Error("a dir that is only a prefix of a live one must not match")
	}
}

// TestClaudeRunningProcMissingRoot pins the non-Linux and locked-down cases:
// an unreadable proc root reads as "nothing running", never as an error the
// caller has to handle.
func TestClaudeRunningProcMissingRoot(t *testing.T) {
	if claudeRunningProcIn(filepath.Join(t.TempDir(), "absent"), "/anything") {
		t.Error("a missing proc root should read as nothing running")
	}
}
