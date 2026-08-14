//go:build darwin && cgo

package sockets

import (
	"errors"
	"testing"
)

// TestActivateSocket_NoLaunchdParent exercises the real cgo call. `go test`
// is never itself a launchd job, so launch_activate_socket must return
// ESRCH for any name, proving the cgo plumbing (CString/free, the fds/count
// out-params, the malloc'd-array free) round-trips without crashing. This
// is the only piece of the launchd contract this repo can exercise without
// a real launchd parent handing us a socket — see the status report for
// what that leaves uncovered.
func TestActivateSocket_NoLaunchdParent(t *testing.T) {
	fds, err := activateSocket("direct")
	if err == nil {
		t.Fatalf("expected an error with no launchd parent, got fds %v", fds)
	}
	if !errors.Is(err, errNotActivated) {
		t.Fatalf("error = %v, want it to wrap errNotActivated", err)
	}
	if fds != nil {
		t.Fatalf("expected nil fds on error, got %v", fds)
	}
}

func TestActivateSocket_UnknownName(t *testing.T) {
	// Same ESRCH path as above in practice (this process is not a launchd
	// job regardless of name), but pins that an arbitrary name does not
	// panic or behave differently from a real profile name.
	fds, err := activateSocket("not-a-real-socket-name")
	if err == nil {
		t.Fatalf("expected an error, got fds %v", fds)
	}
	if !errors.Is(err, errNotActivated) {
		t.Fatalf("error = %v, want it to wrap errNotActivated", err)
	}
}
