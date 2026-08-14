//go:build !darwin || !cgo

package sockets

import (
	"errors"
	"testing"
)

// TestActivateSocket_Stub pins the non-darwin/no-cgo contract: always
// "not activated", never a crash, regardless of name.
func TestActivateSocket_Stub(t *testing.T) {
	fds, err := activateSocket("direct")
	if err == nil {
		t.Fatalf("expected an error from the stub, got fds %v", fds)
	}
	if !errors.Is(err, errNotActivated) {
		t.Fatalf("error = %v, want it to wrap errNotActivated", err)
	}
	if fds != nil {
		t.Fatalf("expected nil fds, got %v", fds)
	}
}
