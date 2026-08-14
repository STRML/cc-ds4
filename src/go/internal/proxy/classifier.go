package proxy

import (
	"bytes"
	"fmt"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
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

// classifierModel is the default Anthropic model id the classifier body is
// pointed at. ds4-high is not a valid Anthropic model, so a classifier routed
// to the subscription must carry a real one. Mirrors classifier.py's default
// model for the Anthropic route. DS4_CLASSIFIER_MODEL overrides it (the ledger
// noted Go hardcoded the default; the env knob is Python-parity).
const classifierModel = "claude-sonnet-5"

// classifierModelOverride returns DS4_CLASSIFIER_MODEL when the key is present,
// else the default. Mirrors proxy.py's os.environ.get("DS4_CLASSIFIER_MODEL",
// "claude-sonnet-5"): an ABSENT key falls back, but an explicitly-set value —
// including an empty string — is used verbatim. LookupEnv distinguishes the two
// (os.Getenv cannot); TrimSpace is deliberately NOT applied, because Python
// sends the env value exactly as-is (review finding, 2026-08-10).
func classifierModelOverride() string {
	if v, ok := os.LookupEnv("DS4_CLASSIFIER_MODEL"); ok {
		return v
	}
	return classifierModel
}

// classifierUA is the User-Agent the classifier relay sends. api.anthropic.com
// is Cloudflare-fronted and 403s default stdlib UAs; Python's _relay_anthropic
// sends the same curl/8.4.0 identity (proxy.py:77-79,1045), and the main relay
// path mirrors it at relay.go:109-111.
const classifierUA = "curl/8.4.0"

// anthropicKeys are the request keys that belong on an Anthropic Messages
// call. Everything else in the ds4 payload — provider (zdr block), metadata,
// reasoning_effort — is ds4-specific and must not leave the proxy.
// Whitelisting keeps a misdetected request from carrying ds4 body shape to
// Anthropic. Mirrors classifier.py's _ANTHROPIC_KEYS.
var anthropicKeys = map[string]bool{
	"model":       true,
	"max_tokens":  true,
	"thinking":    true,
	"messages":    true,
	"tools":       true,
	"tool_choice": true,
	"system":      true,
	"stream":      true,
	"temperature": true,
}

// classifierBody returns the classifier request pointed at Anthropic: only the
// Anthropic-relevant keys are kept (order-preserving), with model set to a
// real Anthropic id. The ds4-specific fields (provider, reasoning_effort,
// metadata) are dropped — Anthropic does not accept them on the subscription,
// and they must not carry ds4 body shape across. Mirrors classifier.py's
// classifier_body(); the whitelist runs BEFORE the model swap, exactly as
// Python builds the dict from the payload and then sets model.
func classifierBody(data []byte, model string) ([]byte, error) {
	return jsonpy.Marshal(data, func(root *jsonpy.OrderedValue) {
		for _, k := range root.Keys() {
			if !anthropicKeys[k] {
				root.Delete(k)
			}
		}
		root.SetString("model", model)
	})
}

// anthropicVersion is the API version header the classifier relay sends,
// matching classifier.py's ANTHROPIC_VERSION.
const anthropicVersion = "2023-06-01"

// classifierTiers is the set of sentinels the classifier can arrive under:
// the flash family, whichever slot Claude Code maps its small fast model to.
// The pro family is the main loop and is never the gate, so excluding it keeps
// a small pro request from ever being mistaken for one.
var classifierTiers = map[string]bool{
	"ds4-flash-xhigh":  true,
	"ds4-flash-medium": true,
}

// isClassifier reports whether a request body is the auto-mode permission
// classifier: a flash-family sentinel with a max_tokens at or below the
// no-think budget. Subagents ride the same sentinel but at a much larger
// max_tokens, so the size threshold separates them. The ok return from
// PeekModelMaxTokens enforces the integer guard — a request with NO max_tokens
// is not a classifier, even though a missing value would read as 0.
//
// Getting this wrong fails silently in the dangerous direction: an undetected
// classifier is judged on ds4 instead of the trusted route, with nothing in
// the log to say so. TestClassifierTiersAreFlashSentinels pins the names.
func (h *Handler) isClassifier(body []byte) bool {
	model, mt, ok := jsonpy.PeekModelMaxTokens(body)
	return ok && classifierTiers[model] && mt <= classifierMaxTokens
}

// classifierMaxTokens is the size ceiling that separates the permission gate
// from a subagent riding the same sentinel.
//
// It deliberately does NOT reuse nothinkBelow, even though both default to
// 8192. nothinkBelow is a documented user knob (DS4_NOTHINK_BELOW, in the
// README and every profile doc) for how big a request may be before thinking
// is disabled, and install.sh persists the whole DS4_* namespace into the
// launch agent. Sharing the value would mean widening the no-think window also
// widened what counts as the classifier — so DS4_NOTHINK_BELOW=32768, set for
// an unrelated reason, would start routing every flash subagent request under
// 32K to api.anthropic.com on the subscription token, rebuilt through a
// whitelist that drops fields. A trust boundary does not move as a side effect
// of a thinking-budget preference; it gets its own knob.
var classifierMaxTokens = envInt("DS4_CLASSIFIER_MAX_TOKENS", 8192)

// requiresZDR reports the proxy-local per-request ZDR demand and strips it from
// the body.
//
// The demand arrives either as the x-ds4-require-zdr header or as a
// ds4_require_zdr body field. Both are proxy-local: the field is not part of
// the Messages API, so it is removed before the body goes upstream rather than
// forwarded as an unknown key.
//
// The returned body is the one to forward. On a parse failure the original is
// returned unchanged, and the header alone decides.
func requiresZDR(r *http.Request, body []byte) (bool, []byte) {
	hdr := strings.ToLower(strings.TrimSpace(r.Header.Get("x-ds4-require-zdr")))
	required := hdr == "1" || hdr == "true" || hdr == "yes"

	// The re-serialize is skipped unless the field is actually there. Every
	// request pays this call, the field is absent on virtually all of them, and
	// on a 1M-context profile a needless parse-and-re-emit of a multi-megabyte
	// body is pure waste on the hot path.
	if !bytes.Contains(body, []byte(zdrRequireField)) {
		return required, body
	}

	var sawField bool
	stripped, err := jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		if v := root.Get(zdrRequireField); v != nil {
			// Python used `payload.pop(field, False) is True`, so only a real
			// boolean true counts. Raw() is the literal as written, which keeps
			// the string "true" and the number 1 from passing as a demand.
			sawField = v.Raw() == "true"
			root.Delete(zdrRequireField)
		}
	})
	if err != nil {
		return required, body
	}
	return required || sawField, stripped
}

// zdrRequireField is the body-field spelling of the per-request ZDR demand.
const zdrRequireField = "ds4_require_zdr"

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
	// Whitelist the body to Anthropic's keys and swap the model BEFORE it is
	// sent: the raw ds4 body carries "model": "ds4-high" (not a valid Anthropic
	// model, so Anthropic would 400 every call) plus ds4-specific fields that
	// must not ride to api.anthropic.com.
	raw, err := classifierBody(body, h.classifierModel)
	if err != nil {
		return false
	}
	return h.sendClassifier(raw, endpoint, token, w)
}

// sendClassifier ships an already-built classifier body. The or-ds4 route
// builds its own (different whitelist, its own model, and a ZDR block that the
// Anthropic rebuild would strip), so the send half is shared and the body half
// is not.
func (h *Handler) sendClassifier(raw []byte, endpoint string, token string, w http.ResponseWriter) bool {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return false
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("content-length", fmt.Sprint(len(raw)))
	// api.anthropic.com is Cloudflare-fronted and 403s default stdlib UAs;
	// mirror the main relay path's curl identity (relay.go:109-111).
	req.Header.Set("user-agent", classifierUA)
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

// classifierRoute is DS4_CLASSIFIER: which boundary the auto-mode permission
// gate is judged at. "anthropic" (default) sends it to the subscription, "zdr"
// to the or-ds4 OpenRouter route with the ZDR block forced on, and "ds4" keeps
// it on the profile's own upstream.
//
// This is a trust-boundary knob, not a performance one: "ds4" is an explicit
// decision to let DeepSeek judge whether a tool call is safe, and "zdr" a
// preference for the ZDR route over the subscription. Ignoring it would
// silently override the user on the one setting where that matters most.
//
// "zdr" is a preference and not a prohibition, which is worth being exact
// about: when or-ds4 cannot serve the route — not installed, or no key — the
// relay falls through to "anthropic" and then to ds4, matching Python's order.
// So the one thing DS4_CLASSIFIER=zdr does not promise is that the classifier
// never reaches the subscription. A user who needs that has to leave or-ds4
// configured; there is deliberately no fail-closed mode, because a classifier
// that cannot run at all would brick auto mode entirely.
func classifierRoute() string {
	switch r := os.Getenv("DS4_CLASSIFIER"); r {
	case "zdr", "ds4", "anthropic":
		return r
	default:
		return "anthropic"
	}
}

// ordsClassifierKeys is the whitelist the or-ds4 classifier body is rebuilt
// from. OpenRouter's /v1/messages takes the Anthropic shape, so this is the
// same set the Anthropic relay uses; everything ds4-specific stays behind.
var ordsClassifierKeys = map[string]bool{
	"max_tokens": true, "messages": true, "system": true,
	"tools": true, "tool_choice": true, "temperature": true,
}

// ORDS4Path is the messages path on the or-ds4 upstream. The profile's base
// already ends in /api, so a leading /api here would double up.
const ORDS4Path = "/v1/messages"

// orDS4Endpoint builds the or-ds4 (OpenRouter ZDR) classifier request.
//
// It returns an empty url when the route cannot serve: the profile is not
// installed, or has no key.
//
// Python also skipped this route when openrouter's own breaker was open. That
// check could never fire in either implementation: the breaker is keyed on a
// profile having a failover target, openrouter has none, so its circuit is
// permanently closed. It is left out rather than reproduced as decoration. If
// openrouter ever gains a target, add it back with a test that proves it fires.
func (h *Handler) orDS4Endpoint(body []byte) (out []byte, url, key string) {
	var ocfg profiles.Profile
	for _, p := range profiles.All() {
		if p.Name == "openrouter" {
			ocfg = p
		}
	}
	if ocfg.Name == "" {
		return nil, "", ""
	}
	if info, err := os.Stat(ocfg.Dir); err != nil || !info.IsDir() {
		return nil, "", ""
	}
	key = os.Getenv("DS4_KEY_OPENROUTER")
	if key == "" {
		key = readKeyFromDir(ocfg.Dir)
	}
	if key == "" {
		return nil, "", ""
	}
	model := os.Getenv("DS4_ORDS4_CLASSIFIER_MODEL")
	if model == "" {
		model = ocfg.Model
	}
	rebuilt, err := jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		for _, k := range root.Keys() {
			if !ordsClassifierKeys[k] {
				root.Delete(k)
			}
		}
		root.SetString("model", model)
		// Thinking is forced off: the classifier is always small and no
		// provider serving V4 implements Claude Code's adaptive thinking.
		root.Set("thinking", jsonpy.MustObj("type", "disabled"))
		// The ZDR block is the reason this route exists, so it is not optional
		// here the way the per-profile DS4_ZDR gate is.
		root.Set("provider", jsonpy.MustObj("zdr", true, "data_collection", "deny"))
	})
	if err != nil {
		return nil, "", ""
	}
	return rebuilt, strings.TrimRight(ocfg.Upstream, "/") + ORDS4Path, key
}
