package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// Failover circuit breaker, mirroring proxy.py:269-300.
//
// A profile whose upstream has sustained transient errors can declare a
// "failover" target in PROFILES. Once FAILOVER_RATE of the last
// FAILOVER_WINDOW outcomes are strikes, the breaker opens and requests are
// served by the target's upstream and key until FAILOVER_PROBES_TO_CLOSE
// consecutive probes succeed (spaced FAILOVER_RECHECK apart).
//
// Knobs are read at package init from DS4_* env, matching Python's
// import-time binding. The differential harness sets FAILOVER_WINDOW=3,
// FAILOVER_RATE=1.0, FAILOVER_PROBES_TO_CLOSE=1 for its failover case.

var (
	failoverEnabled       = os.Getenv("DS4_FAILOVER") != "0"
	failoverWindow        = envInt("DS4_FAILOVER_WINDOW", 12)
	failoverRate          = envFloat("DS4_FAILOVER_RATE", 0.25)
	failoverRecheck       = envInt("DS4_FAILOVER_RECHECK", 60)
	failoverProbesToClose = envInt("DS4_FAILOVER_PROBES_TO_CLOSE", 3)
	failoverProbeTimeout  = envInt("DS4_FAILOVER_PROBE_TIMEOUT", 6)
)

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// creditExhausted reports an upstream refusing service because the account is
// out of money. Nous spells this as a 404 not_found_error whose message is
// about credits, and OpenRouter as a 402.
//
// It is deliberately NOT treated as transient: retrying does not help, and the
// in-proxy retry would just burn the request three times. But it IS a reason to
// route elsewhere, and it is permanent until someone tops up — so it counts as
// a breaker strike and triggers the failover rescue. Without this a drained
// balance is a total outage with a healthy target sitting idle, and the CLI
// reports it as "that model may not exist", which sends you looking in
// completely the wrong place.
func creditExhausted(status int, body []byte) bool {
	if status == 402 {
		return true
	}
	if status != 404 && status != 403 {
		return false
	}
	b := strings.ToLower(string(body))
	return strings.Contains(b, "credit") || strings.Contains(b, "balance") ||
		strings.Contains(b, "insufficient funds")
}

func failoverThreshold() int {
	t := int(float64(failoverWindow) * failoverRate)
	if t < 1 {
		return 1
	}
	return t
}

// breaker is one profile's failover state. Guarded by mu.
type breaker struct {
	mu        sync.Mutex
	outcomes  []bool // last failoverWindow outcomes, oldest first
	open      bool
	probes    int
	lastProbe time.Time
	// trial holds an unclaimed trial: the probe streak was long enough to
	// believe the upstream is back, but no request has taken it to that
	// upstream yet. It is set by releaseTrial when a request claimed the trial
	// and then returned without ever reaching the upstream, so the next request
	// inherits it instead of the streak's work being thrown away.
	trial bool
	// trialActive is set while a claimed trial request is in flight. The split
	// is what makes the claim exclusive: exactly one request goes to the
	// profile's own upstream, and its outcome — not the probe's — decides
	// whether the circuit closes. Without it a burst arriving together would
	// all take the trial and all hit an upstream that may still be down.
	trialActive bool
}

// recordOutcome feeds one relay's outcome into the failover breaker. It runs
// BEFORE the response body is streamed (the ordering contract: a mid-stream
// stall counts as a HIT, never a strike). A transient status is a strike,
// anything else is a hit — the same classification failover_record applies.
// Only the profile's own upstream outcomes count (not the failover target's).
func (h *Handler) recordOutcome(statusCode int) {
	h.recordStrike(isTransient(statusCode))
}

// recordConnFailure records a failure that never produced a status at all: a
// refused connection, a DNS failure, or a read that stalled past the deadline.
// It is a strike. Nous behind Cloudflare rarely returns a clean 503 during an
// outage, it simply stops answering, so a breaker that only counted statuses
// would sit closed through the exact event it exists for.
func (h *Handler) recordConnFailure() {
	h.recordStrike(true)
}

func (h *Handler) recordStrike(bad bool) {
	if !failoverEnabled || h.cfg.Failover == "" {
		return
	}
	h.br.mu.Lock()
	defer h.br.mu.Unlock()
	b := &h.br
	b.outcomes = append(b.outcomes, bad)
	if len(b.outcomes) > failoverWindow {
		b.outcomes = b.outcomes[len(b.outcomes)-failoverWindow:]
	}
	strikes := 0
	for _, o := range b.outcomes {
		if o {
			strikes++
		}
	}
	if !b.open && strikes >= failoverThreshold() {
		b.open = true
		b.probes = 0
		// Say so. Python printed this and Go did not, which meant a profile
		// could silently spend an entire session on its failover target with
		// nothing in the log to explain the change in latency or cost.
		fmt.Printf("  [%s] failover: %d transient errors in the last %d requests, routing to %s\n",
			h.cfg.Name, strikes, len(b.outcomes), h.cfg.Failover)
		// Tripping resets the probe clock to now (mirroring failover_record's
		// probed_at = time.time()): the target gets a quiet FAILOVER_RECHECK
		// before the first re-probe. lastProbe=0 would probe the very next
		// request, which Python does not do.
		b.lastProbe = time.Now()
	}
}

// breakerOpen reports whether the profile's circuit is open (so requests route
// to the failover target) and, separately, whether THIS request is the armed
// trial and should go to the profile's own upstream instead.
//
// A clean probe is not enough to close the circuit. The probe is a minimal
// ping: it says the upstream accepts a connection, not that it can carry real
// load, and a probe that passes during a lull followed by the next heavy
// request failing is exactly the close-then-reopen flap this avoids. So a long
// enough probe streak only ARMS a trial; the first real request the profile's
// own upstream serves cleanly is what closes the circuit, via trialClose.
//
// The probe runs outside the lock.
func (h *Handler) breakerOpen() (open, trial bool) {
	if !failoverEnabled || h.cfg.Failover == "" {
		return false, false
	}
	h.br.mu.Lock()
	b := &h.br
	if !b.open {
		h.br.mu.Unlock()
		return false, false
	}
	// An already-armed trial takes this request to the profile's own upstream.
	// It stays armed until that request reports back, so a burst does not spend
	// the trial on several requests at once.
	if b.trial {
		b.trial = false      // claimed by this request
		b.trialActive = true // and in flight until trialClose reports back
		h.br.mu.Unlock()
		return true, true
	}
	now := time.Now()
	if !b.lastProbe.IsZero() && now.Sub(b.lastProbe) < time.Duration(failoverRecheck)*time.Second {
		h.br.mu.Unlock()
		return true, false // within recheck window: keep failing over
	}
	b.lastProbe = now
	h.br.mu.Unlock()

	if h.probeUpstream(h.cfg) {
		h.br.mu.Lock()
		b.probes++
		if b.probes >= failoverProbesToClose {
			// Arm and claim in one step: this request is the trial.
			b.trial = false
			b.trialActive = true
			b.probes = 0
			h.br.mu.Unlock()
			return true, true
		}
		h.br.mu.Unlock()
	} else {
		h.br.mu.Lock()
		b.probes = 0
		h.br.mu.Unlock()
	}
	return true, false
}

// trialClose reports the outcome of a trial request served by the profile's
// own upstream. A clean result closes the circuit; a transient one disarms the
// trial and leaves it open, so the next recheck has to earn a fresh probe
// streak. Returns true when the circuit closed.
func (h *Handler) trialClose(clean bool) bool {
	h.br.mu.Lock()
	defer h.br.mu.Unlock()
	b := &h.br
	if !b.trialActive {
		return false
	}
	b.trialActive = false
	if !clean {
		// Keep the circuit open and make the next attempt wait out a full
		// recheck interval rather than retrying immediately.
		b.lastProbe = time.Now()
		return false
	}
	b.open = false
	b.probes = 0
	b.outcomes = nil
	fmt.Printf("  [%s] failover: a real request was served cleanly, back on its own upstream\n",
		h.cfg.Name)
	return true
}

// releaseTrial hands a claimed trial back unclaimed, for a request that took it
// and then answered without ever contacting the upstream.
//
// Routing is decided at the top of the relay, but several paths return before
// the upstream is dialed: the ZDR 409 refusal, and the permission classifier,
// which in auto mode is the most frequent small request there is. Those learn
// nothing about whether the upstream recovered. Without this the streak's work
// is spent on a request that never tested anything, probes reset to zero, and
// the profile stays on its failover target for another full recheck interval
// before it can even try to come home.
func (h *Handler) releaseTrial() {
	h.br.mu.Lock()
	defer h.br.mu.Unlock()
	if h.br.trialActive {
		h.br.trialActive = false
		h.br.trial = true
	}
}

// probeUpstream sends a minimal POST /v1/messages to the profile's own
// upstream (the exact path real requests ride), mirroring _failover_probe.
// A clean 200 means completions recovered; anything else keeps the circuit
// open.
func (h *Handler) probeUpstream(cfg profiles.Profile) bool {
	model := cfg.Model
	if model == "" {
		model = "deepseek-v4-flash[1m]"
	}
	body := []byte(`{"model": "` + model + `", "max_tokens": 1, "thinking": {"type": "disabled"}, "messages": [{"role": "user", "content": "ping"}]}`)
	req, err := http.NewRequest(http.MethodPost, cfg.Upstream+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return false
	}
	// The probe key must fall back to the profile dir's settings.json, not just
	// DS4_KEY_<NAME> env — in production the keys live in the settings.json
	// files (api_key reads the file first, proxy.py:484-498). A probe with an
	// empty bearer gets a 401 and the circuit would never recover.
	key := os.Getenv("DS4_KEY_" + strings.ToUpper(cfg.Name))
	if key == "" {
		key = readKeyFromDir(cfg.Dir)
	}
	req.Header.Set("authorization", "Bearer "+key)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "curl/8.4.0")
	// The probe has its own absolute timeout (FAILOVER_PROBE_TIMEOUT), not the
	// relay's idle deadline — a stalled probe must not hold the request long.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(failoverProbeTimeout)*time.Second)
	defer cancel()
	resp, err := h.client.Do(req.WithContext(ctx))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
