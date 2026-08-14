package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// tripBreaker forces h's circuit open without waiting for real failures.
func tripBreaker(h *Handler) {
	h.br.mu.Lock()
	h.br.open = true
	h.br.lastProbe = time.Now() // inside the recheck window, so no probe fires
	h.br.mu.Unlock()
}

// TestEffectiveProfileClosedBreakerStaysHome pins the default: with the
// circuit closed a profile serves its own upstream and nothing is remapped.
func TestEffectiveProfileClosedBreakerStaysHome(t *testing.T) {
	cfg := testNous()
	cfg.Dir = t.TempDir()
	h := NewHandler(cfg, time.Minute)

	eff, _ := h.effectiveProfile()
	if eff.Name != "nous" {
		t.Fatalf("closed breaker routed to %q, want nous", eff.Name)
	}
	if eff.FailoverTarget {
		t.Error("FailoverTarget set while serving the profile's own upstream")
	}
}

// TestEffectiveProfileOpenBreakerRoutesToTarget pins the failover hop. Nous
// fails over to openrouter, and the returned profile must be openrouter's own
// row, marked as a target so the remap in rewrite runs.
func TestEffectiveProfileOpenBreakerRoutesToTarget(t *testing.T) {
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	eff, _ := h.effectiveProfile()
	if eff.Name != "openrouter" {
		t.Fatalf("open breaker routed to %q, want openrouter", eff.Name)
	}
	if !eff.FailoverTarget {
		t.Error("target not marked FailoverTarget; the model remap would not run")
	}
	var want string
	for _, p := range profiles.All() {
		if p.Name == "openrouter" {
			want = p.Upstream
		}
	}
	if eff.Upstream != want {
		t.Errorf("upstream = %q, want the target's own %q", eff.Upstream, want)
	}
}

// TestEffectiveProfileUnknownTargetStaysHome pins the safe direction for a
// misconfigured failover name. Serving the profile's own upstream degrades to
// "no failover"; silently serving a zero-value Profile would point the request
// at an empty upstream instead.
func TestEffectiveProfileUnknownTargetStaysHome(t *testing.T) {
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "no-such-profile"
	cfg.Upstream = "https://nous.example"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	eff, _ := h.effectiveProfile()
	if eff.Name != "nous" {
		t.Fatalf("unknown target routed to %q, want to stay on nous", eff.Name)
	}
	if eff.Upstream == "" {
		t.Error("upstream was blanked; the request would go nowhere")
	}
}

// TestEffectiveProfileNoTargetStaysHome pins that a profile with no failover
// configured never leaves home even with its circuit open.
func TestEffectiveProfileNoTargetStaysHome(t *testing.T) {
	cfg := testOpenRouter()
	cfg.Dir = t.TempDir()
	cfg.Failover = ""
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	eff, _ := h.effectiveProfile()
	if eff.Name != "openrouter" {
		t.Fatalf("routed to %q with no failover target configured", eff.Name)
	}
}

// TestFailoverUsesTargetKeyNotOwn pins the credential swap. A failed-over
// request authenticates to the TARGET's upstream, so sending the origin
// profile's key would 401 every request the moment the breaker trips, turning
// a partial outage into a total one.
func TestFailoverUsesTargetKeyNotOwn(t *testing.T) {
	t.Setenv("DS4_KEY_NOUS", "key-for-nous")
	t.Setenv("DS4_KEY_OPENROUTER", "key-for-openrouter")

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	if _, key := h.effectiveProfile(); key != "key-for-nous" {
		t.Errorf("closed breaker used key %q, want nous's own", key)
	}
	tripBreaker(h)
	if _, key := h.effectiveProfile(); key != "key-for-openrouter" {
		t.Errorf("failed over with key %q, want the target's", key)
	}
}

// TestFailoverEndToEndRemapsModelAndHitsTarget drives a real request through
// ServeHTTP with the circuit open and asserts against what the target upstream
// actually received. The unit tests above check the routing decision; this
// checks that the decision survives the whole relay path.
func TestFailoverEndToEndRemapsModelAndHitsTarget(t *testing.T) {
	var gotBody, gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody, gotAuth = string(b), r.Header.Get("authorization")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()

	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)
	t.Setenv("DS4_KEY_OPENROUTER", "target-key")
	t.Setenv("DS4_KEY_NOUS", "origin-key")

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	cfg.Upstream = "http://nous-upstream.invalid"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model": "ds4-flash-xhigh", "max_tokens": 32000, "messages": []}`))
	req.Header.Set("content-type", "application/json")
	// The CLIENT still authenticates with the origin profile's key even while
	// the proxy is talking to the failover target. The two credentials are
	// independent, and conflating them would either reject the local client or
	// leak its key upstream.
	req.Header.Set("authorization", "Bearer origin-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code == 401 {
		t.Fatal("origin key rejected while failed over; the client should be unaffected")
	}
	if gotBody == "" {
		t.Fatal("the failover target never received the request")
	}
	// The sentinel resolves through the TARGET's family map, so the origin
	// profile's plain id must not reach an upstream that does not serve it.
	if !strings.Contains(gotBody, `"model": "deepseek/deepseek-v4-flash-0731:nitro"`) {
		t.Errorf("model not resolved for the target: %s", gotBody)
	}
	if gotAuth != "Bearer target-key" {
		t.Errorf("auth = %q, want the target's key", gotAuth)
	}
}

// TestFailoverDoesNotRetryTheMainLoop pins that the retry decision reads the
// CLIENT-sent tier, not the post-remap model. The remap rewrites the body's
// model to the target's literal id, and keying off that would make every
// failed-over request look like a retryable subagent call, doubling up with
// the main loop's own backoff exactly when the upstream is already struggling.
func TestFailoverDoesNotRetryTheMainLoop(t *testing.T) {
	var attempts int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(503)
	}))
	defer target.Close()

	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)
	t.Setenv("DS4_KEY_NOUS", "origin-key")

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model": "`+mainLoopTier+`", "max_tokens": 32000, "messages": []}`))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer origin-key")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if attempts != 1 {
		t.Errorf("main-loop request attempted %d times through failover, want 1", attempts)
	}
}
