//go:build !darwin || !cgo

package sockets

import "fmt"

// activateSocket has no launchd collector outside darwin+cgo. Linux never
// runs under launchd, and a darwin build with cgo disabled lacks the C
// shim, so every call reports "not activated" — Listeners falls back to a
// plain bind, or fails loud under DS4_REQUIRE_OWNED_SOCKET=1, exactly as it
// would on darwin if nothing had launched the process.
func activateSocket(name string) ([]int, error) {
	return nil, fmt.Errorf("%w: launchd socket activation is only implemented for darwin+cgo", errNotActivated)
}
