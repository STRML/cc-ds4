package relay

import (
	"context"
	"net"
	"time"
)

type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err == nil {
		c.reset()
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err == nil {
		c.reset()
	}
	return n, err
}

func (c *idleConn) reset() {
	if c.timeout > 0 {
		c.Conn.SetDeadline(time.Now().Add(c.timeout))
	}
}

// DialContextWithIdleTimeout returns a DialContext that wraps every dialed
// conn in idleConn. timeout<=0 disables deadlines entirely.
func DialContextWithIdleTimeout(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{}
		raw, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		ic := &idleConn{Conn: raw, timeout: timeout}
		ic.reset()
		return ic, nil
	}
}
