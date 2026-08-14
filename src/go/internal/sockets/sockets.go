// Package sockets handles launchd socket activation for the ds4-proxy.
//
// launchd's Sockets block binds the profile ports and hands the listening
// fds to the process it starts on the first connection. The process must
// collect them via launch_activate_socket (darwin+cgo) or bind its own ports
// (everywhere else, and darwin when the plist never activated us for that
// name). This is the boundary proxy.py's launchd_sockets() + server_on_fd()
// live at; activateSocket (platform-specific, see activate_darwin.go and
// activate_other.go) is launch_activate_socket's Go side.
package sockets

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// errNotActivated means launchd did not hand this process a socket for the
// requested name. That covers every documented launch_activate_socket
// failure (ESRCH: not a launchd job at all, ENOENT: no such Sockets key in
// the plist, EALREADY: already claimed) and the non-darwin/non-cgo stub,
// which can never be activated. All of them mean the same thing to the
// caller: bind the port yourself, or fail loud if the environment insists
// launchd must own it.
// activate is activateSocket, swappable so a test can drive the requireOwned
// branch. Reaching the real ENOENT needs a launchd job with a Sockets entry
// missing for one name, which a test binary cannot arrange.
var activate = activateSocket

var errNotActivated = errors.New("no launchd-owned socket for this name")

// errNotOwned means launchd is managing us but has no Sockets entry under this
// name (ENOENT), so it is not listening on that port and never was. Binding it
// directly cannot collide with launchd, so DS4_REQUIRE_OWNED_SOCKET must not
// treat it as fatal: that flag exists to refuse a port launchd already holds,
// not to require that launchd hold every port.
//
// ESRCH is deliberately NOT included. It means we are not running under launchd
// at all, and the flag is only ever set by the launch agent itself, so ESRCH
// says the process is not the thing it was configured to be. That stays fatal.
//
// The distinction is load-bearing. The plist's Sockets entries are written
// from the profiles installed AT INSTALL TIME, while the serve list is
// directory existence AT RUNTIME. Creating a profile directory before running
// install.sh for it — which is exactly what the profile docs tell you to do,
// settings.json first and install.sh after — then gave that name no Sockets
// entry, and one fatal error took down every OTHER profile too, on every
// launch, until install.sh was re-run. With idle exit on by default a restart
// is never far away, so the window was not narrow.
var errNotOwned = errors.New("launchd does not own this port")

// Listeners returns one listener per profile port, preferring inherited
// launchd fds. It mirrors serve()'s port resolution: DS4_PORT_<NAME>
// overrides, else the table port. That override only applies to the plain
// bind path — once launchd owns a socket, the port it bound is authoritative
// the same way proxy.py's serve() reports "launchd" origin instead of the
// configured port.
//
// DS4_REQUIRE_OWNED_SOCKET=1 turns a missing launchd socket into a hard
// error instead of a silent self-bind, for production installs where a
// self-bound fallback would mean two processes racing for the same port.
//
// listeners is a map of profile name -> bound listener; the caller serves each.
func Listeners(names []string, ports map[string]int) (map[string]net.Listener, error) {
	requireOwned := os.Getenv("DS4_REQUIRE_OWNED_SOCKET") == "1"
	out := make(map[string]net.Listener, len(names))
	var failed []string
	for _, name := range names {
		ln, err := listenerFor(name, ports, requireOwned)
		if err != nil {
			// Fatal only when launchd may actually own this port. A bind that
			// failed on a port launchd demonstrably does NOT own (errNotOwned,
			// e.g. a leftover process still holding it) is one profile's
			// problem, and killing the process over it recreates the very
			// outage the errNotOwned split was added to prevent — just one step
			// further along.
			if requireOwned && !errors.Is(err, errNotOwned) {
				// Fail loudly rather than bind a port launchd believes it owns.
				// Close what we have so a partial bind leaves no dangling ports.
				for _, l := range out {
					l.Close()
				}
				return nil, err
			}
			// One profile's port being busy (a leftover process, usually) is
			// not a reason to take the other profiles down with it. Report it
			// and keep serving the rest; the caller sees a missing entry.
			fmt.Fprintf(os.Stderr, "ds4-proxy: %s: %v\n", name, err)
			failed = append(failed, name)
			continue
		}
		out[name] = ln
	}
	if len(out) == 0 && len(names) > 0 {
		return nil, fmt.Errorf("no profile could bind (%s)", strings.Join(failed, ", "))
	}
	return out, nil
}

// listenerFor resolves one profile's listener: try the launchd-activated fd
// first, then fall back to (or refuse, under requireOwned) a plain bind.
func listenerFor(name string, ports map[string]int, requireOwned bool) (net.Listener, error) {
	fds, err := activate(name)
	if err == nil {
		// getaddrinfo(3) can hand back more than one fd for a single Sockets
		// key (e.g. one interface per address family). install.sh only ever
		// writes one SockNodeName per profile, so this is always 1 in
		// practice, but take the first and close the rest rather than
		// silently leaving an extra listener nobody accepts on.
		ln, lnErr := newListenerFromFD(fds[0])
		if ln == nil {
			// launchd holds this port bound whether or not we managed to wrap
			// its fd. Skipping the profile here leaves the socket listening
			// with nobody accepting, so a client connects, gets accepted by
			// launchd, and hangs forever instead of being refused — strictly
			// worse than connection-refused, and the failure the Sockets
			// contract exists to avoid. Close what we were handed and make it
			// fatal: this is NOT errNotOwned, so Listeners will not skip past
			// it.
			for _, fd := range fds {
				closeFD(fd)
			}
			return nil, fmt.Errorf("%s: inherited fd %d: %w", name, fds[0], lnErr)
		}
		if lnErr != nil {
			// A usable listener plus an error means the dup succeeded and only
			// the close of the original fd failed. That costs one descriptor
			// for the life of the process. Refusing to start over it would
			// hand back a port launchd already bound and take every profile
			// down, so serve the listener and say so.
			fmt.Fprintf(os.Stderr, "ds4-proxy: %s: %v\n", name, lnErr)
		}
		for _, extra := range fds[1:] {
			closeFD(extra)
		}
		return ln, nil
	}
	if requireOwned && !errors.Is(err, errNotOwned) {
		return nil, fmt.Errorf("%s: %w (DS4_REQUIRE_OWNED_SOCKET=1 refuses to bind its own port)", name, err)
	}

	port := ports[name]
	if p := os.Getenv("DS4_PORT_" + upper(name)); p != "" {
		if n, perr := strconv.Atoi(p); perr == nil {
			port = n
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Carry errNotOwned forward: we only reached the plain bind because
		// launchd holds nothing here, and that stays true whether or not the
		// bind succeeded.
		return nil, fmt.Errorf("%s: %w: %w", name, errNotOwned, err)
	}
	return ln, nil
}

// newListenerFromFD wraps an already-listening fd (handed to us by launchd)
// as a net.Listener. net.FileListener dups fd internally, so once it
// succeeds the os.File wrapping the original is redundant and must be
// closed — we never opened that fd ourselves, so leaving it open leaks one
// descriptor per socket every time launchd restarts this process.
// newListenerFromFD wraps an inherited fd as a listener.
//
// The return is deliberately (non-nil, non-nil) in one case: the dup
// succeeded but closing the original fd did not. Callers must test the
// LISTENER for nil, not the error — a listener that came back usable is
// usable, and discarding it would give up a port launchd already bound.
func newListenerFromFD(fd int) (net.Listener, error) {
	f := os.NewFile(uintptr(fd), "launchd-socket")
	if f == nil {
		return nil, fmt.Errorf("invalid fd %d", fd)
	}
	ln, err := net.FileListener(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if cerr := f.Close(); cerr != nil {
		// ln is already valid (FileListener's dup succeeded), so this is a
		// leaked-descriptor warning, not a reason to discard a working
		// listener — but it is unexpected enough (e.g. EBADF) to surface
		// rather than swallow.
		return ln, fmt.Errorf("close original fd %d: %w", fd, cerr)
	}
	return ln, nil
}

// closeFD releases an inherited fd we are not going to use (the "extra"
// fds beyond the first for a single Sockets key). Errors are not
// actionable here — the fd is being discarded either way — so they are
// dropped rather than propagated.
func closeFD(fd int) {
	os.NewFile(uintptr(fd), "launchd-socket-extra").Close()
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
