package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type closeBarrierDialer struct {
	entered chan struct{}
	release chan struct{}
}

func (d *closeBarrierDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &closeBarrierConn{Conn: c, entered: d.entered, release: d.release}, nil
}

type closeBarrierConn struct {
	net.Conn
	once             sync.Once
	entered, release chan struct{}
}

func (c *closeBarrierConn) Close() error {
	c.once.Do(func() {
		c.Conn.Close()
		close(c.entered)
		<-c.release
	})
	return nil
}

func TestRequestCapacityHeldUntilTunnelCloseCompletes(t *testing.T) {
	g, proxy, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	d := &closeBarrierDialer{entered: make(chan struct{}), release: make(chan struct{})}
	g.Dialer = d
	defer close(d.release)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		res, err := http.DefaultClient.Do(request("POST", proxy.URL+"/v1/chat/completions", nil))
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	select {
	case <-d.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not start closing its tunnel")
	}
	// The client may see SSE bytes while cleanup is running, but admission
	// must still count that channel until the serial CLOSE barrier completes.
	select {
	case <-clientDone:
		if g.Active() != 1 {
			t.Fatal("HTTP capacity released while the serial channel is still closing")
		}
	case <-time.After(100 * time.Millisecond):
		if g.Active() != 1 {
			t.Fatal("closing request not included in active capacity")
		}
	}
}

type lateDialer struct {
	started, cancelled chan struct{}
	conn               net.Conn
}

func (d *lateDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	close(d.started)
	<-ctx.Done()
	close(d.cancelled)
	// Model a dial that races cancellation and still succeeds.
	return d.conn, nil
}

func TestRequestCleanupCancelsAndClosesLateDial(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	d := &lateDialer{started: make(chan struct{}), cancelled: make(chan struct{}), conn: a}
	owned := newRequestConnections(d, time.Second, time.Second)
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		owned.dial(context.Background(), "tcp", "test.invalid:443")
	}()
	<-d.started
	owned.close()
	<-d.cancelled
	<-dialDone
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("late tunnel leaked: %v", err)
	}
	if _, err := owned.dial(context.Background(), "tcp", "test.invalid:443"); err != net.ErrClosed {
		t.Fatalf("dial admitted after request cleanup: %v", err)
	}
}
