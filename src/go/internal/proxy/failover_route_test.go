package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	installProfiles(t, "nous", "openrouter")
	cfg := testNous()
	cfg.Dir = t.TempDir()
	h := NewHandler(cfg, time.Minute)

	eff, _, _ := h.effectiveProfile()
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
	installProfiles(t, "nous", "openrouter")
	t.Setenv("DS4_KEY_OPENROUTER", "target-key") // an installed target has one
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	eff, _, _ := h.effectiveProfile()
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
	installProfiles(t, "nous", "openrouter")
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "no-such-profile"
	cfg.Upstream = "https://nous.example"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	eff, _, _ := h.effectiveProfile()
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
	installProfiles(t, "nous", "openrouter")
	cfg := testOpenRouter()
	cfg.Dir = t.TempDir()
	cfg.Failover = ""
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	eff, _, _ := h.effectiveProfile()
	if eff.Name != "openrouter" {
		t.Fatalf("routed to %q with no failover target configured", eff.Name)
	}
}

// TestFailoverUsesTargetKeyNotOwn pins the credential swap. A failed-over
// request authenticates to the TARGET's upstream, so sending the origin
// profile's key would 401 every request the moment the breaker trips, turning
// a partial outage into a total one.
func TestFailoverUsesTargetKeyNotOwn(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	t.Setenv("DS4_KEY_NOUS", "key-for-nous")
	t.Setenv("DS4_KEY_OPENROUTER", "key-for-openrouter")

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	if _, key, _ := h.effectiveProfile(); key != "key-for-nous" {
		t.Errorf("closed breaker used key %q, want nous's own", key)
	}
	tripBreaker(h)
	if _, key, _ := h.effectiveProfile(); key != "key-for-openrouter" {
		t.Errorf("failed over with key %q, want the target's", key)
	}
}

// TestFailoverEndToEndRemapsModelAndHitsTarget drives a real request through
// ServeHTTP with the circuit open and asserts against what the target upstream
// actually received. The unit tests above check the routing decision; this
// checks that the decision survives the whole relay path.
func TestFailoverEndToEndRemapsModelAndHitsTarget(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
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
	installProfiles(t, "nous", "openrouter")
	var attempts int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(503)
	}))
	defer target.Close()

	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)
	t.Setenv("DS4_KEY_NOUS", "origin-key")
	t.Setenv("DS4_KEY_OPENROUTER", "target-key")

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

// armTrial puts the breaker where a clean probe streak leaves it: a trial is
// available but no request has taken it yet.
func armTrial(h *Handler) {
	h.br.mu.Lock()
	h.br.open = true
	h.br.trial = true
	h.br.trialActive = false
	h.br.mu.Unlock()
}

// claimTrial puts the breaker where effectiveProfile leaves it once a request
// HAS taken the trial: nothing left to hand out, one request in flight whose
// outcome decides the circuit. trialClose acts on this state, not on armTrial's.
func claimTrial(h *Handler) {
	h.br.mu.Lock()
	h.br.open = true
	h.br.trial = false
	h.br.trialActive = true
	h.br.mu.Unlock()
}

func breakerIsOpen(h *Handler) bool {
	h.br.mu.Lock()
	defer h.br.mu.Unlock()
	return h.br.open
}

// TestTrialRoutesToOwnUpstream pins the point of the trial: it goes to the
// profile's OWN upstream, not the failover target. Routing it to the target
// would measure the target's health and close the circuit on evidence about
// the wrong server.
func TestTrialRoutesToOwnUpstream(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Upstream = "https://nous.example"
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	armTrial(h)

	eff, _, trial := h.effectiveProfile()
	if !trial {
		t.Fatal("armed trial not reported to the caller")
	}
	if eff.Name != "nous" {
		t.Errorf("trial routed to %q, want the profile's own upstream", eff.Name)
	}
}

// TestTrialCloseOnlyOnCleanRequest pins that a probe alone never closes the
// circuit. A clean probe arms a trial; only a real request served without a
// transient error closes it. Closing on the probe is the flap this prevents:
// the probe passes during a lull, the next heavy request fails, and the
// circuit reopens immediately.
func TestTrialCloseOnlyOnCleanRequest(t *testing.T) {
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"

	t.Run("transient trial keeps it open", func(t *testing.T) {
		h := NewHandler(cfg, time.Minute)
		claimTrial(h)
		if closed := h.trialClose(false); closed {
			t.Error("a failed trial closed the circuit")
		}
		if !breakerIsOpen(h) {
			t.Error("circuit closed despite the trial failing")
		}
	})

	t.Run("clean trial closes it", func(t *testing.T) {
		h := NewHandler(cfg, time.Minute)
		claimTrial(h)
		if closed := h.trialClose(true); !closed {
			t.Error("a clean trial did not close the circuit")
		}
		if breakerIsOpen(h) {
			t.Error("circuit still open after a clean trial")
		}
	})
}

// TestTrialIsSpentOnce pins that a burst does not spend one trial across
// several concurrent requests. The second report finds the trial disarmed and
// changes nothing, so a single clean response cannot be double-counted.
func TestTrialIsSpentOnce(t *testing.T) {
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	claimTrial(h)

	if !h.trialClose(true) {
		t.Fatal("first report did not close the circuit")
	}
	if h.trialClose(true) {
		t.Error("a second report acted on an already-spent trial")
	}
}

// TestFailedTrialResetsTheProbeClock pins that a failed trial does not let the
// next request probe immediately. Without this the proxy would retry the
// struggling upstream on every request instead of waiting out the recheck
// interval, which is the load pattern the breaker exists to stop.
func TestFailedTrialResetsTheProbeClock(t *testing.T) {
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	claimTrial(h)
	h.br.mu.Lock()
	h.br.lastProbe = time.Time{} // as if a probe were due right now
	h.br.mu.Unlock()

	h.trialClose(false)

	h.br.mu.Lock()
	due := h.br.lastProbe.IsZero()
	h.br.mu.Unlock()
	if due {
		t.Error("a failed trial left the upstream immediately re-probeable")
	}
}

// TestCleanProbeArmsButDoesNotClose is the regression test for the flap. It
// drives the real probe path rather than arming the trial by hand, because the
// bug being guarded against lives in that path: an earlier version closed the
// circuit the moment the probe streak was long enough, and a probe passing
// during a lull is not evidence the upstream can carry load.
func TestCleanProbeArmsButDoesNotClose(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer own.Close()

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Upstream = own.URL // the probe targets the profile's OWN upstream
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	// Open the circuit with the probe clock due, so the next call probes.
	h.br.mu.Lock()
	h.br.open = true
	h.br.lastProbe = time.Time{}
	h.br.mu.Unlock()

	// Probe until the streak is long enough to arm.
	var armed bool
	for i := 0; i < failoverProbesToClose+2 && !armed; i++ {
		h.br.mu.Lock()
		h.br.lastProbe = time.Time{} // make each iteration eligible to probe
		h.br.mu.Unlock()
		_, armed = h.breakerOpen()
	}

	if !armed {
		t.Fatal("a clean probe streak never armed a trial")
	}
	if !breakerIsOpen(h) {
		t.Fatal("probes alone closed the circuit; only a real request may do that")
	}
}

// TestFailoverNeverLeaksTheOriginKey pins the credential boundary in the case
// that actually leaks: the failover target has NO key of its own.
//
// The client authenticates to the proxy with the origin profile's key. When the
// proxy then talks to a different provider, that credential must not ride
// along. With a target key present the override masks the bug, so this test
// deliberately supplies none: the request must go out unauthenticated and fail,
// never authenticated with a credential issued by someone else.
func TestKeylessTargetIsNotATarget(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	var seen bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()

	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)
	t.Setenv("DS4_KEY_NOUS", "origin-secret")
	t.Setenv("DS4_KEY_OPENROUTER", "") // installed, never finished configuring

	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	tripBreaker(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model": "ds4-flash-xhigh", "max_tokens": 32000, "messages": []}`))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer origin-secret")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen {
		t.Error("routed to a target with no credential; it can only answer 401, " +
			"which hides the origin's real outage behind an auth error from a " +
			"provider the user never configured")
	}
}

// TestPrepareUpstreamHeadersDropsOriginKeyOnFailover pins the credential
// boundary at the level it is decided. End-to-end this is masked whenever the
// failover target has a key of its own, which it usually does; the leak is the
// case where it does not, and that is what this covers.
func TestPrepareUpstreamHeadersDropsOriginKeyOnFailover(t *testing.T) {
	client := http.Header{}
	client.Set("authorization", "Bearer origin-secret")
	client.Set("x-custom", "keep me")

	t.Run("failed over with no target key", func(t *testing.T) {
		out := http.Header{}
		prepareUpstreamHeaders(out, client, true, "", 3)
		if got := out.Get("authorization"); got != "" {
			t.Errorf("authorization = %q, want it dropped", got)
		}
		if out.Get("x-custom") != "keep me" {
			t.Error("an unrelated client header was dropped")
		}
	})

	t.Run("failed over with a target key", func(t *testing.T) {
		out := http.Header{}
		prepareUpstreamHeaders(out, client, true, "target-key", 3)
		if got := out.Get("authorization"); got != "Bearer target-key" {
			t.Errorf("authorization = %q, want the target's own key", got)
		}
	})

	t.Run("not failed over keeps the profile key", func(t *testing.T) {
		out := http.Header{}
		prepareUpstreamHeaders(out, client, false, "own-key", 3)
		if got := out.Get("authorization"); got != "Bearer own-key" {
			t.Errorf("authorization = %q, want the profile's own key", got)
		}
	})

	t.Run("user agent always overrides the client", func(t *testing.T) {
		withUA := http.Header{}
		withUA.Set("user-agent", "claude-cli/2.0")
		out := http.Header{}
		prepareUpstreamHeaders(out, withUA, false, "k", 3)
		if got := out.Get("user-agent"); got != "curl/8.4.0" {
			t.Errorf("user-agent = %q, want the curl identity Cloudflare accepts", got)
		}
	})
}

// TestTrialIsHandedOutToExactlyOneRequest pins the reservation. During an
// outage a burst can arrive in the window where the trial is armed. If they all
// take it, they all bypass failover and hit the upstream that is still down,
// each returning a hard error to its caller instead of being served by the
// target. Only one may be spent that way.
func TestTrialIsHandedOutToExactlyOneRequest(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)
	armTrial(h)

	const n = 25
	var mu sync.Mutex
	trials := 0
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, trial := h.effectiveProfile(); trial {
				mu.Lock()
				trials++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if trials != 1 {
		t.Errorf("%d of %d concurrent requests took the trial, want exactly 1", trials, n)
	}
}

// TestFailoverIsGracefulEndToEnd pins the property that matters to whoever is
// typing: while the profile's upstream is down, requests keep being answered.
//
// The breaker still learns — it trips, it probes, it arms trials — but none of
// that is allowed to surface as a failed turn. Before this, the window before
// the circuit tripped and every trial afterwards handed the caller a hard
// error while a healthy target sat idle.
func TestFailoverIsGracefulEndToEnd(t *testing.T) {
	installProfiles(t, "nous", "openrouter")

	var nousHits int
	var mu sync.Mutex
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		nousHits++
		mu.Unlock()
		w.WriteHeader(503) // the profile's own upstream is down throughout
	}))
	defer dead.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"served"}`))
	}))
	defer target.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_KEY_OPENROUTER", "ok")
	t.Setenv("DS4_UPSTREAM_OPENROUTER", target.URL)

	cfg := withUpstream(testNous(), dead.URL)
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	fire := func() (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"ds4-pro-xhigh","max_tokens":32000,"messages":[]}`))
		req.Header.Set("authorization", "Bearer k")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}

	// Before the circuit trips: the origin is failing every request, and the
	// caller must not see it.
	for i := 0; i < 6; i++ {
		if code, body := fire(); code != 200 {
			t.Fatalf("request %d before trip: status %d (%s)", i, code, body)
		}
	}
	if !breakerIsOpen(h) {
		t.Fatal("the breaker never tripped; it must still learn from the failures")
	}

	// An armed trial goes back to the dead origin. It must still be answered.
	armTrial(h)
	before := func() int { mu.Lock(); defer mu.Unlock(); return nousHits }()
	code, body := fire()
	if code != 200 {
		t.Errorf("the trial request surfaced a failure to the caller: %d (%s)", code, body)
	}
	after := func() int { mu.Lock(); defer mu.Unlock(); return nousHits }()
	if after == before {
		t.Error("the trial never reached the origin upstream, so it measured nothing")
	}
	if breakerIsOpen(h) != true {
		t.Error("a failed trial closed the circuit")
	}
}

// TestRescueDoesNotMaskATargetOutage pins the limit of the rescue. When the
// target is failing too there is nothing to fall back to, and the caller should
// see the origin's own error rather than a second, more confusing one.
func TestRescueDoesNotMaskATargetOutage(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer dead.Close()
	alsoDead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
	}))
	defer alsoDead.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_KEY_OPENROUTER", "ok")
	t.Setenv("DS4_UPSTREAM_OPENROUTER", alsoDead.URL)

	cfg := withUpstream(testNous(), dead.URL)
	cfg.Dir = t.TempDir()
	cfg.Failover = "openrouter"
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-pro-xhigh","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 503 {
		t.Errorf("status = %d, want the origin's own 503 when both are down", rr.Code)
	}
}

// TestDrainedBalanceFailsOverInsteadOfErroring pins the case that actually bit.
//
// Nous refuses a paid model with a 404 whose body says the balance is too low.
// A 404 is not transient, so before this the breaker never tripped, the request
// never failed over, and every turn failed — while Claude Code reported it as
// "that model may not exist", which points at the wrong thing entirely.
func TestDrainedBalanceFailsOverInsteadOfErroring(t *testing.T) {
	installProfiles(t, "nous", "openrouter")
	const drained = `{"type":"error","error":{"type":"not_found_error","message":` +
		`"Model 'deepseek/deepseek-v4-flash-0731' requires available credits. ` +
		`Your account balance is too low to use paid models."}}`

	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(404)
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
		t.Fatalf("status = %d, want the target to serve it (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "served") {
		t.Errorf("body = %s, want the target's reply", rr.Body.String())
	}
}

// TestCreditExhausted pins the detection itself, including the negatives: an
// ordinary 404 for a genuinely unknown model must NOT route away, or a typo in
// a model name would silently spend the failover target's credits instead of
// telling the user their model does not exist.
func TestCreditExhausted(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"nous drained", 404, `{"error":{"message":"requires available credits, balance too low"}}`, true},
		{"openrouter 402", 402, `{"error":"payment required"}`, true},
		{"403 insufficient funds", 403, `{"error":"insufficient funds"}`, true},
		{"genuine unknown model", 404, `{"error":{"message":"model not found"}}`, false},
		{"ordinary 400", 400, `{"error":{"message":"bad request"}}`, false},
		{"transient 503", 503, ``, false},
		{"success", 200, `{"ok":true}`, false},
	} {
		if got := creditExhausted(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: creditExhausted = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDrainedBalanceWithNoTargetExplainsItself pins the message. With no
// failover target the request cannot be served, and the reason the caller sees
// has to be the real one: a 404 from the upstream reaches the CLI as "that
// model may not exist", which sends the reader to check model names and keys
// while the actual problem is an empty account.
func TestDrainedBalanceWithNoTargetExplainsItself(t *testing.T) {
	installProfiles(t, "nous")
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"message":"requires available credits, balance too low"}}`))
	}))
	defer broke.Close()

	t.Setenv("DS4_KEY_NOUS", "k")
	cfg := withUpstream(testNous(), broke.URL)
	cfg.Dir = t.TempDir()
	cfg.Failover = "" // nothing to fall back to
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-pro-xhigh","max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 402 {
		t.Errorf("status = %d, want 402 (404 reads as a missing model)", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"out of credit", "billing problem", "nous"} {
		if !strings.Contains(body, want) {
			t.Errorf("message does not mention %q: %s", want, body)
		}
	}
}
