package proxy

// recordOutcome feeds one relay's outcome into the failover breaker. The
// breaker itself (windowed strike counting, open/close transitions, probes,
// failover routing) is a later task; this stub exists so the call site is in
// place at the one point the ordering contract demands: it runs BEFORE the
// response body is streamed, so a mid-stream stall counts the request as a
// HIT (the upstream served a response), never a strike.
//
// A transient status is a strike, anything else is a hit — the same
// classification failover_record in proxy.py applies. Nothing is recorded yet
// because there is no state to record into.
func (h *Handler) recordOutcome(statusCode int) {
	_ = statusCode
}
