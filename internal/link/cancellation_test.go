package link

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestDeadlineRefreshWakesBothBlockedDirections(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	defer c.Close()
	p.echo.Store(false)
	p.busyRemaining.Store(16)
	c.SetDeadline(time.Now().Add(60 * time.Millisecond))
	wrote, read := make(chan error, 1), make(chan error, 1)
	go func() { _, err := c.Write([]byte("outbound")); wrote <- err }()
	go func() { var b [1]byte; _, err := c.Read(b[:]); read <- err }()
	refreshDone := make(chan struct{})
	defer close(refreshDone)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.SetDeadline(time.Now().Add(60 * time.Millisecond))
			case <-refreshDone:
				return
			}
		}
	}()
	timer := time.AfterFunc(200*time.Millisecond, func() { p.send(1, Data, []byte{1}) })
	defer timer.Stop()
	for _, result := range []chan error{wrote, read} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("renewed deadline did not wake blocked IO: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("IO did not finish after deadline refresh")
		}
	}
}

func TestBusyPreservesConnectionBeyondRetryBudget(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	defer c.Close()
	p.busyRemaining.Store(12)
	if _, e := c.Write([]byte("bounded but patient")); e != nil {
		t.Fatal(e)
	}
	if l.Err() != nil || p.dataExecutions.Load() != 1 {
		t.Fatal("busy exhausted retry budget or duplicated data")
	}
}
func TestWriteDeadlineDrainsFrameAndPreservesOtherChannels(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	other := dial(t, l)
	defer other.Close()
	p.dataAckDelay.Store(int64(25 * time.Millisecond))
	c.SetWriteDeadline(time.Now().Add(5 * time.Millisecond))
	if _, e := c.Write([]byte("cancelled")); !errors.Is(e, os.ErrDeadlineExceeded) {
		t.Fatalf("expected deadline: %v", e)
	}
	c.Close()
	if l.Err() != nil {
		t.Fatal("cancelled request killed healthy session")
	}
	p.dataAckDelay.Store(0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, e := l.RPC(ctx, "rpc", []byte("still alive")); e != nil {
		t.Fatal(e)
	}
	if _, e := other.Write([]byte("other request")); e != nil {
		t.Fatal(e)
	}
}
func TestCloseInterruptsBlockedWrite(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	p.dataAckDelay.Store(int64(25 * time.Millisecond))
	writing := make(chan error, 1)
	go func() { _, e := c.Write(make([]byte, 10000)); writing <- e }()
	time.Sleep(5 * time.Millisecond)
	c.Close()
	select {
	case e := <-writing:
		if !errors.Is(e, net.ErrClosed) {
			t.Fatalf("write after close: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("close left blocked writer")
	}
	if l.Err() != nil {
		t.Fatal("close killed healthy session")
	}
}

func TestAbortReleasesCongestedStreamWithoutLosingManagement(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	other := dial(t, l)
	defer other.Close()
	p.busyRemaining.Store(1000000)
	writing := make(chan error, 1)
	go func() { _, err := c.Write(make([]byte, 8192)); writing <- err }()
	time.Sleep(25 * time.Millisecond)
	c.Close()
	select {
	case err := <-writing:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("cancelled writer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ABORT did not drain outstanding frame")
	}
	if l.Err() != nil || p.abortCount.Load() == 0 {
		t.Fatal("congested cancellation destroyed session or did not send ABORT")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := l.RPC(ctx, "rpc", []byte("still alive")); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Write([]byte("unaffected stream")); err != nil {
		t.Fatal(err)
	}
	// A delayed ABORT from the old incarnation must not affect this slot.
	next := dial(t, l)
	defer next.Close()
	p.handle(Frame{Kind: Abort, Session: l.session, Channel: c.(*Conn).id, Sequence: c.(*Conn).openSequence.Load()})
	if _, err := next.Write([]byte("new incarnation")); err != nil {
		t.Fatal(err)
	}
	var got [15]byte
	if _, err := io.ReadFull(next, got[:]); err != nil || string(got[:]) != "new incarnation" {
		t.Fatalf("stale ABORT affected new connection: %q %v", got, err)
	}
}

func TestCancelledOpenCompletesCloseBarrierBeforeReuse(t *testing.T) {
	l, p := testLink(t)
	p.openAckDelay.Store(int64(25 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := l.DialContext(ctx, "tcp", "cancelled.example:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancelled OPEN, got %v", err)
	}
	if l.Err() != nil {
		t.Fatal("cancelled OPEN destroyed healthy session")
	}
	p.openAckDelay.Store(0)
	c := dial(t, l)
	defer c.Close()
	if _, err := c.Write([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	var b [5]byte
	if _, err := io.ReadFull(c, b[:]); err != nil || string(b[:]) != "fresh" {
		t.Fatalf("stale OPEN leaked across slot reuse: %q %v", b, err)
	}
}
