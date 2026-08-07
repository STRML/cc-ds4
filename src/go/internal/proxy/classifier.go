package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
)

// classifierUpstream is the Anthropic Messages endpoint the classifier is
// forwarded to, mirroring classifier.py's CLASSIFIER_UPSTREAM. The classifier
// is already an Anthropic-shaped request; only the model id and a token are
// needed to point it at the subscription. It is a variable so tests can point
// it at a fake (Python tests patch the module constant the same way).
var classifierUpstream = "https://api.anthropic.com/v1/messages"

// anthropicVersion is the API version header the classifier relay sends,
// matching classifier.py's ANTHROPIC_VERSION.
const anthropicVersion = "2023-06-01"

// isClassifier reports whether a request body is the auto-mode permission
// classifier: model ds4-high with a max_tokens at or below the no-think
// budget. Subagents also run at ds4-high but at a much larger max_tokens, so
// the size threshold separates them. The ok return from PeekModelMaxTokens
// enforces Python's isinstance(mt, int) guard — a request with NO max_tokens
// is not a classifier, even though a missing value would be 0.
func (h *Handler) isClassifier(body []byte) bool {
	model, mt, ok := jsonpy.PeekModelMaxTokens(body)
	return ok && model == "ds4-high" && mt <= nothinkBelow
}

// isZDRRequest mirrors request_requires_zdr in proxy.py: the proxy-local
// per-request ZDR demand arrives in the x-ds4-require-zdr header. A
// ZDR-demanding request must stay on a route that can enforce the block; the
// classifier relay's whitelist cannot carry it, so such a request is excluded
// from the classifier path (proxy.py:828-835). The body-field variant
// (ds4_require_zdr) is handled by a later task.
func isZDRRequest(r *http.Request) bool {
	hdr := strings.ToLower(strings.TrimSpace(r.Header.Get("x-ds4-require-zdr")))
	return hdr == "1" || hdr == "true" || hdr == "yes"
}

// classifierToken mirrors classifier_token() in classifier.py: the
// subscription token from DS4_CLASSIFIER_TOKEN, or empty (fail open to ds4).
// Env-only, exactly like Python — the profile dir's settings.json holds the
// per-profile provider API key (DeepSeek/Nous/OpenRouter), a different
// credential than the Anthropic subscription token a classifier call needs.
// Falling back to it would both diverge from Python and leak a provider key
// to api.anthropic.com.
func classifierToken() string {
	return strings.TrimSpace(os.Getenv("DS4_CLASSIFIER_TOKEN"))
}

// relayClassifier forwards a classifier-shaped request to the Anthropic
// subscription using a SEPARATE deadline-free client (no RELAY_TIMEOUT
// wrapper). Returns true when fully handled (streamed), false when it failed
// and the caller should fall through to the ds4 relay. A 400 from Anthropic
// is streamed as-is — the classifier's shape is Anthropic's own, so a 400
// means Claude Code sent something unexpected and failing open would mask it.
func (h *Handler) relayClassifier(body []byte, endpoint string, token string, w http.ResponseWriter) bool {
	raw, err := jsonpy.Marshal(body, nil)
	if err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return false
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("content-length", fmt.Sprint(len(raw)))
	// deadline-free client
	resp, err := h.classifierClient.Do(req)
	if err != nil {
		return false
	}
	switch {
	case resp.StatusCode == 400:
		// 400 is terminal — relay it so Claude Code sees the real error
		// (Python streams the HTTPError body), never masking it by failing open.
		streamResponse(w, resp, endpoint)
		return true
	case resp.StatusCode < 400:
		// A successful response (2xx, or a 3xx after redirect-following) is
		// streamed back, matching Python's urlopen success path.
		streamResponse(w, resp, endpoint)
		return true
	default:
		// Any other status (401/403/429/5xx...) is an error in Python (urlopen
		// raises HTTPError) — fail open to the ds4 relay.
		resp.Body.Close()
		return false
	}
}
