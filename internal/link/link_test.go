package link

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type peer struct {
	conn           net.Conn
	out            chan []byte
	done           chan struct{}
	mu             sync.Mutex
	session        uint32
	seq, expected  [ChannelCount]uint32
	openSequence   [5]uint32
	aborted        [5]bool
	abortCount     atomic.Int32
	acks           chan Frame
	dropAck        atomic.Bool
	busyRemaining  atomic.Int32
	dataAckDelay   atomic.Int64
	openAckDelay   atomic.Int64
	dataExecutions atomic.Int32
	echo           atomic.Bool
	rpcChannels    chan byte
	wrongRPCLane   atomic.Bool
}

func newPeer(c net.Conn) *peer {
	p := &peer{conn: c, out: make(chan []byte, 128), done: make(chan struct{}), acks: make(chan Frame, 128), rpcChannels: make(chan byte, 128)}
	p.echo.Store(true)
	for i := range p.seq {
		p.seq[i] = 1
		p.expected[i] = 1
	}
	go func() {
		defer close(p.done)
		b := make([]byte, 2048)
		var encoded []byte
		for {
			n, e := c.Read(b)
			for _, v := range b[:n] {
				if v == 0 {
					f, err := Decode(encoded)
					encoded = nil
					if err == nil {
						p.handle(f)
					}
				} else {
					encoded = append(encoded, v)
				}
			}
			if e != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case b := <-p.out:
				if _, e := c.Write(b); e != nil {
					return
				}
			case <-p.done:
				return
			}
		}
	}()
	return p
}
func (p *peer) emit(f Frame) {
	b, _ := Encode(f)
	select {
	case p.out <- b:
	case <-p.done:
	}
}
func (p *peer) send(ch, kind byte, payload []byte) Frame {
	p.mu.Lock()
	f := Frame{kind, p.session, ch, p.seq[ch], append([]byte(nil), payload...)}
	p.seq[ch]++
	p.mu.Unlock()
	p.emit(f)
	return f
}
func (p *peer) handle(f Frame) {
	if f.Kind == Hello {
		p.mu.Lock()
		p.session = f.Session
		p.mu.Unlock()
		p.emit(Frame{Kind: HelloAck, Session: f.Session})
		return
	}
	if f.Kind == Abort {
		p.mu.Lock()
		matching := f.Channel > 0 && f.Channel <= 4 && f.Session == p.session && p.openSequence[f.Channel] == f.Sequence
		if matching {
			p.aborted[f.Channel] = true
		}
		p.mu.Unlock()
		if matching {
			p.abortCount.Add(1)
			p.busyRemaining.Store(0)
		}
		return
	}
	if f.Kind == Ack || f.Kind == Busy {
		select {
		case p.acks <- f:
		default:
		}
		return
	}
	if f.Kind == Data && p.busyRemaining.Load() > 0 {
		p.busyRemaining.Add(-1)
		p.emit(Frame{Kind: Busy, Session: f.Session, Channel: f.Channel, Sequence: f.Sequence})
		return
	}
	p.mu.Lock()
	expected := p.expected[f.Channel]
	if f.Sequence == expected {
		p.expected[f.Channel]++
	}
	p.mu.Unlock()
	if f.Sequence > expected {
		return
	}
	drop := f.Kind == Data && p.dropAck.CompareAndSwap(true, false)
	if !drop {
		ack := Frame{Kind: Ack, Session: f.Session, Channel: f.Channel, Sequence: f.Sequence}
		delay := time.Duration(0)
		if f.Kind == Data {
			delay = time.Duration(p.dataAckDelay.Load())
		}
		if f.Kind == Open {
			delay = time.Duration(p.openAckDelay.Load())
		}
		if delay > 0 {
			time.AfterFunc(delay, func() { p.emit(ack) })
		} else {
			p.emit(ack)
		}
	}
	if f.Sequence < expected {
		return
	}
	switch f.Kind {
	case Open:
		p.mu.Lock()
		p.openSequence[f.Channel] = f.Sequence
		p.aborted[f.Channel] = false
		p.mu.Unlock()
		p.send(f.Channel, Opened, nil)
	case Data:
		p.mu.Lock()
		aborted := p.aborted[f.Channel]
		p.mu.Unlock()
		if aborted {
			return
		}
		p.dataExecutions.Add(1)
		if p.echo.Load() {
			p.send(f.Channel, Data, f.Payload)
		}
	case CloseFrame:
		p.send(f.Channel, CloseFrame, nil)
	case RPCFrame:
		p.rpcChannels <- f.Channel
		b := make([]byte, 5)
		copy(b, f.Payload[:4])
		n := int(f.Payload[4])
		b = append(b, f.Payload[5+n:]...)
		lane := f.Channel
		if p.wrongRPCLane.Load() {
			lane = ChannelManagement
		}
		p.send(lane, RPCReply, b)
	}
}
func testLink(t *testing.T) (*Link, *peer) {
	t.Helper()
	a, b := net.Pipe()
	p := newPeer(b)
	l := New(a, Options{RetryInterval: 10 * time.Millisecond, MaxRetries: 5})
	t.Cleanup(func() { l.Close(); b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := l.Handshake(ctx); e != nil {
		t.Fatal(e)
	}
	return l, p
}
func dial(t *testing.T, l *Link) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, e := l.DialContext(ctx, "tcp", "api.example.test:443")
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestFrameRoundTripAndCorruption(t *testing.T) {
	for _, n := range []int{0, 1, 253, 254, 255, 1024, 4096} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i)
		}
		encoded, e := Encode(Frame{Data, 17, 4, 321, b})
		if e != nil {
			t.Fatal(e)
		}
		f, e := Decode(encoded[:len(encoded)-1])
		if e != nil || !bytes.Equal(f.Payload, b) || f.Sequence != 321 {
			t.Fatalf("roundtrip %d: %v", n, e)
		}
		encoded[len(encoded)-2] ^= 1
		if _, e = Decode(encoded[:len(encoded)-1]); e == nil {
			t.Fatal("CRC corruption accepted")
		}
	}
}
func TestFourStreamsRPCAndFifthRejected(t *testing.T) {
	l, _ := testLink(t)
	var conns []net.Conn
	for i := 0; i < 4; i++ {
		conns = append(conns, dial(t, l))
	}
	if _, e := l.DialContext(context.Background(), "tcp", "test:443"); !errors.Is(e, ErrCapacity) {
		t.Fatalf("fifth: %v", e)
	}
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			defer c.Close()
			want := bytes.Repeat([]byte{byte(i + 1)}, 7000)
			if _, e := c.Write(want); e != nil {
				t.Error(e)
				return
			}
			got := make([]byte, len(want))
			if _, e := io.ReadFull(c, got); e != nil || !bytes.Equal(got, want) {
				t.Errorf("stream mismatch: %v", e)
			}
		}(i, c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, e := l.RPC(ctx, "rpc", []byte("management"))
	if e != nil || string(got) != "management" {
		t.Fatalf("RPC %s %v", got, e)
	}
	wg.Wait()
	c := dial(t, l)
	c.Close()
	if l.Err() != nil {
		t.Fatal("slot reuse broke link")
	}
}
func TestLostAckDoesNotDuplicateTCPData(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	defer c.Close()
	p.dropAck.Store(true)
	if _, e := c.Write([]byte("only once")); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 9)
	if _, e := io.ReadFull(c, b); e != nil {
		t.Fatal(e)
	}
	if p.dataExecutions.Load() != 1 || l.Stats().Retries == 0 {
		t.Fatal("frame was duplicated or not retried")
	}
}
func TestDuplicateAndCorruptReceive(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	defer c.Close()
	f := p.send(1, Data, []byte("once"))
	p.emit(f)
	bad, _ := Encode(Frame{Data, f.Session, 1, f.Sequence + 1, []byte("bad")})
	bad[len(bad)-2] ^= 1
	p.out <- bad
	p.send(1, Data, []byte("twice"))
	b := make([]byte, 9)
	if _, e := io.ReadFull(c, b); e != nil || string(b) != "oncetwice" {
		t.Fatalf("data=%s error=%v", b, e)
	}
	if l.Stats().Invalid == 0 || l.Stats().Duplicates == 0 {
		t.Fatal("invalid or duplicate not counted")
	}
}
func waitAck(t *testing.T, p *peer, f Frame, kind byte) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case a := <-p.acks:
			if a.Channel == f.Channel && a.Sequence == f.Sequence && a.Kind == kind {
				return
			}
		case <-timer.C:
			t.Fatalf("missing kind %d for seq %d", kind, f.Sequence)
		}
	}
}
func TestFullReceiveQueueDoesNotBlockManagement(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	defer c.Close()
	for i := 0; i < 8; i++ {
		f := p.send(1, Data, []byte{byte(i)})
		waitAck(t, p, f, Ack)
	}
	blocked := p.send(1, Data, []byte{8})
	waitAck(t, p, blocked, Busy)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if b, e := l.RPC(ctx, "rpc", []byte("still alive")); e != nil || string(b) != "still alive" {
		t.Fatalf("management blocked: %v", e)
	}
	var b [1]byte
	if _, e := c.Read(b[:]); e != nil {
		t.Fatal(e)
	}
	p.emit(blocked)
	waitAck(t, p, blocked, Ack)
	if cap(c.(*Conn).incoming) != 8 {
		t.Fatal("unbounded queue")
	}
}
func TestSessionLossAndReadDeadline(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	var b [1]byte
	if _, e := c.Read(b[:]); e == nil {
		t.Fatal("missing read timeout")
	}
	c.SetReadDeadline(time.Time{})
	p.conn.Close()
	select {
	case <-l.Done():
	case <-time.After(time.Second):
		t.Fatal("disconnect undetected")
	}
	if _, e := c.Read(b[:]); e == nil {
		t.Fatal("old stream survived reset")
	}
}
func TestStaleSessionFramesIgnored(t *testing.T) {
	l, p := testLink(t)
	c := dial(t, l)
	defer c.Close()
	p.emit(Frame{Data, l.session + 1, 1, 2, []byte("stale")})
	p.send(1, Data, []byte("new"))
	b := make([]byte, 3)
	if _, e := io.ReadFull(c, b); e != nil || string(b) != "new" {
		t.Fatal("accepted stale session")
	}
}
func FuzzFrameDecoder(f *testing.F) {
	b, _ := Encode(Frame{Data, 1, 1, 1, []byte("sample")})
	f.Add(b[:len(b)-1])
	f.Add([]byte{255, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		frame, e := Decode(b)
		if e == nil {
			encoded, e := Encode(frame)
			if e != nil {
				t.Fatal(e)
			}
			again, e := Decode(encoded[:len(encoded)-1])
			if e != nil || !bytes.Equal(again.Payload, frame.Payload) {
				t.Fatal("round trip")
			}
		}
	})
}
func TestRPCFrameEnvelope(t *testing.T) {
	l, _ := testLink(t)
	for _, endpoint := range []string{"sec-session", "rpc"} {
		payload := make([]byte, 512)
		binary.LittleEndian.PutUint32(payload, 0x1234)
		out, e := l.RPC(context.Background(), endpoint, payload)
		if e != nil || !bytes.Equal(out, payload) {
			t.Fatal("RPC payload altered")
		}
	}
}
