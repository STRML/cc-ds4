package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// effortEntry is a cached level plus the file identity it was read from.
//
// info is kept for os.SameFile, which compares the underlying inode without
// naming a platform's stat layout. Identity is the half that matters: the
// /ds4-effort command writes via an atomic replace, so a new level arrives on
// a fresh inode. Timestamp and size alone would go stale on a filesystem whose
// clock tick is coarser than the gap between two writes.
type effortEntry struct {
	info    os.FileInfo
	modTime int64
	size    int64
	level   string
}

var (
	effortMu    sync.Mutex
	effortCache = map[string]effortEntry{}
)

// effortOverride returns the per-profile effort pin from
// <profile-dir>/effort-override, or "" for none.
//
// One line holding one of effortLevels; the /ds4-effort command is the writer.
// The file is the only piece of this state that survives a proxy restart. The
// stat-keyed cache keeps the read off the per-request path: a request costs one
// stat plus a map lookup unless the file actually changed.
//
// An absent file, or one holding anything outside effortLevels, reads as ""
// (use the sentinel's default). An invalid level has to fail here rather than
// go upstream — OpenRouter accepts the parameter and DeepSeek drops unknown
// values without error, so a typo would otherwise vanish silently.
func effortOverride(cfg profiles.Profile) string {
	path := filepath.Join(cfg.Dir, "effort-override")
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	modTime, size := info.ModTime().UnixNano(), info.Size()

	effortMu.Lock()
	hit, found := effortCache[path]
	effortMu.Unlock()
	if found && hit.modTime == modTime && hit.size == size &&
		os.SameFile(hit.info, info) {
		return hit.level
	}

	level := ""
	if raw, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(raw)); effortLevels[s] {
			level = s
		}
	}

	effortMu.Lock()
	effortCache[path] = effortEntry{info, modTime, size, level}
	effortMu.Unlock()
	return level
}
