package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// retryBackoff is the base sleep between retries, in seconds, scaled by
// attempt number: 1.5s * (attempt+1), matching RETRY_BACKOFF in proxy.py. It
// is a variable so tests can shrink the sleep; production always sees 1.5.
// DS4_RETRY_BACKOFF overrides it (the differential harness pins it to 0).
var retryBackoff = 1.5

// retryAttemptsOverride mirrors RETRY_ATTEMPTS in proxy.py: an env override
// the differential harness sets to 1 for the failover case so Python's
// in-proxy retries don't muddy the breaker strike accounting.
var retryAttemptsOverride = 0

func init() {
	if v := os.Getenv("DS4_RETRY_BACKOFF"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			retryBackoff = f
		}
	}
	if v := os.Getenv("DS4_RETRY_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			retryAttemptsOverride = n
		}
	}
}

// skipRelayHeaders are hop-by-hop and proxy-owned headers dropped when the
// client's request is forwarded upstream, matching the filter in proxy.py's
// _relay. Host and content-length are re-derived; accept-encoding is dropped
// because DisableCompression already negotiates it; x-ds4-require-zdr is the
// proxy-local signal and must not ride out.
var skipRelayHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"accept-encoding":   true,
	"x-ds4-require-zdr": true,
	"connection":        true,
}

// retryAttempts returns how many upstream attempts a request gets. It mirrors
// should_retry in proxy.py: only subagent tiers (anything but ds4-xhigh)
// retry in-proxy. The main thread (ds4-xhigh) has its own 10x-backoff retry,
// so retrying here would double up. A non-sentinel model (a failed-over
// request, or a raw Anthropic model) is treated as retryable by the same
// rule.
func retryAttempts(origTier string) int {
	if retryAttemptsOverride > 0 {
		return retryAttemptsOverride
	}
	if origTier != "ds4-xhigh" {
		return 3
	}
	return 1
}

// modelFromJSON extracts the top-level "model" string from a request body
// without rewriting it. Marshal runs the closure between parse and emit with
// the tree intact, so it sees the client-sent tier before any rewrite remaps
// it. A non-JSON body or a non-string model yields "".
func modelFromJSON(body []byte) string {
	model := ""
	_, err := jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		if m := root.Get("model"); m != nil && m.IsString() {
			model = m.String()
		}
	})
	if err != nil {
		return ""
	}
	return model
}

// relay mirrors _relay + _stream in proxy.py: it rewrites the body, POSTs it
// to the profile's upstream, retries transient statuses for the retryable
// tiers, records the outcome for the breaker BEFORE streaming, and streams the
// response body back with flush.
// effectiveUpstream returns the upstream + auth key to use, applying the
// failover target when the breaker is open (mirroring failover_effective).
// Returns the effective profile (own, or the target with the FAILOVER_MODEL
// remap and the target's upstream/key overrides) plus the auth key.
func (h *Handler) effectiveProfile() (profiles.Profile, string) {
	eff := h.cfg
	key := os.Getenv("DS4_KEY_" + strings.ToUpper(eff.Name))
	if key == "" {
		key = readKeyFromDir(eff.Dir)
	}
	if !h.breakerOpen() {
		return eff, key
	}
	// Failover: route to the target profile's upstream, key, and config. The
	// target's upstream honors the DS4_UPSTREAM_<NAME> override (the harness
	// points both sides at fakes). The direct target takes real model names
	// and ignores reasoning_effort — its cfg.Model is "" (None), so rewrite()
	// leaves the sentinel and FAILOVER_MODEL remaps it to flash.
	target := h.cfg.Failover
	for _, p := range profiles.All() {
		if p.Name == target {
			eff = p
			eff.FailoverTarget = true
			if o := os.Getenv("DS4_UPSTREAM_" + strings.ToUpper(p.Name)); o != "" {
				eff.Upstream = o
			}
			key = os.Getenv("DS4_KEY_" + strings.ToUpper(p.Name))
			if key == "" {
				key = readKeyFromDir(p.Dir)
			}
			return eff, key
		}
	}
	return eff, key
}

func (h *Handler) relay(w http.ResponseWriter, r *http.Request, body []byte, upstreamURL string) {
	// The classifier is the small ds4-high call that gates every tool call in
	// auto mode. It is already an Anthropic-shaped request, so it is forwarded
	// BEFORE the ds4 rewrite touches it (the sentinel/effort logic must not see
	// it), mirroring proxy.py:821-846. Fail open to ds4 on any failure so auto
	// mode never bricks. A request that demands ZDR (x-ds4-require-zdr header)
	// is excluded: its ZDR provider block is injected by rewrite() on the ZDR
	// route, and the classifier relay's whitelist cannot carry it — the marker
	// is a routing demand, so the request stays on its ZDR route.
	if !isZDRRequest(r) && h.isClassifier(body) {
		if tok := classifierToken(); tok != "" {
			if h.relayClassifier(body, classifierUpstream, tok, w) {
				return
			}
		}
	}

	// The client-sent tier is captured before the rewrite remaps payload["model"]
	// to the target's literal id, so a failed-over main-loop request does not
	// look retryable.
	origTier := modelFromJSON(body)

	// Effective profile + key: the profile's own, or the failover target's
	// when the breaker is open. The rewrite uses the EFFECTIVE config so a
	// failed-over request is remapped for the target (FAILOVER_MODEL, no
	// reasoning_effort), exactly like Python's failover routing.
	effCfg, effKey := h.effectiveProfile()
	effUpstream := effCfg.Upstream
	rewritten, err := rewrite(body, effCfg)
	if err == nil {
		body = rewritten
	}
	effURL := strings.TrimRight(effUpstream, "/") + strings.TrimPrefix(upstreamURL, strings.TrimRight(h.cfg.Upstream, "/"))

	attempts := retryAttempts(origTier)
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		// A fresh request per attempt with the retained body re-wound; the
		// reader is re-created so a consumed body does not poison a retry.
		req, err := http.NewRequest(http.MethodPost, effURL, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		copyHeaders(req.Header, r.Header)
		req.Header.Set("content-length", strconv.Itoa(len(body)))
		if req.Header.Get("content-type") == "" {
			req.Header.Set("content-type", "application/json")
		}
		if req.Header.Get("user-agent") == "" {
			req.Header.Set("user-agent", "curl/8.4.0")
		}
		if effKey != "" {
			req.Header.Set("authorization", "Bearer "+effKey)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			// Connection-level failure: never retried (proxy.py breaks).
			lastErr = err
			break
		}
		if isTransient(resp.StatusCode) && attempt+1 < attempts {
			resp.Body.Close()
			time.Sleep(backoff(attempt))
			continue
		}
		// Breaker-before-stream: the outcome is recorded while the upstream
		// is still being read. A mid-stream stall therefore counts the
		// request as a HIT (the upstream served a response), not a strike.
		h.recordOutcome(resp.StatusCode)
		streamResponse(w, resp, effUpstream)
		return
	}
	// Pre-first-byte failure: 502 "proxy upstream failure".
	w.Header().Set("content-type", "application/json")
	w.Header().Set("x-ds4-upstream", h.cfg.Upstream)
	w.Header().Set("connection", "close")
	w.WriteHeader(502)
	fmt.Fprintf(w, `{"error": {"message": "proxy upstream failure: %v"}}`, lastErr)
}

func backoff(attempt int) time.Duration {
	return time.Duration(retryBackoff * float64(attempt+1) * float64(time.Second))
}

func isTransient(code int) bool {
	switch code {
	case 429, 500, 502, 503, 524, 529:
		return true
	}
	return false
}

// copyHeaders forwards the client's headers minus the hop-by-hop set,
// mirroring the loop in proxy.py's _relay.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if skipRelayHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// streamResponse copies an upstream response to the client: status, filtered
// headers, x-ds4-upstream, connection: close, then the body with flush. It
// mirrors _stream in proxy.py. A mid-stream error after the header has been
// written cannot be turned into a clean 502 — the partial 200 already went
// out, so the connection just dies.
func streamResponse(w http.ResponseWriter, resp *http.Response, via string) {
	h := w.Header()
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "transfer-encoding" || lk == "content-encoding" || lk == "connection" {
			continue
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	h.Set("x-ds4-upstream", via)
	h.Set("connection", "close")
	w.WriteHeader(resp.StatusCode)
	defer resp.Body.Close()
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
}
