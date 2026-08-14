package proxy

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDrainedBalanceDetectedOnChunkedBody pins that the drained-balance rescue
// does not depend on how the upstream framed its error.
//
// The peek that feeds creditExhausted used to run only when Content-Length was
// declared and under the limit. A chunked error declares -1, so the body was
// never read, the 404 was recorded as an ordinary failure, and the caller got
// back the bare 404 that Claude Code renders as "that model may not exist" —
// the exact misdiagnosis the rescue exists to prevent, surviving in a second
// framing that nothing pinned.
func TestDrainedBalanceDetectedOnChunkedBody(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	const drained = `{"type":"error","error":{"type":"not_found_error","message":` +
		`"Model 'deepseek/deepseek-v4-flash-0731' requires available credits. ` +
		`Your account balance is too low to use paid models."}}`

	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(404)
		// Flushing before the body commits the response without a
		// Content-Length, which is what makes it chunked.
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(drained))
	}))
	defer broke.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"served"}`))
	}))
	defer target.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_KEY_OPENROUTER", "ok")
	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)

	cfg := withUpstream(testNous(), broke.URL)
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-pro-xhigh","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want the target to serve a chunked drained-balance refusal (%s)",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "served") {
		t.Errorf("body = %s, want the target's reply", rr.Body.String())
	}
}

// TestErrorBodyPeekDoesNotTruncateTheResponse pins that reading the peek leaves
// the response intact. The peek is pushed back in front of the body rather than
// replacing it, so an error larger than the peek limit still reaches the caller
// whole — otherwise diagnosing any big upstream error would mean reading a
// silently truncated one.
func TestErrorBodyPeekDoesNotTruncateTheResponse(t *testing.T) {
	installProfiles(t, "openrouter")
	big := strings.Repeat("z", errPeekLimit*3)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(400)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(`{"error":"` + big + `"}`))
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_OPENROUTER", "k")
	cfg := withUpstream(testOpenRouter(), up.URL)
	cfg.Dir = t.TempDir()
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := len(rr.Body.String()), len(big); got < want {
		t.Errorf("relayed body is %d bytes, want at least the %d-byte error: the peek ate it",
			got, want)
	}
}

// failingClaudeBin writes a describer that always fails, and counts how many
// times it was spawned. This is the expensive case the per-request image budget
// exists to bound: in production the failure is a child that hangs until
// childTimeout, and a test cannot wait two minutes an image to prove it.
func failingClaudeBin(t *testing.T) (bin string, calls func() int) {
	t.Helper()
	dir := t.TempDir()
	tally := filepath.Join(dir, "calls")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho x >> " + tally + "\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int {
		raw, err := os.ReadFile(tally)
		if err != nil {
			return 0
		}
		return strings.Count(string(raw), "x")
	}
}

// imageBody builds a request carrying n distinct images, so no two share a
// cache key and every one is a fresh lookup.
func imageBody(n int) string {
	var b strings.Builder
	b.WriteString(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[{"role":"user","content":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		data := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("image-bytes-%d", i)))
		b.WriteString(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}`)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

// TestVisionBudgetChargesFailedChildren pins the budget against the case it
// exists for.
//
// Charging only successful describes meant a child that timed out, exited
// nonzero, or returned junk cost nothing, so the cap could not stop the one
// thing it was written to stop: a transcript full of images whose describer is
// broken, each burning up to childTimeout with the request held open. Failure
// is not cheaper than success — it is the expensive case.
func TestVisionBudgetChargesFailedChildren(t *testing.T) {
	bin, calls := failingClaudeBin(t)
	t.Setenv("DS4_CLAUDE_BIN", bin)
	t.Setenv("DS4_VISION", "1")

	cfg := testNous()
	cfg.Dir = t.TempDir()

	const images = maxImagesPerRequest * 2
	out, err := applyVision([]byte(imageBody(images)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := calls(); n > maxImagesPerRequest {
		t.Errorf("spawned %d children for %d images, want at most the %d-image budget",
			n, images, maxImagesPerRequest)
	}
	// Whatever the budget did, no image block may survive to the upstream.
	if strings.Contains(string(out), `"type": "image"`) {
		t.Error("an image block reached the upstream body")
	}
}

// TestVisionSkipsBodiesWithNoImages pins the short-circuit: a text-only body is
// returned byte-identical, not parsed and re-emitted. Most requests on a 1M
// context profile are text-only and multi-megabyte, so this is the common path.
func TestVisionSkipsBodiesWithNoImages(t *testing.T) {
	t.Setenv("DS4_VISION", "1")
	cfg := testNous()
	cfg.Dir = t.TempDir()

	// Deliberately not canonically spaced: a re-emit would normalize it, so
	// byte equality proves the body was never round-tripped.
	body := []byte(`{"model":"ds4-flash-medium",   "max_tokens":32000,"messages":[]}`)
	out, err := applyVision(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("text-only body was round-tripped:\n got %s\nwant %s", out, body)
	}
}

// TestClassifierThresholdIsNotTheNoThinkKnob pins the two thresholds apart.
//
// isClassifier once shared nothinkBelow, which DS4_NOTHINK_BELOW moves. That
// made a documented thinking-budget preference silently widen a trust
// boundary: raising it to 32768 would route every flash subagent request under
// 32K to api.anthropic.com on the subscription token, rebuilt through a
// whitelist that drops fields. Nothing about the permission gate should move
// because someone tuned how hard the model thinks.
func TestClassifierThresholdIsNotTheNoThinkKnob(t *testing.T) {
	oldNoThink, oldClassifier := nothinkBelow, classifierMaxTokens
	defer func() { nothinkBelow, classifierMaxTokens = oldNoThink, oldClassifier }()

	h := &Handler{cfg: testNous()}
	big := []byte(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`)

	// A user widens the no-think window and nothing else.
	nothinkBelow, classifierMaxTokens = 65536, 8192
	if h.isClassifier(big) {
		t.Error("a 32K subagent request became the classifier because DS4_NOTHINK_BELOW moved")
	}

	// Only the classifier's own knob changes what counts as the gate.
	classifierMaxTokens = 65536
	if !h.isClassifier(big) {
		t.Error("the classifier threshold does not govern classifier detection")
	}
}

// TestClassifierRouteReleasesTheTrial pins that a request answered off-profile
// hands the failover trial back.
//
// Routing is resolved at the top of the relay, so a request can claim the trial
// and then return without ever contacting the upstream. The classifier is the
// most frequent small request in auto mode, and it learns nothing about whether
// the profile's upstream recovered. Burning the trial on it also reset the
// probe streak, keeping the profile on its failover target for another full
// recheck interval before it could even try to come home.
func TestClassifierRouteReleasesTheTrial(t *testing.T) {
	installProfiles(t, "nous", "openrouter")

	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"classified"}`))
	}))
	defer anthropic.Close()

	old := classifierUpstream
	classifierUpstream = anthropic.URL
	defer func() { classifierUpstream = old }()

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_CLASSIFIER", "anthropic")
	t.Setenv("DS4_CLASSIFIER_TOKEN", "tok")

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	armTrial(h)

	// A classifier-shaped request: flash sentinel, small max_tokens.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-medium","max_tokens":512,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !strings.Contains(rr.Body.String(), "classified") {
		t.Fatalf("setup: the classifier was not routed off-profile, body = %s", rr.Body.String())
	}

	h.br.mu.Lock()
	trial, active := h.br.trial, h.br.trialActive
	h.br.mu.Unlock()
	if active {
		t.Error("the trial is still marked in flight after a request that never reached the upstream")
	}
	if !trial {
		t.Error("the trial was consumed by a request that never tested the upstream")
	}
}

// TestZDRRefusalReleasesTheTrial is the same contract on the other early
// return: a request refused for want of ZDR never reaches an upstream either.
func TestZDRRefusalReleasesTheTrial(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	t.Setenv("DS4_KEY_NOUS", "k")

	cfg := testNous() // no ZDR
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	armTrial(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	req.Header.Set("x-ds4-require-zdr", "1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 409 {
		t.Fatalf("setup: status = %d, want the ZDR refusal (409)", rr.Code)
	}
	h.br.mu.Lock()
	trial, active := h.br.trial, h.br.trialActive
	h.br.mu.Unlock()
	if active {
		t.Error("the trial is still marked in flight after a refused request")
	}
	if !trial {
		t.Error("a refused request consumed the trial")
	}
}
