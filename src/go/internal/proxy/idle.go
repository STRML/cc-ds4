// Package proxy: idle-exit watcher.
//
// The proxy is socket-activated by launchd. It is meant to give the
// machine its memory back between sessions, not to run forever, so it
// watches its own liveness and exits once nothing is using it. launchd
// restarts it on the next connection. Mirrors proxy.py's sessions_live,
// claude_running, anything_in_use, and idle_watch (proxy.py:777-841).
//
// Getting this wrong in the "still in use" direction is silent and cheap:
// one extra poll. Getting it wrong in the "nothing is using it" direction
// kills the proxy mid-session, which looks to the user like a dead
// endpoint or a bad key. Every ambiguous case below resolves to "in use".
package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// IdleExitDefault is the fallback for DS4_IDLE_EXIT, matching IDLE_EXIT's
// default in proxy.py (900s = 15 minutes).
const IdleExitDefault = 900 * time.Second

// IdleExitFromEnv reads DS4_IDLE_EXIT the way proxy.py does:
// int(os.environ.get("DS4_IDLE_EXIT", "900")), seconds, 0 disables. A
// value that fails to parse falls back to the default rather than
// crashing the proxy over a typo'd env var.
func IdleExitFromEnv() time.Duration {
	v := os.Getenv("DS4_IDLE_EXIT")
	if v == "" {
		return IdleExitDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return IdleExitDefault
	}
	return time.Duration(n) * time.Second
}

// IdlePollInterval is the cadence idle_watch polls at: half the timeout,
// clamped to [1s, 30s], so a short DS4_IDLE_EXIT is honored promptly
// instead of being rounded up to a fixed interval (proxy.py:831-833).
// Exported so the caller can build a ticker with the same cadence
// WatchIdle's tick channel assumes; WatchIdle itself does not know the
// timeout's relationship to the tick source, only that it fires often
// enough to matter.
func IdlePollInterval(timeout time.Duration) time.Duration {
	every := timeout / 2
	if every < time.Second {
		return time.Second
	}
	if every > 30*time.Second {
		return 30 * time.Second
	}
	return every
}

// WatchIdle polls tick until the proxy has been idle for timeout with
// nothing in use, then calls shutdown once and returns. It also returns,
// without calling shutdown, if ctx is canceled first — that is the
// caller's way to stop the watch during tests or a graceful shutdown that
// did not originate here.
//
// clock is time.Now in production and a fake in tests, matching the clock
// Activity's timestamps are compared against.
//
// timeout <= 0 disables the watch (mirrors "while IDLE_EXIT > 0" — a 0
// means run forever) and WatchIdle returns immediately without consuming
// from tick.

// Activity is the liveness the idle watch reads before deciding to exit.
//
// The two signals are separate on purpose. Python ORs a last-seen timestamp
// with a nonzero in-flight counter, and folding them into one timestamp made
// the in-flight half a documented obligation on the caller: return "now" while
// a request is open, or a slow stream that outlives the timeout gets killed
// mid-response. Making it a field means the compiler asks for it instead.
type Activity struct {
	// LastSeen is when a request last touched the proxy.
	LastSeen func() time.Time
	// InFlight is the number of requests currently being relayed. A streaming
	// response can outlive the idle timeout with no new request arriving, so a
	// nonzero count is on its own a reason not to exit.
	InFlight func() int
}

func WatchIdle(ctx context.Context, cfgs []profiles.Profile, timeout time.Duration, act Activity, clock func() time.Time, tick <-chan time.Time, shutdown func()) {
	if timeout <= 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if !shouldExit(cfgs, timeout, act, clock) {
				continue
			}
			fmt.Printf("no profile in use and idle for %ds, exiting\n", int(timeout.Seconds()))
			shutdown()
			return
		}
	}
}

// shouldExit is the decision idle_watch makes on every poll (proxy.py:838):
// exit only when recent activity is absent AND no profile is in use. It is
// split out from WatchIdle so it can be tested directly, with no channel or
// goroutine involved.
func shouldExit(cfgs []profiles.Profile, timeout time.Duration, act Activity, clock func() time.Time) bool {
	// An open request is liveness on its own. Checking it first also means a
	// caller that supplies only InFlight still behaves correctly.
	if act.InFlight != nil && act.InFlight() > 0 {
		return false
	}
	if act.LastSeen != nil && clock().Sub(act.LastSeen()) < timeout {
		return false
	}
	return !anythingInUse(cfgs)
}

// anythingInUse reports whether any served profile has a live session or a
// running claude process, mirroring anything_in_use in proxy.py.
func anythingInUse(cfgs []profiles.Profile) bool {
	for _, p := range cfgs {
		if sessionsLive(p.Dir) || claudeRunning(p.Dir) {
			return true
		}
	}
	return false
}

// sessionsLive reports whether any registered session PID under
// <dir>/.ds4-sessions is still alive, and clears the token file for a PID
// that is not. Mirrors sessions_live in proxy.py. The directory is
// .ds4-sessions, never sessions: the latter is Claude Code's own state
// directory and nothing here may reap a file it did not create.
func sessionsLive(dir string) bool {
	sessDir := filepath.Join(dir, ".ds4-sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return false
	}
	live := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// Signal 0 is a pure existence probe; it delivers nothing. ESRCH
		// means the pid is gone, so the token is stale and gets reaped.
		// Any other error (EPERM and friends) still means the process
		// exists, exactly like Python's bare `except OSError: pass`.
		if err := syscall.Kill(pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				os.Remove(filepath.Join(sessDir, e.Name()))
			}
			continue
		}
		live = true
	}
	return live
}

// claudeRunningTimeout bounds the ps call the way Python's
// subprocess.run(..., timeout=10) does: a wedged ps must not hang the
// watcher forever.
const claudeRunningTimeout = 10 * time.Second

// claudeRunning reports whether a live claude process is using this profile.
// Claude Code always runs with CLAUDE_CONFIG_DIR set, so a session is visible
// however it was launched; this does not depend on the launcher script. Both
// implementations below compare the whole value, so a backup directory that has
// this one as a path prefix does not pin the proxy up.
func claudeRunning(dir string) bool {
	// ps -E is BSD syntax for "show the environment". Linux ps spells that
	// differently and silently means something else, so the ps path there
	// would never match and the watch would exit out from under a live
	// session. Read /proc instead, which is the same question asked natively.
	if runtime.GOOS == "linux" {
		return claudeRunningProc(dir)
	}
	return claudeRunningPS(dir)
}

func claudeRunningPS(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), claudeRunningTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-E", "-ax", "-o", "command=").Output()
	if err != nil {
		return false
	}
	pattern := regexp.QuoteMeta("CLAUDE_CONFIG_DIR="+dir) + `(\s|$)`
	matched, err := regexp.Match(pattern, out)
	return err == nil && matched
}

// claudeRunningProc scans /proc/<pid>/environ for the config dir.
//
// environ is NUL-separated, so an entry is compared whole rather than by
// substring. That is what keeps a "-backup" profile whose path contains
// another profile's path from pinning the shorter one up too.
//
// A process owned by another user returns EACCES here. That is not a case to
// work around: the proxy only cares about sessions belonging to the user who
// started it, and those are readable.
func claudeRunningProc(dir string) bool {
	return claudeRunningProcIn("/proc", dir)
}

// claudeRunningProcIn is claudeRunningProc against an arbitrary proc root, so
// the Linux scan can be tested on any platform against a fake tree. Without
// this the path is only ever exercised on a Linux CI runner, which is how it
// shipped broken in the first place.
func claudeRunningProcIn(procRoot, dir string) bool {
	want := "CLAUDE_CONFIG_DIR=" + dir
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "environ"))
		if err != nil {
			continue // exited between readdir and read, or not ours
		}
		for _, kv := range strings.Split(string(raw), "\x00") {
			if kv == want {
				return true
			}
		}
	}
	return false
}
