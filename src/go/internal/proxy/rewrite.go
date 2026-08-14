// Package proxy is the Go reimplementation of src/proxy.py's per-profile HTTP
// proxy: it authenticates local clients, rewrites the Claude Code request for
// the profile's upstream (model sentinel -> real model + reasoning_effort,
// thinking-disable below the no-think budget, ZDR block, max_tokens clamp, and
// placeholder thinking blocks), retries transient upstream errors, and streams
// the response back.
package proxy

import (
	"os"
	"strings"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// failoverModel remaps a model id onto the failover target's own id. The
// target is openrouter (nous -> openrouter), and this is only a safety net:
// rewrite resolves every sentinel through the target's family map before the
// remap runs, so what reaches here is an id the sentinel path did not claim —
// chiefly a profile's own qualified id riding along after failover.
//
// Both families land on flash: pro-0813 has no usable OpenRouter host.
var failoverModel = map[string]string{
	"ds4-pro-xhigh":    "deepseek/deepseek-v4-flash-0731:nitro",
	"ds4-pro-medium":   "deepseek/deepseek-v4-flash-0731:nitro",
	"ds4-flash-xhigh":  "deepseek/deepseek-v4-flash-0731:nitro",
	"ds4-flash-medium": "deepseek/deepseek-v4-flash-0731:nitro",
	// Nous's own qualified id. A :nitro variant strips to this base id first;
	// without the key the plain id would ride onto the target unchanged.
	"deepseek/deepseek-v4-flash-0731": "deepseek/deepseek-v4-flash-0731:nitro",
}

// sentinel is what a ds4-* model name decodes to: which model family serves
// the request, and how hard that model thinks by default.
type sentinel struct {
	family string
	effort string
}

// sentinelTable maps the ds4-* names Claude Code sends to (family, default
// effort). The two halves are independent: family picks the model, effort
// picks the thinking budget.
//
//	fable  -> ds4-pro-xhigh      opus  -> ds4-pro-medium
//	sonnet -> ds4-flash-xhigh    haiku -> ds4-flash-medium
//
// The default effort applies only when the client sent none. /effort puts
// reasoning_effort in the body and that wins — the knob is meant to change how
// hard the model thinks, not to be overwritten by the sentinel it rode in on.
var sentinelTable = map[string]sentinel{
	"ds4-pro-xhigh":    {"pro", "xhigh"},
	"ds4-pro-medium":   {"pro", "medium"},
	"ds4-flash-xhigh":  {"flash", "xhigh"},
	"ds4-flash-medium": {"flash", "medium"},
}

// effortLevels is the set OpenRouter accepts. A client-sent reasoning_effort
// outside it is ignored rather than forwarded: OpenRouter takes the parameter
// and DeepSeek drops unknown values without error, so a typo would silently
// change nothing while looking like it worked.
var effortLevels = map[string]bool{
	"max": true, "xhigh": true, "high": true, "medium": true,
	"low": true, "minimal": true, "none": true,
}

// nothinkBelow is the max_tokens threshold at or below which thinking is
// disabled. DS4_NOTHINK_BELOW moves it; README and every profile doc tell users
// so, and install.sh sweeps the whole DS4_* namespace into the plist, so a
// hardcoded constant made a documented knob silently inert.
//
// Read once at init, matching Python's import-time binding: the value must not
// change under a running process mid-session.
var nothinkBelow = envInt("DS4_NOTHINK_BELOW", 8192)

// zdrEnabled reports whether the ZDR provider block may be injected at all.
// DS4_ZDR=0 is the documented escape hatch for a provider-blocked build (a 403
// with "error code: 1010" from OpenRouter). It only ever DISABLES: a profile
// whose table row has no ZDR support never gains it from this.
func zdrEnabled() bool {
	return os.Getenv("DS4_ZDR") != "0"
}

// lowContext is proxy.py's LOW_CONTEXT: OpenRouter endpoints whose context is
// smaller than the 1M the profile advertises. The ZDR block pins their names
// so a long session routed here overflows the endpoint rather than the
// declared window.
var lowContext = []string{"Io Net"}

// thinkingDisabled is the payload["thinking"] sentinel proxy.py writes when
// max_tokens is at or below the no-think budget. Equality against it decides
// whether the placeholder-injection pass runs.
var thinkingDisabled = jsonpy.MustObj("type", "disabled")

// placeholder is proxy.py's PLACEHOLDER: a fabricated thinking block that
// satisfies DeepSeek's history validation. DeepSeek 400s on an assistant
// tool_use message with no thinking block, and does not validate the
// signature, so the placeholder is enough.
var placeholder = jsonpy.MustObj(
	"type", "thinking",
	"thinking", "(elided)",
	"signature", "ds4-proxy",
)

// rewrite edits a request body for one profile. The tree is mutated through
// jsonpy's exported accessors and re-emitted with Python-identical spacing and
// escaping.
//
// effortPin is passed in rather than read from cfg because the two belong to
// different profiles on a failed-over request: the model comes from the target,
// but /ds4-effort pinned the ORIGIN. Reading it from cfg silently dropped a
// user's pin the moment their profile failed over.
func rewrite(body []byte, cfg profiles.Profile, effortPin string) ([]byte, error) {
	return jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		// Sentinel -> real model (+ reasoning_effort where the upstream honors
		// it). The sentinel's family half selects the model from this
		// profile's family map; a profile with no family entry falls back to
		// its single default model.
		if m := root.Get("model"); m != nil && m.IsString() {
			tier := m.String()
			sen, isSentinel := sentinelTable[tier]
			model := cfg.FamilyModels[sen.family]
			if model == "" {
				model = cfg.Model
			}
			switch {
			case model == "":
				// Nothing to rewrite to. Leave the request alone rather than
				// blanking its model.
			case isSentinel && effortLevels[root.Get("reasoning_effort").String()]:
				// /effort, or an explicit request. It beats the sentinel's
				// default; the level is already in the body, so only the model
				// needs swapping.
				root.SetString("model", model)
			case isSentinel && cfg.Effort:
				root.SetString("model", model)
				effort := sen.effort
				if effortPin != "" {
					effort = effortPin
				}
				// Set appends, matching Python's dict insertion order:
				// json.dumps places reasoning_effort at the end of the object,
				// after messages.
				root.Set("reasoning_effort", jsonpy.Val(effort))
			case isSentinel:
				// The upstream ignores reasoning_effort (direct). Swap the
				// sentinel for the model id and inject nothing.
				root.SetString("model", model)
			case isAnthropicModel(tier):
				// A literal Anthropic model (sonnet, claude-sonnet-4-5,
				// opus, ...) bypassed the sentinel system and would bill real
				// Anthropic rates on this profile's upstream. Remap it
				// defensively; no reasoning_effort is added.
				root.SetString("model", model)
			}
		}

		if cfg.FailoverTarget {
			// This profile is serving another profile's traffic. Anything the
			// sentinel path above did not claim — chiefly the failed-over
			// profile's own qualified id — is remapped onto an id this target
			// actually serves. A :nitro variant is not a failoverModel key, so
			// match on the base id.
			if m := root.Get("model"); m != nil && m.IsString() {
				key := m.String()
				if i := strings.IndexByte(key, ':'); i >= 0 {
					key = key[:i]
				}
				if remapped, ok := failoverModel[key]; ok {
					root.SetString("model", remapped)
				}
			}
		}

		// ZDR provider block (OpenRouter only). DS4_ZDR=0 never enables ZDR on
		// a profile whose table row does not support it; that env gate is a
		// serve-time concern, cfg.ZDR is the table row.
		//
		// Some models have no ZDR-capable host — pro-0813's only endpoint is
		// DeepSeek itself, which rejects the block with a 404 ("no endpoints
		// matching data policy"). Such models are listed per profile so the
		// escape hatch is configuration, not a silent special case.
		if cfg.ZDR && zdrEnabled() && !skipZDR(root.Get("model").String(), cfg.ZDRSkipModels) {
			prov := root.Get("provider")
			if !prov.IsObject() {
				prov = jsonpy.MustObj()
			}
			prov.Set("zdr", jsonpy.Val(true))
			prov.Set("data_collection", jsonpy.Val("deny"))
			ignore := []string{}
			for _, it := range prov.Get("ignore").Items() {
				s := it.String()
				if !containsStr(lowContext, s) {
					ignore = append(ignore, s)
				}
			}
			ignore = append(ignore, lowContext...)
			prov.Set("ignore", jsonpy.MustArr(strsToAny(ignore)...))
			root.Set("provider", prov)
		}

		// max_tokens clamp, then the thinking decision from the post-clamp
		// value — a DS4_NOTHINK_BELOW raised above MaxOut must still disable
		// thinking on a clamped request. Only a present integer is a candidate,
		// mirroring Python's isinstance(want, int) guard: a request with no
		// max_tokens keeps its thinking on.
		if mt, ok := root.AsInt("max_tokens"); ok {
			if cfg.MaxOut > 0 && mt > cfg.MaxOut {
				root.SetInt("max_tokens", cfg.MaxOut)
				mt = cfg.MaxOut
			}
			if mt <= nothinkBelow {
				root.Set("thinking", thinkingDisabled)
			}
		}

		// With thinking off the endpoint stops asking for the block, so only
		// repair a history on requests that still have thinking on. The check
		// is deep equality against the disabled sentinel, matching Python's
		// payload.get("thinking") != DISABLED — an absent thinking (None !=
		// DISABLED) or an object with any extra key still counts as on.
		if cfg.Inject && root.Get("thinking").Raw() != thinkingDisabled.Raw() {
			injectMissingThinking(root)
		}
	})
}

// injectMissingThinking walks messages and inserts a placeholder thinking block
// at position 0 of any assistant message whose content holds a tool_use block
// and no thinking block. Returns the number of messages repaired, mirroring
// inject_missing_thinking in proxy.py.
func injectMissingThinking(root *jsonpy.OrderedValue) int {
	n := 0
	for _, m := range root.Get("messages").Items() {
		if m == nil || !m.IsObject() || m.Get("role").String() != "assistant" {
			continue
		}
		blocks := m.Get("content")
		if !blocks.IsArray() {
			continue
		}
		hasToolUse := false
		hasThinking := false
		for _, b := range blocks.Items() {
			if !b.IsObject() {
				continue
			}
			switch b.Get("type").String() {
			case "tool_use":
				hasToolUse = true
			case "thinking":
				hasThinking = true
			}
		}
		if hasToolUse && !hasThinking {
			blocks.Insert(0, placeholder)
			n++
		}
	}
	return n
}

// isAnthropicModel mirrors proxy.py's _is_anthropic_model: True for a literal
// Anthropic model id that the sentinel system missed. The ds4-* sentinels are
// already handled by effortMap; the profiles' real upstream model ids
// (deepseek/deepseek-v4-flash-0731, ...) must not match any of these substrings.
func isAnthropicModel(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "sonnet") ||
		strings.Contains(n, "opus") ||
		strings.Contains(n, "haiku") ||
		strings.Contains(n, "claude-")
}

// skipZDR reports whether model matches any prefix in skips. Prefix rather
// than exact match: the id may carry a variant suffix (:nitro) that has no
// bearing on which host serves it.
func skipZDR(model string, skips []string) bool {
	for _, p := range skips {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func strsToAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
