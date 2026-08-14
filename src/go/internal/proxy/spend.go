package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// spend serves GET /__spend for one profile: the status line's per-render
// cost source. A profile with Spend off 404s exactly like any other unknown
// GET path (proxy.py's do_GET gate: `if self.path != "/__spend" or not
// cfg["spend"]`), so the two paths share notFound rather than diverging on
// error shape.
//
// The body always carries model + zdr. pricing/remaining/usage/week are
// added only when their upstream calls resolve, mirroring spend()'s `if p:`
// / `if c:` guards in proxy.py — a cold or unreachable upstream still answers
// with a usable (if partial) body instead of failing the request.
func (h *Handler) spend(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Spend {
		h.notFound(w)
		return
	}
	body, err := json.Marshal(h.spendPayload())
	if err != nil {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error": {"message": "spend encode failed"}}`)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// spendResponse is the /__spend JSON shape, matching spend()'s dict in
// proxy.py field-for-field. Optional fields are pointers/maps so a nil value
// omits the key entirely (Python's `if p:` / `if c:` never emit an empty or
// absent value) rather than serializing a zero.
type spendResponse struct {
	Model       string             `json:"model"`
	ZDR         bool               `json:"zdr"`
	Pricing     map[string]float64 `json:"pricing,omitempty"`
	Remaining   *float64           `json:"remaining,omitempty"`
	Usage       *float64           `json:"usage,omitempty"`
	Week        *float64           `json:"week,omitempty"`
	WeekPartial *bool              `json:"week_partial,omitempty"`
}

// spendPayload builds one /__spend body, mirroring spend() in proxy.py:
// pricing is a best-effort, forever-cached lookup; credits drives
// remaining/usage and, on success, samples the ledger and derives the
// rolling 7-day figure.
func (h *Handler) spendPayload() spendResponse {
	out := spendResponse{Model: h.cfg.Model, ZDR: h.cfg.ZDR}
	if p := h.pricing(); len(p) > 0 {
		out.Pricing = p
	}
	if total, usage, ok := h.credits(); ok {
		remaining := total - usage
		out.Remaining = &remaining
		out.Usage = &usage
		now := nowUnix()
		ledgerAppend(h.cfg, usage, now)
		if wk, hasWeek, partial := weekSpend(h.cfg, usage, now); hasWeek {
			out.Week = &wk
			out.WeekPartial = &partial
		}
	}
	return out
}

// ── upstream lookups (pricing, credits) ──────────────────────────────────

// getJSONTimeout mirrors get_json's default timeout=6 in proxy.py.
const getJSONTimeout = 6 * time.Second

// creditsTTL mirrors CREDITS_TTL in proxy.py: /__spend rides the status
// line's 1.5s render budget, so the credits lookup must not hit the
// upstream on every render. A var, not a const, so a test can shrink it to
// exercise expiry without a real 60s sleep.
var creditsTTL = 60 * time.Second

// userAgent mirrors UA in proxy.py: DS4_UA overrides the default, read once
// at package init like Python's module-load-time os.environ.get.
var userAgent = envUA()

func envUA() string {
	if v := os.Getenv("DS4_UA"); v != "" {
		return v
	}
	return "curl/8.4.0"
}

// baseModel mirrors base_model() in proxy.py: a profile's published model id
// with any OpenRouter variant suffix (":nitro") stripped. Pricing look-ups
// match on the exact upstream id, and the suffix is not a published id.
func baseModel(cfg profiles.Profile) string {
	m := cfg.Model
	if i := strings.IndexByte(m, ':'); i >= 0 {
		return m[:i]
	}
	return m
}

// apiKeyFor resolves the credential get_json's Bearer header carries. This
// repeats the env-then-file precedence already established in auth.go's
// authOK and breaker.go's probeUpstream (DS4_KEY_<NAME> wins over the
// profile dir's settings.json) rather than introducing a third order — the
// differential harness relies on the env override being able to shadow a
// real profile dir's key.
func apiKeyFor(cfg profiles.Profile) string {
	key := os.Getenv("DS4_KEY_" + strings.ToUpper(cfg.Name))
	if key == "" {
		key = readKeyFromDir(cfg.Dir)
	}
	return key
}

// getJSON mirrors get_json() in proxy.py: a bearer-authed GET against the
// profile's upstream, decoded as a JSON object. Any non-2xx status or
// decode failure is an error, matching urlopen's raise-on-HTTPError and the
// broad `except Exception` every caller wraps it in.
func (h *Handler) getJSON(cfg profiles.Profile, path string, timeout time.Duration) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(cfg.Upstream, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+apiKeyFor(cfg))
	req.Header.Set("user-agent", userAgent)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := h.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get %s: status %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// toFloat converts a decoded JSON value to float64. encoding/json decodes
// every JSON number into float64 already; the string case covers a
// provider that publishes price/credit fields as decimal strings, which
// Python's float() coerces silently and this must too.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// pricing mirrors pricing() in proxy.py: per-token rates, cached forever
// once a non-empty result lands. An OpenRouter-shaped endpoint list is
// tried first (the cheapest endpoint's rate is where ZDR routing mostly
// lands); a flat /v1/models catalog (Nous's shape) is the fallback. Either
// miss returns nil, so spendPayload omits the "pricing" key rather than
// serializing a hole.
//
// A successful-but-empty result (none of the three tracked keys present) is
// deliberately not cached: proxy.py's cache read is `if _cache.get(...)`,
// which is falsy for an empty dict, so an empty result is effectively never
// cached there either — every call re-fetches until a provider starts
// answering with real rates.
func (h *Handler) pricing() map[string]float64 {
	sc := h.spendState()
	now := time.Now()
	sc.mu.Lock()
	if sc.hasPricing {
		p := sc.pricing
		sc.mu.Unlock()
		return p
	}
	// A failed or empty lookup is held off for the same interval a credits
	// failure is. Caching only success meant an endpoints-API outage re-issued
	// BOTH lookups on every status-line render — two 6s timeouts inside a 1.5s
	// budget — so one upstream hiccup showed the (proxy?) marker on every
	// render instead of one. Same bug as the credits half of this handler, and
	// the parity argument for leaving it only held while Python still existed.
	if !sc.pricingFailedAt.IsZero() && now.Sub(sc.pricingFailedAt) < creditsTTL {
		sc.mu.Unlock()
		return nil
	}
	sc.mu.Unlock()

	out, found := h.fetchPricing(h.cfg, baseModel(h.cfg))
	if !found || len(out) == 0 {
		sc.mu.Lock()
		sc.pricingFailedAt = now
		sc.mu.Unlock()
		if !found {
			return nil
		}
		return out
	}
	sc.mu.Lock()
	sc.pricing = out
	sc.hasPricing = true
	sc.pricingFailedAt = time.Time{}
	sc.mu.Unlock()
	return out
}

// fetchPricing performs the two-attempt lookup pricing() wraps with the
// cache. found is false only when both attempts fail outright (network
// error, non-2xx, unexpected shape) — matching proxy.py's outer
// `except Exception: return None`. A malformed price on one endpoint fails
// that whole attempt (proxy.py's min()/next() raise on the first bad
// element, not just skip it), not just that one entry.
func (h *Handler) fetchPricing(cfg profiles.Profile, model string) (map[string]float64, bool) {
	if doc, err := h.getJSON(cfg, "/v1/models/"+model+"/endpoints", getJSONTimeout); err == nil {
		if pm, ok := extractEndpointsPricing(doc); ok {
			return filterPricingKeys(pm), true
		}
	}
	if doc, err := h.getJSON(cfg, "/v1/models", getJSONTimeout); err == nil {
		if pm, ok := extractModelsPricing(doc, model); ok {
			return filterPricingKeys(pm), true
		}
	}
	return nil, false
}

// extractEndpointsPricing picks the cheapest endpoint's pricing block from a
// /v1/models/{id}/endpoints response, mirroring
// `min(d["endpoints"], key=lambda e: float(e["pricing"]["prompt"]))["pricing"]`.
// Any endpoint missing a numeric prompt price fails the whole lookup, the
// same way Python's min() raises as soon as its key function hits one.
func extractEndpointsPricing(doc map[string]any) (map[string]any, bool) {
	data, _ := doc["data"].(map[string]any)
	endpoints, _ := data["endpoints"].([]any)
	if len(endpoints) == 0 {
		return nil, false
	}
	var best map[string]any
	bestPrice := 0.0
	for i, e := range endpoints {
		em, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		pm, ok := em["pricing"].(map[string]any)
		if !ok {
			return nil, false
		}
		price, ok := toFloat(pm["prompt"])
		if !ok {
			return nil, false
		}
		if i == 0 || price < bestPrice {
			bestPrice = price
			best = pm
		}
	}
	return best, true
}

// extractModelsPricing finds model's pricing block in a flat /v1/models
// catalog, mirroring `next(m["pricing"] for m in d if m.get("id") == model)`.
func extractModelsPricing(doc map[string]any, model string) (map[string]any, bool) {
	data, ok := doc["data"].([]any)
	if !ok {
		return nil, false
	}
	for _, m := range data {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := mm["id"].(string); id == model {
			pm, ok := mm["pricing"].(map[string]any)
			return pm, ok
		}
	}
	return nil, false
}

// pricingKeys is the fixed set of rate fields spend() publishes, mirroring
// the tuple in proxy.py's pricing(): `("prompt", "completion",
// "input_cache_read")`. Anything else a provider's pricing block carries is
// dropped.
var pricingKeys = []string{"prompt", "completion", "input_cache_read"}

func filterPricingKeys(pm map[string]any) map[string]float64 {
	out := make(map[string]float64, len(pricingKeys))
	for _, k := range pricingKeys {
		v, present := pm[k]
		if !present {
			continue
		}
		// proxy.py's `float(p[k])` is unguarded here and would crash the
		// whole request on a non-numeric value. Skipping it instead is a
		// deliberate divergence: a malformed single field should not take
		// down /__spend for every other field that parsed fine.
		if f, ok := toFloat(v); ok {
			out[k] = f
		}
	}
	return out
}

// credits mirrors credits() in proxy.py: (total, usage), TTL-cached. A
// cache miss that fails to fetch returns ok=false for that call even if a
// (now stale) prior value exists — proxy.py never serves a value past its
// TTL, it just returns None and lets the next render retry.
func (h *Handler) credits() (total, usage float64, ok bool) {
	sc := h.spendState()
	now := time.Now()
	sc.mu.Lock()
	if sc.credits != nil && now.Sub(sc.credits.at) < creditsTTL {
		c := *sc.credits
		sc.mu.Unlock()
		return c.total, c.usage, true
	}
	if !sc.creditsFailedAt.IsZero() && now.Sub(sc.creditsFailedAt) < creditsTTL {
		sc.mu.Unlock()
		return 0, 0, false
	}
	sc.mu.Unlock()

	// A failure is cached too, for the same TTL.
	//
	// Only success used to populate the cache, so a profile whose upstream has
	// no credits endpoint re-fetched on every single /__spend render — a doomed
	// GET with a getJSONTimeout of 6s, inside a status line whose whole budget
	// is 1.5s. That is the "(proxy?)" marker: not a broken proxy, a status line
	// waiting on a request that was never going to succeed. Nous is exactly
	// this case; it has Spend: true and no public credits endpoint.
	fail := func() (float64, float64, bool) {
		sc.mu.Lock()
		sc.credits = nil
		sc.creditsFailedAt = now
		sc.mu.Unlock()
		return 0, 0, false
	}
	doc, err := h.getJSON(h.cfg, "/v1/credits", getJSONTimeout)
	if err != nil {
		return fail()
	}
	data, isObj := doc["data"].(map[string]any)
	if !isObj {
		return fail()
	}
	tc, ok1 := toFloat(data["total_credits"])
	tu, ok2 := toFloat(data["total_usage"])
	if !ok1 || !ok2 {
		return fail()
	}
	sc.mu.Lock()
	sc.credits = &credEntry{at: now, total: tc, usage: tu}
	sc.creditsFailedAt = time.Time{}
	sc.mu.Unlock()
	return tc, tu, true
}

// ── per-handler cache ─────────────────────────────────────────────────────
//
// Handler is defined in handler.go, outside this file's ownership, so this
// state cannot live as a Handler field. It is keyed on the *Handler pointer
// instead: one Handler is built once per profile for the life of the
// process (NewHandler is called once at startup, per port), so pointer
// identity is exactly the "one cache per profile" scope proxy.py gets from
// its (name, kind) keyed global dict — and it isolates tests that build
// several Handlers against the same profile Name for the same reason
// proxy.py's real deployment never has to worry about.

type credEntry struct {
	at    time.Time
	total float64
	usage float64
}

type spendCache struct {
	mu         sync.Mutex
	pricing    map[string]float64
	hasPricing bool
	credits    *credEntry
	// creditsFailedAt is when the last lookup failed, so a profile with no
	// credits endpoint is not re-probed on every status-line render.
	creditsFailedAt time.Time
	// pricingFailedAt is the same guard for the pricing lookup, which costs two
	// upstream GETs rather than one.
	pricingFailedAt time.Time
}

// spendState returns this profile's cache. It is a Handler field: one Handler
// is built per profile at startup and lives for the process, so the field's
// lifetime is exactly the cache's intended scope. An earlier version keyed a
// package-level map by *Handler, which held every Handler ever built alive for
// the life of the process.
func (h *Handler) spendState() *spendCache {
	return &h.sc
}

// ── ledger (rolling 7-day spend) ──────────────────────────────────────────

// weekSeconds and ledgerMinInterval mirror WEEK and LEDGER_MIN_INTERVAL in
// proxy.py. Both are float64 seconds, not time.Duration: they are compared
// directly against the ledger's float epoch timestamps, the same unit
// proxy.py's time.time() produces.
const (
	weekSeconds       = 7 * 86400.0
	ledgerMinInterval = 300.0
)

// ledgerRecord is one line of spend-ledger.jsonl: {"t": <epoch>, "usage":
// <lifetime usage>}. Field order matches proxy.py's json.dumps({"t": ...,
// "usage": ...}) insertion order; nothing outside this package parses the
// file, so only round-trip fidelity through this same code matters.
type ledgerRecord struct {
	T     float64 `json:"t"`
	Usage float64 `json:"usage"`
}

func ledgerPath(cfg profiles.Profile) string {
	return filepath.Join(cfg.Dir, "spend-ledger.jsonl")
}

func nowUnix() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// ledgerAppend mirrors ledger_append() in proxy.py: sample lifetime usage at
// most once every ledgerMinInterval, so a rolling 7-day figure survives a
// proxy restart without growing the file on every /__spend render.
func ledgerAppend(cfg profiles.Profile, usage, now float64) {
	path := ledgerPath(cfg)
	last, ok := ledgerLastSample(path)
	if !ok {
		// The ledger exists but could not be read. proxy.py's whole function
		// body is one try wrapped in `except OSError: pass` — a read failure
		// aborts the append too, not just the read.
		return
	}
	if now-last < ledgerMinInterval {
		return
	}
	line, err := json.Marshal(ledgerRecord{T: now, Usage: usage})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(line)
	f.Write([]byte("\n"))
}

// ledgerLastSample returns the "t" of the most recent parseable ledger line,
// reading only the tail (proxy.py seeks to the last 4096 bytes rather than
// reading a potentially large file just to find the last line). ok is false
// only when the file exists but a read step failed; a missing file or one
// with no valid line reads as (0, true) — "no prior sample", proceed.
func ledgerLastSample(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, true
		}
		return 0, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, false
	}
	const tailWindow = 4096
	start := int64(0)
	if info.Size() > tailWindow {
		start = info.Size() - tailWindow
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, false
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return 0, false
	}

	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // e.g. the first line, truncated by the seek into its middle
		}
		tv, present := raw["t"]
		if !present {
			continue
		}
		if t, ok := toFloat(tv); ok {
			return t, true
		}
	}
	return 0, true // no valid line found; proxy.py's `last` stays 0.0
}

// weekSpend mirrors week_spend() in proxy.py: (spend over the trailing 7
// days, partial). partial is true when the ledger itself is younger than a
// week, so the figure is "spend since the ledger started" rather than a
// true rolling week. hasValue is false only when there is no ledger to read
// at all (missing file or zero parseable rows) — spendPayload's caller
// treats that the same way proxy.py's None does: omit week/week_partial.
func weekSpend(cfg profiles.Profile, usage, now float64) (spend float64, hasValue bool, partial bool) {
	raw, err := os.ReadFile(ledgerPath(cfg))
	if err != nil {
		return 0, false, true
	}

	type row struct{ t, u float64 }
	var rows []row
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		t, ok1 := toFloat(m["t"])
		u, ok2 := toFloat(m["usage"])
		if !ok1 || !ok2 {
			continue
		}
		rows = append(rows, row{t, u})
	}
	if len(rows) == 0 {
		return 0, false, true
	}

	// rows.sort() in proxy.py sorts the (t, usage) tuples lexicographically:
	// primarily by time, usage only breaks a tie. Match that ordering so
	// which same-timestamp row lands last (and so gets picked as "most
	// recent stale sample" below) is deterministic the same way.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].t != rows[j].t {
			return rows[i].t < rows[j].t
		}
		return rows[i].u < rows[j].u
	})

	cutoff := now - weekSeconds
	haveOld := false
	var lastOld row
	for _, r := range rows {
		if r.t <= cutoff {
			lastOld = r
			haveOld = true
		}
	}
	if haveOld {
		return maxFloat(0, usage-lastOld.u), true, false
	}
	return maxFloat(0, usage-rows[0].u), true, true
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
