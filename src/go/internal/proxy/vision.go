// Describe images with a local `claude -p --model haiku` child, mirroring
// src/vision.py. DeepSeek V4 is text-only; applyVision turns image blocks
// into text the main model can reason over, using a minimal, hermetic
// `claude -p` call on the machine's Anthropic profile (subscription credits,
// no new credential).
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

const (
	visionModel  = "haiku"
	visionPrompt = "Describe the image for a text-only model. " +
		"Return only the description."
	visionPlaceholder = "[Image omitted: no usable description was available.]"

	// Bound the per-request describe work. Each child can take up to
	// childTimeout, and the walk is serial, so a transcript with many images
	// must not hold the request for minutes.
	maxImagesPerRequest = 8
	childTimeout        = 120 * time.Second
	// A count cap alone does not bound time: the walk is serial, so the
	// budget's worth of children each burning childTimeout is
	// maxImagesPerRequest * childTimeout of held request — far past any client
	// timeout, and a broken describer hits that ceiling every turn while
	// producing nothing. The deadline is what actually bounds the wait; the
	// count bounds the spend.
	visionWallClock = 3 * time.Minute
	// A single-flight waiter gives the winning child a little longer than its
	// own timeout to finish and write the cache before giving up, matching
	// Python's waiter.wait(timeout=130).
	singleFlightWait = 130 * time.Second
	visionCacheTTL   = 30 * 24 * time.Hour
)

// scrubPrefixes/scrubExact are the env vars that would route the child back
// to DeepSeek or another ds4 profile, or that carry the ds4 credential.
// Prefix-matching ANTHROPIC_ and CLAUDE_CODE_ is deliberate: a new profile
// var must not slip through.
var scrubPrefixes = []string{"ANTHROPIC_", "CLAUDE_CODE_", "DS4_"}
var scrubExact = map[string]bool{"CLAUDE_CONFIG_DIR": true, "CLAUDECODE": true}

// applyVision replaces image blocks in body with text descriptions before the
// body is serialized, so the upstream never receives an image-shaped block.
// It mirrors rewrite_images/placeholder_remaining in src/vision.py plus the
// DS4_VISION gate and the fail-open wrapper the Python call site (proxy.py,
// around the "vision: replace image blocks..." comment) applies around them
// — the whole contract lives in this one entry point so the caller only has
// to call it after rewrite() and before the body is sent upstream.
//
// Returns body unchanged, nil when DS4_VISION=0 (the old pass-through:
// image blocks forwarded unchanged, deliberately) or when body is not valid
// JSON (the error is jsonpy's parse error, matching rewrite()'s contract so
// callers can treat both the same way).
// allowDescribe=false means: serve what is already cached, and placeholder
// everything else without spawning a child. That is the ZDR mode — see the call
// site in relay.
func applyVision(body []byte, cfg profiles.Profile, allowDescribe bool) ([]byte, error) {
	if !visionEnabled() {
		return body, nil
	}
	// Nothing image-shaped means nothing to do, and the walk cannot find a
	// block whose type is "image" if that literal is absent from the bytes. The
	// check saves a full parse-and-re-emit on every text-only request, which on
	// a 1M-context profile is most of them and megabytes each.
	if !bytes.Contains(body, []byte(`"image"`)) {
		return body, nil
	}
	cacheDir := filepath.Join(cfg.Dir, "vision-cache")
	return jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		// Every OS-touching failure inside the walk (missing binary, timeout,
		// bad cache entry, malformed image) is already caught at its own leaf
		// and turned into a placeholder — nothing below this point should be
		// able to panic. This recover is belt-and-braces for a future bug: it
		// preserves the one invariant that matters, that no image-shaped
		// block ever reaches the upstream, even if the walk breaks halfway
		// through. Mirrors the try/except around _vision.rewrite_images in
		// proxy.py, whose except branch calls placeholder_remaining.
		defer func() {
			if recover() != nil {
				placeholderRemaining(root)
			}
		}()
		rewriteImages(root, cacheDir, allowDescribe)
	})
}

// visionEnabled mirrors os.environ.get("DS4_VISION", "1") == "1": the default
// applies only when the var is truly absent, so DS4_VISION="" (set but
// empty) is off, not on. os.Getenv can't tell those apart, so this uses
// LookupEnv.
func visionEnabled() bool {
	v, ok := os.LookupEnv("DS4_VISION")
	if !ok {
		return true
	}
	return v == "1"
}

// rewriteImages walks payload["messages"], swapping every image block (at
// any depth, including nested in tool_result.content) for a text block.
// A non-list messages value is left alone rather than crashing the walk,
// matching rewrite_images's isinstance guard.
func rewriteImages(root *jsonpy.OrderedValue, cacheDir string, allowDescribe bool) {
	messages := root.Get("messages")
	if !messages.IsArray() {
		return
	}
	w := &visionWalk{cacheDir: cacheDir, deadline: visionNow().Add(visionWallClock), allowDescribe: allowDescribe}
	// The budget caps how many images may spawn a child on one request. Each
	// child costs up to childTimeout and they run serially, so an uncapped
	// transcript full of fresh images holds a single request for minutes and
	// bills a describe for every one.
	//
	// The Python original threaded this counter through the recursion and
	// decremented it but never consulted it, so the cap its own docstring
	// promised did nothing. That was ported verbatim to keep the two
	// implementations byte-identical while both existed. Python is gone, so
	// the cap is now enforced (STRML/cc-ds4#42).
	w.budget = maxImagesPerRequest
	for _, msg := range messages.Items() {
		if !msg.IsObject() {
			continue
		}
		w.blocks(msg.Get("content"))
	}
}

// visionNow is time.Now, swappable so a test can prove the deadline stops the
// walk without waiting out a real one.
var visionNow = time.Now

// visionWalk carries the per-request limits down the recursion: how many
// describes may still be spawned, and the wall-clock past which none may be.
type visionWalk struct {
	cacheDir string
	budget   int
	deadline time.Time
	// allowDescribe is false when the request must not leave its route. The
	// describer child talks to real Anthropic Haiku on the machine's own
	// subscription, which is a third party as far as a ZDR demand is concerned,
	// so an image on such a request is placeholdered rather than described.
	// Cache hits are still served: reading a description already on disk sends
	// nothing anywhere.
	allowDescribe bool
}

// spent reports whether this request may still spawn a describer. A cache hit
// is not gated by either limit — it costs neither money nor measurable time,
// and refusing one would placeholder an image the proxy already has a
// description for.
func (w *visionWalk) exhausted() bool {
	return !w.allowDescribe || w.budget <= 0 || !visionNow().Before(w.deadline)
}

// rewriteBlocks walks one content list in place, replacing "image" blocks and
// recursing into "tool_result" blocks (that is where Claude Code puts
// Read/screenshot/MCP images; one left nested there is silently dropped
// upstream). content.Items() returns the array's own backing slice, so
// writing items[i] mutates the tree jsonpy will re-emit.
func (w *visionWalk) blocks(content *jsonpy.OrderedValue) {
	if !content.IsArray() {
		return
	}
	items := content.Items()
	for i, block := range items {
		if !block.IsObject() {
			continue
		}
		switch block.Get("type").String() {
		case "image":
			// The limits gate SPENDING, not answering. swapImage still runs
			// when they are used up, because a cached description costs
			// nothing and the cache probe lives inside it — checking the
			// limits out here placeholdered images the proxy already had
			// descriptions for, which is what exhausted's own comment says
			// must not happen.
			replacement, spent := swapImage(block, w.cacheDir, !w.exhausted())
			items[i] = replacement
			// Budget is charged for time spent, not for descriptions
			// obtained. A cache hit costs nothing and is exempt — charging it
			// would permanently placeholder every image past position N once a
			// cached prefix filled the budget on every turn. But a child that
			// times out, exits nonzero, or returns junk has already burned up
			// to childTimeout, and charging only successes let exactly that
			// case run unbounded: the cap fails open in the one situation it
			// exists to bound.
			if spent {
				w.budget--
			}
		case "tool_result":
			w.blocks(block.Get("content"))
		}
	}
}

// swapImage returns the replacement block for one image block, and whether the
// lookup spent real time (ran a child, or blocked waiting on one) as opposed to
// being served from cache or rejected before any work started. Never panics;
// every failure mode — wrong source shape, missing/invalid metadata, bad
// base64 — becomes the placeholder so nothing image-shaped survives.
//
// Only inline base64 sources are transcribed. A source.type == "file"
// reference is not followed: the proxy must never open a path from the
// request body, which would be a local arbitrary-file-read primitive on an
// unauthenticated loopback listener. Such blocks become the placeholder.
func swapImage(block *jsonpy.OrderedValue, cacheDir string, maySpend bool) (*jsonpy.OrderedValue, bool) {
	src := block.Get("source")
	if !src.IsObject() || src.Get("type").String() != "base64" {
		return placeholderBlock(), false
	}
	data := src.Get("data")
	mediaType := src.Get("media_type")
	if !data.IsString() || data.String() == "" || !mediaType.IsString() || mediaType.String() == "" {
		return placeholderBlock(), false
	}
	imageBytes, err := base64.StdEncoding.DecodeString(data.String())
	if err != nil || len(imageBytes) == 0 {
		return placeholderBlock(), false
	}
	return transcribeBytes(imageBytes, mediaType.String(), cacheDir, maySpend)
}

// transcribeBytes describes raw image bytes and wraps the result, or falls
// back to the placeholder. The "[image transcribed by ...]" prefix appears
// ONLY on a real description, so nothing claims a transcription that never
// happened.
func transcribeBytes(imageBytes []byte, mediaType, cacheDir string, maySpend bool) (*jsonpy.OrderedValue, bool) {
	text, spent := transcribe(imageBytes, mediaType, cacheDir, maySpend)
	if text == "" {
		// transcribe never returns a real description as "": _parse_result
		// requires the trimmed result to be non-empty, so an empty string
		// unambiguously means "no description" (Python's None). The call may
		// still have spent real time getting there, so pass that through.
		return placeholderBlock(), spent
	}
	return textBlock(fmt.Sprintf("[image transcribed by %s]\n%s", visionModel, text)), spent
}

// placeholderRemaining replaces any image block still present with the
// fail-open placeholder. It is applyVision's exception path: the upstream
// must never receive an image block, even if the describer walk breaks
// mid-request.
func placeholderRemaining(root *jsonpy.OrderedValue) {
	messages := root.Get("messages")
	if !messages.IsArray() {
		return
	}
	for _, msg := range messages.Items() {
		if !msg.IsObject() {
			continue
		}
		scrubBlocks(msg.Get("content"))
	}
}

func scrubBlocks(content *jsonpy.OrderedValue) {
	if !content.IsArray() {
		return
	}
	items := content.Items()
	for i, block := range items {
		if !block.IsObject() {
			continue
		}
		switch block.Get("type").String() {
		case "image":
			items[i] = placeholderBlock()
		case "tool_result":
			scrubBlocks(block.Get("content"))
		}
	}
}

func placeholderBlock() *jsonpy.OrderedValue {
	return textBlock(visionPlaceholder)
}

func textBlock(text string) *jsonpy.OrderedValue {
	return jsonpy.MustObj("type", "text", "text", text)
}

// flightKey identifies one billed child: cache_dir keyed too, because one
// proxy serves several profiles with separate caches — a cross-profile
// waiter would miss its own cache and placehold.
type flightKey struct{ cacheDir, key string }

var (
	inflightMu sync.Mutex
	inflight   = map[flightKey]chan struct{}{}
)

// transcribe describes one image. text is "" for "no usable description" (the
// caller substitutes the placeholder). spent reports whether the call cost
// wall-clock: true once a child has been started or a single-flight wait
// entered, false for a cache hit or a reject that ran nothing. Never spawns two
// billed children for the same (cacheDir, key): concurrent callers that miss
// the same key wait on the winner instead of a stampede.
func transcribe(imageBytes []byte, mediaType, cacheDir string, maySpend bool) (text string, spent bool) {
	key := hashKey(imageBytes, mediaType)
	if hit, ok := cacheGet(cacheDir, key); ok {
		return hit, false
	}
	// The cache probe is deliberately ahead of this: a hit is free and is
	// served whatever the per-request limits say. Past them, a MISS becomes the
	// placeholder rather than a child.
	if !maySpend {
		return "", false
	}
	// Resolved after the cache check (Python resolves it once at import and
	// checks it first; that only differs in the case where the binary has
	// never once been resolvable in this process, and in that case nothing
	// has ever populated the cache either, so the observable result is the
	// same either way). Checking cache first means a hit costs no PATH
	// lookup.
	bin := resolveClaudeBin()
	if bin == "" {
		return "", false
	}

	fk := flightKey{cacheDir, key}
	inflightMu.Lock()
	ch, waiting := inflight[fk]
	if !waiting {
		ch = make(chan struct{})
		inflight[fk] = ch
	}
	inflightMu.Unlock()

	if waiting {
		select {
		case <-ch:
		case <-time.After(singleFlightWait):
		}
		// Blocking on another request's child costs up to singleFlightWait
		// whether or not it produced anything, so both outcomes are charged.
		if hit, ok := cacheGet(cacheDir, key); ok {
			return hit, true
		}
		return "", true
	}

	defer func() {
		inflightMu.Lock()
		delete(inflight, fk)
		inflightMu.Unlock()
		close(ch)
	}()

	// From here a child has run. Whatever it returned, the wall-clock is
	// already spent, so every path below charges the budget.
	result := runChild(bin, imageBytes, mediaType)
	if result == "" {
		return "", true
	}
	cachePut(cacheDir, key, result)
	return result, true
}

// runChild spawns one describer child in a fresh temp dir holding only the
// image, and returns its description or "" on any failure (missing dir,
// timeout, nonzero exit, unparseable output). cwd = the temp dir and
// --add-dir/--allowedTools scope the child's only readable context to that
// one file; Stdin left nil connects the child to the null device, so an
// expired OAuth session crashes the child instead of hanging the proxy on an
// interactive login prompt.
func runChild(bin string, imageBytes []byte, mediaType string) string {
	tmp, err := os.MkdirTemp("", "ds4-vision-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmp)

	imgPath := filepath.Join(tmp, "image"+imageExt(mediaType))
	if err := os.WriteFile(imgPath, imageBytes, 0o600); err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), childTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, childArgs(imgPath)...)
	cmd.Env = scrubbedEnv()
	cmd.Dir = tmp
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	result, ok := parseResult(stdout.String())
	if !ok {
		return ""
	}
	return result
}

// childArgs builds the describer child's argv (bin excluded; exec.Command
// takes it separately). --settings disables all hooks and
// --no-session-persistence keeps the child from writing a transcript that
// would retain the image bytes; --strict-mcp-config and --disable-slash-commands
// keep it hermetic.
func childArgs(imgPath string) []string {
	imgDir := filepath.Dir(imgPath)
	return []string{
		"-p",
		"--settings", `{"disableAllHooks":true}`,
		"--model", visionModel,
		"--tools", "Read",
		"--allowedTools", fmt.Sprintf("Read(%s/*)", imgDir),
		"--add-dir", imgDir,
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--append-system-prompt", visionPrompt,
		"--no-session-persistence",
		"--output-format", "json",
		fmt.Sprintf("Read %s and describe the image.", imgPath),
	}
}

// resolveClaudeBin finds the claude binary the describer child runs as.
// DS4_CLAUDE_BIN is baked by install.sh, but on a machine where that path no
// longer exists or lost its execute bit (a stale temp-dir shim, say), fall
// back to a PATH lookup. LookPath validates a slash-containing path directly
// without consulting PATH, which is what os.path.isfile + os.access(X_OK)
// checked in the Python original.
func resolveClaudeBin() string {
	if baked := os.Getenv("DS4_CLAUDE_BIN"); baked != "" {
		if p, err := exec.LookPath(baked); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath("claude")
	return p
}

// scrubbedEnv is the child's env: the parent env minus everything
// Claude/Anthropic/ds4/cmux. A child that inherits the parent session's
// ANTHROPIC_BASE_URL/AUTH_TOKEN would route back into this proxy (garbage
// descriptions, or worse, infinite recursion) instead of reaching real
// Anthropic Haiku. Everything else is kept: the child needs the broader
// environment (XPC_SESSION_*, security's keychain access) to reach the
// Anthropic OAuth in the login keychain.
func scrubbedEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if scrubEnvKey(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func scrubEnvKey(k string) bool {
	for _, p := range scrubPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	if scrubExact[k] || strings.HasPrefix(k, "CMUX") || k == "NODE_OPTIONS" || k == "AI_AGENT" {
		return true
	}
	switch strings.ToLower(k) {
	case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
		return true
	}
	return false
}

// parseResult extracts the description from the child's --output-format json
// stdout. A deprecation warning can prefix the JSON, and the warning itself
// may contain a '{' — so scan every '{' position and keep scanning when a
// decoded value lacks a usable result. A real result object carries
// type=="result" and no is_error; a warning JSON with a fabricated "result"
// field must not be accepted.
func parseResult(stdout string) (string, bool) {
	start := 0
	for {
		rel := strings.IndexByte(stdout[start:], '{')
		if rel < 0 {
			return "", false
		}
		i := start + rel
		dec := json.NewDecoder(strings.NewReader(stdout[i:]))
		var obj map[string]any
		err := dec.Decode(&obj)
		if err != nil {
			start = i + 1
			continue
		}
		if obj["type"] == "result" {
			isErr, _ := obj["is_error"].(bool)
			if !isErr {
				if text, ok := obj["result"].(string); ok && strings.TrimSpace(text) != "" {
					return text, true
				}
			}
		}
		// Advance past the object the decoder just consumed. A decoder that
		// somehow reports zero bytes consumed (a bare "{}" ambiguity never
		// seen in practice) still moves the scan forward by one so the loop
		// can't spin.
		next := i + int(dec.InputOffset())
		if next <= i {
			next = i + 1
		}
		start = next
	}
}

func imageExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".img"
	}
}

// hashKey is the cache key: a content hash of the raw image bytes with a
// media-type prefix, never a stringified repr. Model + prompt are folded in
// so a describer or prompt change invalidates old entries.
func hashKey(imageBytes []byte, mediaType string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s:%s:", visionModel, visionPrompt, mediaType)
	h.Write(imageBytes)
	return hex.EncodeToString(h.Sum(nil))
}

// cacheGet returns the cached description if present and unexpired. A stale
// entry is deleted and treated as a miss — never served once then cleaned
// later. A non-UTF8 file (never written by cachePut, but the directory is
// otherwise unvalidated) is treated as absent rather than returned raw,
// mirroring Python's UnicodeDecodeError->None.
func cacheGet(cacheDir, key string) (string, bool) {
	path := filepath.Join(cacheDir, key)
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if time.Since(info.ModTime()) > visionCacheTTL {
		os.Remove(path)
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// cachePut writes text under key with a tmpfile + rename, so a concurrent
// reader never sees a torn file. Missing dir is created; failures are
// swallowed (best-effort), matching Python's bare `except OSError: pass`.
func cachePut(cacheDir, key, text string) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(cacheDir, "")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below has moved it
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	os.Rename(tmpPath, filepath.Join(cacheDir, key))
}
