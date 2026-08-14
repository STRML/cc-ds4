package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newAuthedHandler returns a handler plus the bearer value a client must send.
func newAuthedHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	t.Setenv("DS4_KEY_NOUS", "client-secret")
	cfg := testNous()
	cfg.Dir = t.TempDir()
	cfg.Upstream = "http://unused.invalid"
	return NewHandler(cfg, time.Minute), "Bearer client-secret"
}

// TestServeHTTPRejectsWrongCredential pins the local auth gate. The proxy holds
// real upstream keys, so anything else on the loopback port that can reach it
// must not be able to spend them.
func TestServeHTTPRejectsWrongCredential(t *testing.T) {
	h, good := newAuthedHandler(t)
	for _, tc := range []struct{ name, auth string }{
		{"absent", ""},
		{"wrong value", "Bearer nope"},
		{"missing bearer prefix", "client-secret"},
		{"prefix of the real key", "Bearer client-secre"},
		{"real key plus suffix", "Bearer client-secretX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			if tc.auth != "" {
				req.Header.Set("authorization", tc.auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != 401 {
				t.Errorf("status = %d, want 401", rr.Code)
			}
			if ct := rr.Header().Get("content-type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}
		})
	}
	// The good credential must not 401, or the test above proves nothing.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("authorization", good)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 401 {
		t.Fatal("the correct credential was rejected; the 401 cases above are meaningless")
	}
}

// TestServeHTTPMethodHandling pins the method surface: POST relays, GET is
// only /__spend, everything else is 501.
func TestServeHTTPMethodHandling(t *testing.T) {
	h, _ := newAuthedHandler(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead} {
		req := httptest.NewRequest(m, "/v1/messages", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 501 {
			t.Errorf("%s: status = %d, want 501", m, rr.Code)
		}
	}
}

// TestServeHTTPGetOnlyServesSpend pins that GET does not become an open
// read surface: any path other than /__spend is a 404, not a relay.
func TestServeHTTPGetOnlyServesSpend(t *testing.T) {
	h, _ := newAuthedHandler(t)
	for _, path := range []string{"/", "/v1/messages", "/__spend/../etc", "/__spendx"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 404 {
			t.Errorf("GET %s: status = %d, want 404", path, rr.Code)
		}
	}
}

// TestSpendNeedsNoClientCredential pins that /__spend is readable without the
// client key. The status line renders on every prompt and does not carry one;
// requiring it would blank the cost display rather than fail loudly.
func TestSpendNeedsNoClientCredential(t *testing.T) {
	h, _ := newAuthedHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/__spend", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 401 {
		t.Fatal("/__spend demanded a client credential the status line does not send")
	}
}

// TestServeHTTPCountsTrafficForIdleWatch pins the wiring between the handler
// and the idle watch. Every request must register, including the status line's
// GET: a proxy that exits under a live status line looks like a dead endpoint
// on the next render.
func TestServeHTTPCountsTrafficForIdleWatch(t *testing.T) {
	h, good := newAuthedHandler(t)
	before := DefaultTraffic.LastSeen()
	// A timestamp only moves if the clock ticked, so force a gap.
	time.Sleep(2 * time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/__spend", nil)
	req.Header.Set("authorization", good)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !DefaultTraffic.LastSeen().After(before) {
		t.Error("a served request did not register as activity")
	}
	if n := DefaultTraffic.InFlight(); n != 0 {
		t.Errorf("InFlight = %d after the request completed, want 0", n)
	}
}

// TestTrafficInFlightIsConcurrencySafe pins the counter under parallel load.
// A lost decrement leaves InFlight permanently positive, which disables idle
// exit silently: the proxy just never reclaims its memory and nobody notices.
func TestTrafficInFlightIsConcurrencySafe(t *testing.T) {
	tr := NewTraffic()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := tr.begin()
			done()
		}()
	}
	wg.Wait()
	if n := tr.InFlight(); n != 0 {
		t.Fatalf("InFlight = %d after all requests finished, want 0", n)
	}
}

// TestTrafficActivityExposesBothSignals pins the adapter WatchIdle consumes. A
// nil field there would panic the watcher on its first tick.
func TestTrafficActivityExposesBothSignals(t *testing.T) {
	tr := NewTraffic()
	act := tr.Activity()
	if act.LastSeen == nil || act.InFlight == nil {
		t.Fatal("Activity left a signal nil; WatchIdle would panic on it")
	}
	done := tr.begin()
	defer done()
	if act.InFlight() != 1 {
		t.Errorf("Activity().InFlight() = %d during a request, want 1", act.InFlight())
	}
}

// TestClaudeRunningProcScansEnviron covers the Linux liveness scan on any
// platform by pointing it at a fake proc tree. The real thing only runs on
// Linux, which is exactly why it shipped broken: nothing here exercised it.
func TestClaudeRunningProcScansEnviron(t *testing.T) {
	root := t.TempDir()
	writeProc := func(pid, environ string) {
		t.Helper()
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// environ is NUL-separated, and the real thing has a trailing NUL.
	writeProc("101", "PATH=/usr/bin\x00CLAUDE_CONFIG_DIR=/home/u/.claude-nous\x00")
	writeProc("102", "PATH=/usr/bin\x00")
	// A non-numeric entry (/proc has plenty) must be skipped, not parsed.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}

	mustScan := func(dir string) bool {
		t.Helper()
		got, err := claudeRunningProcIn(root, dir)
		if err != nil {
			t.Fatalf("scan of %s failed: %v", dir, err)
		}
		return got
	}
	if !mustScan("/home/u/.claude-nous") {
		t.Error("did not find a live process holding the config dir")
	}
	if mustScan("/home/u/.claude-or-ds4") {
		t.Error("matched a config dir no process is using")
	}
	// The prefix case: a -backup profile must not pin the plain one up. This is
	// what the BSD path got from its trailing word-boundary match, and the
	// NUL-separated compare is what preserves it here.
	writeProc("103", "CLAUDE_CONFIG_DIR=/home/u/.claude-nous-backup\x00")
	if !mustScan("/home/u/.claude-nous-backup") {
		t.Error("did not match the backup dir itself")
	}
	writeProc("101", "PATH=/usr/bin\x00") // drop the exact match
	if mustScan("/home/u/.claude-nous") {
		t.Error("a dir that is only a prefix of a live one must not match")
	}
}

// TestClaudeRunningProcMissingRootIsAnError pins the case the (bool, error)
// signature exists for. An unreadable /proc is "could not look", not "nothing
// is running" — and the Linux branch used to hardcode a nil error, so a
// container whose /proc is transiently unreadable answered "idle" on every poll
// and the fail-safe never fired.
func TestClaudeRunningProcMissingRootIsAnError(t *testing.T) {
	running, err := claudeRunningProcIn(filepath.Join(t.TempDir(), "absent"), "/anything")
	if err == nil {
		t.Error("an unreadable proc root reported success; the fail-safe cannot fire")
	}
	if running {
		t.Error("an unreadable proc root should not report a live process")
	}
}

// TestZDRDemandIsRefusedOnANonZDRRoute pins the fail-closed gate. A caller that
// marks a request as requiring ZDR must get an error from a route that cannot
// enforce it, never a silent forward to an upstream with no ZDR. Getting this
// wrong breaks a privacy contract without anyone seeing a symptom.
func TestZDRDemandIsRefusedOnANonZDRRoute(t *testing.T) {
	h, good := newAuthedHandler(t) // nous: ZDR false
	for _, tc := range []struct{ name, header, body string }{
		{"header form", "1", `{"model":"ds4-flash-xhigh","max_tokens":32000,"messages":[]}`},
		{"header true", "true", `{"model":"ds4-flash-xhigh","max_tokens":32000,"messages":[]}`},
		{"body field", "", `{"model":"ds4-flash-xhigh","ds4_require_zdr":true,"max_tokens":32000,"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			req.Header.Set("authorization", good)
			if tc.header != "" {
				req.Header.Set("x-ds4-require-zdr", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != 409 {
				t.Errorf("status = %d, want 409 (body %s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestZDRMarkersNeverRideUpstream pins that the proxy-local demand is consumed
// here, not forwarded. The header is hop-by-hop and the body field is not part
// of the Messages API, so either one reaching a provider is a leak of internal
// routing state into someone else's request log.
func TestZDRMarkersNeverRideUpstream(t *testing.T) {
	var gotHeader, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotHeader, gotBody = r.Header.Get("x-ds4-require-zdr"), string(b)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_OPENROUTER", "k")
	cfg := withUpstream(testOpenRouter(), up.URL) // ZDR true: the demand is satisfiable
	cfg.Dir = t.TempDir()
	h := NewHandler(cfg, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-xhigh","ds4_require_zdr":true,"max_tokens":32000,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	req.Header.Set("x-ds4-require-zdr", "1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d on a ZDR-capable route, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if gotHeader != "" {
		t.Errorf("x-ds4-require-zdr rode upstream as %q", gotHeader)
	}
	if strings.Contains(gotBody, "ds4_require_zdr") {
		t.Errorf("the ZDR body field rode upstream: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"zdr": true`) {
		t.Errorf("the ZDR provider block was not injected: %s", gotBody)
	}
}

// TestClassifierRouteKnob pins DS4_CLASSIFIER. It is a trust-boundary setting:
// "ds4" is an explicit decision to let DeepSeek judge whether a tool call is
// safe, and ignoring it silently overrides the user on the one knob where that
// matters most. It had no test, which is how it stayed unimplemented.
func TestClassifierRouteKnob(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{"", "anthropic"},
		{"anthropic", "anthropic"},
		{"zdr", "zdr"},
		{"ds4", "ds4"},
		{"nonsense", "anthropic"},
	} {
		t.Setenv("DS4_CLASSIFIER", tc.set)
		if got := classifierRoute(); got != tc.want {
			t.Errorf("DS4_CLASSIFIER=%q -> %q, want %q", tc.set, got, tc.want)
		}
	}
}

// TestClassifierDS4RouteStaysOnProfile pins the opt-out. With DS4_CLASSIFIER=ds4
// the gate must ride the profile's own upstream even when a subscription token
// is present, or the proxy sends the user's prompts somewhere they explicitly
// declined.
func TestClassifierDS4RouteStaysOnProfile(t *testing.T) {
	var anthropicHits, profileHits int
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHits++
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"cls"}`)
	}))
	defer anth.Close()
	prof := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		profileHits++
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"ds4"}`)
	}))
	defer prof.Close()

	old := classifierUpstream
	classifierUpstream = anth.URL
	t.Cleanup(func() { classifierUpstream = old })

	t.Setenv("DS4_KEY_NOUS", "k")
	t.Setenv("DS4_CLASSIFIER_TOKEN", "sk-ant-oat01-test")
	t.Setenv("DS4_CLASSIFIER", "ds4")

	cfg := withUpstream(testNous(), prof.URL)
	cfg.Dir = t.TempDir()
	h := NewHandler(cfg, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ds4-flash-xhigh","max_tokens":2048,"messages":[]}`))
	req.Header.Set("authorization", "Bearer k")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if anthropicHits != 0 {
		t.Errorf("classifier went to Anthropic despite DS4_CLASSIFIER=ds4 (%d hits)", anthropicHits)
	}
	if profileHits != 1 {
		t.Errorf("profile upstream hits = %d, want 1", profileHits)
	}
}

// TestNothinkBelowHonorsEnv pins that DS4_NOTHINK_BELOW is live. README and
// every profile doc tell users this knob moves the thinking cutoff, and
// install.sh sweeps it into the plist, so a hardcoded threshold made a
// documented setting silently do nothing.
func TestNothinkBelowHonorsEnv(t *testing.T) {
	if nothinkBelow != envInt("DS4_NOTHINK_BELOW", 8192) {
		t.Fatal("nothinkBelow is not derived from the environment")
	}
	if got := envInt("DS4_NOTHINK_BELOW", 8192); got != 8192 {
		t.Fatalf("default = %d, want 8192", got)
	}
	t.Setenv("DS4_NOTHINK_BELOW", "1234")
	if got := envInt("DS4_NOTHINK_BELOW", 8192); got != 1234 {
		t.Errorf("override = %d, want 1234", got)
	}
}

// TestZDRDisableKnob pins DS4_ZDR=0, the documented escape hatch for a
// provider-blocked build (OpenRouter answering 403 "error code: 1010"). It may
// only ever disable: a profile with no ZDR support must not gain it here.
func TestZDRDisableKnob(t *testing.T) {
	cfg := testOpenRouter()
	body := []byte(`{"model":"ds4-flash-xhigh","max_tokens":32000,"messages":[]}`)

	on, err := rewrite(body, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on), `"zdr": true`) {
		t.Fatalf("ZDR block missing by default: %s", on)
	}

	t.Setenv("DS4_ZDR", "0")
	off, err := rewrite(body, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), `"provider"`) {
		t.Errorf("DS4_ZDR=0 did not suppress the provider block: %s", off)
	}

	// It never enables: nous has no ZDR support in its row.
	t.Setenv("DS4_ZDR", "1")
	nous, err := rewrite(body, testNous(), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nous), `"provider"`) {
		t.Errorf("DS4_ZDR turned ZDR on for a profile that cannot do it: %s", nous)
	}
}
