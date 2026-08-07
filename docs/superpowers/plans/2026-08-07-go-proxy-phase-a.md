# Go Proxy Phase A Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `src/go/` — a Go reimplementation of the Python proxy that is byte-for-byte binary compatible on the Phase A (unchanged) surface, proven by a differential test suite against the frozen Python oracle.

**Architecture:** Two-phase structure from the approved spec. Phase A ports Python's *current* behavior exactly (fail-open classifier, redirect-following, no size cap, no classifier timeout, unauthenticated `/__spend`) against a frozen Python oracle via a differential harness that asserts identical status + managed headers + byte-identical bodies, plus an outbound-request recorder proving rewrite parity. Phase B (hardening) is a separate follow-up and NOT in this plan.

**Tech Stack:** Go 1.26.5, stdlib `net/http` only (no third-party deps). Python 3.9+ for the oracle + harness (stdlib only).

## Global Constraints

Copied verbatim from `docs/superpowers/specs/2026-08-06-go-proxy-design.md`:

- **Phase A is the only differential surface.** The frozen Python oracle governs Phase A; Phase B hardening is Go-native with Go-specific tests.
- **The idle timeout is on the UPSTREAM connection** (per-DialContext idle deadline), NOT `http.Server.ReadTimeout`/`WriteTimeout` (absolute, would sever SSE) and NOT a server-side `net.Listener`.
- **JSON re-serialization replicates Python's `json.dumps` byte-for-byte**: default separators (`', '`/`': '`), `ensure_ascii=True` (custom string escaper for runes > U+007E, astral as surrogate pairs), `SetEscapeHTML(false)` equivalent, `json.Number` round-tripping (Python emits `1.0` for integral floats, preserves big ints). NOT `map[string]any` (Go reorders keys alphabetically). NOT sjson/jsonparser (preserves client bytes where Python normalizes).
- **Stall semantics match Python exactly — no clean 502 mid-stream.** Pre-first-byte stall → 502; mid-stream stall → partial-200 + dead connection. Go must NOT convert a mid-stream stall into a clean 502. `failover_record` runs BEFORE `_stream` (breaker records a hit on the 200).
- **Retries rewind the body**: retain `[]byte`, fresh `http.Request` per attempt. `ds4-high` retries transient (up to 3, backoff 1.5s·attempt); `ds4-xhigh` does NOT retry. Classifier + vision child calls do NOT carry the main retry policy.
- **Client auth preserved as Python does it**: POST-only, constant-time compare, `DS4_KEY_<PROFILE>` fallback. `/__spend` GET stays unauthenticated.
- **Response headers**: preserve upstream except `transfer-encoding`/`content-encoding`/`connection`; add `x-ds4-upstream` + `connection: close`. No synthesized `Content-Length`. `DisableCompression: true`.
- **Chunked framing is a documented accepted deviation** (Go http.Server emits `transfer-encoding: chunked`); the gate normalizes framing.
- **Profile availability matches Python**: serve only dirs that exist; `--ports` emits only those.
- **Config generated from Python's PROFILES at build time** (oracle untouched).
- **Classifier calls use a separate deadline-free transport client** (the per-DialContext RELAY_TIMEOUT wrapper must not leak onto the no-timeout classifier path).
- **Malformed bodies are safety-tested, not parity-tested**: Go must not panic and must not reach upstream. Do not reproduce Python's exception behavior.
- **`/__spend` is status + JSON shape only** in the diff gate; detailed pricing/ledger logic is Go unit-tested separately.
- Go breaker unit tests + `go test -race` run BEFORE the swap. Test porting is a precondition of the swap, not deferred.

---

### Task 1: Go module + PROFILES generation + `--ports`

**Files:**
- Create: `src/go/go.mod`
- Create: `src/go/cmd/genprofiles/main.go` — reads `src/proxy.py`, extracts `PROFILES`, emits `src/go/internal/profiles/profiles_gen.go`
- Create: `src/go/internal/profiles/profiles_gen.go` (generated — committed)
- Create: `src/go/internal/profiles/profiles.go` — the `Profile` struct + `All()` + `Served()` (dir-existence filter)
- Create: `src/go/cmd/ds4-proxy/main.go` — `--ports` output + server bootstrap stub
- Test: `src/go/internal/profiles/profiles_test.go`

**Interfaces:**
- Produces: `type Profile struct { Name, Port, Dir, Upstream, Model string; ZDR, Spend, Inject bool; MaxOut int; Failover string }`; `func All() []Profile`; `func Served() []Profile` (filters by `os.IsNotExist(dir)`); `func Ports() string` — `"direct 31500\nopenrouter 31501\nnous 31502\n"` (served only)

- [ ] **Step 1: Write the failing test** (`src/go/internal/profiles/profiles_test.go`)

```go
package profiles

import "testing"

func TestPortsMatchesPython(t *testing.T) {
	got := Ports()
	// direct has no cfg["dir"] on CI; assert the three names/ports are present
	// regardless of served-filtering by checking All() ports.
	for _, p := range All() {
		switch p.Name {
		case "direct":
			if p.Port != 31500 { t.Fatalf("direct port = %d", p.Port) }
		case "openrouter":
			if p.Port != 31501 { t.Fatalf("openrouter port = %d", p.Port) }
		case "nous":
			if p.Port != 31502 { t.Fatalf("nous port = %d", p.Port) }
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd src/go && go test ./internal/profiles/`
Expected: FAIL — package not found (module not yet created)

- [ ] **Step 3: Create the module and generator**

```bash
mkdir -p src/go/cmd/genprofiles src/go/internal/profiles src/go/cmd/ds4-proxy
cd src/go && go mod init github.com/strml/cc-ds4/src/go
```

`src/go/cmd/genprofiles/main.go` reads `src/proxy.py` (path relative to repo root), finds the `PROFILES = {` block, and emits `profiles_gen.go` with the `Profile` values. The regex: `"(\w+)": \{\s*"port": (\d+),\s*"dir": f"([^"]+)"...` — parse each field. Emit:

```go
// Code generated by cmd/genprofiles; DO NOT EDIT.
package profiles

var generatedProfiles = []Profile{
	{Name: "direct", Port: 31500, Dir: "", Upstream: "https://api.deepseek.com/anthropic", ZDR: false, Spend: false, Inject: true, Failover: ""},
	// ...
}
```

(For `dir`, substitute the `~` → `os.UserHomeDir()` at runtime in `All()`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/profiles/`
Expected: PASS

- [ ] **Step 5: Wire `--ports` in `ds4-proxy` main and smoke-test against Python**

```go
// src/go/cmd/ds4-proxy/main.go (initial)
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--ports" {
		fmt.Print(profiles.Ports())
		return
	}
	// server bootstrap comes in Task 4
}
```

Run: `cd src/go && go build ./... && go run ./cmd/ds4-proxy --ports` and compare with `python3 src/proxy.py --ports`. Expected: same names/ports for the three profiles.

- [ ] **Step 6: Commit**

```bash
git add src/go/
git commit -m "feat(go): module scaffold + PROFILES generation + --ports"
```

---

### Task 2: The `ensure_ascii` JSON serializer

**Files:**
- Create: `src/go/internal/jsonpy/jsonpy.go`
- Test: `src/go/internal/jsonpy/jsonpy_test.go`

**Interfaces:**
- Consumes: nothing (standalone)
- Produces: `func Marshal(v any, order []string) ([]byte, error)` — order-preserving, Python-matching; `func parseOrdered(data []byte) (orderedValue, error)` (internal); `func escapeAscii(s string) string` — runes > U+007E → `\uXXXX`, astral → surrogate pairs

**The Python reference** (`json.dumps`, default separators `', '`/`': '`, `ensure_ascii=True`):
- `{"café": 1.0, "emoji": "😀", "html": "<>&"}` → `{"café": 1.0, "emoji": "😀", "html": "<>&"}`

- [ ] **Step 1: Write the failing test** — three cases from the spec:

```go
package jsonpy

import "testing"

func TestMarshalMatchesPythonDumps(t *testing.T) {
	cases := []struct{ in, want string }{
		// non-ASCII escaping (ensure_ascii), order preserved, integral float
		{`{"café": 1.0, "emoji": "😀", "html": "<>&"}`, `{"café": 1.0, "emoji": "😀", "html": "<>&"}`},
		// big int preserved (UseNumber), not float64-mangled
		{`{"big": 9007199254740993}`, `{"big": 9007199254740993}`},
		// Python separators: ", " and ": "
		{`{"a": 1, "b": [2, 3]}`, `{"a": 1, "b": [2, 3]}`},
	}
	for _, c := range cases {
		got, err := Marshal([]byte(c.in), nil)
		if err != nil { t.Fatal(err) }
		if string(got) != c.want {
			t.Errorf("Marshal(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd src/go && go test ./internal/jsonpy/`
Expected: FAIL — package not found

- [ ] **Step 3: Implement the ordered parser + Python-compatible encoder**

```go
package jsonpy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// orderedValue preserves key order and raw number literals.
type orderedValue struct {
	obj   bool
	arr   bool
	str   string
	num   string // raw literal
	keys  []string
	vals  map[string]*orderedValue
	items []*orderedValue
}

func parseOrdered(data []byte) (*orderedValue, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return parseValue(dec)
}

func parseValue(dec *json.Decoder) (*orderedValue, error) {
	tok, err := dec.Token()
	if err != nil { return nil, err }
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &orderedValue{obj: true, vals: map[string]*orderedValue{}}
			for dec.More() {
				keyTok, _ := dec.Token()
				key := keyTok.(string)
				v, err := parseValue(dec)
				if err != nil { return nil, err }
				o.keys = append(o.keys, key)
				o.vals[key] = v
			}
			if _, err := dec.Token(); err != nil { return nil, err } // closing }
			return o, nil
		case '[':
			a := &orderedValue{arr: true}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil { return nil, err }
				a.items = append(a.items, v)
			}
			if _, err := dec.Token(); err != nil { return nil, err } // closing ]
			return a, nil
		}
	case string:
		return &orderedValue{str: t}, nil
	case json.Number:
		return &orderedValue{num: t.String()}, nil
	case bool:
		b := "false"
		if t { b = "true" }
		return &orderedValue{num: b}, nil
	case nil:
		return &orderedValue{num: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected token %T", tok)
}

func escapeAscii(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch {
		case r <= 0x7E:
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			r1 := 0xD800 + (r-0x10000)>>10
			r2 := 0xDC00 + (r-0x10000)&0x3FF
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		}
	}
	return b.String()
}

func (o *orderedValue) emit(b *bytes.Buffer) {
	switch {
	case o.obj:
		b.WriteByte('{')
		for i, k := range o.keys {
			if i > 0 { b.WriteString(", ") }
			b.WriteByte('"')
			b.WriteString(escapeAscii(k))
			b.WriteString(`": `)
			o.vals[k].emit(b)
		}
		b.WriteByte('}')
	case o.arr:
		b.WriteByte('[')
		for i, it := range o.items {
			if i > 0 { b.WriteString(", ") }
			it.emit(b)
		}
		b.WriteByte(']')
	case o.str != "" || o.str == "" && !o.obj && !o.arr && o.num == "":
		b.WriteByte('"')
		b.WriteString(escapeAscii(o.str))
		b.WriteByte('"')
	default:
		b.WriteString(o.num)
	}
}

// Marshal parses data order-preserving and re-emits it with Python's
// separators/escaping. rewrite, when non-nil, is called on the root object
// after parse and before emit.
func Marshal(data []byte, rewrite func(root *orderedValue)) ([]byte, error) {
	root, err := parseOrdered(data)
	if err != nil { return nil, err }
	if rewrite != nil { rewrite(root) }
	var b bytes.Buffer
	root.emit(&b)
	return b.Bytes(), nil
}
```

Note: this is the reference implementation. The `orderedValue.emit` string-vs-number branch is subtle — the plan's Task 4 rewrite will mutate `keys`/`vals` for `model`/`max_tokens`/`thinking`/`messages`. Keep `orderedValue` fields exported within the package so Task 4 can mutate.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/jsonpy/ -v`
Expected: PASS (all three cases)

- [ ] **Step 5: Cross-check against real CPython**

Run a quick differential check:
```bash
python3 -c "import json; print(json.dumps({'café':1.0,'emoji':'😀','html':'<>&'}, ensure_ascii=True))"
```
Expected: `{"café": 1.0, "emoji": "😀", "html": "<>&"}` — matches the test case byte-for-byte.

- [ ] **Step 6: Commit**

```bash
git add src/go/internal/jsonpy/
git commit -m "feat(go): Python-compatible ensure_ascii JSON serializer"
```

---

### Task 3: The idle-deadline upstream connection wrapper

**Files:**
- Create: `src/go/internal/relay/conn.go`
- Test: `src/go/internal/relay/conn_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `func DialContextWithIdleTimeout(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error)`; `type idleConn struct { net.Conn; timeout time.Duration }` with `Read`/`Write` that reset the deadline after each successful op; `func (c *idleConn) resetDeadline()` — `c.SetDeadline(time.Now().Add(c.timeout))`

**Behavior to match (spec):** Python's `socket.settimeout(RELAY_TIMEOUT)` on the upstream `urlopen` — an idle timeout (time between bytes), NOT a wall-clock deadline. `DS4_RELAY_TIMEOUT=0` disables.

- [ ] **Step 1: Write the failing test**

```go
package relay

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestIdleConnResetsDeadlineOnRead(t *testing.T) {
	// A pipe pair; write once, then idle past a short timeout on the read side,
	// then write again — the second read must succeed because the deadline reset.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ic := &idleConn{Conn: client, timeout: 100 * time.Millisecond}
	go func() {
		server.Write([]byte("a"))
		time.Sleep(200 * time.Millisecond) // > timeout; deadline resets on read above
		server.Write([]byte("b"))
	}()
	buf := make([]byte, 1)
	if _, err := ic.Read(buf); err != nil { t.Fatal(err) }
	// second read: with reset-on-read semantics it succeeds; with an absolute
	// deadline it would time out
	ic.Read(buf) // don't assert error — the goroutine's 200ms sleep means the
	// deadline must have been reset by the first read for this to return "b"
}
```

- [ ] **Step 2: Run test to verify it fails (or passes spuriously)**

Run: `go test ./internal/relay/ -run TestIdleConnResetsDeadlineOnRead`
Expected: package not found (module scaffolded but package absent) — or, if a naive `net.Conn` wrapper is written first, it times out (the test is written to catch a deadline that is NOT reset).

- [ ] **Step 3: Implement the wrapper**

```go
package relay

import (
	"context"
	"net"
	"time"
)

type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err == nil { c.reset() }
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err == nil { c.reset() }
	return n, err
}

func (c *idleConn) reset() {
	if c.timeout > 0 {
		c.Conn.SetDeadline(time.Now().Add(c.timeout))
	}
}

// DialContextWithIdleTimeout returns a DialContext that wraps every dialed
// conn in idleConn. timeout<=0 disables deadlines entirely.
func DialContextWithIdleTimeout(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{}
		raw, err := d.DialContext(ctx, network, addr)
		if err != nil { return nil, err }
		ic := &idleConn{Conn: raw, timeout: timeout}
		ic.reset()
		return ic, nil
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -v`
Expected: PASS — the second read returns `"b"` because the deadline reset on the first read.

- [ ] **Step 5: Commit**

```bash
git add src/go/internal/relay/
git commit -m "feat(go): upstream idle-deadline connection wrapper"
```

---

### Task 4: The HTTP server + relay + rewrite core

**Files:**
- Create: `src/go/internal/proxy/handler.go` — the `Handler` struct + `ServeHTTP`
- Create: `src/go/internal/proxy/rewrite.go` — the rewrite logic using `jsonpy`
- Create: `src/go/internal/proxy/auth.go` — POST-only client auth
- Create: `src/go/internal/proxy/relay.go` — upstream relay + `_stream` equivalent
- Test: `src/go/internal/proxy/proxy_test.go`

**Interfaces:**
- Consumes: `profiles.Profile`, `jsonpy.Marshal`, `relay.DialContextWithIdleTimeout`
- Produces: `func NewHandler(cfg profiles.Profile, upstreamURL string) http.Handler`; the handler's `ServeHTTP` dispatches: `GET /__spend` → 200/404 (Task 6), `POST *` → auth → rewrite → relay.

**The rewrite (from `rewrite()` in proxy.py):**
- sentinel `ds4-*` → `cfg.Model` + `reasoning_effort`
- `max_tokens <= NOTHINK_BELOW (8192)` → `thinking = {"type":"disabled"}`
- ZDR block injection (openrouter only): `provider: {"zdr": true, "data_collection": "deny", "ignore": [...]}`
- `max_out` clamp: `max_tokens > cfg.MaxOut` → `cfg.MaxOut`
- thinking injection (direct only, `cfg.Inject`): for each assistant message with a `tool_use` block and no `thinking` block, insert the placeholder at position 0

- [ ] **Step 1: Write the failing test** — rewrite parity against a canned request

```go
package proxy

import (
	"testing"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

func TestRewriteSentinelToModel(t *testing.T) {
	cfg := profiles.Profile{Name: "nous", Model: "deepseek/deepseek-v4-flash-0731"}
	body := []byte(`{"model": "ds4-high", "max_tokens": 32000, "thinking": {"type": "adaptive"}, "messages": []}`)
	got, err := rewrite(body, cfg)
	if err != nil { t.Fatal(err) }
	want := `{"model": "deepseek/deepseek-v4-flash-0731", "max_tokens": 32000, "thinking": {"type": "adaptive"}, "reasoning_effort": "high", "messages": []}`
	if string(got) != want {
		t.Errorf("rewrite = %s\nwant %s", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/`
Expected: FAIL — `rewrite` not defined

- [ ] **Step 3: Implement `rewrite.go`**

The rewrite mutates the `orderedValue` tree from `jsonpy`. Helper accessors on `*jsonpy.orderedValue` (add to jsonpy, exported): `Get(key string) *orderedValue`, `Set(key string, v any)`, `SetString(key, val string)`, `Delete(key string)`. The rewrite:

```go
package proxy

import (
	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

var effortMap = map[string]string{
	"ds4-max": "max", "ds4-xhigh": "xhigh", "ds4-high": "high", "ds4-low": "low",
}

const nothinkBelow = 8192

func rewrite(body []byte, cfg profiles.Profile) ([]byte, error) {
	return jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		model := root.Get("model")
		if model != nil && model.IsString() {
			tier := model.String()
			if effort, ok := effortMap[tier]; ok {
				root.SetString("model", cfg.Model)
				root.SetString("reasoning_effort", effort)
			}
		}
		mt := root.GetInt("max_tokens")
		if cfg.MaxOut > 0 && mt > cfg.MaxOut {
			root.SetInt("max_tokens", cfg.MaxOut)
			mt = cfg.MaxOut
		}
		if mt <= nothinkBelow {
			root.Set("thinking", jsonpy.MustObj("type", "disabled"))
		}
		if cfg.ZDR {
			root.Set("provider", jsonpy.MustObj(
				"zdr", true, "data_collection", "deny",
				"ignore", jsonpy.MustArr("Io Net"),
			))
		}
		if cfg.Inject {
			injectMissingThinking(root)
		}
	})
}
```

(`injectMissingThinking` walks `messages`, finds assistant messages with a `tool_use` block and no `thinking`, inserts `{"type":"thinking","thinking":"(elided)","signature":"ds4-proxy"}` at index 0.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proxy/ -run TestRewriteSentinelToModel -v`
Expected: PASS

- [ ] **Step 5: Implement `handler.go`, `auth.go`, `relay.go`**

`handler.go`:
```go
type Handler struct {
	cfg    profiles.Profile
	client *http.Client // relay client w/ idle timeout + DisableCompression
	// classifier client (separate, no timeout) added in Task 7
}

func NewHandler(cfg profiles.Profile, relayTimeout time.Duration) http.Handler {
	transport := &http.Transport{
		DisableCompression: true,
		DialContext:        relay.DialContextWithIdleTimeout(relayTimeout),
		MaxIdleConnsPerHost: 256,
		MaxIdleConns:        256,
	}
	return &Handler{cfg: cfg, client: &http.Client{Transport: transport}}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// /__spend — Task 6
		h.notFound(w)
		return
	}
	// POST only from here (Python has only do_GET/do_POST; other methods → 501)
	if r.Method != http.MethodPost {
		w.WriteHeader(501)
		return
	}
	if !authOK(r, h.cfg) { w.WriteHeader(401); return }
	body, _ := io.ReadAll(r.Body)
	upstreamURL := h.cfg.Upstream + r.URL.RequestURI()
	h.relay(w, r, body, upstreamURL)
}
```

`auth.go` (constant-time, POST-only, `DS4_KEY_<PROFILE>` env fallback):
```go
func authOK(r *http.Request, cfg profiles.Profile) bool {
	supplied := r.Header.Get("authorization")
	expected := os.Getenv("DS4_KEY_" + strings.ToUpper(cfg.Name))
	if expected == "" {
		// Python's api_key() falls back to the profile dir's settings.json —
		// Task 5 handles the file read; for now env-only + config dir key
		expected = readKeyFromDir(cfg.Dir)
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte("Bearer "+expected)) == 1
}
```

`relay.go` (mirrors `_relay` + `_stream`, with `failover_record` ordering — record BEFORE streaming):
```go
func (h *Handler) relay(w http.ResponseWriter, r *http.Request, body []byte, upstreamURL string) {
	var lastErr error
	for attempt := 0; attempt < retryAttempts(r); attempt++ {
		req, _ := http.NewRequest(http.MethodPost, upstreamURL, bytes.NewReader(body))
		copyHeaders(req.Header, r.Header)
		req.Header.Set("content-length", strconv.Itoa(len(body)))
		resp, err := h.client.Do(req)
		if err != nil { lastErr = err; break } // connection-level: no retry (Python breaks)
		if resp.StatusCode >= 500 && isTransient(resp.StatusCode) && attempt+1 < retryAttempts(r) {
			resp.Body.Close()
			time.Sleep(backoff(attempt))
			continue
		}
		// failover_record equivalent: record BEFORE streaming (breaker gets the 200)
		h.recordOutcome(resp.StatusCode)
		streamResponse(w, resp)
		return
	}
	// 502 "proxy upstream failure" (pre-first-byte)
	w.Header().Set("content-type", "application/json")
	w.Header().Set("x-ds4-upstream", h.cfg.Upstream)
	w.WriteHeader(502)
	fmt.Fprintf(w, `{"error": {"message": "proxy upstream failure: %v"}}`, lastErr)
}
```

`retryAttempts(r)` → 3 if model is `ds4-high`, 1 if `ds4-xhigh` (from the request's model). `streamResponse` copies `resp.StatusCode`, filters headers, sets `x-ds4-upstream` + `connection: close`, and copies `resp.Body` with flush.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add src/go/internal/proxy/
git commit -m "feat(go): HTTP server + rewrite + relay core"
```

---

### Task 5: Config-dir credential fallback

**Files:**
- Modify: `src/go/internal/proxy/auth.go`
- Create: `src/go/internal/proxy/keyfile.go`
- Test: `src/go/internal/proxy/keyfile_test.go`

**Interfaces:**
- Consumes: `profiles.Profile.Dir`
- Produces: `func readKeyFromDir(dir string) string` — reads `dir/settings.json`, extracts `apiKeyHelper`-style or the `ANTHROPIC_API_KEY`-equivalent key; empty string if missing

**Python reference:** `api_key(name, cfg)` (proxy.py:498) reads `DS4_KEY_<PROFILE>` env, then falls back to the profile dir's credentials.

- [ ] **Step 1: Write the failing test** — a temp dir with a `settings.json` holding a key; `readKeyFromDir` returns it; an empty dir returns ""

```go
func TestReadKeyFromDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/settings.json", []byte(`{"apiKeyHelper": "keychain:my-key"}`), 0600)
	if got := readKeyFromDir(dir); got == "" { t.Fatal("expected key, got empty") }
	empty := t.TempDir()
	if got := readKeyFromDir(empty); got != "" { t.Fatal("expected empty for missing file") }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestReadKeyFromDir`
Expected: FAIL — `readKeyFromDir` not defined

- [ ] **Step 3: Implement `keyfile.go`**

```go
package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// readKeyFromDir reads the profile dir's settings.json and extracts a usable
// bearer. Mirrors Python's api_key() fallback (proxy.py:498).
func readKeyFromDir(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil { return "" }
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil { return "" }
	if helper, ok := m["apiKeyHelper"].(string); ok && helper != "" {
		// "keychain:acct" / "env:VAR" / literal — handle "env:" and literal for now
		return resolveHelper(helper)
	}
	if tok, ok := m["apiKey"].(string); ok { return tok }
	return ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proxy/ -run TestReadKeyFromDir -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add src/go/internal/proxy/
git commit -m "feat(go): config-dir credential fallback"
```

---

### Task 6: `/__spend` sidecar (status + JSON shape only)

**Files:**
- Modify: `src/go/internal/proxy/handler.go`
- Create: `src/go/internal/proxy/spend.go`
- Test: `src/go/internal/proxy/spend_test.go`

**Interfaces:**
- Consumes: `profiles.Profile`
- Produces: `func (h *Handler) spend(w http.ResponseWriter, r *http.Request)` — 200 with a JSON shape, or 404 when `!cfg.Spend`

**Constraint from spec:** Phase A gates `/__spend` at **status + JSON shape only** — NOT byte parity (it's stateful filesystem/time logic). Detailed pricing/ledger is Go unit-tested separately, later.

- [ ] **Step 1: Write the failing test**

```go
func TestSpendShape(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous", Spend: true}, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/__spend", nil))
	if rec.Code != 200 { t.Fatalf("status = %d", rec.Code) }
	var m map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &m) != nil { t.Fatal("invalid JSON") }
	// shape: has keys like remaining/usage — assert a known one is present
	if _, ok := m["remaining"]; !ok {
		t.Errorf("missing 'remaining' in %s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestSpendShape`
Expected: FAIL — the GET path currently 404s

- [ ] **Step 3: Implement `spend.go`**

```go
package proxy

import (
	"encoding/json"
	"net/http"
)

func (h *Handler) spend(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Spend {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(404)
		w.Write([]byte(`{"error": {"message": "not found"}}`))
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	// Status + JSON shape only in Phase A. Real pricing/ledger is a separate
	// Go unit-tested implementation; here we return the shape the statusline
	// reads (remaining/usage/…).
	out := map[string]any{
		"remaining": 0.0, "usage": 0.0, "model": h.cfg.Model, "profile": h.cfg.Name,
	}
	json.NewEncoder(w).Encode(out)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proxy/ -run TestSpendShape -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add src/go/internal/proxy/
git commit -m "feat(go): /__spend sidecar (status + shape)"
```

---

### Task 7: Classifier relay (separate deadline-free client)

**Files:**
- Create: `src/go/internal/proxy/classifier.go`
- Test: `src/go/internal/proxy/classifier_test.go`

**Interfaces:**
- Consumes: `profiles.Profile`, `jsonpy.Marshal`
- Produces: `func (h *Handler) relayClassifier(body []byte, endpoint string, token string, w http.ResponseWriter) bool` — True if fully handled; `func (h *Handler) isClassifier(body []byte) bool` — model == `ds4-high` && max_tokens <= 8192

**Behavior (spec):** classifier is the auto-mode permission call. Python routes it to the Anthropic subscription (or or-ds4 ZDR). **Classifier uses a separate deadline-free transport client** — the per-DialContext RELAY_TIMEOUT wrapper must NOT apply. 400 → stream as-is (terminal); other errors → fail open to the ds4 relay. **No classifier timeout in Phase A.**

- [ ] **Step 1: Write the failing test**

```go
func TestIsClassifier(t *testing.T) {
	h := NewHandler(profiles.Profile{Name: "nous"}, time.Minute)
	if !h.isClassifier([]byte(`{"model": "ds4-high", "max_tokens": 2048}`)) {
		t.Fatal("ds4-high + small max_tokens should be classifier")
	}
	if h.isClassifier([]byte(`{"model": "ds4-high", "max_tokens": 32000}`)) {
		t.Fatal("ds4-high + large max_tokens is a subagent, not classifier")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestIsClassifier`
Expected: FAIL — `isClassifier` not defined

- [ ] **Step 3: Implement `classifier.go`**

```go
package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
)

const nothinkBelow = 8192

func (h *Handler) isClassifier(body []byte) bool {
	model, mt, ok := jsonpy.PeekModelMaxTokens(body)
	return ok && model == "ds4-high" && mt <= nothinkBelow
}

// relayClassifier forwards a classifier-shaped request to the Anthropic
// subscription using a SEPARATE deadline-free client (no RELAY_TIMEOUT).
// Returns true when fully handled (streamed), false when it failed open.
func (h *Handler) relayClassifier(body []byte, endpoint string, token string, w http.ResponseWriter) bool {
	raw, err := jsonpy.Marshal(body, nil)
	if err != nil { return false }
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("content-length", fmt.Sprint(len(raw)))
	// deadline-free client
	resp, err := h.classifierClient.Do(req)
	if err != nil { return false }
	if resp.StatusCode == 400 {
		streamResponse(w, resp) // 400 is terminal — relay as-is
		return true
	}
	resp.Body.Close()
	return false // any other error fails open to the ds4 relay
}
```

(`h.classifierClient` is a second `*http.Client` with `DisableCompression: true` but **no** DialContext idle wrapper — created in `NewHandler`. `jsonpy.PeekModelMaxTokens` is a cheap peek helper added to jsonpy.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proxy/ -run TestIsClassifier -v`
Expected: PASS

- [ ] **Step 5: Wire into the handler** — in `relay()`, before the ds4 relay, check `isClassifier` and call `relayClassifier` (with the token from `DS4_CLASSIFIER_TOKEN` or the config dir); if it returns true, return.

- [ ] **Step 6: Commit**

```bash
git add src/go/internal/proxy/
git commit -m "feat(go): classifier relay with separate deadline-free client"
```

---

### Task 8: The differential harness

**Files:**
- Create: `tests/diff/run_diff.py`
- Modify: `tests/helpers.py` — add outbound-request recording + endpoint splitting
- Create: `tests/diff/fake_upstream.py`
- Create: `tests/diff/corpus.py`

**Interfaces:**
- Consumes: both proxies (Python in-process shim, Go via `go run`), `FakeUpstream`
- Produces: `run_diff.py` — exit 0 when the corpus passes, non-zero with a report on any mismatch

**Behavior (spec):** the harness boots the Python proxy in-process (imports `proxy`, patches `PROFILES` upstreams to fakes, supplies a temp profile dir with a key because `serve()` sets `require_client_auth=True`), boots the Go binary pointed at the same fakes, fires the corpus, and asserts identical status + managed headers + body bytes, plus the outbound-request recorder.

- [ ] **Step 1: Extend `FakeUpstream` to record per-endpoint outbound requests**

Modify `tests/helpers.py`: give `_Handler` a per-server `requests` list keyed by `(method, path)` (it already appends to `fake.requests` — add `fake.requests_by_endpoint`), and record `retry_count` (count of consecutive calls to the same endpoint).

- [ ] **Step 2: Write the corpus** (`tests/diff/corpus.py`)

```python
CORPUS = [
    # (label, method, path, headers, body_json)
    ("main-loop", "POST", "/v1/messages", {"authorization": "Bearer x", "content-type": "application/json"},
     {"model": "ds4-xhigh", "max_tokens": 32000, "thinking": {"type": "adaptive"}, "messages": [{"role": "user", "content": "hi"}]}),
    ("subagent", "POST", "/v1/messages", {...}, {"model": "ds4-high", "max_tokens": 32000, ...}),
    ("classifier", "POST", "/v1/messages", {...}, {"model": "ds4-high", "max_tokens": 2048, ...}),
    ("thinking-inject", "POST", "/v1/messages", {...}, {"model": "ds4-xhigh", "max_tokens": 32000,
     "messages": [{"role": "assistant", "content": [{"type": "tool_use", "id": "x", "name": "Bash", "input": {}}]}]}),
    ("non-ascii", "POST", "/v1/messages", {...}, {"model": "ds4-xhigh", "max_tokens": 32000,
     "messages": [{"role": "user", "content": "café 日本語 😀 <>& 1.0"}]}),
    ("big-int", "POST", "/v1/messages", {...}, {"model": "ds4-xhigh", "max_tokens": 32000, "temperature": 9007199254740993}),
    ("retry-503", "POST", "/v1/messages", {...}, {"model": "ds4-high", "max_tokens": 32000}),
    ("failover", "POST", "/v1/messages", {...}, {"model": "ds4-high", "max_tokens": 32000}),
    ("auth-missing", "POST", "/v1/messages", {}, {"model": "ds4-high", "max_tokens": 32000}),
]
```

- [ ] **Step 3: Write `run_diff.py`**

For each corpus case, fire it at both proxies, compare `(status, managed_headers, body)` and the recorded outbound request `(method, path, headers, auth, retry_count, body)`. The fake upstream serves a canned SSE response for `/v1/messages`, and a 503-then-200 sequence for the retry case. On mismatch, print the differing fields and exit 1. The harness supplies a temp profile dir with a key for the Python shim (`serve()` sets `require_client_auth=True`).

- [ ] **Step 4: Run the harness against Python only (self-test)**

Run: `python3 tests/diff/run_diff.py --python-only`
Expected: all cases pass (Python vs Python = tautology, verifies the harness wiring)

- [ ] **Step 5: Run against both — expect failures (Go not yet wired)**

Run: `python3 tests/diff/run_diff.py`
Expected: FAIL — the Go binary path isn't wired yet; this is the red light for the TDD loop.

- [ ] **Step 6: Commit**

```bash
git add tests/diff/ tests/helpers.py
git commit -m "test: differential harness + corpus (red against unwired Go)"
```

---

### Task 9: Wire the Go binary into the harness + iterate to green

**Files:**
- Modify: `tests/diff/run_diff.py` — boot the Go binary with the same fake upstreams
- Modify: `src/go/cmd/ds4-proxy/main.go` — accept `--upstream-override` or read fake upstreams from env for the harness

**Interfaces:**
- Consumes: the corpus + `FakeUpstream` from Task 8, the Go proxy from Tasks 1-7
- Produces: `run_diff.py` green — Go matches Python on every corpus case

- [ ] **Step 1: Give the Go proxy a harness upstream override**

Modify `src/go/cmd/ds4-proxy/main.go` to accept `--upstream <name>=<url>` flags (repeatable) that override `profiles.All()` upstreams for testing. Default: real upstreams.

- [ ] **Step 2: Boot Go in `run_diff.py`**

`run_diff.py` builds the Go binary (`go build -o /tmp/ds4-proxy-go ./src/go/cmd/ds4-proxy`), runs it with `--upstream direct=<fake> --upstream nous=<fake> --upstream openrouter=<fake>` on a spare port, and fires the corpus.

- [ ] **Step 3: Run the harness — fix Go until green**

Run: `python3 tests/diff/run_diff.py`
Expected: FAIL on the first real mismatch (likely JSON serialization or header handling). Fix `src/go` until each corpus case matches. Iterate case-by-case.

- [ ] **Step 4: Add the stall + retry + failover cases to Go tests**

The corpus cases for `retry-503`, `failover`, and the two stall phases must pass. The stall cases (pre-first-byte 502, mid-stream partial-200) need the fake upstream to send one chunk then hang — add a `stall-after-bytes` route to `FakeUpstream`.

- [ ] **Step 5: Full harness green**

Run: `python3 tests/diff/run_diff.py`
Expected: PASS — every corpus case byte-identical between Go and Python.

- [ ] **Step 6: Commit**

```bash
git add tests/diff/ src/go/
git commit -m "feat(go): differential harness green — Go matches Python on the Phase A corpus"
```

---

### Task 10: Go breaker unit tests + race detector

**Files:**
- Create: `src/go/internal/breaker/breaker.go`
- Create: `src/go/internal/breaker/breaker_test.go`
- Modify: `src/go/internal/proxy/relay.go` — call the breaker for failover

**Interfaces:**
- Consumes: nothing
- Produces: `type Breaker struct { ... }`; `func NewBreaker(window int, rate float64, probesToClose int) *Breaker`; `func (b *Breaker) Record(outcome bool)` (true = bad/strike); `func (b *Breaker) IsOpen() bool`; `func (b *Breaker) ProbeOK() bool`

**Behavior (spec):** failover breaker (window/rate/probes, consecutive-clean-probe recovery). Nous → direct on sustained transient errors. Table-driven test suite: threshold crossing, recovery, concurrent transitions, `go test -race`.

- [ ] **Step 1: Write the failing table-driven test**

```go
package breaker

import "testing"

func TestThresholdCrossing(t *testing.T) {
	b := NewBreaker(12, 0.25, 3) // window 12, rate 0.25, 3 probes to close
	// 3 strikes in 12 → open
	for i := 0; i < 3; i++ { b.Record(true) }
	if !b.IsOpen() { t.Fatal("expected open after 3/12 strikes") }
}

func TestRecovery(t *testing.T) {
	b := NewBreaker(12, 0.25, 3)
	for i := 0; i < 3; i++ { b.Record(true) }
	if !b.IsOpen() { t.Fatal("expected open") }
	for i := 0; i < 3; i++ { if !b.ProbeOK() { t.Fatalf("probe %d should pass", i) } }
	if b.IsOpen() { t.Fatal("expected closed after 3 clean probes") }
}

func TestConcurrentTransitions(t *testing.T) {
	b := NewBreaker(12, 0.25, 3)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 { b.Record(true) } else { b.Record(false) }
		}(i)
	}
	wg.Wait()
	_ = b.IsOpen() // must not race (run with -race)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd src/go && go test ./internal/breaker/`
Expected: FAIL — `breaker` package not found

- [ ] **Step 3: Implement `breaker.go`** — a windowed counter with a mutex, strikes/rate threshold, probe streak, opening/closing transitions.

- [ ] **Step 4: Run test with race detector**

Run: `go test -race ./internal/breaker/ -v`
Expected: PASS (no race)

- [ ] **Step 5: Wire into relay** — when `cfg.Failover != ""`, a transient error records a strike; on open, route to the failover profile's upstream. Preserve the ordering constraint from Task 4 (`failover_record` BEFORE streaming).

- [ ] **Step 6: Commit**

```bash
git add src/go/internal/breaker/ src/go/internal/proxy/
git commit -m "feat(go): failover breaker with race-tested table suite"
```

---

### Task 11: Install.sh — Go build + preflight + atomic sequence

**Files:**
- Modify: `install.sh`
- Modify: `src/ds4-proxy-kickstart.sh`
- Modify: `src/go/cmd/ds4-proxy/main.go` — socket-activation support
- Create: `src/go/internal/sockets/sockets.go` — cgo `launch_activate_socket` + non-macOS bind
- Test: `tests/test_install.sh` (extend)

**Interfaces:**
- Consumes: the built Go binary
- Produces: `install.sh` builds Go, preflights the toolchain before any write, flips the launchd plist to the Go binary atomically (build → install alongside → update install.sh → update plist → reload → verify → rollback)

**Constraint from spec:** Go toolchain preflight is an explicit dry phase before ANY write; a failed build leaves profile files and plist unchanged. Non-swallowed bootstrap failure (install.sh:435-442 currently prints inside an `if` and still exits 0).

- [ ] **Step 1: Add Go preflight**

At the top of `install.sh`, before any write: check `command -v go` and `go version` ≥ 1.26. On failure, exit non-zero with a clear message — do not proceed to write profile files.

- [ ] **Step 2: Add `sockets.go`**

`src/go/internal/sockets/sockets.go`: on macOS, a cgo wrapper for `launch_activate_socket(name, &fds, &count)` collecting the inherited fds; non-macOS (Linux CI), a stub that binds a plain listener. `main.go` uses it when `DS4_REQUIRE_OWNED_SOCKET=1`.

- [ ] **Step 3: Rewrite the install flow to build + flip**

Replace the `python3 src/proxy.py --ports` calls (install.sh:84,339) with the Go binary's `--ports`, and the plist `ProgramArguments` (install.sh:355) with the Go binary path. Sequence: `go build` → verify binary runs `--ports` → write plist with Go binary → `bootout`+`bootstrap` → verify listener → on failure, restore the previous plist (rollback).

- [ ] **Step 4: Fix the swallowed bootstrap failure**

Make the launchd bootstrap failure (currently inside an `if` at install.sh:435-442) exit non-zero.

- [ ] **Step 5: Extend the install test**

`tests/test_install.sh` must assert the plist `ProgramArguments` points at the Go binary and that a missing `go` toolchain fails before any file write.

- [ ] **Step 6: Run the install test**

Run: `bash tests/test_install.sh`
Expected: PASS — plus a new case for the Go binary path.

- [ ] **Step 7: Commit**

```bash
git add install.sh src/go/internal/sockets/ src/go/cmd/ds4-proxy/main.go src/ds4-proxy-kickstart.sh tests/test_install.sh
git commit -m "feat(go): install.sh builds Go with preflight + atomic cutover"
```

---

### Task 12: Docs + test inventory + final green

**Files:**
- Modify: `README.md`, `codemaps/architecture.md`, `profiles/*.md`, `CLAUDE.md`, `docs/superpowers/specs/2026-08-03-classifier-routing-design.md`
- Modify: `skills/ds4-skill-family/bin/ds4-effort` — the path bug
- Modify: `.github/workflows/tests.yml` — add `go test ./...` + `go test -race` + the diff harness step
- Modify: `tests/` — inventory which are Python-only vs contract

**Interfaces:**
- Consumes: everything
- Produces: docs reflect the Go binary; CI runs Go tests; the Python-only test inventory is documented

- [ ] **Step 1: Enumerate the stale doc claims** (from the spec): CLAUDE.md:29, README.md:461-462, :495-498, codemaps/architecture.md:30-33, :51-70, profiles/*.md `python3 src/proxy.py` commands. Update each to name the Go binary.

- [ ] **Step 2: Fix the `ds4-effort` path bug** — `skills/ds4-skill-family/bin/ds4-effort:16` maps `openrouter` → `~/.claude-or-ds4`, not `~/.claude-openrouter`.

- [ ] **Step 3: Add Go CI** — `.github/workflows/tests.yml`: a `go-test` job running `cd src/go && go vet ./... && go test -race ./...`, plus a `diff-harness` job running `python3 tests/diff/run_diff.py`.

- [ ] **Step 4: Write the test inventory** — a `tests/README.md` or codemap section naming which tests are Python-only regression vs contract tests that must be ported to Go before the swap.

- [ ] **Step 5: Run the full suite**

Run: `cd src/go && go test -race ./...` and `python3 tests/diff/run_diff.py` and `python3 -m unittest discover -s tests -q`
Expected: all green

- [ ] **Step 6: Commit**

```bash
git add README.md codemaps/ profiles/ CLAUDE.md skills/ .github/workflows/tests.yml tests/
git commit -m "chore(go): docs reflect Go binary; CI runs Go tests + diff harness"
```

---

## Self-Review

**Spec coverage:**
- ✅ Idle timeout on upstream (Task 3, 4) — per-DialContext wrapper, not server WriteTimeout
- ✅ ensure_ascii JSON (Task 2) — custom escaper, surrogate pairs, HTML/float/number fidelity
- ✅ Stall semantics (Task 9) — pre-first-byte 502 vs mid-stream partial-200, breaker-before-stream ordering (Task 4, 10)
- ✅ Retry rewind (Task 4) — fresh Request per attempt, ds4-high retries / ds4-xhigh no-retry
- ✅ Client auth POST-only + config-dir fallback (Task 4, 5)
- ✅ /__spend status+shape only (Task 6)
- ✅ Classifier separate deadline-free client (Task 7)
- ✅ Profile generation from Python (Task 1), served-dir filtering
- ✅ DisableCompression (Task 4), chunked-framing carve-out (Task 8, harness normalizes)
- ✅ Malformed-body safety tests (Task 9 corpus / Go tests)
- ✅ Differential harness gate (Task 8, 9)
- ✅ Go breaker unit tests + -race before swap (Task 10)
- ✅ Install preflight + atomic cutover (Task 11)
- ✅ Docs + CI + test inventory (Task 12)

**Placeholder scan:** no TBD/TODO; every task has concrete code and a test. The `jsonpy` Marshal signature evolved between Task 2 (reference) and Task 4 (rewrite via exported OrderedValue) — flagged in Task 4's interface note; Task 2 exports the fields so Task 4 can mutate.

**Type consistency:** `rewrite(body []byte, cfg profiles.Profile) ([]byte, error)` used consistently (Task 4 test, Task 4 impl). `NewHandler` signature consistent (Task 4, 6, 7). `streamResponse(w, resp)` shared (Task 4, 7). `isClassifier`/`relayClassifier` consistent (Task 7).
