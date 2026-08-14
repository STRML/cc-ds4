package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// TestIsClassifier pins the classifier signature: ds4-flash-xhigh + max_tokens at or
// below the no-think budget is the auto-mode permission call. A large
// max_tokens on the same tier is a subagent and must NOT be rerouted.
func TestIsClassifier(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous"}, time.Minute)
	if !h.isClassifier([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 2048}`)) {
		t.Fatal("ds4-flash-xhigh + small max_tokens should be classifier")
	}
	if h.isClassifier([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 32000}`)) {
		t.Fatal("ds4-flash-xhigh + large max_tokens is a subagent, not classifier")
	}
}

// TestIsClassifierBoundary pins the inclusive upper edge and the presence
// semantics from Python's isinstance(mt, int) guard: 8192 is still a
// classifier, while a request with NO max_tokens (Python's missing value is
// None, and None is not an int) is not.
func TestIsClassifierBoundary(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous"}, time.Minute)
	if !h.isClassifier([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 8192}`)) {
		t.Fatal("max_tokens at the 8192 boundary is still a classifier")
	}
	if h.isClassifier([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 8193}`)) {
		t.Fatal("max_tokens above the 8192 boundary is not a classifier")
	}
	if h.isClassifier([]byte(`{"model": "ds4-flash-xhigh"}`)) {
		t.Fatal("missing max_tokens is not a classifier (Python None != int)")
	}
	if h.isClassifier([]byte(`{"model": "ds4-pro-xhigh", "max_tokens": 2048}`)) {
		t.Fatal("a different tier at small max_tokens is not a classifier")
	}
}

// TestClassifierModelOverride pins the DS4_CLASSIFIER_MODEL env knob to
// proxy.py's verbatim semantics (os.environ.get(key, default)): an ABSENT key
// falls back to claude-sonnet-5, but a present key — including an explicitly
// empty value — is used exactly as set, with no trimming.
func TestClassifierModelOverride(t *testing.T) {
	t.Setenv("DS4_CLASSIFIER_MODEL", "claude-sonnet-5-20250929")
	raw, err := classifierBody([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 2048}`), classifierModelOverride())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"model": "claude-sonnet-5-20250929"`) {
		t.Fatalf("override model not in rewritten body: %s", raw)
	}

	// Absent key -> default. t.Setenv cannot unset; unset explicitly.
	t.Setenv("DS4_CLASSIFIER_MODEL", "")
	os.Unsetenv("DS4_CLASSIFIER_MODEL")
	if got := classifierModelOverride(); got != classifierModel {
		t.Fatalf("absent env should fall back to default, got %q", got)
	}

	// Present-but-empty -> empty verbatim (Python sends it as-is, no fallback).
	t.Setenv("DS4_CLASSIFIER_MODEL", "")
	if got := classifierModelOverride(); got != "" {
		t.Fatalf("explicitly-empty env should be sent verbatim, got %q", got)
	}

	// Present with surrounding whitespace -> verbatim (no TrimSpace).
	t.Setenv("DS4_CLASSIFIER_MODEL", " claude-sonnet-5-20250929 ")
	if got := classifierModelOverride(); got != " claude-sonnet-5-20250929 " {
		t.Fatalf("env value should be used verbatim (no trim), got %q", got)
	}
}

// TestClassifierModelCachedAtBuild pins that the model is resolved ONCE when the
// Handler is built (matching proxy.py's module-load read), not re-read from env
// per request — a later env change does not affect an already-built Handler.
func TestClassifierModelCachedAtBuild(t *testing.T) {
	t.Setenv("DS4_CLASSIFIER_MODEL", "claude-sonnet-5-20250929")
	h := NewHandler(profiles.Profile{Name: "nous"}, time.Minute)
	t.Setenv("DS4_CLASSIFIER_MODEL", "changed-after-build")
	if h.classifierModel != "claude-sonnet-5-20250929" {
		t.Fatalf("model should be frozen at build, got %q", h.classifierModel)
	}
}

// TestRelayClassifierRoutesToAnthropic pins the classifier path: a
// classifier-shaped request is POSTed to the Anthropic endpoint with the
// subscription token, and the 200 is streamed back as-is. It also pins the
// outbound body: the ds4 body is whitelisted to Anthropic's keys with the
// model swapped to a real Anthropic id, so ds4-flash-xhigh and the ds4-specific
// fields never reach api.anthropic.com, and the relay carries the curl
// User-Agent that Cloudflare requires.
func TestRelayClassifierRoutesToAnthropic(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotVersion, gotCT, gotUA, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotAuth = r.Header.Get("authorization")
		gotVersion = r.Header.Get("anthropic-version")
		gotCT = r.Header.Get("content-type")
		gotUA = r.Header.Get("user-agent")
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		io_WriteString(w, `{"id": "msg_cls", "content": []}`)
	}))
	defer up.Close()

	cfg := withUpstream(testNous(), "http://ds4-upstream.invalid")
	h := NewHandler(cfg, time.Minute)
	// Point the classifier at the fake Anthropic; the profile upstream is a
	// dead host so a fail-open would hang, proving the classifier handled it.
	// The payload deliberately carries ds4-specific fields (reasoning_effort,
	// provider, metadata) that must be stripped before the body is sent.
	body := `{"model": "ds4-flash-xhigh", "max_tokens": 2048, "reasoning_effort": 80, "provider": {"require": "zdr"}, "metadata": {"k": "v"}, "messages": [{"role": "user", "content": "hi"}]}`
	if !h.relayClassifier([]byte(body), up.URL, "sk-ant-oat01-test", httptest.NewRecorder()) {
		t.Fatal("relayClassifier returned false on a 200")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sk-ant-oat01-test" {
		t.Errorf("authorization = %q, want subscription token", gotAuth)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if gotUA != "curl/8.4.0" {
		t.Errorf("user-agent = %q, want curl/8.4.0", gotUA)
	}
	// The body must be whitelisted: model swapped to a real Anthropic id, and
	// no ds4-specific key riding across.
	if !strings.Contains(gotBody, `"model": "claude-sonnet-5"`) {
		t.Errorf("body = %s, want model swapped to claude-sonnet-5", gotBody)
	}
	for _, banned := range []string{"ds4-flash-xhigh", "reasoning_effort", "provider", "metadata"} {
		if strings.Contains(gotBody, banned) {
			t.Errorf("body = %s, must not contain %q", gotBody, banned)
		}
	}
	// The Anthropic keys survive the whitelist.
	for _, kept := range []string{`"max_tokens": 2048`, `"messages": [{"role": "user", "content": "hi"}]`} {
		if !strings.Contains(gotBody, kept) {
			t.Errorf("body = %s, want kept key %q", gotBody, kept)
		}
	}
}

// TestRelayClassifier400IsTerminal pins the one deliberate exception to
// fail-open: a 400 from the classifier upstream is Anthropic rejecting the
// request shape, so it is streamed back as-is (terminal) rather than masking
// the mismatch by falling through to ds4.
func TestRelayClassifier400IsTerminal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(400)
		io_WriteString(w, `{"error": {"message": "unexpected"}}`)
	}))
	defer up.Close()

	cfg := withUpstream(testNous(), "http://ds4-upstream.invalid")
	h := NewHandler(cfg, time.Minute)
	rr := httptest.NewRecorder()
	if !h.relayClassifier([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 2048}`), up.URL, "sk-tok", rr) {
		t.Fatal("a 400 is terminal: relayClassifier must return true")
	}
	if rr.Code != 400 {
		t.Errorf("status = %d, want 400 relayed", rr.Code)
	}
	if rr.Body.String() != `{"error": {"message": "unexpected"}}` {
		t.Errorf("body = %q, want the 400 payload relayed as-is", rr.Body.String())
	}
}

// TestRelayClassifierNon400FailsOpen pins the fail-open rule: a 503 (or any
// non-400 status) returns false so the caller falls through to the ds4 relay.
func TestRelayClassifierNon400FailsOpen(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error": {"message": "busy"}}`, 503)
	}))
	defer up.Close()

	cfg := withUpstream(testNous(), "http://ds4-upstream.invalid")
	h := NewHandler(cfg, time.Minute)
	if h.relayClassifier([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 2048}`), up.URL, "sk-tok", httptest.NewRecorder()) {
		t.Fatal("a 503 must fail open: relayClassifier must return false")
	}
}

// TestRelayClassifierNetworkErrorFailsOpen pins that a transport error (here:
// the endpoint refusing the connection) fails open like any non-400 status.
func TestRelayClassifierNetworkErrorFailsOpen(t *testing.T) {
	cfg := withUpstream(testNous(), "http://ds4-upstream.invalid")
	h := NewHandler(cfg, time.Minute)
	if h.relayClassifier([]byte(`{"model": "ds4-flash-xhigh", "max_tokens": 2048}`), "http://127.0.0.1:1", "sk-tok", httptest.NewRecorder()) {
		t.Fatal("a refused connection must fail open: relayClassifier must return false")
	}
}

// TestClassifierClientHasNoDeadlineWrapper pins the load-bearing transport
// split: the classifier rides a SEPARATE client whose transport carries no
// idle-deadline DialContext, so the RELAY_TIMEOUT wrapper cannot apply to the
// no-timeout classifier path. The relay client, in contrast, wraps its dials.
func TestClassifierClientHasNoDeadlineWrapper(t *testing.T) {
	cfg := profiles.Profile{Name: "nous"}
	h := NewHandler(cfg, time.Minute)

	if h.classifierClient == nil {
		t.Fatal("classifierClient is nil")
	}
	ct, ok := h.classifierClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("classifierClient.Transport = %T, want *http.Transport", h.classifierClient.Transport)
	}
	if ct.DialContext != nil {
		t.Fatal("classifier transport must have no DialContext (no idle-deadline wrapper)")
	}
	if !ct.DisableCompression {
		t.Error("classifier transport must set DisableCompression")
	}

	rt, ok := h.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("relay client.Transport = %T, want *http.Transport", h.client.Transport)
	}
	if rt.DialContext == nil {
		t.Fatal("relay transport must keep its idle-deadline DialContext")
	}
}

// TestRelayRoutesClassifierBeforeDs4 pins the wire-in: a classifier-shaped
// request with a token set is served by the classifier relay (hits the
// classifier upstream), NOT the profile upstream. The profile upstream is a
// dead host, so the classifier must have handled it for the 200 to arrive.
func TestRelayRoutesClassifierBeforeDs4(t *testing.T) {
	var mu sync.Mutex
	classifierHits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		classifierHits++
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		io_WriteString(w, `{"id": "msg_cls"}`)
	}))
	defer up.Close()

	oldUpstream := classifierUpstream
	classifierUpstream = up.URL
	t.Cleanup(func() { classifierUpstream = oldUpstream })

	t.Setenv("DS4_KEY_NOUS", "test")
	t.Setenv("DS4_CLASSIFIER_TOKEN", "sk-ant-oat01-test")
	cfg := withUpstream(testNous(), "http://ds4-upstream.invalid")
	h := NewHandler(cfg, time.Minute)

	body := `{"model": "ds4-flash-xhigh", "max_tokens": 2048, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 from the classifier relay (body %s)", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if classifierHits != 1 {
		t.Errorf("classifier upstream hits = %d, want 1 (classifier must be served before the ds4 relay)", classifierHits)
	}
}

// TestRelayClassifierNoTokenFailsOpenToDs4 pins the fail-open wiring: without
// DS4_CLASSIFIER_TOKEN (and no config-dir fallback), a classifier-shaped
// request falls through to the ds4 relay — it is NOT served by the classifier
// upstream. The profile upstream returns 200, proving the ds4 relay ran.
func TestRelayClassifierNoTokenFailsOpenToDs4(t *testing.T) {
	var mu sync.Mutex
	classifierHits := 0
	classUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		classifierHits++
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		io_WriteString(w, `{"id": "msg_cls"}`)
	}))
	defer classUp.Close()

	profileUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io_WriteString(w, `{"id": "msg_ds4"}`)
	}))
	defer profileUp.Close()

	oldUpstream := classifierUpstream
	classifierUpstream = classUp.URL
	t.Cleanup(func() { classifierUpstream = oldUpstream })

	// No DS4_CLASSIFIER_TOKEN, no settings.json in the profile dir.
	t.Setenv("DS4_KEY_NOUS", "test")
	t.Setenv("DS4_CLASSIFIER_TOKEN", "")
	cfg := withUpstream(testNous(), profileUp.URL)
	h := NewHandler(cfg, time.Minute)

	body := `{"model": "ds4-flash-xhigh", "max_tokens": 2048, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	mu.Lock()
	defer mu.Unlock()
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if classifierHits != 0 {
		t.Errorf("classifier upstream hits = %d, want 0 (no token must fail open to ds4)", classifierHits)
	}
	if rr.Body.String() != `{"id": "msg_ds4"}` {
		t.Errorf("body = %q, want the ds4 relay's reply (fail-open)", rr.Body.String())
	}
}

// TestRelayZDRDemandSkipsClassifier pins that a request demanding ZDR is
// excluded from the classifier path: its ZDR provider block is injected by the
// route's own rewrite, and the classifier relay's whitelist cannot carry it.
// With the marker set, the request is served by the profile upstream (the
// classifier upstream must not be hit), mirroring Python's
// `not requires_zdr` gate.
func TestRelayZDRDemandSkipsClassifier(t *testing.T) {
	var mu sync.Mutex
	classifierHits := 0
	classUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		classifierHits++
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		io_WriteString(w, `{"id": "msg_cls"}`)
	}))
	defer classUp.Close()

	profileUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io_WriteString(w, `{"id": "msg_ds4"}`)
	}))
	defer profileUp.Close()

	oldUpstream := classifierUpstream
	classifierUpstream = classUp.URL
	t.Cleanup(func() { classifierUpstream = oldUpstream })

	t.Setenv("DS4_KEY_NOUS", "test")
	t.Setenv("DS4_CLASSIFIER_TOKEN", "sk-ant-oat01-test")
	cfg := withUpstream(testNous(), profileUp.URL)
	h := NewHandler(cfg, time.Minute)

	body := `{"model": "ds4-flash-xhigh", "max_tokens": 2048, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")
	req.Header.Set("x-ds4-require-zdr", "1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	mu.Lock()
	defer mu.Unlock()
	if classifierHits != 0 {
		t.Errorf("classifier upstream hits = %d, want 0 (ZDR demand must skip the classifier path)", classifierHits)
	}
	if rr.Body.String() != `{"id": "msg_ds4"}` {
		t.Errorf("body = %q, want the profile relay's reply", rr.Body.String())
	}
}

// io_WriteString writes s to w, mirroring io.WriteString.
func io_WriteString(w interface{ Write(p []byte) (int, error) }, s string) {
	w.Write([]byte(s))
}
