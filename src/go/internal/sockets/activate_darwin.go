//go:build darwin && cgo

package sockets

/*
#include <launch.h>
#include <stdlib.h>
#include <errno.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// activateSocket collects the fds launchd bound for name via
// launch_activate_socket, mirroring proxy.py's launchd_sockets(). This is
// the only supported way to receive a socket-activated launchd fd — the
// symbol lives in libSystem, not in package syscall, so there is no
// cgo-free path to it.
//
// A nonzero return is the normal path whenever nothing launched this
// process for that name (see errnoName for what each code means); it comes
// back wrapped in errNotActivated so the caller can fall back to a plain
// bind, or quote the reason when DS4_REQUIRE_OWNED_SOCKET=1 fails loud.
func activateSocket(name string) ([]int, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var fds *C.int
	var count C.size_t

	if ret := C.launch_activate_socket(cname, &fds, &count); ret != 0 {
		// ENOENT means launchd holds nothing under this name, so a plain bind
		// cannot collide with it. Anything else (EALREADY above all, and ESRCH,
		// which says we are not the launchd job we were configured as) may mean
		// launchd does hold the port, which is what requireOwned refuses.
		if ret == C.ENOENT {
			return nil, fmt.Errorf("%w: %w: launch_activate_socket(%q): %s",
				errNotActivated, errNotOwned, name, errnoName(ret))
		}
		return nil, fmt.Errorf("%w: launch_activate_socket(%q): %s", errNotActivated, name, errnoName(ret))
	}
	// launch.h documents the array as malloc'd for us and ours to free.
	// Copying the values out first lets the free happen unconditionally.
	defer C.free(unsafe.Pointer(fds))

	n := int(count)
	if n == 0 {
		return nil, fmt.Errorf("%w: launch_activate_socket(%q) returned zero fds", errNotActivated, name)
	}
	out := make([]int, n)
	for i, v := range unsafe.Slice(fds, n) {
		out[i] = int(v)
	}
	return out, nil
}

// errnoName renders the launch_activate_socket result codes launch.h
// documents. Anything else is still a real errno (the function's contract
// is "an appropriate POSIX-domain error"), so it prints numerically rather
// than guessing at a name.
func errnoName(errno C.int) string {
	switch errno {
	case C.ESRCH:
		return "ESRCH (this process is not managed by launchd)"
	case C.ENOENT:
		return "ENOENT (no Sockets entry with this name in the plist)"
	case C.EALREADY:
		return "EALREADY (already activated)"
	default:
		return fmt.Sprintf("errno %d", int(errno))
	}
}
