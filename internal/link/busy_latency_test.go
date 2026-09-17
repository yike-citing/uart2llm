package link

import (
	"context"
	"net"
	"testing"
	"time"
)

func productionRetryLink(t *testing.T) (*Link, *peer) {
	t.Helper()
	a, b := net.Pipe()
	p := newPeer(b)
	l := New(a, Options{})
	t.Cleanup(func() { l.Close(); b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	return l, p
}

func TestBusyProbesPromptlyWithoutDuplicateTCPWrites(t *testing.T) {
	l, p := productionRetryLink(t)
	c := dial(t, l)
	defer c.Close()
	p.busyRemaining.Store(3)
	started := time.Now()
	if _, err := c.Write([]byte("credit recovered")); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < 50*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Fatalf("three BUSY probes should be paced at 20 ms, not lost-ACK intervals: %v", elapsed)
	}
	if p.dataExecutions.Load() != 1 {
		t.Fatal("BUSY retry duplicated TCP data")
	}
	if l.Err() != nil {
		t.Fatal("BUSY closed the session")
	}
}

func TestLostAckStillUsesNormalRetryInterval(t *testing.T) {
	l, p := productionRetryLink(t)
	c := dial(t, l)
	defer c.Close()
	p.dropAck.Store(true)
	started := time.Now()
	if _, err := c.Write([]byte("ack dropped")); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 450*time.Millisecond {
		t.Fatalf("lost ACK used aggressive BUSY timer: %v", elapsed)
	}
	if p.dataExecutions.Load() != 1 {
		t.Fatal("lost ACK duplicated TCP data")
	}
}
