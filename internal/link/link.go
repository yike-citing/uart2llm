package link

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var ErrClosed = errors.New("UART session closed")
var ErrCapacity = errors.New("all four TCP channels are in use")

type Options struct {
	RetryInterval time.Duration
	MaxRetries    int
}
type Snapshot struct {
	Session    uint32 `json:"session"`
	Connected  bool   `json:"connected"`
	Received   uint64 `json:"received_frames"`
	Sent       uint64 `json:"sent_frames"`
	Invalid    uint64 `json:"invalid_frames"`
	Retries    uint64 `json:"retries"`
	Duplicates uint64 `json:"duplicate_frames"`
	Active     int    `json:"active_connections"`
}
type channel struct {
	gate     chan struct{}
	sequence uint32
	expected uint32
	ack      chan uint32
	busy     chan uint32
}
type Link struct {
	rw         io.ReadWriteCloser
	opts       Options
	session    uint32
	done       chan struct{}
	once       sync.Once
	ready      atomic.Bool
	hello      chan struct{}
	high       chan Frame
	outgoing   [ChannelCount]chan Frame
	wake       chan struct{}
	channels   [ChannelCount]channel
	mu         sync.Mutex
	conns      [5]*Conn
	rpcMu      sync.Mutex
	rpcID      uint32
	replies    chan Frame
	received   atomic.Uint64
	sent       atomic.Uint64
	invalid    atomic.Uint64
	retries    atomic.Uint64
	duplicates atomic.Uint64
}

func New(rw io.ReadWriteCloser, opts Options) *Link {
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = 500 * time.Millisecond
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 10
	}
	l := &Link{rw: rw, opts: opts, done: make(chan struct{}), hello: make(chan struct{}, 1), high: make(chan Frame, 32), wake: make(chan struct{}, 1), replies: make(chan Frame, 2)}
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	l.session = binary.LittleEndian.Uint32(seed[:])
	if l.session == 0 {
		l.session = 1
	}
	for i := range l.channels {
		l.channels[i] = channel{gate: make(chan struct{}, 1), sequence: 1, expected: 1, ack: make(chan uint32, 2), busy: make(chan uint32, 2)}
		l.channels[i].gate <- struct{}{}
		l.outgoing[i] = make(chan Frame, 1)
	}
	go l.writer()
	go l.reader()
	return l
}
func (l *Link) Close() error {
	var err error
	l.once.Do(func() { l.ready.Store(false); close(l.done); err = l.rw.Close() })
	return err
}
func (l *Link) Done() <-chan struct{} { return l.done }
func (l *Link) Err() error {
	select {
	case <-l.done:
		return ErrClosed
	default:
		return nil
	}
}
func (l *Link) Stats() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.conns {
		if c != nil {
			n++
		}
	}
	return Snapshot{l.session, l.ready.Load(), l.received.Load(), l.sent.Load(), l.invalid.Load(), l.retries.Load(), l.duplicates.Load(), n}
}
func (l *Link) Handshake(ctx context.Context) error {
	if l.ready.Load() {
		return nil
	}
	for i := 0; i <= l.opts.MaxRetries; i++ {
		if err := l.enqueue(ctx, Frame{Kind: Hello, Session: l.session}, true); err != nil {
			return err
		}
		t := time.NewTimer(l.opts.RetryInterval)
		select {
		case <-l.hello:
			t.Stop()
			l.ready.Store(true)
			if err := l.Err(); err != nil {
				l.ready.Store(false)
				return err
			}
			return nil
		case <-l.done:
			t.Stop()
			return ErrClosed
		case <-ctx.Done():
			t.Stop()
			l.Close()
			return ctx.Err()
		case <-t.C:
		}
	}
	l.Close()
	return errors.New("UART HELLO timed out")
}
func (l *Link) enqueue(ctx context.Context, f Frame, priority bool) error {
	q := l.outgoing[f.Channel]
	if priority {
		q = l.high
	}
	select {
	case q <- f:
		select {
		case l.wake <- struct{}{}:
		default:
		}
		return nil
	case <-l.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (l *Link) writer() {
	next := 0
	for {
		var f Frame
		found := false
		select {
		case f = <-l.high:
			found = true
		default:
		}
		if !found {
			for i := 0; i < ChannelCount; i++ {
				j := (next + i) % ChannelCount
				select {
				case f = <-l.outgoing[j]:
					found = true
					next = (j + 1) % ChannelCount
				default:
				}
				if found {
					break
				}
			}
		}
		if !found {
			select {
			case <-l.wake:
				continue
			case <-l.done:
				return
			}
		}
		b, err := Encode(f)
		if err != nil {
			l.Close()
			return
		}
		for len(b) > 0 {
			n, e := l.rw.Write(b)
			if e != nil || n <= 0 {
				l.Close()
				return
			}
			b = b[n:]
		}
		l.sent.Add(1)
	}
}
func (l *Link) reader() {
	buf := make([]byte, 2048)
	encoded := make([]byte, 0, MaxPayload+64)
	discard := false
	for {
		n, err := l.rw.Read(buf)
		for _, b := range buf[:n] {
			if b == 0 {
				if !discard && len(encoded) > 0 {
					f, e := Decode(encoded)
					if e != nil {
						l.invalid.Add(1)
					} else {
						l.accept(f)
					}
				}
				encoded = encoded[:0]
				discard = false
			} else if !discard {
				if len(encoded) >= MaxPayload+64 {
					discard = true
					l.invalid.Add(1)
				} else {
					encoded = append(encoded, b)
				}
			}
		}
		if err != nil {
			l.Close()
			return
		}
	}
}
func (l *Link) accept(f Frame) {
	l.received.Add(1)
	if f.Session != l.session {
		return
	}
	if f.Kind == Abort { // ABORT is host-to-device only, outside sequence space.
		return
	}
	if f.Kind == HelloAck && f.Channel == 0 && f.Sequence == 0 {
		select {
		case l.hello <- struct{}{}:
		default:
		}
		return
	}
	if f.Kind == Ack {
		select {
		case l.channels[f.Channel].ack <- f.Sequence:
		default:
		}
		return
	}
	if f.Kind == Busy {
		select {
		case l.channels[f.Channel].busy <- f.Sequence:
		default:
		}
		return
	}
	if f.Kind < Open || f.Sequence == 0 {
		return
	}
	if (isRPCChannel(f.Channel) && f.Kind != RPCReply) ||
		(!isRPCChannel(f.Channel) && f.Kind != Opened && f.Kind != Data && f.Kind != CloseFrame && f.Kind != ErrorFrame) {
		l.invalid.Add(1)
		return
	}
	ch := &l.channels[f.Channel]
	accepted := false
	if f.Sequence < ch.expected {
		l.duplicates.Add(1)
		accepted = true
	} else if f.Sequence == ch.expected {
		if isRPCChannel(f.Channel) && f.Kind == RPCReply {
			select {
			case l.replies <- f:
				accepted = true
			default:
			}
		} else if f.Channel > 0 && f.Channel <= 4 {
			l.mu.Lock()
			c := l.conns[f.Channel]
			l.mu.Unlock()
			if c == nil {
				accepted = true
			} else {
				select {
				case c.incoming <- f:
					accepted = true
					if f.Kind == CloseFrame {
						c.remoteOnce.Do(func() { close(c.remoteClosed) })
					}
				default:
				}
			}
		}
		if accepted {
			ch.expected++
			if ch.expected == 0 {
				l.Close()
				return
			}
		}
	}
	if accepted {
		if err := l.enqueue(context.Background(), Frame{Kind: Ack, Session: l.session, Channel: f.Channel, Sequence: f.Sequence}, true); err != nil {
			return
		}
	} else if f.Sequence == ch.expected {
		_ = l.enqueue(context.Background(), Frame{Kind: Busy, Session: l.session, Channel: f.Channel, Sequence: f.Sequence}, true)
	}
}
func (l *Link) send(ctx context.Context, id, kind byte, payload []byte) error {
	if !l.ready.Load() {
		return ErrClosed
	}
	if len(payload) > MaxPayload {
		return errors.New("payload exceeds UART maximum")
	}
	ch := &l.channels[id]
	select {
	case <-ch.gate:
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return ErrClosed
	}
	defer func() { ch.gate <- struct{}{} }()
	seq := ch.sequence
	if kind == Open && id > 0 && id <= 4 {
		l.mu.Lock()
		if c := l.conns[id]; c != nil {
			c.openSequence.Store(seq)
		}
		l.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f := Frame{kind, l.session, id, seq, append([]byte(nil), payload...)}
	var originalErr error
	queued := false
	for attempt := 0; attempt <= l.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			l.retries.Add(1)
		}
		err := l.enqueue(ctx, f, false)
		if err != nil && queued && originalErr == nil && ctx.Err() != nil {
			originalErr = ctx.Err()
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ctx = cleanup
			l.startAbort(ctx, id)
			err = l.enqueue(ctx, f, false)
		}
		if err != nil {
			if !queued {
				return err
			}
			l.Close()
			return err
		}
		queued = true
		timer := time.NewTimer(l.opts.RetryInterval)
		busySeen := false
	wait:
		for {
			select {
			case a := <-ch.ack:
				if a == seq {
					timer.Stop()
					ch.sequence++
					if ch.sequence == 0 {
						l.Close()
						return ErrClosed
					}
					return originalErr
				}
			case b := <-ch.busy:
				if b == seq {
					attempt = -1
					// BUSY proves the peer is alive but has no queue credit. Probe
					// at most once per 20 ms instead of waiting for the lost-ACK
					// timer. Repeated BUSY cannot postpone an already armed probe.
					if !busySeen {
						busySeen = true
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						probe := 20 * time.Millisecond
						if l.opts.RetryInterval < probe {
							probe = l.opts.RetryInterval
						}
						timer.Reset(probe)
					}
				}
			case <-timer.C:
				break wait
			case <-ctx.Done():
				if originalErr != nil {
					timer.Stop()
					l.Close()
					return originalErr
				}
				// A cancelled HTTP request must not corrupt other channels. Finish the
				// already transmitted frame before sending this stream's CLOSE. If the
				// peer cannot drain it within three seconds, only a new session is safe.
				originalErr = ctx.Err()
				cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				ctx = cleanup
				l.startAbort(ctx, id)
			case <-l.done:
				timer.Stop()
				return ErrClosed
			}
		}
	}
	l.Close()
	return errors.New("UART acknowledgement timeout")
}

// ABORT asks the peer to cancel a socket operation and drain queued DATA so an
// outstanding DATA frame can be acknowledged before normal reliable CLOSE.
// The OPEN sequence binds it to one connection incarnation, avoiding stale
// aborts affecting a later connection that reuses the same UART channel.
func (l *Link) startAbort(ctx context.Context, id byte) {
	if id == 0 || id > 4 {
		return
	}
	l.mu.Lock()
	c := l.conns[id]
	l.mu.Unlock()
	if c == nil || c.openSequence.Load() == 0 {
		return
	}
	f := Frame{Kind: Abort, Session: l.session, Channel: id, Sequence: c.openSequence.Load()}
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := l.enqueue(ctx, f, true); err != nil {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			case <-l.done:
				return
			}
		}
	}()
}

type rpcChannelKey struct{}

// WithRPCChannel selects an independent reliable control lane. RPC accepts
// management, telemetry, logs and OTA; security handshakes always use management.
// It does not change the shared Security2 cipher's serialized nonce ordering.
func WithRPCChannel(ctx context.Context, channel uint8) context.Context {
	return context.WithValue(ctx, rpcChannelKey{}, channel)
}

func isRPCChannel(channel uint8) bool {
	return channel == ChannelManagement || channel == ChannelTelemetry || channel == ChannelLogs || channel == ChannelOTA
}

func (l *Link) RPC(ctx context.Context, endpoint string, payload []byte) ([]byte, error) {
	lane, _ := ctx.Value(rpcChannelKey{}).(uint8)
	if endpoint == "sec-session" {
		lane = ChannelManagement
	}
	if !isRPCChannel(lane) {
		return nil, errors.New("invalid RPC channel")
	}
	l.rpcMu.Lock()
	defer l.rpcMu.Unlock()
	if len(endpoint) == 0 || len(endpoint) > 255 || len(payload)+5+len(endpoint) > MaxPayload {
		return nil, errors.New("RPC exceeds frame limit")
	}
	l.rpcID++
	id := l.rpcID
	b := make([]byte, 5+len(endpoint)+len(payload))
	binary.LittleEndian.PutUint32(b, id)
	b[4] = byte(len(endpoint))
	copy(b[5:], endpoint)
	copy(b[5+len(endpoint):], payload)
	if err := l.send(ctx, lane, RPCFrame, b); err != nil {
		return nil, err
	}
	for {
		select {
		case f := <-l.replies:
			if f.Channel != lane || len(f.Payload) < 5 || binary.LittleEndian.Uint32(f.Payload) != id {
				l.Close()
				return nil, errors.New("mismatched RPC response")
			}
			if f.Payload[4] != 0 {
				return nil, fmt.Errorf("device RPC: %s", f.Payload[5:])
			}
			return f.Payload[5:], nil
		case <-ctx.Done():
			l.Close()
			return nil, ctx.Err()
		case <-l.done:
			return nil, ErrClosed
		}
	}
}
func (l *Link) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("only TCP tunnels supported")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return nil, err
	}
	if len(address) > 512 {
		return nil, errors.New("address too long")
	}
	l.mu.Lock()
	var c *Conn
	for i := byte(1); i <= 4; i++ {
		if l.conns[i] == nil {
			c = &Conn{link: l, id: i, address: address, incoming: make(chan Frame, 8), closed: make(chan struct{}), remoteClosed: make(chan struct{}), deadlineChanged: make(chan struct{}, 1), writeChanged: make(chan struct{}, 1)}
			l.conns[i] = c
			break
		}
	}
	l.mu.Unlock()
	if c == nil {
		return nil, ErrCapacity
	}
	if err := l.send(ctx, c.id, Open, []byte(address)); err != nil {
		// OPEN may have reached the peer even if the caller was cancelled
		// while waiting for its ACK. Complete the CLOSE barrier before reuse.
		c.Close()
		return nil, err
	}
	select {
	case f := <-c.incoming:
		if f.Kind == Opened {
			return c, nil
		}
		c.Close()
		return nil, fmt.Errorf("TCP open failed: %s", f.Payload)
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	case <-l.done:
		c.release()
		return nil, ErrClosed
	}
}
