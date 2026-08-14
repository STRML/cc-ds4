package proxy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

// spawnHelperProcess and TestHelperHang together are the standard Go
// pattern for standing up a real, killable child process in a test (the
// same trick os/exec's own tests use): re-exec the test binary itself with
// a flag that makes it just block, instead of shelling out to a system
// binary. This matters here specifically because /bin/sleep (and other
// binaries under /bin, /usr/bin, ...) is SIP-restricted on macOS, so `ps
// -E` cannot see its environment at all — that would make claudeRunning
// look broken for a reason that has nothing to do with claudeRunning. The
// real claude binary is a Node install under the user's home directory,
// never SIP-restricted, so re-exec'ing this locally-built test binary is
// the closer stand-in, not sleep.
func spawnHelperProcess(t *testing.T, configDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHang$")
	cmd.Env = append(os.Environ(),
		"CLAUDE_CONFIG_DIR="+configDir,
		"DS4_IDLE_TEST_HELPER=1",
	)
	return cmd
}

// TestHelperHang is not a real test: it is a no-op unless
// DS4_IDLE_TEST_HELPER is set, in which case it blocks until killed. `go
// test` runs it like any other test function when invoked directly, but
// spawnHelperProcess only ever runs it via -test.run, as a subprocess.
func TestHelperHang(t *testing.T) {
	if os.Getenv("DS4_IDLE_TEST_HELPER") != "1" {
		return
	}
	// A bare `select {}` here would block on nothing at all; with this
	// goroutine the only one left alive (the parent is asleep in
	// testing.(*T).Run waiting on it), Go's runtime deadlock detector treats
	// that as the whole program being stuck and kills it before ps ever
	// gets a look. Sleeping on a timer instead reads as "will eventually
	// make progress," so the detector leaves it alone.
	time.Sleep(time.Hour)
}

// ---- sessionsLive ----------------------------------------------------------

func TestSessionsLive_NoSessionsDir(t *testing.T) {
	if sessionsLive(t.TempDir()) {
		t.Fatal("no .ds4-sessions dir should read as not live")
	}
}

func TestSessionsLive_AliveProcess(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, ".ds4-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Our own pid is guaranteed alive for the duration of the test.
	tokenPath := filepath.Join(sessDir, strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tokenPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !sessionsLive(dir) {
		t.Fatal("a token naming our own live pid should read as live")
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("live token should not be reaped: %v", err)
	}
}

func TestSessionsLive_DeadProcessIsReapedAndNotLive(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, ".ds4-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Spawn and fully wait on a short-lived child, so its pid is reaped
	// (zombie cleared) and a kill(pid, 0) reliably reports ESRCH afterward.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn test process: %v", err)
	}
	deadPID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("test process should exit cleanly: %v", err)
	}
	tokenPath := filepath.Join(sessDir, strconv.Itoa(deadPID))
	if err := os.WriteFile(tokenPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if sessionsLive(dir) {
		t.Fatal("a token naming a dead pid should not read as live")
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("stale token for a dead pid should be reaped, stat err = %v", err)
	}
}

func TestSessionsLive_IgnoresNonPIDEntries(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, ".ds4-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "not-a-pid"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if sessionsLive(dir) {
		t.Fatal("a non-numeric entry should be skipped, not treated as a live pid")
	}
}

// ---- claudeRunning ----------------------------------------------------------

// claudeRunningPoll is how long the liveness tests wait for ps to report a
// process they just spawned.
//
// These tests call scanClaudeRunning, not claudeRunning, and skip when the
// scan reports an error. `ps -E -ax` dumps every process's environment, so its
// cost scales with the rest of the machine and on a loaded box it blows
// claudeRunningTimeout — which is a fact about the box, not a defect in the
// matching this test exists to check. Asserting through the fail-safe wrapper
// instead would be worse than flaky: claudeRunning answers true on a failed
// scan, so the positive cases would pass without the match ever being tested.
const claudeRunningPoll = 5 * time.Second

// pollScan waits for the scan to report dir as running. It returns false only
// if the deadline passed with the scan working and never matching; a scan that
// cannot run skips the test.
func pollScan(t *testing.T, dir string) bool {
	t.Helper()
	deadline := time.Now().Add(claudeRunningPoll)
	for {
		running, err := scanClaudeRunning(dir)
		if err != nil {
			t.Skipf("process scan unavailable on this machine: %v", err)
		}
		if running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond) // avoid hammering ps in a busy spin
	}
}

// requireScan skips when the process scan cannot run at all. claudeRunning
// fails safe to "in use" in that case, so any test asserting that a quiet
// profile reads as NOT in use is really asserting the scan works — and would
// otherwise report a sandbox that forbids exec'ing ps as a product defect.
func requireScan(t *testing.T) {
	t.Helper()
	if _, err := scanClaudeRunning(filepath.Join(t.TempDir(), "nobody")); err != nil {
		t.Skipf("process scan unavailable on this machine: %v", err)
	}
}

func TestClaudeRunning_MatchesLiveProcessEnv(t *testing.T) {
	dir := t.TempDir()
	cmd := spawnHelperProcess(t, dir)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn test process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	// ps needs a moment to see the new process; poll rather than sleep a
	// fixed guess.
	if !pollScan(t, dir) {
		t.Fatal("the scan never saw the CLAUDE_CONFIG_DIR of a live process")
	}
}

func TestClaudeRunning_NoMatch(t *testing.T) {
	running, err := scanClaudeRunning(filepath.Join(t.TempDir(), "nobody-uses-this-dir"))
	if err != nil {
		t.Skipf("process scan unavailable on this machine: %v", err)
	}
	if running {
		t.Fatal("a dir no process was launched with should not read as running")
	}
}

// TestClaudeRunningFailsSafe pins the direction of the unknown case. A scan
// that cannot run must read as "in use": answering false would let the idle
// watcher shut the proxy down while a session is live, and the session's next
// request would fail for a reason the user cannot see.
func TestClaudeRunningFailsSafe(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("the /proc scan does not shell out, so it has no unavailable case")
	}
	// An empty PATH makes the ps lookup fail immediately, which is the same
	// error class as the timeout on a loaded machine.
	t.Setenv("PATH", "")
	dir := filepath.Join(t.TempDir(), "nobody-uses-this-dir")

	if _, err := scanClaudeRunning(dir); err == nil {
		t.Fatal("setup: the scan was expected to fail with no ps on PATH")
	}
	if !claudeRunning(dir) {
		t.Fatal("a scan that cannot run must read as in use, not as idle")
	}
}

func TestClaudeRunning_PrefixDoesNotMatchAsSubstring(t *testing.T) {
	dir := t.TempDir()
	backupDir := dir + "-backup"
	cmd := spawnHelperProcess(t, backupDir)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn test process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if !pollScan(t, backupDir) {
		t.Fatal("setup: the scan never saw the backup dir process")
	}
	// dir is a strict prefix of backupDir's CLAUDE_CONFIG_DIR value; it must
	// not match, or a live "-backup" profile would pin the plain one up too.
	running, err := scanClaudeRunning(dir)
	if err != nil {
		t.Skipf("process scan unavailable on this machine: %v", err)
	}
	if running {
		t.Fatal("a dir that is only a prefix of the real CLAUDE_CONFIG_DIR must not match")
	}
}

// ---- anythingInUse ----------------------------------------------------------

func TestAnythingInUse_TrueIfAnyProfileHasLiveSession(t *testing.T) {
	quiet := t.TempDir()
	live := t.TempDir()
	sessDir := filepath.Join(live, ".ds4-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(os.Getpid())), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs := []profiles.Profile{{Name: "quiet", Dir: quiet}, {Name: "live", Dir: live}}
	if !anythingInUse(cfgs) {
		t.Fatal("one profile with a live session should mark anything-in-use true")
	}
}

func TestAnythingInUse_FalseWhenAllQuiet(t *testing.T) {
	requireScan(t)
	cfgs := []profiles.Profile{
		{Name: "a", Dir: t.TempDir()},
		{Name: "b", Dir: t.TempDir()},
	}
	if anythingInUse(cfgs) {
		t.Fatal("no sessions and no matching process should read as not in use")
	}
}

// ---- shouldExit (the WatchIdle decision, isolated from any channel/timer) --

func TestShouldExit_RecentActivityPreventsExit(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-30 * time.Second)
	clock := func() time.Time { return now }
	lastActivity := func() time.Time { return lastSeen }
	cfgs := []profiles.Profile{{Name: "a", Dir: t.TempDir()}}
	if shouldExit(cfgs, time.Minute, Activity{LastSeen: lastActivity}, clock) {
		t.Fatal("activity 30s ago with a 60s timeout must not exit yet")
	}
}

func TestShouldExit_PastTimeoutWithNothingInUseExits(t *testing.T) {
	requireScan(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-2 * time.Minute)
	clock := func() time.Time { return now }
	lastActivity := func() time.Time { return lastSeen }
	cfgs := []profiles.Profile{{Name: "a", Dir: t.TempDir()}}
	if !shouldExit(cfgs, time.Minute, Activity{LastSeen: lastActivity}, clock) {
		t.Fatal("activity 2m ago with a 60s timeout and nothing in use should exit")
	}
}

func TestShouldExit_PastTimeoutButInUseDoesNotExit(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-2 * time.Minute)
	clock := func() time.Time { return now }
	lastActivity := func() time.Time { return lastSeen }
	live := t.TempDir()
	sessDir := filepath.Join(live, ".ds4-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(os.Getpid())), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs := []profiles.Profile{{Name: "live", Dir: live}}
	if shouldExit(cfgs, time.Minute, Activity{LastSeen: lastActivity}, clock) {
		t.Fatal("a live session must veto exit even past the idle timeout")
	}
}

// A long-running relay (e.g. a slow stream) is expected to be folded into
// lastActivity by the caller, so it reads as "just now" for as long as it is
// open — this pins that contract at the shouldExit boundary: as far as
// shouldExit is concerned, "in flight" and "freshly touched" are the same
// input.
func TestShouldExit_TreatsFreshLastActivityAsInFlightEquivalent(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	lastActivity := func() time.Time { return now } // caller-folded: request still open
	cfgs := []profiles.Profile{{Name: "a", Dir: t.TempDir()}}
	if shouldExit(cfgs, time.Second, Activity{LastSeen: lastActivity}, clock) {
		t.Fatal("lastActivity reporting 'now' must never exit, regardless of timeout size")
	}
}

// ---- WatchIdle loop wiring ---------------------------------------------------

func TestWatchIdle_DisabledTimeoutReturnsImmediately(t *testing.T) {
	called := false
	// nil tick: if WatchIdle tried to read from it, this would hang forever
	// and the test would time out, proving the disabled path never selects.
	WatchIdle(context.Background(), nil, 0, Activity{LastSeen: time.Now}, time.Now, nil, func() { called = true })
	if called {
		t.Fatal("shutdown must not run when the watch is disabled")
	}
}

func TestWatchIdle_ContextCancelStopsWithoutShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	shutdownCalled := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		WatchIdle(ctx, nil, time.Minute, Activity{LastSeen: time.Now}, time.Now, tick,
			func() { shutdownCalled <- struct{}{} })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchIdle did not return after ctx cancellation")
	}
	select {
	case <-shutdownCalled:
		t.Fatal("shutdown must not run when the watch stops via ctx cancellation")
	default:
	}
}

func TestWatchIdle_IdleTickTriggersShutdownExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	shutdownCalled := make(chan struct{}, 1)
	// Past the timeout and nothing in use: the first tick should exit.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-2 * time.Minute)
	done := make(chan struct{})
	go func() {
		WatchIdle(ctx, nil, time.Minute,
			Activity{LastSeen: func() time.Time { return lastSeen }},
			func() time.Time { return now },
			tick,
			func() { shutdownCalled <- struct{}{} })
		close(done)
	}()
	tick <- now
	select {
	case <-shutdownCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown was not called on an idle tick")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchIdle did not return after calling shutdown")
	}
}

func TestWatchIdle_ActivityResetsTimerAcrossTicks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	shutdownCalled := make(chan struct{}, 1)

	// now and lastSeen are written by the test goroutine and read from the
	// WatchIdle goroutine. An unbuffered channel send only synchronizes the
	// two goroutines at the moment of rendezvous, not for whatever either
	// side does next, so both need a mutex rather than relying on the send
	// itself as a memory barrier.
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lastSeen := now // activity keeps being "just now"
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	lastActivity := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return lastSeen
	}

	done := make(chan struct{})
	go func() {
		WatchIdle(ctx, nil, time.Minute, Activity{LastSeen: lastActivity}, clock, tick,
			func() { shutdownCalled <- struct{}{} })
		close(done)
	}()

	// Several ticks with activity staying fresh: never exits, even though the
	// clock keeps advancing past the timeout.
	//
	// State is advanced BEFORE each send, never after. Advancing after the
	// rendezvous races WatchIdle's evaluation of the tick it just accepted:
	// the two reads of clock and last-seen are separate, so a write landing
	// between them can make one tick look stale and trip an exit the test then
	// reports as a stuck loop.
	for i := 0; i < 3; i++ {
		mu.Lock()
		now = now.Add(90 * time.Second) // clock past the timeout
		lastSeen = now                  // but activity is just as recent
		sendAt := now
		mu.Unlock()
		select {
		case tick <- sendAt:
		case <-time.After(5 * time.Second):
			t.Fatal("WatchIdle did not accept a tick (loop appears stuck)")
		}
	}
	select {
	case <-shutdownCalled:
		t.Fatal("shutdown ran even though activity kept resetting the timer")
	default:
	}

	// Now activity goes stale: an exit is due.
	//
	// The send and the shutdown are raced deliberately rather than sequenced.
	// Going stale can be observed by the tick still being evaluated from the
	// loop above, in which case WatchIdle exits without ever reading another
	// tick and a plain send would block forever. Either ordering is correct
	// behavior; only "no shutdown at all" is a failure.
	mu.Lock()
	lastSeen = now.Add(-2 * time.Minute)
	sendAt := now
	mu.Unlock()
	deadline := time.After(5 * time.Second)
	for shutdown := false; !shutdown; {
		select {
		case tick <- sendAt:
		case <-shutdownCalled:
			shutdown = true
		case <-deadline:
			t.Fatal("shutdown did not run once activity went stale")
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchIdle did not return after calling shutdown")
	}
}

// ---- IdlePollInterval / IdleExitFromEnv --------------------------------------

func TestIdlePollInterval(t *testing.T) {
	cases := []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{0, time.Second},                     // half of 0 clamps to the 1s floor
		{time.Second, time.Second},           // half of 1s (500ms) clamps up to 1s
		{2 * time.Minute, 30 * time.Second},  // half of 2m (60s) clamps down to 30s
		{20 * time.Second, 10 * time.Second}, // half of 20s, no clamp needed
	}
	for _, c := range cases {
		if got := IdlePollInterval(c.timeout); got != c.want {
			t.Errorf("IdlePollInterval(%s) = %s, want %s", c.timeout, got, c.want)
		}
	}
}

func TestIdleExitFromEnv(t *testing.T) {
	t.Setenv("DS4_IDLE_EXIT", "")
	if got := IdleExitFromEnv(); got != IdleExitDefault {
		t.Errorf("unset env: got %s, want default %s", got, IdleExitDefault)
	}

	t.Setenv("DS4_IDLE_EXIT", "30")
	if got := IdleExitFromEnv(); got != 30*time.Second {
		t.Errorf("DS4_IDLE_EXIT=30: got %s, want 30s", got)
	}

	t.Setenv("DS4_IDLE_EXIT", "0")
	if got := IdleExitFromEnv(); got != 0 {
		t.Errorf("DS4_IDLE_EXIT=0 should disable (0s): got %s", got)
	}

	t.Setenv("DS4_IDLE_EXIT", "not-a-number")
	if got := IdleExitFromEnv(); got != IdleExitDefault {
		t.Errorf("unparsable env: got %s, want default %s", got, IdleExitDefault)
	}
}

// TestShouldExit_InFlightRequestBlocksExit pins the signal that the old
// single-timestamp signature made a caller obligation. A streaming response
// can outlive the idle timeout without any new request arriving, so LastSeen
// alone goes stale while the stream is still being served. Exiting there would
// kill a live response mid-flight.
func TestShouldExit_InFlightRequestBlocksExit(t *testing.T) {
	stale := time.Unix(1_000_000, 0)
	now := stale.Add(time.Hour)
	act := Activity{
		LastSeen: func() time.Time { return stale },
		InFlight: func() int { return 1 },
	}
	if shouldExit(nil, time.Minute, act, func() time.Time { return now }) {
		t.Fatal("exited while a request was still in flight")
	}
	// Same stale timestamp, nothing open: now it may exit.
	act.InFlight = func() int { return 0 }
	if !shouldExit(nil, time.Minute, act, func() time.Time { return now }) {
		t.Fatal("did not exit with nothing in flight and a stale last-seen")
	}
}

// TestTrafficCountsRequests pins the counter Handler feeds. A double release
// would drive it negative and make the proxy look permanently busy, which
// disables idle exit silently.
func TestTrafficCountsRequests(t *testing.T) {
	tr := NewTraffic()
	if got := tr.InFlight(); got != 0 {
		t.Fatalf("fresh Traffic InFlight = %d, want 0", got)
	}
	done := tr.begin()
	if got := tr.InFlight(); got != 1 {
		t.Fatalf("InFlight during request = %d, want 1", got)
	}
	done()
	done() // a second release must not double-count
	if got := tr.InFlight(); got != 0 {
		t.Fatalf("InFlight after release = %d, want 0", got)
	}
	if tr.LastSeen().IsZero() {
		t.Fatal("LastSeen never recorded")
	}
}
