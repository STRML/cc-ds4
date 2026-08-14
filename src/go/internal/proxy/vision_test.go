package proxy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// fakeClaudeBin writes an executable shell script at dir/claude that echoes
// stdout, and returns its path for DS4_CLAUDE_BIN. Standing in for the real
// `claude -p --model haiku` child so tests never shell out to Anthropic.
func fakeClaudeBin(t *testing.T, stdout string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// countingClaudeBin is fakeClaudeBin plus a spawn counter. The script appends
// to a tally file on each invocation and the returned func reads it, so a test
// asserts how many children actually ran rather than inferring it from output.
func countingClaudeBin(t *testing.T) (bin string, calls func() int) {
	t.Helper()
	dir := t.TempDir()
	tally := filepath.Join(dir, "calls")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho x >> " + tally + "\n" +
		"cat <<'EOF'\n" + `{"type":"result","result":"a description"}` + "\nEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int {
		raw, err := os.ReadFile(tally)
		if err != nil {
			return 0 // never invoked
		}
		return strings.Count(string(raw), "x")
	}
}

// distinctPNG returns valid base64 whose decoded bytes differ per index, so
// each image hashes to its own cache key and none is served from cache.
func distinctPNG(i int) string {
	return base64.StdEncoding.EncodeToString([]byte("png-payload-" + strconv.Itoa(i)))
}

func imageBlock(mediaType, data string) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- env scrubbing -------------------------------------------------------

// TestScrubbedEnvStripsClaudeAnthropicDs4Cmux is the regression test the
// brief calls out by name: a describer child that inherits any of these
// families routes back into this proxy (or another ds4 profile), which is
// infinite recursion, not a describe. Every prefix/exact family the Python
// _env() recipe scrubs must be absent from the child env, and something
// ordinary must survive so the test can't pass by accidentally stripping
// everything.
func TestScrubbedEnvStripsClaudeAnthropicDs4Cmux(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:31500")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/x/.claude-ds4")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("DS4_VISION", "1")
	t.Setenv("DS4_CLAUDE_BIN", "/usr/local/bin/claude")
	t.Setenv("CMUX_TASK_RUN_ID", "abc123")
	t.Setenv("CMUX", "1")
	t.Setenv("DS4_VISION_TEST_SURVIVOR", "keep-me") // starts with DS4_, must NOT survive
	t.Setenv("VISION_TEST_SURVIVOR", "keep-me")     // ordinary var, must survive

	env := scrubbedEnv()

	forbidden := []string{"ANTHROPIC_", "CLAUDE_CODE_", "DS4_", "CLAUDE_CONFIG_DIR", "CLAUDECODE", "CMUX"}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		for _, bad := range forbidden {
			if strings.HasPrefix(k, bad) || k == bad {
				t.Errorf("child env leaked %q (matches scrub family %q)", k, bad)
			}
		}
	}
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "VISION_TEST_SURVIVOR=") {
			found = true
		}
	}
	if !found {
		t.Error("scrubbedEnv stripped an ordinary var it should have kept")
	}
}

// TestScrubEnvKeyExactAndProxyVars pins the non-prefix parts of the scrub:
// CLAUDECODE/CLAUDE_CONFIG_DIR (exact match, not prefix), NODE_OPTIONS,
// AI_AGENT, and the *_proxy family checked case-insensitively.
func TestScrubEnvKeyExactAndProxyVars(t *testing.T) {
	cases := map[string]bool{
		"CLAUDECODE":         true,
		"CLAUDE_CONFIG_DIR":  true,
		"NODE_OPTIONS":       true,
		"AI_AGENT":           true,
		"http_proxy":         true,
		"HTTPS_PROXY":        true,
		"all_proxy":          true,
		"NO_PROXY":           true,
		"HOME":               false,
		"PATH":               false,
		"TERM":               false,
		"CLAUDECODEX":        false, // exact match only, not a prefix hit
		"CLAUDE_CONFIG_DIRS": false, // exact match only
	}
	for k, want := range cases {
		if got := scrubEnvKey(k); got != want {
			t.Errorf("scrubEnvKey(%q) = %v, want %v", k, got, want)
		}
	}
}

// ---- DS4_VISION gate ------------------------------------------------------

// TestApplyVisionDisabledPassesThrough pins the DS4_VISION=0 path: the body
// comes back byte-identical, image block and all, restoring the old
// pass-through deliberately (proxy.py's comment at the call site).
func TestApplyVisionDisabledPassesThrough(t *testing.T) {
	t.Setenv("DS4_VISION", "0")
	body := mustJSON(t, map[string]any{
		"model": "ds4-flash-medium",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{imageBlock("image/png", "aGVsbG8=")}},
		},
	})
	got, err := applyVision(body, profiles.Profile{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("DS4_VISION=0 must pass the body through unchanged:\n got:  %s\n want: %s", got, body)
	}
}

// TestApplyVisionDefaultOnMatchesUnset pins that an absent DS4_VISION behaves
// like "1", not like "0" — os.environ.get(..., "1") in Python only
// substitutes the default when the key is truly absent.
func TestApplyVisionDefaultOnMatchesUnset(t *testing.T) {
	os.Unsetenv("DS4_VISION")
	if !visionEnabled() {
		t.Error("visionEnabled() with DS4_VISION unset should default to true")
	}
	t.Setenv("DS4_VISION", "")
	if visionEnabled() {
		t.Error("visionEnabled() with DS4_VISION=\"\" (set but empty) should be false, not the default")
	}
	t.Setenv("DS4_VISION", "0")
	if visionEnabled() {
		t.Error("visionEnabled() with DS4_VISION=0 should be false")
	}
}

// ---- malformed / unsupported image sources become the placeholder --------

func firstTextBlock(t *testing.T, body []byte) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, body)
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("no messages in %s", body)
	}
	msg, _ := messages[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content blocks in %s", body)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("block[0] type = %v, want text: %s", block["type"], body)
	}
	text, _ := block["text"].(string)
	return text
}

func TestApplyVisionPlaceholdersFileSource(t *testing.T) {
	t.Setenv("DS4_VISION", "1")
	body := mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image", "source": map[string]any{"type": "file", "path": "/etc/passwd"}},
			}},
		},
	})
	got, err := applyVision(body, profiles.Profile{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if text := firstTextBlock(t, got); text != visionPlaceholder {
		t.Errorf("file source: text = %q, want placeholder", text)
	}
}

func TestApplyVisionPlaceholdersMissingMetadata(t *testing.T) {
	t.Setenv("DS4_VISION", "1")
	cases := []map[string]any{
		{"type": "base64", "media_type": "image/png"},                // no data
		{"type": "base64", "data": "aGVsbG8="},                       // no media_type
		{"type": "base64", "media_type": "", "data": "aGVsbG8="},     // empty media_type
		{"type": "base64", "media_type": "image/png", "data": ""},    // empty data
		{"type": "base64", "media_type": "image/png", "data": "!!!"}, // invalid base64
	}
	for i, src := range cases {
		body := mustJSON(t, map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "image", "source": src},
				}},
			},
		})
		got, err := applyVision(body, profiles.Profile{Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if text := firstTextBlock(t, got); text != visionPlaceholder {
			t.Errorf("case %d (%v): text = %q, want placeholder", i, src, text)
		}
	}
}

// TestApplyVisionRecursesIntoToolResult pins that an image nested in
// tool_result.content is found and swapped — Claude Code puts
// Read/screenshot/MCP images there, and one left behind is silently dropped
// upstream.
func TestApplyVisionRecursesIntoToolResult(t *testing.T) {
	t.Setenv("DS4_VISION", "1")
	body := mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{
					"type": "tool_result",
					"content": []any{
						map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/x.png"}},
					},
				},
			}},
		},
	})
	got, err := applyVision(body, profiles.Profile{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	msg := payload["messages"].([]any)[0].(map[string]any)
	toolResult := msg["content"].([]any)[0].(map[string]any)
	inner := toolResult["content"].([]any)[0].(map[string]any)
	if inner["type"] != "text" || inner["text"] != visionPlaceholder {
		t.Errorf("nested image not swapped: %v", inner)
	}
}

// ---- full describe pipeline, against a fake claude binary -----------------

func fakeResultJSON(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": text,
	})
	return string(b)
}

func TestApplyVisionDescribesAndCachesImage(t *testing.T) {
	t.Setenv("DS4_VISION", "1")
	t.Setenv("DS4_CLAUDE_BIN", fakeClaudeBin(t, fakeResultJSON("a red circle on white")))
	cfg := profiles.Profile{Dir: t.TempDir()}

	png := base64.StdEncoding.EncodeToString([]byte("not really a png but bytes are bytes"))
	body := mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{imageBlock("image/png", png)}},
		},
	})

	got, err := applyVision(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := "[image transcribed by haiku]\na red circle on white"
	if text := firstTextBlock(t, got); text != want {
		t.Errorf("text = %q, want %q", text, want)
	}

	// cache_dir must be <profile dir>/vision-cache, and it must now hold one
	// entry a second call can hit without shelling out again.
	entries, err := os.ReadDir(filepath.Join(cfg.Dir, "vision-cache"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("vision-cache dir: %v, entries=%d", err, len(entries))
	}

	// Break the describer on purpose (DS4_CLAUDE_BIN would 500 or the file no
	// longer exists) — a cache hit must not need it, since the cache check
	// runs before bin resolution in transcribe().
	t.Setenv("DS4_CLAUDE_BIN", "/does/not/exist")
	got2, err := applyVision(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if text := firstTextBlock(t, got2); text != want {
		t.Errorf("cache hit: text = %q, want %q", text, want)
	}
}

// TestApplyVisionNoBinaryPlaceholders pins the fail-open path when no claude
// binary resolves at all (DS4_CLAUDE_BIN unset and none on PATH): the image
// becomes the placeholder rather than blocking or erroring the request.
func TestApplyVisionNoBinaryPlaceholders(t *testing.T) {
	t.Setenv("DS4_VISION", "1")
	os.Unsetenv("DS4_CLAUDE_BIN")
	t.Setenv("PATH", t.TempDir()) // a PATH with no `claude` on it

	png := base64.StdEncoding.EncodeToString([]byte("bytes"))
	body := mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{imageBlock("image/png", png)}},
		},
	})
	got, err := applyVision(body, profiles.Profile{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if text := firstTextBlock(t, got); text != visionPlaceholder {
		t.Errorf("text = %q, want placeholder", text)
	}
}

// TestApplyVisionChildFailurePlaceholders pins the fail-open path when the
// child runs but exits nonzero (an expired OAuth session, say).
func TestApplyVisionChildFailurePlaceholders(t *testing.T) {
	t.Setenv("DS4_VISION", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DS4_CLAUDE_BIN", path)

	png := base64.StdEncoding.EncodeToString([]byte("bytes"))
	body := mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{imageBlock("image/png", png)}},
		},
	})
	got, err := applyVision(body, profiles.Profile{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if text := firstTextBlock(t, got); text != visionPlaceholder {
		t.Errorf("text = %q, want placeholder", text)
	}
}

// ---- parseResult ------------------------------------------------------

func TestParseResult(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		want  string
		ok    bool
	}{
		{"clean", fakeResultJSON("a cat"), "a cat", true},
		{
			"warning prefix with an embedded brace",
			`(node:123) Warning: something {broken} happened` + "\n" + fakeResultJSON("a dog"),
			"a dog", true,
		},
		{"is_error true is rejected", `{"type": "result", "subtype": "error", "is_error": true, "result": "oops"}`, "", false},
		{"wrong type is rejected", `{"type": "system", "result": "not a result"}`, "", false},
		{"blank result is rejected", fakeResultJSON("   "), "", false},
		{"garbage only", `not json at all`, "", false},
		{"empty stdout", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseResult(c.stdin)
			if ok != c.ok || got != c.want {
				t.Errorf("parseResult(%q) = (%q, %v), want (%q, %v)", c.stdin, got, ok, c.want, c.ok)
			}
		})
	}
}

// ---- cache: TTL + atomic write ------------------------------------------

func TestCacheGetPutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cachePut(dir, "abc123", "a description")
	got, ok := cacheGet(dir, "abc123")
	if !ok || got != "a description" {
		t.Errorf("cacheGet = (%q, %v), want (%q, true)", got, ok, "a description")
	}
}

func TestCacheGetExpiresStaleEntry(t *testing.T) {
	dir := t.TempDir()
	cachePut(dir, "stale", "old description")
	path := filepath.Join(dir, "stale")
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheGet(dir, "stale"); ok {
		t.Error("cacheGet should treat a 31-day-old entry as expired")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expired cache entry should be deleted, not just skipped")
	}
}

func TestCacheGetMissingIsMiss(t *testing.T) {
	if _, ok := cacheGet(t.TempDir(), "nope"); ok {
		t.Error("cacheGet on a missing key should miss")
	}
}

// ---- hashKey / imageExt --------------------------------------------------

func TestHashKeyIsStableAndContentSensitive(t *testing.T) {
	a := hashKey([]byte("image bytes one"), "image/png")
	b := hashKey([]byte("image bytes one"), "image/png")
	if a != b {
		t.Error("hashKey must be deterministic for identical input")
	}
	if a == hashKey([]byte("image bytes two"), "image/png") {
		t.Error("hashKey must change with the image bytes")
	}
	if a == hashKey([]byte("image bytes one"), "image/jpeg") {
		t.Error("hashKey must change with the media type")
	}
}

func TestImageExt(t *testing.T) {
	cases := map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif",
		"image/webp": ".webp", "image/bmp": ".img", "": ".img",
	}
	for mt, want := range cases {
		if got := imageExt(mt); got != want {
			t.Errorf("imageExt(%q) = %q, want %q", mt, got, want)
		}
	}
}

// TestVisionBudgetCapsChildSpawns pins the cap that STRML/cc-ds4#42 was filed
// about. Each describe spawns a child with a 120s ceiling and they run
// serially, so without the cap one transcript full of uncached images holds a
// request for minutes and bills a describe for every one.
func TestVisionBudgetCapsChildSpawns(t *testing.T) {
	bin, calls := countingClaudeBin(t)
	t.Setenv("DS4_CLAUDE_BIN", bin)

	cfg := testNous()
	cfg.Dir = t.TempDir()

	// One more image than the budget allows, each with distinct bytes so no
	// two share a cache entry.
	var blocks []string
	for i := 0; i < maxImagesPerRequest+3; i++ {
		blocks = append(blocks, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+
			distinctPNG(i)+`"}}`)
	}
	body := []byte(`{"model":"ds4-flash-xhigh","max_tokens":32000,"messages":[{"role":"user","content":[` +
		strings.Join(blocks, ",") + `]}]}`)

	out, err := applyVision(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := calls(); n > maxImagesPerRequest {
		t.Errorf("spawned %d children for %d images, want at most %d",
			n, maxImagesPerRequest+3, maxImagesPerRequest)
	}
	// Nothing image-shaped may survive, budget or not: the upstream cannot
	// read one, so an over-budget image must become a placeholder rather than
	// being passed through.
	if strings.Contains(string(out), `"type": "image"`) {
		t.Errorf("an image block reached the upstream body: %s", out)
	}
}
