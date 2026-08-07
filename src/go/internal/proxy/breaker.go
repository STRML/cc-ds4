package proxy

import (
	"bytes"
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
	failoverEnabled        = os.Getenv("DS4_FAILOVER") != "0"
	failoverWindow         = envInt("DS4_FAILOVER_WINDOW", 12)
	failoverRate           = envFloat("DS4_FAILOVER_RATE", 0.25)
	failoverRecheck        = envInt("DS4_FAILOVER_RECHECK", 60)
	failoverProbesToClose  = envInt("DS4_FAILOVER_PROBES_TO_CLOSE", 3)
	failoverProbeTimeout   = envInt("DS4_FAILOVER_PROBE_TIMEOUT", 6)
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
}

// recordOutcome feeds one relay's outcome into the failover breaker. It runs
// BEFORE the response body is streamed (the ordering contract: a mid-stream
// stall counts as a HIT, never a strike). A transient status is a strike,
// anything else is a hit — the same classification failover_record applies.
// Only the profile's own upstream outcomes count (not the failover target's).
func (h *Handler) recordOutcome(statusCode int) {
	if !failoverEnabled || h.cfg.Failover == "" {
		return
	}
	bad := isTransient(statusCode)
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
		// Tripping resets the probe clock to now (mirroring failover_record's
		// probed_at = time.time()): the target gets a quiet FAILOVER_RECHECK
		// before the first re-probe. lastProbe=0 would probe the very next
		// request, which Python does not do.
		b.lastProbe = time.Now()
	}
}

// breakerOpen reports whether the profile's circuit is open and requests
// should route to the failover target. When open, a request whose recheck
// interval has lapsed performs one probe (mirroring failover_effective): a
// clean probe increments the streak, and PROBES_TO_CLOSE clean probes close
// the circuit. The probe runs outside the lock.
func (h *Handler) breakerOpen() bool {
	if !failoverEnabled || h.cfg.Failover == "" {
		return false
	}
	h.br.mu.Lock()
	b := &h.br
	if !b.open {
		h.br.mu.Unlock()
		return false
	}
	now := time.Now()
	if !b.lastProbe.IsZero() && now.Sub(b.lastProbe) < time.Duration(failoverRecheck)*time.Second {
		h.br.mu.Unlock()
		return true // within recheck window: keep failing over
	}
	b.lastProbe = now
	h.br.mu.Unlock()

	if h.probeUpstream(h.cfg) {
		h.br.mu.Lock()
		b.probes++
		if b.probes >= failoverProbesToClose {
			b.open = false
			b.probes = 0
			b.outcomes = nil
		}
		h.br.mu.Unlock()
	} else {
		h.br.mu.Lock()
		b.probes = 0
		h.br.mu.Unlock()
	}
	return true
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
	req.Header.Set("authorization", "Bearer "+os.Getenv("DS4_KEY_"+strings.ToUpper(cfg.Name)))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "curl/8.4.0")
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
