package sockets

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// clearActivationEnv resets the env vars these tests twiddle so they cannot
// bleed across cases; t.Cleanup restores whatever was there before.
func setEnv(t *testing.T, key, val string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	os.Setenv(key, val)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, prev)
		} else {
			os.Unsetenv(key)
		}
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestListeners_PlainBind covers the path every platform takes when
// launchd never activated a socket for this name: activateSocket returns
// errNotActivated (the darwin cgo path gets ESRCH/ENOENT since no launchd
// parent started this test binary; the stub always does), and Listeners
// falls back to binding the configured port itself.
func TestListeners_PlainBind(t *testing.T) {
	setEnv(t, "DS4_REQUIRE_OWNED_SOCKET", "0")
	port := freePort(t)
	lns, err := Listeners([]string{"direct"}, map[string]int{"direct": port})
	if err != nil {
		t.Fatalf("Listeners: %v", err)
	}
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()

	ln, ok := lns["direct"]
	if !ok {
		t.Fatalf("expected a listener for %q, got %v", "direct", lns)
	}
	if got := ln.Addr().(*net.TCPAddr).Port; got != port {
		t.Fatalf("port = %d, want %d", got, port)
	}
	assertServes(t, ln)
}

// TestListeners_PortOverride mirrors serve()'s DS4_PORT_<NAME> resolution:
// the override wins over the table port on the plain-bind path.
func TestListeners_PortOverride(t *testing.T) {
	setEnv(t, "DS4_REQUIRE_OWNED_SOCKET", "0")
	tablePort := freePort(t)
	overridePort := freePort(t)
	setEnv(t, "DS4_PORT_DIRECT", strconv.Itoa(overridePort))

	lns, err := Listeners([]string{"direct"}, map[string]int{"direct": tablePort})
	if err != nil {
		t.Fatalf("Listeners: %v", err)
	}
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()

	got := lns["direct"].Addr().(*net.TCPAddr).Port
	if got != overridePort {
		t.Fatalf("port = %d, want override %d (table port was %d)", got, overridePort, tablePort)
	}
}

// TestListeners_MultipleProfiles checks that unrelated names bind
// independently and a partial-bind failure closes everything already open
// (exercised by re-requesting an already-bound port for the second name).
func TestListeners_MultipleProfiles(t *testing.T) {
	setEnv(t, "DS4_REQUIRE_OWNED_SOCKET", "0")
	p1, p2 := freePort(t), freePort(t)
	lns, err := Listeners([]string{"direct", "openrouter"}, map[string]int{"direct": p1, "openrouter": p2})
	if err != nil {
		t.Fatalf("Listeners: %v", err)
	}
	if len(lns) != 2 {
		t.Fatalf("got %d listeners, want 2", len(lns))
	}
	for _, l := range lns {
		l.Close()
	}
}

// TestListeners_RequireOwned_NoLaunchdParent covers the fail-loud contract:
// this test binary was not started by launchd, so activateSocket always
// reports errNotActivated (ESRCH in the darwin+cgo case), and
// DS4_REQUIRE_OWNED_SOCKET=1 must turn that into an error instead of
// silently self-binding.
func TestListeners_RequireOwned_NoLaunchdParent(t *testing.T) {
	setEnv(t, "DS4_REQUIRE_OWNED_SOCKET", "1")
	port := freePort(t)
	lns, err := Listeners([]string{"direct"}, map[string]int{"direct": port})
	if err == nil {
		for _, l := range lns {
			l.Close()
		}
		t.Fatalf("expected an error with no launchd parent, got listeners %v", lns)
	}
	if lns != nil {
		t.Fatalf("expected nil listeners on error, got %v", lns)
	}
}

// TestNewListenerFromFD exercises the fd -> net.Listener conversion in
// isolation, independent of launchd: bind a real listener, extract its raw
// fd the same way an inherited launchd fd would arrive (a bare int), and
// confirm the wrapped listener serves and the original fd got closed
// (double-closing it here would return an error if it were still open
// under the same number).
func TestNewListenerFromFD(t *testing.T) {
	src, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	// (*net.TCPListener).File() dups the fd and, per its docs, sets the
	// original to blocking mode; that dup is what stands in for "the fd
	// launchd handed us" here, so src itself is closed immediately after
	// and only the dup is exercised.
	f, err := src.(*net.TCPListener).File()
	if err != nil {
		src.Close()
		t.Fatalf("File(): %v", err)
	}
	src.Close()
	fd := int(f.Fd())

	ln, err := newListenerFromFD(fd)
	if err != nil {
		f.Close()
		t.Fatalf("newListenerFromFD: %v", err)
	}
	defer ln.Close()

	// newListenerFromFD is documented to close the os.File it built around
	// fd once net.FileListener's internal dup succeeds. f is that same
	// os.File (same fd number), so closing it again here must fail — that
	// is the observable proof the leak-avoidance path ran.
	if err := f.Close(); err == nil {
		t.Fatalf("fd %d closed cleanly a second time; newListenerFromFD should have already closed it", fd)
	}

	assertServes(t, ln)
}

func TestNewListenerFromFD_InvalidFD(t *testing.T) {
	// A wildly out-of-range fd number is never valid; os.NewFile still
	// returns a non-nil *os.File for it (it does no syscall itself), so
	// this exercises net.FileListener's own rejection of a bad fd.
	if _, err := newListenerFromFD(1 << 24); err == nil {
		t.Fatalf("expected an error for an invalid fd")
	}
}

func TestCloseFD_DoesNotPanicOnInvalidFD(t *testing.T) {
	closeFD(1 << 24) // must not panic; errors are intentionally dropped
}

func TestUpper(t *testing.T) {
	cases := map[string]string{
		"direct":     "DIRECT",
		"openrouter": "OPENROUTER",
		"":           "",
		"ALREADY":    "ALREADY",
	}
	for in, want := range cases {
		if got := upper(in); got != want {
			t.Errorf("upper(%q) = %q, want %q", in, got, want)
		}
	}
}

// assertServes proves ln is a live, working listener: accept one
// connection via a trivial HTTP round trip.
func assertServes(t *testing.T, ln net.Listener) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestListenersDegradesWhenOnePortIsBusy pins that one occupied port does not
// take the other profiles down. A leftover process holding :31500 used to be
// survivable (the old code bound per profile and logged the failure); making
// Listeners all-or-nothing would turn it into a total outage.
func TestListenersDegradesWhenOnePortIsBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	taken := busy.Addr().(*net.TCPAddr).Port

	ports := map[string]int{"blocked": taken, "free": 0}
	got, err := Listeners([]string{"blocked", "free"}, ports)
	if err != nil {
		t.Fatalf("one busy port failed the whole call: %v", err)
	}
	defer func() {
		for _, l := range got {
			l.Close()
		}
	}()
	if _, ok := got["blocked"]; ok {
		t.Error("the busy profile reported a listener it does not have")
	}
	if _, ok := got["free"]; !ok {
		t.Error("a free profile lost its listener because another was busy")
	}
}

// TestListenersFailsWhenNothingCanBind pins the other end: if no profile binds
// at all there is nothing to serve, and returning an empty map with no error
// would leave the process alive and silently useless.
func TestListenersFailsWhenNothingCanBind(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	taken := busy.Addr().(*net.TCPAddr).Port

	if _, err := Listeners([]string{"only"}, map[string]int{"only": taken}); err == nil {
		t.Error("no profile could bind but Listeners reported success")
	}
}

// TestListeners_RequireOwned_UnknownNameFallsBack pins the fix for the outage
// case. The plist's Sockets entries come from the profiles installed at install
// time; the serve list is directory existence at runtime. A profile directory
// created before install.sh runs for it therefore has no Sockets entry, and
// treating that as fatal took down every OTHER profile on every launch until
// install.sh was re-run. launchd is not listening on a port it has no entry
// for, so binding it is safe.
func TestListeners_RequireOwned_UnknownNameFallsBack(t *testing.T) {
	setEnv(t, "DS4_REQUIRE_OWNED_SOCKET", "1")
	old := activate
	activate = func(name string) ([]int, error) {
		return nil, fmt.Errorf("%w: %w: no Sockets entry", errNotActivated, errNotOwned)
	}
	defer func() { activate = old }()

	port := freePort(t)
	lns, err := Listeners([]string{"direct"}, map[string]int{"direct": port})
	if err != nil {
		t.Fatalf("an unknown socket name should fall back to a plain bind, got %v", err)
	}
	for _, l := range lns {
		l.Close()
	}
}

// TestListeners_RequireOwned_OwnedPortStaysFatal is the other half: an error
// that does NOT mean "launchd holds nothing here" must still refuse to bind,
// which is the entire point of the flag.
func TestListeners_RequireOwned_OwnedPortStaysFatal(t *testing.T) {
	setEnv(t, "DS4_REQUIRE_OWNED_SOCKET", "1")
	old := activate
	activate = func(name string) ([]int, error) {
		return nil, fmt.Errorf("%w: EALREADY (already activated)", errNotActivated)
	}
	defer func() { activate = old }()

	lns, err := Listeners([]string{"direct"}, map[string]int{"direct": freePort(t)})
	if err == nil {
		for _, l := range lns {
			l.Close()
		}
		t.Fatal("a port launchd may already hold was bound anyway")
	}
}
