package proxy

import (
	"sync/atomic"
	"time"
)

// Traffic tracks the two signals the idle watch needs, across every profile.
// One process serves all of them and exits as a unit, so the counters are
// process-wide rather than per-Handler.
//
// Python keeps the same pair as module globals (_last_seen and _inflight,
// guarded by a lock). Atomics are enough here: the watch only ever compares
// them against a threshold, so a read that races a concurrent update reads
// either the old or the new value, and both answers are correct one tick apart.
type Traffic struct {
	lastSeenUnixNano atomic.Int64
	inFlight         atomic.Int64
}

// DefaultTraffic is what Handler records into and what main() reads. A package
// global matches Python's shape and keeps the recording off the Handler's
// constructor, so every profile's Handler feeds one process-wide view.
var DefaultTraffic Traffic

// begin marks a request as started. The returned func marks it done and must
// run exactly once, so callers defer it.
func (t *Traffic) begin() func() {
	t.lastSeenUnixNano.Store(time.Now().UnixNano())
	t.inFlight.Add(1)
	var done atomic.Bool
	return func() {
		// A double call would drive the counter negative and make the proxy
		// look permanently busy, which silently disables idle exit.
		if done.CompareAndSwap(false, true) {
			t.lastSeenUnixNano.Store(time.Now().UnixNano())
			t.inFlight.Add(-1)
		}
	}
}

// LastSeen is when a request last started or finished.
func (t *Traffic) LastSeen() time.Time {
	return time.Unix(0, t.lastSeenUnixNano.Load())
}

// InFlight is how many requests are being relayed right now.
func (t *Traffic) InFlight() int {
	return int(t.inFlight.Load())
}

// Activity renders this Traffic in the shape WatchIdle takes.
func (t *Traffic) Activity() Activity {
	return Activity{LastSeen: t.LastSeen, InFlight: t.InFlight}
}
