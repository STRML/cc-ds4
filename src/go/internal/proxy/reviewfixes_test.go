package proxy

import (
	"encoding/base64"
	"fmt"
	"io"
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

// TestProbeDoesNotArmASecondTrial pins the exclusivity the breaker's comment
// promises. A trial can be a long streaming turn that outlasts a whole recheck
// interval; if the probe path arms another underneath it, trialClose becomes
// ambiguous — whichever request finishes first clears trialActive, and the
// other returns early without closing the circuit even when a recovered
// upstream served it cleanly.
func TestProbeDoesNotArmASecondTrial(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`)) // a clean probe
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	cfg := withUpstream(testNous(), up.URL)
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	// A trial is already in flight, and the recheck window has lapsed so the
	// next request probes.
	h.br.mu.Lock()
	h.br.open = true
	h.br.trialActive = true
	h.br.probes = failoverProbesToClose - 1
	h.br.lastProbe = time.Now().Add(-time.Duration(failoverRecheck+60) * time.Second)
	h.br.mu.Unlock()

	if _, trial := h.breakerOpen(); trial {
		t.Error("a second trial was armed while one was still in flight")
	}
}

// TestRescueToDrainedTargetExplainsItself pins the case where both accounts are
// empty. Streaming the target's own refusal would put back the misdiagnosis the
// whole drained path exists to remove: the CLI renders any 404 naming a model
// as "that model may not exist".
func TestRescueToDrainedTargetExplainsItself(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	const credits = `{"error":{"message":"requires available credits, balance too low"}}`

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(credits))
	}))
	defer origin.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":"payment required"}`))
	}))
	defer target.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_KEY_OPENROUTER", "ok")
	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)

	cfg := withUpstream(testNous(), origin.URL)
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 402 {
		t.Fatalf("status = %d, want 402 explaining the balance (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "nous") {
		t.Errorf("the 402 does not name the drained profile: %s", rr.Body.String())
	}
}

// TestRescueRefusesToBreakZDR pins the privacy gate across the rescue. The 409
// gate runs against the profile picked at the top of the relay, and the rescue
// swaps that profile afterwards — so without this check a demand that passed
// the gate could still be served by a target enforcing nothing.
func TestRescueRefusesToBreakZDR(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()

	t.Setenv("DS4_KEY_OPENROUTER", "k")
	t.Setenv("DS4_KEY_NOUS", "nk")
	t.Setenv("DS4_UPSTREAM_NOUS", target.URL)

	// A ZDR-capable origin whose target is not ZDR-capable. The shipped table
	// has no such pair today, but its own comments contemplate changing these
	// rows, and a fail-open hole in a privacy gate should not wait for that.
	cfg := testOpenRouter()
	cfg.Dir = t.TempDir()
	cfg.Failover = "nous"
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	if h.rescueViaFailover(httptest.NewRecorder(), req,
		[]byte(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`),
		cfg.Upstream+"/v1/messages", true) {
		t.Error("a ZDR-demanding request was rescued onto a target that cannot enforce it")
	}
	if reached {
		t.Error("the non-ZDR target received a request that demanded ZDR")
	}
}

// TestVisionDeadlineStopsTheWalk pins the wall-clock bound. The count cap alone
// does not bound time: the walk is serial and each child may run to
// childTimeout, so a budget's worth of slow describes is minutes of held
// request. Past the deadline the remaining images are placeholdered without
// spawning anything.
func TestVisionDeadlineStopsTheWalk(t *testing.T) {
	bin, calls := failingClaudeBin(t)
	t.Setenv("DS4_CLAUDE_BIN", bin)
	t.Setenv("DS4_VISION", "1")

	// A clock that jumps past the deadline once the walk has started, standing
	// in for describes that each burn real time.
	base := time.Now()
	var ticks int
	old := visionNow
	visionNow = func() time.Time {
		ticks++
		if ticks <= 2 { // construction, then the first image's check
			return base
		}
		return base.Add(visionWallClock + time.Second)
	}
	defer func() { visionNow = old }()

	cfg := testNous()
	cfg.Dir = t.TempDir()
	out, err := applyVision([]byte(imageBody(6)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := calls(); n > 1 {
		t.Errorf("spawned %d children after the deadline passed, want at most the 1 in flight", n)
	}
	if strings.Contains(string(out), `"type": "image"`) {
		t.Error("an image block reached the upstream body")
	}
}

// TestDrainedTargetNamesTheProfileThatServed pins who the 402 blames. Once the
// circuit is open the request is served by the target, so an empty balance
// there is the TARGET's. Naming the origin sends the user to top up an account
// that already has money while nothing changes — the same class of
// wrong-signpost error the drained path was written to remove.
func TestDrainedTargetNamesTheProfileThatServed(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient credits"}}`))
	}))
	defer target.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_KEY_OPENROUTER", "ok")
	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h) // already failed over: the target is what serves this

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 402 {
		t.Fatalf("status = %d, want 402 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "openrouter") {
		t.Errorf("the 402 blames the wrong profile: %s", rr.Body.String())
	}
}

// TestEffortPinBeatsAClientSentLevel pins the precedence the status line
// advertises. The bar renders the pin from the override file unconditionally,
// so a client-sent reasoning_effort that quietly outranked it made the display
// assert an effort the proxy did not apply.
func TestEffortPinBeatsAClientSentLevel(t *testing.T) {
	cfg := testOpenRouter()
	cfg.Dir = t.TempDir()
	body := []byte(`{"model":"ds4-flash-medium","reasoning_effort":"low","max_tokens":32000,"messages":[]}`)

	got, err := rewrite(body, cfg, "max")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"reasoning_effort": "max"`) {
		t.Errorf("the pin lost to the client's level: %s", got)
	}
	if strings.Contains(string(got), `"reasoning_effort": "low"`) {
		t.Errorf("the client's level survived over the pin: %s", got)
	}

	// With no pin set, the client's level still beats the sentinel default.
	got, err = rewrite(body, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"reasoning_effort": "low"`) {
		t.Errorf("without a pin the client's level should stand: %s", got)
	}
}

// TestRescueResolvesTheSentinelForTheTarget pins that the rescue sees the body
// as the client sent it.
//
// The rescue re-runs rewrite for the target so the target's family map applies
// — but it was handed a body the origin's rewrite had already resolved, so no
// sentinel remained to match and only the hardcoded failoverModel net did
// anything. With the shipped table both paths happen to land on the same id,
// which is precisely why this needs a profile whose model the net does not
// know: that is the case where a table edit silently starts forwarding an id
// the target does not serve.
func TestRescueResolvesTheSentinelForTheTarget(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	var gotModel string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotModel = modelFromJSON(raw)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"served"}`))
	}))
	defer target.Close()
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503) // transient: rescue territory
	}))
	defer broke.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_KEY_OPENROUTER", "ok")
	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)

	// An origin whose own model id the failoverModel net has never heard of,
	// standing in for any future edit to the profile table.
	cfg := withUpstream(testNous(), broke.URL)
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	cfg.Model = "nous/some-future-build"
	cfg.FamilyModels = map[string]string{
		"pro":   "nous/some-future-build",
		"flash": "nous/some-future-build",
	}
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-medium","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotModel == "" {
		t.Fatal("the target never received the rescued request")
	}
	if strings.HasPrefix(gotModel, "nous/") {
		t.Errorf("the target was sent the ORIGIN's model id %q, which it does not serve", gotModel)
	}
}
