// Package proxy is the Go reimplementation of src/proxy.py's per-profile HTTP
// proxy: it authenticates local clients, rewrites the Claude Code request for
// the profile's upstream (model sentinel -> real model + reasoning_effort,
// thinking-disable below the no-think budget, ZDR block, max_tokens clamp, and
// placeholder thinking blocks), retries transient upstream errors, and streams
// the response back.
package proxy

import (
	"strings"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// effortMap maps the ds4-* sentinel Claude Code sends to the reasoning_effort
// value the upstream accepts. This is proxy.py's EFFORT table.
// failoverModel remaps ds4-* sentinels onto the direct target's flash model,
// mirroring FAILOVER_MODEL in proxy.py. Flash only: the direct profile's own
// config runs flash for every tier, and the cost difference is what makes
// failover worth it.
var failoverModel = map[string]string{
	"ds4-max":   "deepseek-v4-flash[1m]",
	"ds4-xhigh": "deepseek-v4-flash[1m]",
	"ds4-high":  "deepseek-v4-flash[1m]",
	"ds4-low":   "deepseek-v4-flash[1m]",
	// The qualified id of the or-ds4/nous profiles is here too (proxy.py's
	// FAILOVER_MODEL has it): a :nitro variant strips to this base id, and
	// without the key it would ride the variant onto the direct target, which
	// 400s on it.
	"deepseek/deepseek-v4-flash-0731": "deepseek-v4-flash[1m]",
}

var effortMap = map[string]string{
	"ds4-max":   "max",
	"ds4-xhigh": "xhigh",
	"ds4-high":  "high",
	"ds4-low":   "low",
}

// nothinkBelow is the max_tokens threshold at or below which thinking is
// disabled (DS4_NOTHINK_BELOW, default 8192 in proxy.py).
const nothinkBelow = 8192

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

// rewrite edits a request body for one profile, mirroring rewrite() in
// src/proxy.py. The tree is mutated through jsonpy's exported accessors and
// re-emitted with Python-identical spacing and escaping.
func rewrite(body []byte, cfg profiles.Profile) ([]byte, error) {
	return jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		// Sentinel -> real model + reasoning_effort, gated on the profile
		// having a model at all (cfg["model"] truthiness in proxy.py). The
		// direct profile has an empty model, so its requests keep the
		// sentinel. The override pin (effort-override file) is a Task 5
		// concern; the tier default is all Task 4 needs.
		if cfg.Model != "" {
			model := root.Get("model")
			if model != nil && model.IsString() {
				tier := model.String()
				if effort, ok := effortMap[tier]; ok {
					root.SetString("model", cfg.Model)
					// Set appends, matching Python's dict insertion order:
					// json.dumps places reasoning_effort at the end of the
					// object, after messages.
					root.Set("reasoning_effort", jsonpy.Val(effort))
				} else if isAnthropicModel(tier) {
					// A literal Anthropic model (sonnet, claude-sonnet-4-5,
					// opus, ...) bypassed the sentinel system and would bill
					// real Anthropic rates on this profile's upstream. Remap it
					// defensively, mirroring proxy.py's rewrite() third branch;
					// no reasoning_effort is added here, exactly like Python.
					root.SetString("model", cfg.Model)
				}
			}
		} else if cfg.FailoverTarget && root.Get("model") != nil && root.Get("model").IsString() {
			// A profile with an empty model being used as the FAILOVER TARGET
			// (e.g. direct) takes real model names and ignores
			// reasoning_effort. A ds4-* sentinel is remapped via FAILOVER_MODEL
			// to flash (proxy.py:164-171); no reasoning_effort is added. A
			// standalone direct-profile request (not a failover target) keeps
			// the sentinel, matching Python.
			// A :nitro variant suffix is not a failoverModel key; the direct
			// target 400s on it. Match on the base id (mirrors proxy.py).
			key := root.Get("model").String()
			if i := strings.IndexByte(key, ':'); i >= 0 {
				key = key[:i]
			}
			if flash, ok := failoverModel[key]; ok {
				root.SetString("model", flash)
			}
		}

		// ZDR provider block (OpenRouter only). DS4_ZDR=0 never enables ZDR on
		// a profile whose table row does not support it; that env gate is a
		// serve-time concern, cfg.ZDR is the table row.
		if cfg.ZDR {
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
