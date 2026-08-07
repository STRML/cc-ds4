// Command ds4-proxy is the Go reimplementation of src/proxy.py.
//
// It serves one HTTP listener per installed profile, mirroring Python's
// serve()/main() (proxy.py:1096-1160): --ports prints the served profiles for
// install.sh, and the default mode binds each served profile on its port
// (DS4_PORT_<NAME> override honored) with a proxy.Handler that rewrites and
// relays to the profile's upstream (DS4_UPSTREAM_<NAME> override honored for
// the differential harness).
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
	"github.com/strml/cc-ds4/src/go/internal/proxy"
)

func main() {
	served := profiles.Served()
	if len(served) == 0 {
		fmt.Fprintln(os.Stderr, "no profile directories found; nothing to serve")
		os.Exit(1)
	}

	if len(os.Args) > 1 && os.Args[1] == "--ports" {
		// int() so a junk DS4_PORT_* override fails here rather than being
		// interpolated into the plist install.sh builds from this output —
		// mirrors proxy.py:1139-1145.
		for _, p := range served {
			port, err := strconv.Atoi(os.Getenv("DS4_PORT_" + up(p.Name)))
			if err != nil {
				port = p.Port
			}
			fmt.Printf("%s %d\n", p.Name, port)
		}
		os.Stdout.Sync()
		return
	}

	// Serve the profiles. When any DS4_UPSTREAM_* override is present (the
	// differential-harness mode), serve ONLY the overridden profiles — the
	// others' real ports may be owned by the host's live Python proxy, and
	// binding them would both fail and pollute the harness. Otherwise serve
	// every installed profile, like Python's main(). The harness and install.sh
	// rely on the "name :port -> upstream" banner line (identical shape to
	// Python's) to learn the bound port.
	serveList := served
	anyOverride := false
	for _, p := range served {
		if os.Getenv("DS4_UPSTREAM_"+up(p.Name)) != "" {
			anyOverride = true
			break
		}
	}
	if anyOverride {
		serveList = nil
		for _, p := range served {
			if os.Getenv("DS4_UPSTREAM_"+up(p.Name)) != "" {
				serveList = append(serveList, p)
			}
		}
	}
	for _, p := range serveList {
		if err := serve(p); err != nil {
			fmt.Fprintf(os.Stderr, "  %s FAILED to bind: %v\n", p.Name, err)
		}
	}

	select {} // run forever; the harness kills us
}

func up(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

func serve(p profiles.Profile) error {
	// DS4_UPSTREAM_<NAME> overrides the upstream for the differential harness
	// (proxy.py has no such knob; the harness points both sides at fakes).
	upstream := p.Upstream
	if o := os.Getenv("DS4_UPSTREAM_" + up(p.Name)); o != "" {
		upstream = o
	}
	// Per-profile cfg overrides the harness passes for the corpus cases
	// (inject/spend/failover), mirroring the flags the Python oracle applies
	// via _profile_cfg. DS4_<KNOB>_<NAME>=1/0.
	if v := os.Getenv("DS4_INJECT_" + up(p.Name)); v == "1" {
		p.Inject = true
	} else if v == "0" {
		p.Inject = false
	}
	if v := os.Getenv("DS4_SPEND_" + up(p.Name)); v == "1" {
		p.Spend = true
	} else if v == "0" {
		p.Spend = false
	}
	if v := os.Getenv("DS4_FAILOVER_" + up(p.Name)); v != "" {
		p.Failover = v
	}

	port, err := strconv.Atoi(os.Getenv("DS4_PORT_" + up(p.Name)))
	if err != nil {
		port = p.Port
	}
	// DS4_RELAY_TIMEOUT mirrors proxy.py's RELAY_TIMEOUT (idle socket timeout,
	// default 60s; 0 disables).
	relayTimeout := 60 * time.Second
	if v := os.Getenv("DS4_RELAY_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			relayTimeout = time.Duration(secs) * time.Second
		}
	}

	// Clone the profile with the (possibly overridden) upstream so the handler
	// relays to the fake in harness runs.
	pc := p
	pc.Upstream = upstream

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	// The harness needs the ACTUAL bound port (a DS4_PORT_* of 0 means "pick
	// one"); report it in Python's banner shape.
	actual := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("  %-11s :%d -> %s\n", p.Name, actual, upstream)
	os.Stdout.Sync()

	// The harness can also discover ports via a file instead of the pipe
	// banner (pipe reads under the harness's subprocess select proved
	// unreliable). When DS4_PORT_FILE is set, append "<name>:<port>" so the
	// harness can poll the file.
	if pf := os.Getenv("DS4_PORT_FILE"); pf != "" {
		f, err := os.OpenFile(pf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%s:%d\n", p.Name, actual)
			f.Close()
		}
	}

	h := proxy.NewHandler(pc, relayTimeout)
	srv := &http.Server{Handler: h}
	go srv.Serve(ln) //nolint:errcheck // per-profile listener; errors surface via the process
	return nil
}
