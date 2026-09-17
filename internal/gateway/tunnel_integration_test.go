package gateway

// These tests use a framed UART device simulator. Only the simulator can dial
// the controlled TLS server; the gateway's dialer is the real link.Link.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"uart2llm/internal/config"
	"uart2llm/internal/link"
)

type tunnelTestPeer struct {
	serial      net.Conn
	target      string
	done        chan struct{}
	once        sync.Once
	wire        chan []byte
	session     atomic.Uint32
	opens, busy atomic.Int32
	channels    [5]struct {
		in             chan link.Frame
		ack            chan link.Frame
		tx             sync.Mutex
		next, expected uint32
	}
	mu      sync.Mutex
	sockets [5]*tunnelTestSocket
}
type tunnelTestSocket struct {
	tcp          net.Conn
	mu           sync.Mutex
	closed       bool
	openSequence uint32
}

func newTunnelTestPeer(serial net.Conn, target string) *tunnelTestPeer {
	p := &tunnelTestPeer{serial: serial, target: target, done: make(chan struct{}), wire: make(chan []byte, 64)}
	for i := range p.channels {
		p.channels[i].in = make(chan link.Frame, 8)
		p.channels[i].ack = make(chan link.Frame, 8)
		p.channels[i].next = 1
		p.channels[i].expected = 1
		go p.channel(byte(i))
	}
	go func() {
		for {
			select {
			case b := <-p.wire:
				if _, e := serial.Write(b); e != nil {
					p.stop()
					return
				}
			case <-p.done:
				return
			}
		}
	}()
	go p.reader()
	return p
}
func (p *tunnelTestPeer) stop() {
	p.once.Do(func() {
		close(p.done)
		p.serial.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, s := range p.sockets {
			if s != nil {
				s.tcp.Close()
			}
		}
	})
}
func (p *tunnelTestPeer) emit(f link.Frame) bool {
	b, e := link.Encode(f)
	if e != nil {
		return false
	}
	select {
	case p.wire <- b:
		return true
	case <-p.done:
		return false
	}
}
func (p *tunnelTestPeer) send(ch, kind byte, payload []byte) bool {
	c := &p.channels[ch]
	c.tx.Lock()
	defer c.tx.Unlock()
	f := link.Frame{Kind: kind, Session: p.session.Load(), Channel: ch, Sequence: c.next, Payload: append([]byte(nil), payload...)}
	for {
		if !p.emit(f) {
			return false
		}
		timer := time.NewTimer(20 * time.Millisecond)
	again:
		for {
			select {
			case ack := <-c.ack:
				if ack.Sequence == f.Sequence && ack.Kind == link.Ack {
					timer.Stop()
					c.next++
					return true
				}
			case <-timer.C:
				break again
			case <-p.done:
				timer.Stop()
				return false
			}
		}
	}
}
func (p *tunnelTestPeer) reader() {
	b := make([]byte, 2048)
	var encoded []byte
	for {
		n, e := p.serial.Read(b)
		for _, v := range b[:n] {
			if v != 0 {
				encoded = append(encoded, v)
				continue
			}
			f, err := link.Decode(encoded)
			encoded = nil
			if err != nil {
				continue
			}
			if f.Kind == link.Hello {
				p.session.Store(f.Session)
				p.emit(link.Frame{Kind: link.HelloAck, Session: f.Session})
				continue
			}
			if f.Session != p.session.Load() {
				continue
			}
			if f.Kind == link.Abort {
				p.mu.Lock()
				socket := p.sockets[f.Channel]
				p.mu.Unlock()
				if socket != nil && socket.openSequence == f.Sequence {
					socket.tcp.Close()
				}
				continue
			}
			c := &p.channels[f.Channel]
			if f.Kind == link.Ack || f.Kind == link.Busy {
				if f.Kind == link.Busy {
					p.busy.Add(1)
				}
				select {
				case c.ack <- f:
				default:
				}
				continue
			}
			accepted := f.Sequence < c.expected
			if f.Sequence == c.expected {
				select {
				case c.in <- f:
					c.expected++
					accepted = true
				default:
				}
			}
			kind := link.Busy
			if accepted {
				kind = link.Ack
			}
			p.emit(link.Frame{Kind: kind, Session: f.Session, Channel: f.Channel, Sequence: f.Sequence})
		}
		if e != nil {
			p.stop()
			return
		}
	}
}
func (p *tunnelTestPeer) finish(ch byte, s *tunnelTestSocket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.tcp.Close()
		p.send(ch, link.CloseFrame, nil)
	}
}
func (p *tunnelTestPeer) channel(ch byte) {
	for {
		var f link.Frame
		select {
		case f = <-p.channels[ch].in:
		case <-p.done:
			return
		}
		if ch == 0 {
			if f.Kind == link.RPCFrame && len(f.Payload) >= 5 {
				endpointLen := int(f.Payload[4])
				if 5+endpointLen > len(f.Payload) {
					p.stop()
					return
				}
				b := make([]byte, 5)
				copy(b, f.Payload[:4])
				b = append(b, f.Payload[5+endpointLen:]...)
				p.send(0, link.RPCReply, b)
			}
			continue
		}
		p.mu.Lock()
		s := p.sockets[ch]
		p.mu.Unlock()
		switch f.Kind {
		case link.Open:
			// This deliberately unresolvable hostname proves gateway DNS/TCP cannot
			// bypass the serial tunnel. Only this test peer maps it to the local server.
			if string(f.Payload) != "device-resolved.invalid:443" {
				p.send(ch, link.ErrorFrame, []byte("unapproved test target"))
				continue
			}
			tcp, e := net.DialTimeout("tcp", p.target, time.Second)
			if e != nil {
				p.send(ch, link.ErrorFrame, []byte(e.Error()))
				continue
			}
			s = &tunnelTestSocket{tcp: tcp, openSequence: f.Sequence}
			p.mu.Lock()
			p.sockets[ch] = s
			p.mu.Unlock()
			p.opens.Add(1)
			if !p.send(ch, link.Opened, nil) {
				return
			}
			go p.readTCP(ch, s)
		case link.Data:
			if s == nil {
				p.send(ch, link.ErrorFrame, []byte("closed socket"))
				continue
			}
			if _, e := s.tcp.Write(f.Payload); e != nil {
				p.finish(ch, s)
			}
		case link.CloseFrame:
			if s != nil {
				p.finish(ch, s)
				p.mu.Lock()
				if p.sockets[ch] == s {
					p.sockets[ch] = nil
				}
				p.mu.Unlock()
			} else {
				p.send(ch, link.CloseFrame, nil)
			}
		}
	}
}
func (p *tunnelTestPeer) readTCP(ch byte, s *tunnelTestSocket) {
	b := make([]byte, link.DataPayload)
	for {
		n, e := s.tcp.Read(b)
		if n > 0 {
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			ok := p.send(ch, link.Data, b[:n])
			s.mu.Unlock()
			if !ok {
				return
			}
		}
		if e != nil {
			p.finish(ch, s)
			return
		}
	}
}

type tunnelFixture struct {
	gateway         *Gateway
	link            *link.Link
	peer            *tunnelTestPeer
	local, upstream *httptest.Server
	tls             *tls.Config
	client          *http.Client
}

func newTunnelFixture(t *testing.T, handler http.HandlerFunc) *tunnelFixture {
	t.Helper()
	up := httptest.NewTLSServer(handler)
	a, b := net.Pipe()
	peer := newTunnelTestPeer(b, up.Listener.Addr().String())
	l := link.New(a, link.Options{RetryInterval: 20 * time.Millisecond, MaxRetries: 10})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := l.Handshake(ctx); e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	cfg := config.Default()
	cfg.UpstreamURL = "https://device-resolved.invalid/v1"
	cfg.ConnectTimeout = 3
	cfg.HeaderTimeout = 5
	cfg.IdleTimeout = 5
	g := &Gateway{Config: func() config.Host { return cfg }, Dialer: l, Key: func() (string, error) { return "test-upstream-token", nil }, Token: "test-local-token", TLSConfig: tlsConfig}
	local := httptest.NewServer(g)
	f := &tunnelFixture{g, l, peer, local, up, tlsConfig, &http.Client{Timeout: 10 * time.Second}}
	t.Cleanup(func() { local.CloseClientConnections(); l.Close(); peer.stop(); local.Close(); up.Close() })
	return f
}
func (f *tunnelFixture) request(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	r, e := http.NewRequest(method, f.local.URL+path, body)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer test-local-token")
	return r
}

func TestTunnelSSEFlushesBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n"
	f := newTunnelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
			t.Error("wrong upstream credential")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "tunnel-sse")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	})
	r := f.request(t, "POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	response, e := f.client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("X-Request-ID") != "tunnel-sse" {
		t.Fatal("response headers changed")
	}
	received := make(chan string, 1)
	go func() {
		b := make([]byte, len(first))
		_, e := io.ReadFull(response.Body, b)
		if e != nil {
			received <- e.Error()
			return
		}
		received <- string(b)
	}()
	select {
	case got := <-received:
		if got != first {
			t.Fatalf("SSE changed: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE event buffered until completion")
	}
	finish()
	tail, err := io.ReadAll(response.Body)
	if err != nil || string(tail) != "data: [DONE]\n\n" {
		t.Fatalf("SSE completion changed: %q %v", tail, err)
	}
	if f.peer.opens.Load() != 1 {
		t.Fatal("request bypassed UART")
	}
}

func TestTunnelMultipartUploadAndBinaryDownload(t *testing.T) {
	file := make([]byte, 256*1024+17)
	for i := range file {
		file[i] = byte(i * 37)
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, e := writer.CreateFormFile("file", "data.bin")
	if e != nil {
		t.Fatal(e)
	}
	part.Write(file)
	writer.WriteField("purpose", "fine-tune")
	writer.Close()
	wantUpload := append([]byte(nil), upload.Bytes()...)
	received := make(chan []byte, 1)
	f := newTunnelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			received <- b
			if r.Header.Get("Content-Type") != writer.FormDataContentType() {
				t.Error("multipart boundary changed")
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"file-test"}`)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(file)))
		w.Write(file)
	})
	r := f.request(t, "POST", "/v1/files", bytes.NewReader(wantUpload))
	r.Header.Set("Content-Type", writer.FormDataContentType())
	response, e := f.client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("upload status %d", response.StatusCode)
	}
	if got := <-received; !bytes.Equal(got, wantUpload) {
		t.Fatal("multipart bytes modified")
	}
	r = f.request(t, "GET", "/v1/files/file-test/content", nil)
	response, e = f.client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(response.Body)
	response.Body.Close()
	if e != nil || sha256.Sum256(got) != sha256.Sum256(file) {
		t.Fatalf("download checksum mismatch: %v", e)
	}
	if f.peer.opens.Load() != 2 {
		t.Fatal("upload/download bypassed tunnel")
	}
}

func TestTunnelFourRequestsAndFifth503(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := newTunnelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: waiting\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	responses := make(chan *http.Response, 4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		r := f.request(t, "POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
		go func() {
			response, e := f.client.Do(r)
			if e != nil {
				errs <- e
				return
			}
			responses <- response
		}()
	}
	var live []*http.Response
	for i := 0; i < 4; i++ {
		select {
		case response := <-responses:
			if response.StatusCode != 200 {
				t.Fatalf("request %d status %d", i, response.StatusCode)
			}
			live = append(live, response)
		case e := <-errs:
			t.Fatal(e)
		case <-time.After(4 * time.Second):
			t.Fatal("four UART requests did not start")
		}
	}
	defer func() {
		for _, r := range live {
			r.Body.Close()
		}
	}()
	response, e := f.client.Do(f.request(t, "GET", "/v1/models", nil))
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 503 || response.Header.Get("Retry-After") == "" {
		t.Fatalf("fifth returned %d", response.StatusCode)
	}
	if f.peer.opens.Load() != 4 {
		t.Fatal("capacity rejection still dialed a fifth TCP connection")
	}
}

func TestTunnelInterruptedSSEIsNotSuccessfulCompletion(t *testing.T) {
	f := newTunnelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: partial\n\n")
		w.(http.Flusher).Flush()
		c, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		c.Close()
	})
	response, e := f.client.Do(f.request(t, "POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`)))
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(response.Body)
	response.Body.Close()
	if e == nil {
		t.Fatalf("truncated upstream stream reported success: %q", b)
	}
	if strings.Contains(string(b), "[DONE]") {
		t.Fatal("proxy invented successful completion")
	}
}

func TestTunnelManagementSurvivesSaturatedTLSDownload(t *testing.T) {
	f := newTunnelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(8*1024*1024))
		chunk := make([]byte, 4096)
		for i := 0; i < 2048; i++ {
			if _, e := w.Write(chunk); e != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, e := f.link.DialContext(ctx, "tcp", "device-resolved.invalid:443")
	if e != nil {
		t.Fatal(e)
	}
	secured := tls.Client(c, f.tls.Clone())
	defer secured.Close()
	if e = secured.HandshakeContext(ctx); e != nil {
		t.Fatal(e)
	}
	io.WriteString(secured, "GET /v1/files/test/content HTTP/1.1\r\nHost: device-resolved.invalid\r\nConnection: close\r\n\r\n")
	response, e := http.ReadResponse(bufio.NewReader(secured), nil)
	if e != nil {
		t.Fatal(e)
	}
	_ = response // Intentionally do not read the body: the bounded link queue fills.
	deadline := time.Now().Add(2 * time.Second)
	for f.peer.busy.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.peer.busy.Load() == 0 {
		t.Fatal("test did not saturate the receive queue")
	}
	payload := make([]byte, 64)
	binary.LittleEndian.PutUint32(payload, 0xfeedbeef)
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), time.Second)
	defer rpcCancel()
	reply, e := f.link.RPC(rpcCtx, "rpc", payload)
	if e != nil || !bytes.Equal(reply, payload) {
		t.Fatalf("management stalled under backpressure: %v", e)
	}
}

func TestTunnelActiveUploadMayExceedIdleTimeout(t *testing.T) {
	f := newTunnelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"long-upload"}`)
	})
	cfg := f.gateway.Config()
	cfg.IdleTimeout = 1
	f.gateway.Config = func() config.Host { return cfg }
	reader, writer := io.Pipe()
	defer reader.Close()
	finished := make(chan error, 1)
	go func() {
		defer writer.Close()
		chunk := make([]byte, 128)
		for i := 0; i < 16; i++ {
			if _, err := writer.Write(chunk); err != nil {
				finished <- err
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		finished <- nil
	}()
	req := f.request(t, http.MethodPost, "/v1/files", reader)
	req.ContentLength = 16 * 128
	response, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("continuous upload timed out: status=%d body=%s", response.StatusCode, body)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
