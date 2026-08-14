package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

func TestBaseModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{"nitro suffix stripped", "deepseek/deepseek-v4-flash-0731:nitro", "deepseek/deepseek-v4-flash-0731"},
		{"no suffix untouched", "deepseek/deepseek-v4-flash-0731", "deepseek/deepseek-v4-flash-0731"},
		{"bare direct id untouched", "deepseek-v4-flash", "deepseek-v4-flash"},
		{"empty model", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := profiles.Profile{Model: tt.model}
			if got := baseModel(cfg); got != tt.want {
				t.Errorf("baseModel(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

// TestSpendDisabled404 pins the Spend gate: a profile with Spend unset 404s
// with an Anthropic-shaped error, matching do_GET's `if ... or not
// cfg["spend"]` in proxy.py.
func TestSpendDisabled404(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous"}, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__spend", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if _, ok := m["error"]; !ok {
		t.Errorf("missing 'error' in %s", rec.Body.String())
	}
}

// TestSpendNonSpendPath404 pins the GET-routing parity: a GET to any path
// other than /__spend 404s even on a Spend profile, exactly as Python's
// do_GET does (`if self.path != "/__spend"`).
func TestSpendNonSpendPath404(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous", Spend: true}, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/other/path", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for non-/__spend GET", rec.Code)
	}
}

// spendUpstream is a fake OpenRouter-shaped upstream: /v1/models/.../endpoints
// for pricing and /v1/credits for balance, both hit-counted so cache
// behaviour can be asserted. Any other path 404s.
type spendUpstream struct {
	*httptest.Server
	endpointsHits int32
	modelsHits    int32
	creditsHits   int32
	endpointsBody string
	endpointsCode int
	modelsBody    string
	modelsCode    int
	creditsBody   string
	creditsCode   int
	wantAuth      string
}

func newSpendUpstream(t *testing.T, model string) *spendUpstream {
	t.Helper()
	su := &spendUpstream{
		endpointsCode: 200,
		modelsCode:    200,
		creditsCode:   200,
		endpointsBody: `{"data": {"endpoints": [{"pricing": {"prompt": "0.0000001", "completion": "0.0000002", "input_cache_read": "0.00000005"}}]}}`,
		creditsBody:   `{"data": {"total_credits": 100, "total_usage": 12.5}}`,
	}
	endpointsPath := "/v1/models/" + model + "/endpoints"
	su.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if su.wantAuth != "" {
			if got := r.Header.Get("authorization"); got != su.wantAuth {
				t.Errorf("authorization header = %q, want %q (path %s)", got, su.wantAuth, r.URL.Path)
			}
		}
		switch r.URL.Path {
		case endpointsPath:
			atomic.AddInt32(&su.endpointsHits, 1)
			w.WriteHeader(su.endpointsCode)
			io.WriteString(w, su.endpointsBody)
		case "/v1/models":
			atomic.AddInt32(&su.modelsHits, 1)
			w.WriteHeader(su.modelsCode)
			io.WriteString(w, su.modelsBody)
		case "/v1/credits":
			atomic.AddInt32(&su.creditsHits, 1)
			w.WriteHeader(su.creditsCode)
			io.WriteString(w, su.creditsBody)
		default:
			w.WriteHeader(404)
		}
	}))
	return su
}

func spendTestCfg(t *testing.T, up *spendUpstream) profiles.Profile {
	t.Helper()
	cfg := withUpstream(testNous(), up.URL)
	cfg.Dir = t.TempDir()
	cfg.Spend = true
	t.Setenv("DS4_KEY_NOUS", "test-key")
	up.wantAuth = "Bearer test-key"
	return cfg
}

// TestSpendShape exercises the full /__spend round trip against a fake
// upstream that answers both pricing and credits, and pins the JSON shape:
// model/zdr always present, pricing/remaining/usage/week/week_partial
// present once their upstream calls resolve. The very first call already
// produces a ledger row, so week=0/week_partial=true shows up immediately —
// mirroring proxy.py's ledger_append-then-week_spend ordering in spend().
func TestSpendShape(t *testing.T) {
	up := newSpendUpstream(t, nousModel)
	defer up.Close()
	cfg := spendTestCfg(t, up)
	h := NewHandler(cfg, time.Minute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__spend", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var got spendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Model != nousModel {
		t.Errorf("model = %q, want %q", got.Model, nousModel)
	}
	if got.ZDR {
		t.Errorf("zdr = true, want false (testNous fixture)")
	}
	if got.Pricing == nil {
		t.Fatal("pricing missing from response")
	}
	if got.Pricing["prompt"] != 0.0000001 || got.Pricing["completion"] != 0.0000002 || got.Pricing["input_cache_read"] != 0.00000005 {
		t.Errorf("pricing = %+v, want the fake endpoint's rates", got.Pricing)
	}
	if got.Remaining == nil || *got.Remaining != 87.5 {
		t.Errorf("remaining = %v, want 87.5", got.Remaining)
	}
	if got.Usage == nil || *got.Usage != 12.5 {
		t.Errorf("usage = %v, want 12.5", got.Usage)
	}
	if got.Week == nil || *got.Week != 0 {
		t.Errorf("week = %v, want 0 (only sample so far)", got.Week)
	}
	if got.WeekPartial == nil || !*got.WeekPartial {
		t.Errorf("week_partial = %v, want true (ledger younger than a week)", got.WeekPartial)
	}
	if up.endpointsHits != 1 {
		t.Errorf("endpoints hits = %d, want 1", up.endpointsHits)
	}
	if up.creditsHits != 1 {
		t.Errorf("credits hits = %d, want 1", up.creditsHits)
	}
}

// TestSpendCachesAcrossCalls pins the two caching contracts spend.go must
// preserve for the status line's render budget: pricing is cached forever
// and credits is TTL-cached, so a second /__spend render inside the TTL
// costs zero upstream calls.
func TestSpendCachesAcrossCalls(t *testing.T) {
	up := newSpendUpstream(t, nousModel)
	defer up.Close()
	cfg := spendTestCfg(t, up)
	h := NewHandler(cfg, time.Minute)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__spend", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200 (body %s)", i, rec.Code, rec.Body.String())
		}
	}
	if up.endpointsHits != 1 {
		t.Errorf("endpoints hits after 2 renders = %d, want 1 (pricing caches forever)", up.endpointsHits)
	}
	if up.creditsHits != 1 {
		t.Errorf("credits hits after 2 renders = %d, want 1 (within CREDITS_TTL)", up.creditsHits)
	}
}

// TestCreditsTTLExpiry pins that a credits cache entry does expire: past
// creditsTTL, the next call must re-hit the upstream. creditsTTL is a var
// specifically so this test can shrink it instead of sleeping 60s.
func TestCreditsTTLExpiry(t *testing.T) {
	orig := creditsTTL
	creditsTTL = 10 * time.Millisecond
	defer func() { creditsTTL = orig }()

	up := newSpendUpstream(t, nousModel)
	defer up.Close()
	cfg := spendTestCfg(t, up)
	h := NewHandler(cfg, time.Minute)

	if _, _, ok := h.credits(); !ok {
		t.Fatal("first credits() call failed")
	}
	time.Sleep(20 * time.Millisecond)
	if _, _, ok := h.credits(); !ok {
		t.Fatal("second credits() call failed")
	}
	if up.creditsHits != 2 {
		t.Errorf("credits hits = %d, want 2 (TTL should have expired)", up.creditsHits)
	}
}

// TestPricingFallsBackToModelsCatalog pins the two-attempt pricing lookup:
// an endpoints 404 (e.g. Nous, which has no per-deployment sub-resource)
// falls back to the flat /v1/models catalog.
func TestPricingFallsBackToModelsCatalog(t *testing.T) {
	up := newSpendUpstream(t, nousModel)
	defer up.Close()
	up.endpointsCode = 404
	up.endpointsBody = `{"error": "not found"}`
	up.modelsBody = fmt.Sprintf(`{"data": [{"id": %q, "pricing": {"prompt": "0.000002", "completion": "0.000004"}}]}`, nousModel)
	cfg := spendTestCfg(t, up)
	h := NewHandler(cfg, time.Minute)

	p := h.pricing()
	if p == nil {
		t.Fatal("pricing() = nil, want the models-catalog fallback rates")
	}
	if p["prompt"] != 0.000002 || p["completion"] != 0.000004 {
		t.Errorf("pricing = %+v, want prompt=0.000002 completion=0.000004", p)
	}
	if up.endpointsHits != 1 || up.modelsHits != 1 {
		t.Errorf("endpointsHits=%d modelsHits=%d, want 1 and 1", up.endpointsHits, up.modelsHits)
	}
}

// TestPricingBothAttemptsFail pins the "no pricing available" path: both
// upstream calls fail, pricing() returns nil, and the /__spend body omits
// the "pricing" key entirely rather than serializing null or {}.
func TestPricingBothAttemptsFail(t *testing.T) {
	up := newSpendUpstream(t, nousModel)
	defer up.Close()
	up.endpointsCode = 500
	up.modelsCode = 500
	cfg := spendTestCfg(t, up)
	h := NewHandler(cfg, time.Minute)

	if p := h.pricing(); p != nil {
		t.Fatalf("pricing() = %+v, want nil", p)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__spend", nil))
	if strings.Contains(rec.Body.String(), `"pricing"`) {
		t.Errorf("body has a pricing key with no resolved pricing: %s", rec.Body.String())
	}
}

// TestPricingEmptyResultNeverCached pins a Python quirk this port
// deliberately preserves: proxy.py's cache read is `if _cache.get(...)`,
// which is falsy for an empty dict, so a successful-but-empty pricing
// result is never treated as a cache hit. Every call re-fetches.
func TestPricingEmptyResultIsRetriedButNotHammered(t *testing.T) {
	up := newSpendUpstream(t, nousModel)
	defer up.Close()
	// A pricing block that carries none of the three tracked keys: found,
	// but filters down to an empty map.
	up.endpointsBody = `{"data": {"endpoints": [{"pricing": {"unrelated_field": "1"}}]}}`
	cfg := spendTestCfg(t, up)
	h := NewHandler(cfg, time.Minute)

	// Python re-fetched an empty result on every call, because its cache read
	// was `if _cache.get(...)` and an empty dict is falsy. Ported verbatim,
	// that meant two upstream GETs at a 6s timeout each on EVERY status-line
	// render, inside a 1.5s budget — so an endpoints API answering without
	// rates showed the (proxy?) marker forever rather than once. Parity with a
	// deleted implementation is not worth that.
	h.pricing()
	h.pricing()
	h.pricing()
	if up.endpointsHits != 1 {
		t.Errorf("endpoints hits = %d, want 1 (an empty result must not re-fetch every render)",
			up.endpointsHits)
	}

	// It is a hold-off, not a giveup: once the interval lapses the next call
	// tries again, so a provider that starts answering with real rates is
	// picked up within the TTL.
	sc := h.spendState()
	sc.mu.Lock()
	sc.pricingFailedAt = time.Now().Add(-2 * creditsTTL)
	sc.mu.Unlock()

	h.pricing()
	if up.endpointsHits != 2 {
		t.Errorf("endpoints hits = %d, want 2 (the hold-off must expire)", up.endpointsHits)
	}
}

// TestCreditsMissingDataShape pins credits() failing closed on a response
// shape it doesn't recognize (no top-level "data" object).
func TestCreditsMissingDataShape(t *testing.T) {
	up := newSpendUpstream(t, nousModel)
	defer up.Close()
	up.creditsBody = `{"unexpected": true}`
	cfg := spendTestCfg(t, up)
	h := NewHandler(cfg, time.Minute)

	if _, _, ok := h.credits(); ok {
		t.Fatal("credits() ok = true for a body with no data object")
	}
}

func readLedgerLines(t *testing.T, cfg profiles.Profile) []string {
	t.Helper()
	raw, err := os.ReadFile(ledgerPath(cfg))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestLedgerAppendMinInterval pins ledger_append's sampling floor: a second
// sample inside ledgerMinInterval is dropped, and one at or past the
// boundary lands (proxy.py's guard is `<`, so the boundary itself appends).
func TestLedgerAppendMinInterval(t *testing.T) {
	cfg := profiles.Profile{Dir: t.TempDir()}
	const start = 1_000_000.0

	ledgerAppend(cfg, 1.0, start)
	if got := len(readLedgerLines(t, cfg)); got != 1 {
		t.Fatalf("after first append: %d lines, want 1", got)
	}

	ledgerAppend(cfg, 2.0, start+ledgerMinInterval-1)
	if got := len(readLedgerLines(t, cfg)); got != 1 {
		t.Fatalf("after a same-window append: %d lines, want 1 (dropped)", got)
	}

	ledgerAppend(cfg, 3.0, start+ledgerMinInterval)
	if got := len(readLedgerLines(t, cfg)); got != 2 {
		t.Fatalf("after the window elapsed: %d lines, want 2", got)
	}
}

// TestLedgerAppendNoDirIsSilent pins the OSError-swallowing contract: a Dir
// that cannot be written to must not panic or return an error to the
// caller, matching proxy.py's `except OSError: pass`.
func TestLedgerAppendNoDirIsSilent(t *testing.T) {
	cfg := profiles.Profile{Dir: "/nonexistent/ds4-spend-test-dir"}
	ledgerAppend(cfg, 1.0, 1000.0) // must not panic
}

// TestWeekSpend covers week_spend's three outcomes: no ledger at all, a
// ledger entirely younger than a week (partial), and one with a sample past
// the week boundary (a true rolling figure).
func TestWeekSpend(t *testing.T) {
	const now = 2_000_000.0

	tests := []struct {
		name         string
		rows         [][2]float64 // {t, usage}
		usage        float64
		wantHasValue bool
		wantSpend    float64
		wantPartial  bool
	}{
		{
			name:         "no ledger file",
			usage:        10,
			wantHasValue: false,
		},
		{
			name:         "every sample within the week: partial off the earliest",
			rows:         [][2]float64{{now - 3600, 5}, {now - 7200, 3}},
			usage:        10,
			wantHasValue: true,
			wantSpend:    10 - 3,
			wantPartial:  true,
		},
		{
			name:         "one sample past the week boundary: full rolling spend",
			rows:         [][2]float64{{now - weekSeconds - 100, 2}, {now - 3600, 8}},
			usage:        10,
			wantHasValue: true,
			wantSpend:    10 - 2,
			wantPartial:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := profiles.Profile{Dir: t.TempDir()}
			if tt.rows != nil {
				var sb strings.Builder
				for _, r := range tt.rows {
					fmt.Fprintf(&sb, `{"t": %v, "usage": %v}`+"\n", r[0], r[1])
				}
				if err := os.WriteFile(ledgerPath(cfg), []byte(sb.String()), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			spend, hasValue, partial := weekSpend(cfg, tt.usage, now)
			if hasValue != tt.wantHasValue {
				t.Fatalf("hasValue = %v, want %v", hasValue, tt.wantHasValue)
			}
			if !hasValue {
				return
			}
			if spend != tt.wantSpend {
				t.Errorf("spend = %v, want %v", spend, tt.wantSpend)
			}
			if partial != tt.wantPartial {
				t.Errorf("partial = %v, want %v", partial, tt.wantPartial)
			}
		})
	}
}

// TestWeekSpendSkipsMalformedLines pins the per-line tolerance week_spend
// needs: one bad row (missing "usage") must not abort the whole read, the
// same way proxy.py's per-line `except Exception: continue` behaves.
func TestWeekSpendSkipsMalformedLines(t *testing.T) {
	cfg := profiles.Profile{Dir: t.TempDir()}
	body := "not json at all\n" +
		`{"t": 1000, "usage": 5}` + "\n" +
		`{"t": 2000}` + "\n" + // missing usage
		`{"t": 3000, "usage": 9}` + "\n"
	if err := os.WriteFile(ledgerPath(cfg), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	spend, hasValue, partial := weekSpend(cfg, 20, 3000)
	if !hasValue {
		t.Fatal("hasValue = false, want true (two valid rows survive)")
	}
	// Neither valid row (t=1000, t=3000) is past the week boundary from
	// now=3000, so this is the partial branch off the earliest valid row.
	if !partial {
		t.Error("partial = false, want true")
	}
	if spend != 20-5 {
		t.Errorf("spend = %v, want %v", spend, 20-5)
	}
}
