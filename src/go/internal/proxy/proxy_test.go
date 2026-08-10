package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// TestRewriteSentinelToModel is the brief's primary parity test: a ds4-high
// sentinel becomes the profile's real model plus reasoning_effort "high", and
// reasoning_effort appends at the end of the object — the byte output of
// Python's json.dumps, verified against the real rewrite().
func TestRewriteSentinelToModel(t *testing.T) {
	cfg := profiles.Profile{Name: "nous", Model: "deepseek/deepseek-v4-flash-0731"}
	body := []byte(`{"model": "ds4-high", "max_tokens": 32000, "thinking": {"type": "adaptive"}, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "deepseek/deepseek-v4-flash-0731", "max_tokens": 32000, "thinking": {"type": "adaptive"}, "messages": [], "reasoning_effort": "high"}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRewriteAnthropicLiteralModel pins proxy.py's third model branch: a
// literal Anthropic id that bypassed the sentinel system (sonnet, opus,
// claude-*) is remapped to the profile's model so nothing bills real Anthropic
// rates, without adding reasoning_effort (Python adds none on this branch).
func TestRewriteAnthropicLiteralModel(t *testing.T) {
	cfg := profiles.Profile{Name: "nous", Model: "deepseek/deepseek-v4-flash-0731"}
	body := []byte(`{"model": "claude-sonnet-5", "max_tokens": 32000, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "deepseek/deepseek-v4-flash-0731", "max_tokens": 32000, "messages": []}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRewriteDoesNotRemapProfileLiteral pins the negative side of the
// Anthropic-literal branch: the profile's own upstream model id already names
// the target, matches no Anthropic substring, and is left untouched (and no
// reasoning_effort is invented).
func TestRewriteDoesNotRemapProfileLiteral(t *testing.T) {
	cfg := profiles.Profile{Name: "nous", Model: "deepseek/deepseek-v4-flash-0731"}
	body := []byte(`{"model": "deepseek/deepseek-v4-flash-0731", "max_tokens": 32000, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "deepseek/deepseek-v4-flash-0731", "max_tokens": 32000, "messages": []}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRewriteThinkingDisabled pins the NOTHINK_BELOW branch: max_tokens at or
// below 8192 replaces the thinking block with {"type":"disabled"}, and the
// placeholder injection does NOT run even on a direct profile (thinking is
// off, the endpoint stops asking for the block).
func TestRewriteThinkingDisabled(t *testing.T) {
	cfg := profiles.Profile{Name: "direct", Model: "", Inject: true}
	body := []byte(`{"model": "ds4-high", "max_tokens": 8192, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// thinking is a newly-set key, so it appends at the end of the object
	// (Python dict insertion order) — it does not jump before "messages".
	want := `{"model": "ds4-high", "max_tokens": 8192, "messages": [], "thinking": {"type": "disabled"}}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRewriteMaxOutClamp pins the MaxOut clamp: a request over the profile's
// cap is clamped down. The clamped value (65536) is still above the no-think
// budget, so thinking is NOT disabled.
func TestRewriteMaxOutClamp(t *testing.T) {
	cfg := profiles.Profile{Name: "openrouter", Model: "deepseek/deepseek-v4-flash-0731", MaxOut: 65536}
	body := []byte(`{"model": "ds4-max", "max_tokens": 131072, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "deepseek/deepseek-v4-flash-0731", "max_tokens": 65536, "messages": [], "reasoning_effort": "max"}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRewriteThinkingInjection pins the placeholder-injection pass: a direct
// profile inserts a thinking block at position 0 of an assistant message that
// has a tool_use block and no thinking block.
func TestRewriteThinkingInjection(t *testing.T) {
	cfg := profiles.Profile{Name: "direct", Model: "", Inject: true}
	body := []byte(`{"model": "ds4-high", "max_tokens": 32000, "messages": [{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "ls"}}]}]}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "ds4-high", "max_tokens": 32000, "messages": [{"role": "assistant", "content": [{"type": "thinking", "thinking": "(elided)", "signature": "ds4-proxy"}, {"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "ls"}}]}]}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRewriteThinkingInjectionSkipsAlreadyThinking pins that a message with an
// existing thinking block is left alone.
func TestRewriteThinkingInjectionSkipsAlreadyThinking(t *testing.T) {
	cfg := profiles.Profile{Name: "direct", Model: "", Inject: true}
	body := []byte(`{"model": "ds4-high", "max_tokens": 32000, "messages": [{"role": "assistant", "content": [{"type": "thinking", "thinking": "real"}, {"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "ls"}}]}]}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "ds4-high", "max_tokens": 32000, "messages": [{"role": "assistant", "content": [{"type": "thinking", "thinking": "real"}, {"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "ls"}}]}]}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRewriteMissingMaxTokensKeepsThinking pins that a request with no
// max_tokens is not treated as "0": Python only disables thinking when the
// key is present and an integer, so a classifier or minimal request keeps its
// thinking block.
func TestRewriteMissingMaxTokensKeepsThinking(t *testing.T) {
	cfg := profiles.Profile{Name: "nous", Model: "deepseek/deepseek-v4-flash-0731"}
	body := []byte(`{"model": "ds4-high", "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "disabled") {
		t.Errorf("rewrite disabled thinking without max_tokens: %s", got)
	}
	if !strings.Contains(string(got), `"reasoning_effort": "high"`) {
		t.Errorf("reasoning_effort missing: %s", got)
	}
}

// TestRewriteZDR pins the OpenRouter ZDR block: provider gets zdr + deny, and
// the low-context endpoint is pinned into ignore.
func TestRewriteZDR(t *testing.T) {
	cfg := profiles.Profile{Name: "openrouter", Model: "deepseek/deepseek-v4-flash-0731", ZDR: true}
	body := []byte(`{"model": "ds4-low", "max_tokens": 32000, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "deepseek/deepseek-v4-flash-0731", "max_tokens": 32000, "messages": [], "reasoning_effort": "low", "provider": {"zdr": true, "data_collection": "deny", "ignore": ["Io Net"]}}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRetryAttempts pins the retry distinction: ds4-high retries (3 attempts),
// ds4-xhigh does not (1 attempt), and an unknown tier follows the subagent
// default (3).
func TestRetryAttempts(t *testing.T) {
	cases := []struct {
		tier string
		want int
	}{
		{"ds4-high", 3},
		{"ds4-max", 3},
		{"ds4-low", 3},
		{"ds4-xhigh", 1},
		{"", 3},
		{"deepseek/deepseek-v4-flash-0731", 3},
	}
	for _, c := range cases {
		if got := retryAttempts(c.tier); got != c.want {
			t.Errorf("retryAttempts(%q) = %d, want %d", c.tier, got, c.want)
		}
	}
}

// TestServeHTTPMethodGate pins the method dispatch: GET is the spend endpoint
// (404 today), PUT/PATCH are 501, and a POST with a bad key is 401.
func TestServeHTTPMethodGate(t *testing.T) {
	cfg := profiles.Profile{Name: "nous"}
	h := NewHandler(cfg, 0)

	// GET -> not found (Task 6 owns the real spend body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/__spend", nil))
	if rr.Code != 404 {
		t.Errorf("GET /__spend = %d, want 404", rr.Code)
	}

	// unsupported method -> 501
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/v1/messages", nil))
	if rr.Code != 501 {
		t.Errorf("PUT = %d, want 501", rr.Code)
	}

	// POST without a key -> 401
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rr.Code != 401 {
		t.Errorf("POST no key = %d, want 401", rr.Code)
	}
}

// TestRewriteInjectDoesNotRunWhenThinkingDisabled pins that even a direct
// profile skips the placeholder pass once thinking is disabled — otherwise
// every small request would gain a pointless thinking block.
func TestRewriteInjectDoesNotRunWhenThinkingDisabled(t *testing.T) {
	cfg := profiles.Profile{Name: "direct", Model: "", Inject: true}
	body := []byte(`{"model": "ds4-high", "max_tokens": 1000, "messages": [{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "ls"}}]}]}`)
	got, err := rewrite(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model": "ds4-high", "max_tokens": 1000, "messages": [{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "ls"}}]}], "thinking": {"type": "disabled"}}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}

// TestRelayPreFirstByteStall pins the pre-first-byte stall: an upstream that
// accepts the connection but never sends a response must yield the 502
// "proxy upstream failure" (mirroring Python's no-response path).
func TestRelayPreFirstByteStall(t *testing.T) {
	// An upstream that never writes a response: the client's idle-deadline
	// wrapper fires on the upstream read and the relay 502s.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the connection open without responding; the relay's idle
		// deadline (RELAY_TIMEOUT) bounds this. A short deadline keeps the
		// test fast.
		time.Sleep(2 * time.Second)
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_NOUS", "test")
	cfg := profiles.Profile{Name: "nous", Upstream: up.URL, Model: "deepseek/deepseek-v4-flash-0731"}
	h := NewHandler(cfg, 200*time.Millisecond) // short relay idle timeout

	body := `{"model": "ds4-high", "max_tokens": 32000, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")

	rr := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != 502 {
		t.Fatalf("status = %d, want 502 (body %s)", rr.Code, rr.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Errorf("502 took %v — the idle deadline should have fired, not the upstream sleep", elapsed)
	}
	if !strings.Contains(rr.Body.String(), "proxy upstream failure") {
		t.Errorf("body = %q, want proxy-upstream-failure", rr.Body.String())
	}
}

// TestRelayMidStreamStall pins the mid-stream stall: an upstream that sends
// headers + one chunk then goes silent must NOT become a clean 502 — the
// partial 200 already went out, so the connection just dies.
func TestRelayMidStreamStall(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Second) // hang after the partial body
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_NOUS", "test")
	cfg := profiles.Profile{Name: "nous", Upstream: up.URL, Model: "deepseek/deepseek-v4-flash-0731"}
	h := NewHandler(cfg, 200*time.Millisecond)

	body := `{"model": "ds4-high", "max_tokens": 32000, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// The header was already committed as a 200; the body has the partial SSE.
	// It must NOT be a 502 (a clean 502 after headers would be a parity break).
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (partial, not 502; body %q)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "event: message_start") {
		t.Errorf("body = %q, want the partial SSE frame", rr.Body.String())
	}
}

// TestRelayRewritesAndStreams pins the whole relay path against a live
// httptest upstream: a POST with a sentinel model arrives rewritten (real
// model + reasoning_effort), proxy-local headers are stripped, and the 200 is
// streamed back with the x-ds4-upstream header.
func TestRelayRewritesAndStreams(t *testing.T) {
	var mu sync.Mutex
	var gotReqBody string
	var gotAuth, gotZDR string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotReqBody = string(b)
		gotAuth = r.Header.Get("authorization")
		gotZDR = r.Header.Get("x-ds4-require-zdr")
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id": "msg_ok", "content": []}`)
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_NOUS", "test")
	cfg := profiles.Profile{Name: "nous", Upstream: up.URL, Model: "deepseek/deepseek-v4-flash-0731"}
	h := NewHandler(cfg, 0)

	body := `{"model": "ds4-high", "max_tokens": 32000, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")
	req.Header.Set("x-ds4-require-zdr", "1")
	req.Header.Set("x-custom", "stays")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("x-ds4-upstream") != up.URL {
		t.Errorf("x-ds4-upstream = %q, want %q", rr.Header().Get("x-ds4-upstream"), up.URL)
	}
	if rr.Body.String() != `{"id": "msg_ok", "content": []}` {
		t.Errorf("body = %q", rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(gotReqBody, `"model": "deepseek/deepseek-v4-flash-0731"`) {
		t.Errorf("upstream model not rewritten: %s", gotReqBody)
	}
	if !strings.Contains(gotReqBody, `"reasoning_effort": "high"`) {
		t.Errorf("reasoning_effort missing: %s", gotReqBody)
	}
	if gotZDR != "" {
		t.Errorf("zdr marker leaked upstream: %q", gotZDR)
	}
	// The client's Authorization is the profile's own key, so it rides
	// through to the upstream (Python forwards it unless failed-over).
	if gotAuth != "Bearer test" {
		t.Errorf("authorization = %q, want Bearer test forwarded", gotAuth)
	}
}

// TestRelayRetriesTransientForHigh pins the retry contract end-to-end: a
// ds4-high request gets three attempts against an upstream that 503s twice
// then succeeds, and only the final attempt is streamed to the client.
func TestRelayRetriesTransientForHigh(t *testing.T) {
	retryBackoff = 0.001
	t.Cleanup(func() { retryBackoff = 1.5 })
	var mu sync.Mutex
	attempts := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			http.Error(w, `{"error": {"message": "overloaded"}}`, 503)
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id": "msg_ok"}`)
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_NOUS", "test")
	cfg := profiles.Profile{Name: "nous", Upstream: up.URL, Model: "deepseek/deepseek-v4-flash-0731"}
	h := NewHandler(cfg, 0)

	body := `{"model": "ds4-high", "max_tokens": 32000, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 after retries", rr.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (ds4-high retries)", attempts)
	}
}

// TestRelayRetries429ForHigh pins that 429 is in the transient set (the brief's
// list included it) and retries like any other transient status.
func TestRelayRetries429ForHigh(t *testing.T) {
	retryBackoff = 0.001
	t.Cleanup(func() { retryBackoff = 1.5 })
	var mu sync.Mutex
	attempts := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 2 {
			http.Error(w, `{"error": {"message": "rate limited"}}`, 429)
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id": "msg_ok"}`)
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_NOUS", "test")
	cfg := profiles.Profile{Name: "nous", Upstream: up.URL, Model: "deepseek/deepseek-v4-flash-0731"}
	h := NewHandler(cfg, 0)

	body := `{"model": "ds4-high", "max_tokens": 32000, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 after 429 retry", rr.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

// TestRelayDoesNotRetryXHigh pins that a ds4-xhigh request forwards the first
// transient response without retrying in-proxy (the main thread owns its own
// backoff).
func TestRelayDoesNotRetryXHigh(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, `{"error": {"message": "overloaded"}}`, 503)
	}))
	defer up.Close()

	t.Setenv("DS4_KEY_NOUS", "test")
	cfg := profiles.Profile{Name: "nous", Upstream: up.URL, Model: "deepseek/deepseek-v4-flash-0731"}
	h := NewHandler(cfg, 0)

	body := `{"model": "ds4-xhigh", "max_tokens": 32000, "messages": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503 forwarded", rr.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (ds4-xhigh does not retry)", attempts)
	}
}

// TestStreamResponsePinsHeaders pins that _stream's header contract: the

// TestStreamResponsePinsHeaders pins that _stream's header contract: the
// upstream's transfer-encoding / content-encoding / connection are dropped,
// x-ds4-upstream carries the serving gateway, and connection: close is set.
func TestStreamResponsePinsHeaders(t *testing.T) {
	body := strings.NewReader("data: [DONE]\n")
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":      {"application/json"},
			"Transfer-Encoding": {"chunked"},
			"Content-Encoding":  {"gzip"},
			"Connection":        {"keep-alive"},
		},
		Body:          io.NopCloser(body),
		ContentLength: int64(body.Len()),
	}
	rr := httptest.NewRecorder()
	streamResponse(rr, resp, "https://inference-api.nousresearch.com")
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Header().Get("x-ds4-upstream") != "https://inference-api.nousresearch.com" {
		t.Errorf("x-ds4-upstream = %q", rr.Header().Get("x-ds4-upstream"))
	}
	if rr.Header().Get("connection") != "close" {
		t.Errorf("connection = %q, want close", rr.Header().Get("connection"))
	}
	if rr.Header().Get("Transfer-Encoding") != "" {
		t.Errorf("transfer-encoding leaked: %q", rr.Header().Get("Transfer-Encoding"))
	}
	if rr.Header().Get("Content-Encoding") != "" {
		t.Errorf("content-encoding leaked: %q", rr.Header().Get("Content-Encoding"))
	}
	if rr.Body.String() != "data: [DONE]\n" {
		t.Errorf("body = %q", rr.Body.String())
	}
}

// TestCopyHeadersDropsProxyHeaders pins the header filter: host,
// content-length, accept-encoding, the proxy-local zdr marker, and connection
// never reach the upstream.
func TestCopyHeadersDropsProxyHeaders(t *testing.T) {
	src := http.Header{
		"Host":              {"example.com"},
		"Authorization":     {"Bearer secret"},
		"Content-Length":    {"99"},
		"Accept-Encoding":   {"gzip"},
		"X-Ds4-Require-Zdr": {"1"},
		"Connection":        {"keep-alive"},
		"X-Custom":          {"stays"},
	}
	dst := http.Header{}
	copyHeaders(dst, src)
	if dst.Get("Host") != "" {
		t.Errorf("host leaked")
	}
	if dst.Get("Content-Length") != "" {
		t.Errorf("content-length leaked")
	}
	if dst.Get("Accept-Encoding") != "" {
		t.Errorf("accept-encoding leaked")
	}
	if dst.Get("X-Ds4-Require-Zdr") != "" {
		t.Errorf("zdr marker leaked")
	}
	if dst.Get("Connection") != "" {
		t.Errorf("connection leaked")
	}
	if dst.Get("Authorization") != "Bearer secret" {
		t.Errorf("authorization dropped")
	}
	if dst.Get("X-Custom") != "stays" {
		t.Errorf("custom header dropped")
	}
}
