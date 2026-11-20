package main

import (
	"cmp"
	"context"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// connSnapshot is the JSON representation of a single active upstream
// connection returned by GET /debug/connections on the control socket.
type connSnapshot struct {
	ID         uint64    `json:"id"`
	Proto      string    `json:"proto"`
	Target     string    `json:"target"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
}

type trackedEntry struct {
	id        uint64
	proto     string
	target    string
	startedAt time.Time
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
}

// connTracker tracks active upstream connections dialed by SOCKS5, HTTP proxy,
// and -L local TCP port forwards. Its mutex is never held across dials or I/O.
type connTracker struct {
	nextID atomic.Uint64
	mu     sync.Mutex
	conns  map[uint64]*trackedEntry
}

func newConnTracker() *connTracker {
	return &connTracker{
		conns: make(map[uint64]*trackedEntry),
	}
}

// Wrap registers conn as an active upstream connection and returns a
// trackedConn that updates byte counters on Read/Write and unregisters on Close.
func (t *connTracker) Wrap(proto, target string, conn net.Conn) net.Conn {
	if t == nil || conn == nil {
		return conn
	}
	id := t.nextID.Add(1)
	entry := &trackedEntry{
		id:        id,
		proto:     proto,
		target:    target,
		startedAt: time.Now(),
	}
	t.mu.Lock()
	t.conns[id] = entry
	t.mu.Unlock()
	return &trackedConn{
		Conn:    conn,
		tracker: t,
		entry:   entry,
	}
}

// WrapDial wraps a dial function so every successfully established connection
// is tracked under proto. The tracker lock is never held while dial runs.
func (t *connTracker) WrapDial(proto string, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if t == nil {
		return dial
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return t.Wrap(proto, addr, conn), nil
	}
}

func (t *connTracker) remove(id uint64) {
	t.mu.Lock()
	delete(t.conns, id)
	t.mu.Unlock()
}

// Count returns the current number of active tracked connections.
func (t *connTracker) Count() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	n := len(t.conns)
	t.mu.Unlock()
	return n
}

// Snapshot returns a slice of active connections sorted ascending by ID.
func (t *connTracker) Snapshot() []connSnapshot {
	if t == nil {
		return []connSnapshot{}
	}
	now := time.Now()
	t.mu.Lock()
	out := make([]connSnapshot, 0, len(t.conns))
	for _, e := range t.conns {
		dur := now.Sub(e.startedAt).Milliseconds()
		if dur < 0 {
			dur = 0
		}
		out = append(out, connSnapshot{
			ID:         e.id,
			Proto:      e.proto,
			Target:     e.target,
			StartedAt:  e.startedAt.UTC(),
			DurationMS: dur,
			BytesIn:    e.bytesIn.Load(),
			BytesOut:   e.bytesOut.Load(),
		})
	}
	t.mu.Unlock()
	slices.SortFunc(out, func(a, b connSnapshot) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// trackedConn wraps a net.Conn to record transferred bytes and remove the entry
// from connTracker on Close. It explicitly forwards CloseWrite and CloseRead so
// half-close support (both local halfCloser and tailscale nettype.HalfCloser
// used by socks5.Server) continues to work through the wrapper.
type trackedConn struct {
	net.Conn
	tracker   *connTracker
	entry     *trackedEntry
	closeOnce sync.Once
}

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.entry.bytesIn.Add(int64(n))
	}
	return n, err
}

func (c *trackedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.entry.bytesOut.Add(int64(n))
	}
	return n, err
}

func (c *trackedConn) CloseWrite() error {
	if hc, ok := c.Conn.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return nil
}

func (c *trackedConn) CloseRead() error {
	if rc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return rc.CloseRead()
	}
	return nil
}

func (c *trackedConn) Close() error {
	c.closeOnce.Do(func() {
		c.tracker.remove(c.entry.id)
	})
	return c.Conn.Close()
}
