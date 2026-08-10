package relay

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// recordingConn wraps a net.Conn and records every SetDeadline call, so a test
// can assert reset-on-read semantics without any wall-clock timing.
type recordingConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *recordingConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return c.Conn.SetDeadline(t)
}

// TestIdleConnResetsDeadlineOnRead pins the core contract deterministically:
// a successful Read re-arms the idle deadline (a later SetDeadline call).
func TestIdleConnResetsDeadlineOnRead(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rc := &recordingConn{Conn: client}
	ic := &idleConn{Conn: rc, timeout: 100 * time.Millisecond}
	ic.reset() // initial arming

	go func() {
		server.Write([]byte("a"))
	}()

	buf := make([]byte, 1)
	if _, err := ic.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if buf[0] != 'a' {
		t.Fatalf("read = %q, want %q", buf[0], 'a')
	}

	// Initial arming plus one reset-on-read.
	if n := len(rc.deadlines); n != 2 {
		t.Fatalf("SetDeadline called %d times, want 2 (initial + reset-on-read)", n)
	}
	if !rc.deadlines[1].After(rc.deadlines[0]) {
		t.Fatalf("deadline was not reset after the first read: %v not after %v", rc.deadlines[1], rc.deadlines[0])
	}
}

// TestIdleConnResetsDeadlineOnWrite pins the same reset contract for writes.
func TestIdleConnResetsDeadlineOnWrite(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rc := &recordingConn{Conn: client}
	ic := &idleConn{Conn: rc, timeout: 100 * time.Millisecond}
	ic.reset()

	// net.Pipe writes block until the peer reads; drain the server side so the
	// writes can complete (and then the client deadlines can be observed). Each
	// Read consumes exactly one of the two single-byte writes.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		b := make([]byte, 1)
		server.Read(b)
		server.Read(b)
	}()

	if _, err := ic.Write([]byte("x")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := ic.Write([]byte("y")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	<-drained
	if n := len(rc.deadlines); n != 3 {
		t.Fatalf("SetDeadline called %d times, want 3 (initial + one per successful write)", n)
	}
}

// TestIdleConnDoesNotResetAfterError pins that a failed op leaves the deadline
// alone: a read that returns an error must not re-arm it.
func TestIdleConnDoesNotResetAfterError(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rc := &recordingConn{Conn: client}
	ic := &idleConn{Conn: rc, timeout: 100 * time.Millisecond}
	ic.reset()
	before := len(rc.deadlines)

	server.Close() // a read on the closed pipe now fails (EOF) instead of succeeding

	buf := make([]byte, 1)
	if _, err := ic.Read(buf); err != io.EOF {
		t.Fatalf("read error = %v, want io.EOF", err)
	}
	if n := len(rc.deadlines); n != before {
		t.Fatalf("SetDeadline called %d times after an errored read, want still %d", n, before)
	}
}

// TestIdleConnZeroTimeoutArmsNoDeadlines pins the DS4_RELAY_TIMEOUT=0 case:
// timeout<=0 means no deadlines are ever armed, not even on a successful read.
func TestIdleConnZeroTimeoutArmsNoDeadlines(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rc := &recordingConn{Conn: client}
	ic := &idleConn{Conn: rc, timeout: 0}
	ic.reset()
	if n := len(rc.deadlines); n != 0 {
		t.Fatalf("timeout=0 armed %d deadlines on init, want none", n)
	}

	go func() {
		server.Write([]byte("a"))
	}()

	buf := make([]byte, 1)
	if _, err := ic.Read(buf); err != nil {
		t.Fatalf("read with timeout=0: %v", err)
	}
	if n := len(rc.deadlines); n != 0 {
		t.Fatalf("timeout=0 armed %d deadlines after a read, want none", n)
	}
}

// TestIdleConnTimesOutWhenIdle pins that an idle connection actually dies:
// with no activity past the timeout, a blocked read returns an error.
func TestIdleConnTimesOutWhenIdle(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ic := &idleConn{Conn: client, timeout: 100 * time.Millisecond}
	ic.reset()

	go func() {
		server.Write([]byte("a"))
		time.Sleep(300 * time.Millisecond) // far past the timeout; nothing should rescue this read
		server.Write([]byte("b"))
	}()

	buf := make([]byte, 1)
	if _, err := ic.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := ic.Read(buf); err == nil {
		t.Fatal("second read succeeded after an idle gap past the timeout; want a deadline error")
	}
}

// TestIdleConnKeepsAliveAcrossIdleGap pins reset-on-read end to end: a second
// burst that arrives after the *initial* deadline would have expired still
// succeeds, because the first (late) read reset the deadline. With a single
// absolute deadline armed at dial, the second read would time out.
//
// Timing (timeout = 1s):
//   t=0s    deadline armed -> t=1s
//   t=800ms server writes "a" (just inside the initial deadline)
//   t=800ms first read completes, deadline reset -> t=1.8s
//   t=1.4s  server writes "b" (past the initial 1s deadline)
//   t=1.4s  second read succeeds (within the reset 1.8s deadline)
//
// Margins are 200-400ms so the test is not timing-flaky.
func TestIdleConnKeepsAliveAcrossIdleGap(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ic := &idleConn{Conn: client, timeout: 1 * time.Second}
	ic.reset()

	go func() {
		time.Sleep(800 * time.Millisecond)
		server.Write([]byte("a"))
		time.Sleep(600 * time.Millisecond) // -> t=1.4s, past the initial deadline
		server.Write([]byte("b"))
	}()

	buf := make([]byte, 1)
	if _, err := ic.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := ic.Read(buf); err != nil {
		t.Fatalf("second read: %v (an absolute deadline at t+1s would have expired before \"b\" arrived at t+1.4s; the deadline must have been reset by the first read)", err)
	}
	if buf[0] != 'b' {
		t.Fatalf("second read = %q, want %q", buf[0], 'b')
	}
}

// TestDialContextWithIdleTimeoutWrapsConn pins the load-bearing contract: the
// returned DialContext dials a real conn and wraps it in *idleConn carrying the
// given timeout.
func TestDialContextWithIdleTimeoutWrapsConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("a"))
	}()

	dial := DialContextWithIdleTimeout(123 * time.Millisecond)
	conn, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ic, ok := conn.(*idleConn)
	if !ok {
		t.Fatalf("dial returned %T, want *idleConn", conn)
	}
	if ic.timeout != 123*time.Millisecond {
		t.Fatalf("wrapped timeout = %v, want 123ms", ic.timeout)
	}

	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read through wrapped conn: %v", err)
	}
	if buf[0] != 'a' {
		t.Fatalf("read = %q, want %q", buf[0], 'a')
	}
}
