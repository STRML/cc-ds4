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

// errPeekLimit is how much of a non-2xx body is read so the failover decision
// can inspect it. Big enough for any provider's JSON error envelope, small
// enough that a huge error page is not buffered.
const errPeekLimit = 8192

// mainLoopTier is what ANTHROPIC_MODEL is set to out of the box.
const mainLoopTier = "ds4-pro-xhigh"

// retryAttempts decides how hard to retry a transient upstream error.
//
// The main loop runs its own 10x-backoff retry, so retrying in the proxy as
// well would stack two backoffs on an upstream that is already struggling.
// Subagents have no such retry and die with "Execution error" on a raw
// forward, so the proxy absorbs the error for them.
//
// The split is by FAMILY, not by one sentinel name. The main loop rides pro
// (ds4-pro-xhigh by default, ds4-pro-medium once the user picks opus) and
// subagents ride flash. Keying off a single name meant switching the main loop
// to opus silently re-enabled the double retry this exists to prevent.
//
// origTier is the client-sent model captured before any failover remap: the
// remap rewrites the body's model to the target's literal id, which would
// otherwise make every failed-over request look like a subagent call.
func retryAttempts(origTier string) int {
	if retryAttemptsOverride > 0 {
		return retryAttemptsOverride
	}
	if sen, ok := sentinelTable[origTier]; ok && sen.family == "pro" {
		return 1
	}
	return 3
}

// relayUserAgent is the User-Agent every relayed request carries. Python sent
// this unconditionally for the same reason: Cloudflare 403s Claude Code's own
// agent string in front of some upstreams.
func relayUserAgent() string {
	if ua := os.Getenv("DS4_UA"); ua != "" {
		return ua
	}
	return "curl/8.4.0"
}

// prepareUpstreamHeaders builds the outbound header set from the client's.
//
// failedOver is the security-relevant input. The client authenticated to this
// proxy with the ORIGIN profile's key, and on a failed-over request the proxy
// is talking to a different provider. That credential must not ride along, so
// it is dropped before the target's own key (if any) is applied. Dropping it
// unconditionally means a target with no key sends an unauthenticated request
// that fails, rather than one authenticated with someone else's secret.
func prepareUpstreamHeaders(dst, src http.Header, failedOver bool, key string, bodyLen int) {
	copyHeaders(dst, src)
	if failedOver {
		dst.Del("authorization")
	}
	dst.Set("content-length", strconv.Itoa(bodyLen))
	if dst.Get("content-type") == "" {
		dst.Set("content-type", "application/json")
	}
	// Unconditional, overriding the client's. Claude Code always sends its own
	// User-Agent and Cloudflare 403s it in front of Nous Portal and
	// intermittently OpenRouter, so setting this only when absent would never
	// actually apply.
	dst.Set("user-agent", relayUserAgent())
	if key != "" {
		dst.Set("authorization", "Bearer "+key)
	}
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

// effectiveProfile returns the profile a request should be served by, its key,
// and whether this request is the breaker's armed trial.
//
// A trial routes to the profile's OWN upstream even though the circuit is
// open: the whole point is to find out whether that upstream can carry a real
// request again. The caller reports the outcome back through trialClose.
func (h *Handler) effectiveProfile() (profiles.Profile, string, bool) {
	eff := h.cfg
	key := os.Getenv("DS4_KEY_" + strings.ToUpper(eff.Name))
	if key == "" {
		key = readKeyFromDir(eff.Dir)
	}
	open, trial := h.breakerOpen()
	if !open || trial {
		return eff, key, trial
	}
	// Failover: route to the target profile's upstream, key, and config.
	// failoverTarget owns the rules for what counts as a usable target, so the
	// routing decision and the rescue path cannot drift apart.
	if target, tkey, ok := h.failoverTarget(); ok {
		return target, tkey, false
	}
	// No usable target: stay home and keep failing in the way the caller can
	// actually understand.
	return eff, key, false
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
	// The per-request ZDR demand is proxy-local: read it, strip it from the
	// body, and refuse outright if this route cannot honor it. Failing closed
	// is the whole point. Forwarding such a request to an upstream with no ZDR
	// would break the caller's privacy contract silently, which is worse than
	// an error they can see.
	requires, body := requiresZDR(r, body)

	// Routing is decided before the gate, because the gate has to ask about the
	// profile that will actually serve the request. A ZDR-capable profile with
	// a failover target could otherwise pass the gate and then be relayed to a
	// target that enforces nothing.
	effCfg, effKey, trial := h.effectiveProfile()
	failedOver := effCfg.Name != h.cfg.Name
	effUpstream := effCfg.Upstream

	if requires && (!effCfg.ZDR || !zdrEnabled()) {
		// This request is answered here and never reaches an upstream, so it
		// cannot be the trial. Hand it back for the next one.
		if trial {
			h.releaseTrial()
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(409)
		io.WriteString(w, `{"error": {"message": "request requires ZDR, but this route cannot enforce it"}}`)
		return
	}

	// The classifier gates every tool call in auto mode. It is already an
	// Anthropic-shaped request, so it is forwarded BEFORE the ds4 rewrite
	// touches it. DS4_CLASSIFIER picks the route: anthropic (default) sends it
	// to the subscription, zdr sends it to the or-ds4 OpenRouter route with the
	// ZDR block on, and ds4 keeps it on this profile's own upstream. Every
	// route fails open to ds4 so auto mode never bricks.
	//
	// A request that demands ZDR is excluded: the classifier relay rebuilds the
	// body from a whitelist that cannot carry the provider block, and the
	// marker is a routing demand, so it stays on its ZDR route.
	// A classifier answered off-profile also never reaches this upstream, so it
	// releases the trial on the way out for the same reason the ZDR gate does.
	if !requires && h.isClassifier(body) {
		switch classifierRoute() {
		case "zdr":
			if body2, url, key := h.orDS4Endpoint(body); url != "" {
				if h.sendClassifier(body2, url, key, w) {
					if trial {
						h.releaseTrial()
					}
					return
				}
			}
			// zdr falls open to anthropic, then ds4 (Python's order).
			fallthrough
		case "anthropic":
			if tok := classifierToken(); tok != "" {
				if h.relayClassifier(body, classifierUpstream, tok, w) {
					if trial {
						h.releaseTrial()
					}
					return
				}
			}
		case "ds4":
			// Explicit opt-out: the gate stays on this profile's own upstream
			// even when a subscription token is present.
		}
	}

	// The client-sent tier is captured before the rewrite remaps payload["model"]
	// to the target's literal id, so a failed-over main-loop request does not
	// look retryable.
	origTier := modelFromJSON(body)
	// Kept for the rescue path. rewrite() below swaps the sentinel for the
	// EFFECTIVE profile's literal model id, and a rescue re-runs the rewrite
	// for the target — but against an already-rewritten body no sentinel
	// remains, so the target's family map cannot match and only the hardcoded
	// failoverModel net does anything. That coupling is invisible from the
	// table: change a profile's Model without adding a failoverModel entry and
	// every rescued request forwards an id the target does not serve.
	clientBody := body

	// Effective profile + key: the profile's own, or the failover target's
	// when the breaker is open. The rewrite uses the EFFECTIVE config so a
	// failed-over request is remapped for the target (FAILOVER_MODEL, no
	// reasoning_effort), exactly like Python's failover routing.
	rewritten, err := rewrite(body, effCfg, effortOverride(h.cfg))
	if err == nil {
		body = rewritten
	}

	// Vision runs after the rewrite and before the body is serialized upstream,
	// the same position as Python's call site, so no image-shaped block ever
	// reaches an endpoint that cannot read one. It fails open internally: every
	// leaf failure becomes a text placeholder, and the only error it returns is
	// a JSON parse failure, which leaves the body untouched exactly as a failed
	// rewrite does.
	// Vision uses the ORIGIN profile, not the effective one: the cache lives
	// under the profile's dir, and following the failover target would miss
	// every cached image, re-spawn a billed child for each, and orphan the new
	// entries when the circuit closes.
	if visioned, verr := applyVision(body, h.cfg); verr == nil {
		body = visioned
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
		prepareUpstreamHeaders(req.Header, r.Header, failedOver, effKey, len(body))
		resp, err := h.client.Do(req)
		if err != nil {
			// Connection-level failure: never retried (Python breaks too).
			// It still counts as a strike. A refused connection or a stalled
			// read is the failure mode the breaker exists for — nous behind
			// Cloudflare rarely returns a clean 503, it just stops answering —
			// so skipping the record here would leave the circuit shut for the
			// one outage shape it most needs to catch.
			h.recordConnFailure()
			if trial {
				h.trialClose(false)
			}
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
		// A non-2xx small enough to be an error body is read so the failover
		// decision can look at it: some refusals carry the reason in the body
		// rather than the status (a drained balance arrives as a 404).
		//
		// The peek is taken without consulting ContentLength. A chunked error
		// response declares -1, and gating on a declared length meant Nous's
		// drained-balance 404 went unread whenever it arrived chunked — the
		// body never reached creditExhausted, no failover was attempted, and
		// the caller got back the bare 404 that reads as a missing model. That
		// is the exact bug this path exists to prevent, so nothing about
		// detecting it may depend on the upstream's choice of framing.
		//
		// What is read is pushed back in front of the body rather than
		// replacing it, so a response larger than the peek still streams whole.
		var errBody []byte
		if resp.StatusCode >= 400 {
			errBody, _ = io.ReadAll(io.LimitReader(resp.Body, errPeekLimit))
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(errBody), resp.Body), resp.Body}
		}
		drained := creditExhausted(resp.StatusCode, errBody)
		// Only the profile's OWN upstream tells us anything about the profile's
		// health. While failed over, every response came from the target, and
		// folding those into this breaker's window makes it a record of the
		// wrong host. Nothing misbehaves today — the circuit is already open so
		// no trip can fire, and closing nils the window — but the window is
		// read by name elsewhere, and a future reader would inherit the bug
		// with nothing to warn it.
		if !failedOver {
			if drained {
				// Not transient, but the upstream is unusable until someone pays.
				h.recordConnFailure()
			} else {
				h.recordOutcome(resp.StatusCode)
			}
		}
		if trial {
			// This request went to the profile's own upstream to prove it can
			// carry real load. Its result, not the probe's, closes the circuit.
			h.trialClose(!isTransient(resp.StatusCode) && !drained)
		}
		if (isTransient(resp.StatusCode) || drained) && !failedOver {
			// The profile's own upstream is failing and a target may be able to
			// serve this. Learning that the upstream is still down is the point
			// of a trial, but the client should not pay for the lesson, and the
			// same applies to the window before the breaker has tripped.
			if h.rescueViaFailover(w, r, clientBody, upstreamURL, requires) {
				resp.Body.Close()
				return
			}
		}
		if drained {
			// Nothing could serve it. Say what is actually wrong rather than
			// letting a 404 reach the CLI, which renders any 404 naming a model
			// as "that model may not exist or you may not have access to it" —
			// and sends the reader off to check model names and API keys when
			// the real answer is that the account is out of money.
			resp.Body.Close()
			// effCfg, not h.cfg: while failed over the empty account is the
			// TARGET's, and naming the origin sends the user to top up a
			// profile that has money in it while nothing changes.
			writeDrainedBalance(w, effCfg.Name)
			return
		}
		streamResponse(w, resp, effUpstream)
		return
	}
	// Every attempt failed before a first byte. A trial that cannot even
	// connect is the clearest possible evidence the upstream is still down.
	if trial {
		h.trialClose(false)
	}
	if !failedOver && h.rescueViaFailover(w, r, clientBody, upstreamURL, requires) {
		return
	}
	// Pre-first-byte failure: 502 "proxy upstream failure".
	w.Header().Set("content-type", "application/json")
	// Name the upstream that was actually contacted, not the profile's own:
	// a 502 while failed over otherwise points an operator at a host the
	// request never reached.
	w.Header().Set("x-ds4-upstream", effUpstream)
	w.Header().Set("connection", "close")
	w.WriteHeader(502)
	fmt.Fprintf(w, `{"error": {"message": "proxy upstream failure: %v"}}`, lastErr)
}

// rescueViaFailover re-sends a request to the profile's failover target after
// the profile's own upstream failed it.
//
// Without this, two situations hand the caller a hard error while a working
// target sits idle: the window before the breaker has tripped, and every trial
// request afterwards. The trial exists to find out whether the upstream
// recovered, which is worth doing, but the answer should cost a retry rather
// than a failed session turn.
//
// Returns true when the target served the request. The breaker is deliberately
// NOT told about the outcome here: these attempts measure the target, and only
// the profile's own upstream decides whether its circuit opens or closes.
func (h *Handler) rescueViaFailover(w http.ResponseWriter, r *http.Request, body []byte, upstreamURL string, requiresZDR bool) bool {
	if h.cfg.Failover == "" {
		return false
	}
	target, key, ok := h.failoverTarget()
	if !ok {
		return false
	}
	if requiresZDR && (!target.ZDR || !zdrEnabled()) {
		// The 409 gate ran against the profile chosen at the top of the relay,
		// and this changes the profile serving the request afterwards. Rescuing
		// a ZDR-demanding request onto a target that enforces nothing would walk
		// around a privacy gate that already passed. Let the origin's error
		// stand instead.
		return false
	}
	// Re-run the rewrite for the target: its family map, its ZDR policy, and
	// the model remap all differ from the origin's.
	out := body
	if rewritten, err := rewrite(body, target, effortOverride(h.cfg)); err == nil {
		out = rewritten
	}
	if visioned, verr := applyVision(out, h.cfg); verr == nil {
		out = visioned
	}
	url := strings.TrimRight(target.Upstream, "/") +
		strings.TrimPrefix(upstreamURL, strings.TrimRight(h.cfg.Upstream, "/"))

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(out))
	if err != nil {
		return false
	}
	prepareUpstreamHeaders(req.Header, r.Header, true, key, len(out))
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	var errBody []byte
	if resp.StatusCode >= 400 {
		errBody, _ = io.ReadAll(io.LimitReader(resp.Body, errPeekLimit))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(errBody), resp.Body), resp.Body}
	}
	if isTransient(resp.StatusCode) || creditExhausted(resp.StatusCode, errBody) {
		// The target is failing too, or is out of credit itself. Let the caller
		// surface the origin's own error rather than swapping in a second, more
		// confusing one — and in the drained case that caller answers with the
		// 402 that says what is actually wrong, which is the whole point of the
		// path. Streaming the target's own 404 here would put back exactly the
		// "that model may not exist" misdiagnosis.
		resp.Body.Close()
		return false
	}
	streamResponse(w, resp, target.Upstream)
	return true
}

// failoverTarget resolves this profile's failover target and key, or ok=false
// when there is none installed.
func (h *Handler) failoverTarget() (profiles.Profile, string, bool) {
	for _, p := range profiles.All() {
		if p.Name != h.cfg.Failover {
			continue
		}
		if info, err := os.Stat(p.Dir); err != nil || !info.IsDir() {
			return profiles.Profile{}, "", false
		}
		p.FailoverTarget = true
		if o := os.Getenv("DS4_UPSTREAM_" + strings.ToUpper(p.Name)); o != "" {
			p.Upstream = o
		}
		key := os.Getenv("DS4_KEY_" + strings.ToUpper(p.Name))
		if key == "" {
			key = readKeyFromDir(p.Dir)
		}
		if key == "" {
			// The directory exists but carries no credential — a profile
			// created and never finished, most often. Relaying there strips the
			// client's authorization (it belongs to this profile, not that one)
			// and sends nothing in its place, so every request comes back 401
			// from a provider the user never configured, hiding the origin's
			// actual outage behind an auth error. A target we cannot
			// authenticate to is not a target.
			return profiles.Profile{}, "", false
		}
		return p, key, true
	}
	return profiles.Profile{}, "", false
}

// writeDrainedBalance answers with the reason, in the Anthropic error shape the
// CLI already parses.
//
// The status is 402 rather than the upstream's 404 on purpose: 402 is what
// "you are out of credit" means, and it does not collide with the CLI's
// model-not-found handling.
func writeDrainedBalance(w http.ResponseWriter, profile string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(402)
	fmt.Fprintf(w, `{"type": "error", "error": {"type": "insufficient_credit", "message": `+
		`"The %s profile's account is out of credit, and no failover target could serve this `+
		`request. This is a billing problem, not a bad model name. Top up the provider account, `+
		`or switch profiles."}}`, profile)
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
