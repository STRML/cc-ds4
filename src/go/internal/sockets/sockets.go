// Package sockets handles launchd socket activation for the ds4-proxy.
//
// launchd's Sockets block binds the profile ports and hands the listening fds
// to the process it starts on first connection. The process must collect them
// via launch_activate_socket (the cgo path, macOS) or bind its own ports
// (plain, non-macOS / no-ownership). This is the boundary proxy.py's
// launchd_sockets() + server_on_fd() live at.
//
// Phase A ships the plain-bind path (DS4_REQUIRE_OWNED_SOCKET=0, the default
// and what the differential harness exercises). The cgo launch_activate_socket
// collector is a follow-up for the socket-activated production install.
package sockets

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// Listeners returns one listener per profile port, preferring inherited
// launchd fds when REQUIRE_OWNED_SOCKET is set (not yet implemented — the
// plain bind is authoritative for Phase A). It mirrors serve()'s port
// resolution: DS4_PORT_<NAME> overrides, else the table port.
//
// listeners is a map of profile name -> bound listener; the caller serves each.
func Listeners(names []string, ports map[string]int) (map[string]net.Listener, error) {
	if os.Getenv("DS4_REQUIRE_OWNED_SOCKET") == "1" {
		// Socket activation via launch_activate_socket requires cgo. Phase A
		// falls back to plain binds; a production launchd install can keep the
		// Python proxy until the cgo collector lands.
		return nil, fmt.Errorf("DS4_REQUIRE_OWNED_SOCKET=1 requires the cgo launch_activate_socket collector (not yet implemented); use the default plain bind")
	}
	out := make(map[string]net.Listener, len(names))
	for _, name := range names {
		port := ports[name]
		if p := os.Getenv("DS4_PORT_" + upper(name)); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			// Close what we have so a partial bind leaves no dangling ports.
			for _, l := range out {
				l.Close()
			}
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = ln
	}
	return out, nil
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
