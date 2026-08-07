package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
)

// retryBackoff is the base sleep between retries, in seconds, scaled by
// attempt number: 1.5s * (attempt+1), matching RETRY_BACKOFF in proxy.py. It
// is a variable so tests can shrink the sleep; production always sees 1.5.
var retryBackoff = 1.5

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
func (h *Handler) relay(w http.ResponseWriter, r *http.Request, body []byte, upstreamURL string) {
	// The client-sent tier is captured before the rewrite remaps payload["model"]
	// to the target's literal id, so a failed-over main-loop request does not
	// look retryable.
	origTier := modelFromJSON(body)

	rewritten, err := rewrite(body, h.cfg)
	if err == nil {
		body = rewritten
	}

	attempts := retryAttempts(origTier)
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		// A fresh request per attempt with the retained body re-wound; the
		// reader is re-created so a consumed body does not poison a retry.
		req, err := http.NewRequest(http.MethodPost, upstreamURL, bytes.NewReader(body))
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
		streamResponse(w, resp, h.cfg.Upstream)
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
