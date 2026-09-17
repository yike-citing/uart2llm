package link

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestSeparateRPCLanesProgressWithFourSaturatedStreams(t *testing.T) {
	l, p := testLink(t)
	var conns [4]net.Conn
	var blocked [4]Frame
	for i := range conns {
		conns[i] = dial(t, l)
		for j := 0; j < 8; j++ {
			waitAck(t, p, p.send(byte(i+1), Data, []byte{byte(j)}), Ack)
		}
		blocked[i] = p.send(byte(i+1), Data, []byte{8})
		waitAck(t, p, blocked[i], Busy)
	}
	if _, err := l.DialContext(context.Background(), "tcp", "fifth.test:443"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("control lanes expanded TCP capacity: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, lane := range []uint8{ChannelManagement, ChannelTelemetry, ChannelLogs, ChannelOTA} {
		wg.Add(1)
		go func(lane uint8) {
			defer wg.Done()
			got, err := l.RPC(WithRPCChannel(ctx, lane), "rpc", []byte{lane})
			if err != nil || len(got) != 1 || got[0] != lane {
				t.Errorf("lane %d reply %x: %v", lane, got, err)
			}
		}(lane)
	}
	wg.Wait()
	seen := map[byte]bool{}
	for i := 0; i < 4; i++ {
		select {
		case lane := <-p.rpcChannels:
			seen[lane] = true
		case <-ctx.Done():
			t.Fatal("RPC never reached UART peer")
		}
	}
	for _, lane := range []uint8{0, 5, 6, 7} {
		if !seen[lane] {
			t.Errorf("missing wire channel %d", lane)
		}
	}
	for i, c := range conns {
		var one [1]byte
		if _, err := c.Read(one[:]); err != nil {
			t.Fatal(err)
		}
		p.emit(blocked[i])
		waitAck(t, p, blocked[i], Ack)
		// The fixture emits CLOSE only once (unlike the reliable device).
		// Drain and verify the saturated queue before closing so CLOSE cannot
		// race the cleanup goroutine, receive BUSY and disappear forever.
		for want := byte(1); want <= 8; want++ {
			if _, err := c.Read(one[:]); err != nil || one[0] != want {
				t.Fatalf("channel %d byte %d: got %d, error %v", i+1, want, one[0], err)
			}
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if l.Err() != nil {
		t.Fatal("control lane activity damaged data channels")
	}
}

func TestRPCChannelValidationAndHandshakeLane(t *testing.T) {
	l, p := testLink(t)
	for _, lane := range []uint8{1, 2, 3, 4, 8, 255} {
		if _, err := l.RPC(WithRPCChannel(context.Background(), lane), "rpc", nil); err == nil {
			t.Errorf("accepted invalid RPC channel %d", lane)
		}
	}
	if _, err := l.RPC(WithRPCChannel(context.Background(), ChannelOTA), "sec-session", []byte("handshake")); err != nil {
		t.Fatal(err)
	}
	if lane := <-p.rpcChannels; lane != ChannelManagement {
		t.Fatalf("security handshake used channel %d", lane)
	}
	for _, lane := range []uint8{0, 1, 4, 5, 6, 7} {
		b, err := Encode(Frame{Kind: Ack, Session: 1, Channel: lane, Sequence: 1})
		if err != nil {
			t.Fatal(err)
		}
		f, err := Decode(b[:len(b)-1])
		if err != nil || f.Channel != lane {
			t.Fatalf("wire lane %d: %v", lane, err)
		}
	}
	if _, err := Encode(Frame{Kind: Ack, Channel: ChannelCount}); err == nil {
		t.Fatal("encoded invalid channel")
	}
}

func TestRPCReplyCannotCrossControlLanes(t *testing.T) {
	l, p := testLink(t)
	p.wrongRPCLane.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := l.RPC(WithRPCChannel(ctx, ChannelTelemetry), "rpc", []byte("same ID, wrong lane")); err == nil {
		t.Fatal("accepted response on a different logical channel")
	}
	if l.Err() == nil {
		t.Fatal("ambiguous RPC session remained usable")
	}
}

func TestTCPFramesOnControlLanesAreRejected(t *testing.T) {
	l, p := testLink(t)
	for _, lane := range []uint8{ChannelManagement, ChannelTelemetry, ChannelLogs, ChannelOTA} {
		for _, kind := range []uint8{Open, Opened, Data, CloseFrame, ErrorFrame} {
			p.emit(Frame{Kind: kind, Session: l.session, Channel: lane, Sequence: 1})
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := l.RPC(WithRPCChannel(ctx, lane), "rpc", []byte("still synchronized"))
		cancel()
		if err != nil {
			t.Fatalf("TCP frames corrupted control lane %d: %v", lane, err)
		}
	}
	if l.Stats().Invalid != 20 {
		t.Fatalf("wrong-kind frames not counted: %d", l.Stats().Invalid)
	}
}
