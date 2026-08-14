package profiles

// table is the profile table: name, port, config dir, upstream, and the
// per-profile rewriting flags. This file is the single source of truth for
// every one of those values — nothing else in the tree may declare a profile
// port, upstream, or model.
//
// It used to be generated from the PROFILES dict in src/proxy.py by
// cmd/genprofiles, which regex-parsed the Python literal. That inverted the
// dependency the wrong way: the shipping binary's schema was pinned to a file
// it did not build against, and the generator could only carry flat scalars,
// so a nested value (family models) or a list (ZDR skips) had no way across.
// Adding a field here now means adding a field, not teaching a regex.
//
// Dir keeps a literal "~"; All() expands it, so the table stays
// host-independent and testable without a home directory.
var table = []Profile{
	{
		Name:     "direct",
		Port:     31500,
		Dir:      "~/.claude-ds4",
		Upstream: "https://api.deepseek.com/anthropic",
		// DeepSeek's own endpoint takes real model names and ignores
		// reasoning_effort, so there is no single default model here and no
		// effort injection — the family map below is what resolves a sentinel.
		Model:  "",
		Effort: false,
		// api.deepseek.com accepts only the bare ids; the versioned -0813 /
		// -0731 names are an OpenRouter convention and 400 here. This is the
		// only profile where the pro family actually serves pro.
		FamilyModels: map[string]string{
			"pro":   "deepseek-v4-pro",
			"flash": "deepseek-v4-flash",
		},
		ZDRSkipModels: nil,
		ZDR:           false,
		Spend:         false,
		// The direct profile is a failover target for the 1M profiles. Its
		// endpoint counts input + completion against the same 1M cap, while
		// Claude Code budgets 131072 output against the advertised window — so
		// an uncapped failover session overflows at ~923K input and 400s
		// ("maximum context length"). The same cap as the other 1M profiles
		// keeps a failed-over request inside the endpoint's real limit.
		MaxOut: 65536,
		// Only this endpoint requires an assistant tool_use message to replay
		// its thinking block. Claude Code 2.x does replay it, so this guards a
		// path that drops it rather than fixing an observed failure.
		Inject:   true,
		Failover: "",
	},
	{
		Name:     "openrouter",
		Port:     31501,
		Dir:      "~/.claude-or-ds4",
		Upstream: "https://openrouter.ai/api",
		// :nitro is OR's sort=throughput variant — the fastest providers. The
		// suffix rides the model id into every request; exact-id consumers
		// (pricing, the failover remap) strip it back to the base id first.
		Model:  "deepseek/deepseek-v4-flash-0731:nitro",
		Effort: true,
		// pro-0813 has no usable host on OR (404), so the pro family falls
		// back to flash here. Never the unversioned originals — OR has them
		// but they bill differently.
		FamilyModels: map[string]string{
			"pro":   "deepseek/deepseek-v4-flash-0731:nitro",
			"flash": "deepseek/deepseek-v4-flash-0731:nitro",
		},
		ZDR: true,
		// pro-0813's only host is DeepSeek itself, which rejects the ZDR block
		// (404 "no endpoints matching data policy"). Nothing routes to that id
		// today, since FamilyModels above points pro at flash, so this entry is
		// currently unreachable. It is kept as the paired half of that
		// decision: whoever points pro back at pro-0813 needs the ZDR skip in
		// the same row, and finding it already here is the difference between
		// a working pro tier and a 404 on every request.
		ZDRSkipModels: []string{"deepseek/deepseek-v4-pro-0813"},
		Spend:         true,
		// Smallest max_completion_tokens in the ZDR pool (DeepInfra, Io Net).
		MaxOut:   65536,
		Inject:   false,
		Failover: "",
	},
	{
		Name:     "nous",
		Port:     31502,
		Dir:      "~/.claude-nous",
		Upstream: "https://inference-api.nousresearch.com",
		Model:    "deepseek/deepseek-v4-flash-0731",
		Effort:   true,
		// Nous lists no deepseek pro at all, so both families resolve to
		// flash. A failed-over nous request rides openrouter's family map.
		FamilyModels: map[string]string{
			"pro":   "deepseek/deepseek-v4-flash-0731",
			"flash": "deepseek/deepseek-v4-flash-0731",
		},
		ZDRSkipModels: nil,
		// Nous 403s any provider block, ZDR or otherwise.
		ZDR:    false,
		Spend:  true,
		MaxOut: 65536,
		Inject: false,
		// Nous sits behind Cloudflare and has real bad stretches (524/503).
		// When its transient errors pass the breaker threshold its requests
		// are served by openrouter (cheap flash) until a probe recovers — not
		// direct, which bills per-token on api.deepseek.com and is what
		// emptied the balance.
		Failover: "openrouter",
	},
}
