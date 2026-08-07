// Package proxy is the Go reimplementation of src/proxy.py's per-profile HTTP
// proxy: it authenticates local clients, rewrites the Claude Code request for
// the profile's upstream (model sentinel -> real model + reasoning_effort,
// thinking-disable below the no-think budget, ZDR block, max_tokens clamp, and
// placeholder thinking blocks), retries transient upstream errors, and streams
// the response back.
package proxy

import (
	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// effortMap maps the ds4-* sentinel Claude Code sends to the reasoning_effort
// value the upstream accepts. This is proxy.py's EFFORT table.
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
					// Insert before "messages" so a freshly-set key does not
					// land at the end of the object; that was the byte output
					// of the Python implementation.
					root.SetBefore("reasoning_effort", jsonpy.Val(effort), "messages")
				}
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
