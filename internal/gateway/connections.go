package gateway

import (
	"context"
	"net"
	"sync"
	"time"
)

// requestConnections owns all dials and raw tunnels of one HTTP request.
// Cancelling a request also covers a Dialer that returns a connection late.
type requestConnections struct {
	dialer               Dialer
	connectTimeout, idle time.Duration
	ctx                  context.Context
	cancel               context.CancelFunc
	mu                   sync.Mutex
	closed               bool
	pending              sync.WaitGroup
	connections          []*ownedConn
}

func newRequestConnections(d Dialer, connectTimeout, idle time.Duration) *requestConnections {
	ctx, cancel := context.WithCancel(context.Background())
	return &requestConnections{dialer: d, connectTimeout: connectTimeout, idle: idle, ctx: ctx, cancel: cancel}
}

func (r *requestConnections) dial(ctx context.Context, network, address string) (net.Conn, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, net.ErrClosed
	}
	r.pending.Add(1)
	r.mu.Unlock()
	defer r.pending.Done()
	ctx, cancel := context.WithTimeout(ctx, r.connectTimeout)
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	defer cancel()
	c, err := r.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	owned := &ownedConn{Conn: &idleConn{Conn: c, idle: r.idle}}
	r.mu.Lock()
	r.connections = append(r.connections, owned)
	r.mu.Unlock()
	return owned, nil
}

func (r *requestConnections) close() {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.pending.Wait()
	for _, c := range r.connections {
		_ = c.Close()
	}
}

type ownedConn struct {
	net.Conn
	once sync.Once
	err  error
}

func (c *ownedConn) Close() error {
	// Concurrent transport and handler closes wait for the same completion.
	c.once.Do(func() { c.err = c.Conn.Close() })
	return c.err
}
