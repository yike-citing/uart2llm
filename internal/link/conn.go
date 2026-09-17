package link

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type Conn struct {
	link                          *Link
	id                            byte
	address                       string
	incoming                      chan Frame
	closed                        chan struct{}
	remoteClosed                  chan struct{}
	once, remoteOnce              sync.Once
	readMu                        sync.Mutex
	writeMu                       sync.Mutex
	mu                            sync.Mutex
	buffer                        []byte
	readDeadline, writeDeadline   time.Time
	deadlineChanged, writeChanged chan struct{}
	eof                           bool
	openSequence                  atomic.Uint32
}
type address string

func (a address) Network() string    { return "uart-tcp" }
func (a address) String() string     { return string(a) }
func (c *Conn) LocalAddr() net.Addr  { return address("uart") }
func (c *Conn) RemoteAddr() net.Addr { return address(c.address) }
func (c *Conn) release() {
	c.link.mu.Lock()
	if c.link.conns[c.id] == c {
		c.link.conns[c.id] = nil
	}
	c.link.mu.Unlock()
}
func (c *Conn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c.link.startAbort(ctx, c.id)
		go func() {
			for {
				select {
				case <-c.incoming:
				case <-c.remoteClosed:
					return
				case <-c.link.done:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
		if err := c.link.send(ctx, c.id, CloseFrame, nil); err == nil {
			select {
			case <-c.remoteClosed:
			case <-c.link.done:
			case <-ctx.Done():
				c.link.Close()
			}
		} else {
			// Reusing this slot without a completed CLOSE would allow old TCP
			// bytes to enter a later connection, so fail the ambiguous session.
			c.link.Close()
		}
		c.release()
	})
	return nil
}
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	c.mu.Unlock()
	c.notify()
	return nil
}
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	c.notify()
	return nil
}
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	c.notify()
	return nil
}
func (c *Conn) notify() {
	select {
	case c.deadlineChanged <- struct{}{}:
	default:
	}
	select {
	case c.writeChanged <- struct{}{}:
	default:
	}
}
func (c *Conn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(c.buffer) > 0 {
			n := copy(p, c.buffer)
			c.buffer = c.buffer[n:]
			return n, nil
		}
		if c.eof {
			return 0, io.EOF
		}
		c.mu.Lock()
		deadline := c.readDeadline
		c.mu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}
		var f Frame
		select {
		case f = <-c.incoming:
		case <-c.closed:
			if timer != nil {
				timer.Stop()
			}
			return 0, net.ErrClosed
		case <-c.link.done:
			if timer != nil {
				timer.Stop()
			}
			return 0, ErrClosed
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-c.deadlineChanged:
			if timer != nil {
				timer.Stop()
			}
			continue
		}
		if timer != nil {
			timer.Stop()
		}
		switch f.Kind {
		case Data:
			c.buffer = f.Payload
		case CloseFrame:
			c.eof = true
		case ErrorFrame:
			c.eof = true
			return 0, errors.New(string(f.Payload))
		default:
			return 0, errors.New("unexpected TCP frame")
		}
	}
}

// writeContext tracks deadline changes even while a frame is waiting for credit.
func (c *Conn) writeContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c.mu.Lock()
			deadline := c.writeDeadline
			c.mu.Unlock()
			var timer *time.Timer
			var timeout <-chan time.Time
			if !deadline.IsZero() {
				timer = time.NewTimer(time.Until(deadline))
				timeout = timer.C
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-c.closed:
				if timer != nil {
					timer.Stop()
				}
				cancel()
				return
			case <-timeout:
				cancel()
				return
			case <-c.writeChanged:
				if timer != nil {
					timer.Stop()
				}
			}
		}
	}()
	return ctx, cancel
}
func (c *Conn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	expired := !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline)
	c.mu.Unlock()
	if expired {
		return 0, os.ErrDeadlineExceeded
	}
	ctx, cancel := c.writeContext()
	defer cancel()
	total := 0
	for len(p) > 0 {
		select {
		case <-c.closed:
			return total, net.ErrClosed
		default:
		}
		n := len(p)
		if n > DataPayload {
			n = DataPayload
		}
		err := c.link.send(ctx, c.id, Data, p[:n])
		if err != nil {
			if errors.Is(err, context.Canceled) {
				select {
				case <-c.closed:
					err = net.ErrClosed
				default:
					err = os.ErrDeadlineExceeded
				}
			}
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
