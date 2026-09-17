package gateway

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"uart2llm/internal/config"
)

type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}
type Gateway struct {
	Config             func() config.Host
	Dialer             Dialer
	Key                func() (string, error)
	Token              string
	TLSConfig          *tls.Config
	LogFailure         func(string)
	active             atomic.Int32
	maintenance        atomic.Bool
	Requests, Failures atomic.Uint64
	userPaused         atomic.Bool
	metrics            statistics
}

func (g *Gateway) UserPaused() bool          { return g.userPaused.Load() }
func (g *Gateway) SetUserPaused(paused bool) { g.userPaused.Store(paused) }

func (g *Gateway) Active() int32 { return g.active.Load() }
func (g *Gateway) Pause() bool {
	if !g.maintenance.CompareAndSwap(false, true) {
		return false
	}
	if g.active.Load() != 0 {
		g.maintenance.Store(false)
		return false
	}
	return true
}
func (g *Gateway) Resume() { g.maintenance.Store(false) }
func Authorized(header, token string) bool {
	if token == "" || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	v := strings.TrimPrefix(header, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(v), []byte(token)) == 1
}
func Allowed(path string) bool {
	for _, p := range []string{"/v1/chat/completions", "/v1/fine_tuning/jobs", "/v1/files", "/v1/models"} {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "proxy_error", "code": code, "message": message}})
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !Authorized(r.Header.Get("Authorization"), g.Token) {
		WriteError(w, 401, "unauthorized", "local API token required")
		return
	}
	if !Allowed(r.URL.Path) || strings.Contains(r.URL.Path, "\\") || strings.Contains(r.URL.Path, "..") {
		WriteError(w, 404, "unsupported_endpoint", "endpoint is outside the supported API families")
		return
	}
	if r.Header.Get("Upgrade") != "" || r.Method == "CONNECT" {
		WriteError(w, 400, "unsupported_transport", "HTTP and SSE only")
		return
	}
	o := g.metrics.begin(r.Method == "POST" && r.URL.Path == "/v1/chat/completions")
	w = &observedWriter{ResponseWriter: w, o: o}
	defer func() {
		failure := recover()
		g.metrics.finish(o, failure != nil || r.Context().Err() != nil)
		if failure != nil {
			panic(failure)
		}
	}()
	if g.UserPaused() {
		o.rejected = true
		w.Header().Set("Retry-After", "1")
		WriteError(w, 503, "proxy_paused", "proxy is paused; resume from tray or management")
		return
	}
	cfg := g.Config()
	if g.maintenance.Load() {
		o.rejected = true
		WriteError(w, 503, "maintenance", "device configuration or firmware maintenance in progress")
		return
	}
	for {
		n := g.active.Load()
		if n >= int32(cfg.MaxConcurrent) {
			o.rejected = true
			w.Header().Set("Retry-After", "1")
			WriteError(w, 503, "capacity_exceeded", "all upstream request slots are occupied")
			return
		}
		if g.active.CompareAndSwap(n, n+1) {
			break
		}
	}
	defer g.active.Add(-1)
	if g.maintenance.Load() {
		o.rejected = true
		WriteError(w, 503, "maintenance", "device configuration or firmware maintenance in progress")
		return
	}
	g.Requests.Add(1)
	key, e := g.Key()
	if e != nil || key == "" {
		WriteError(w, 503, "upstream_key_missing", "configure an upstream API key")
		return
	}
	target, e := url.Parse(cfg.UpstreamURL)
	if e != nil || target.Scheme != "https" {
		WriteError(w, 500, "invalid_upstream", "invalid upstream configuration")
		return
	}
	// A fresh transport per request disables connection reuse/replay of ambiguous requests.
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ForceAttemptHTTP2: false, MaxConnsPerHost: 1, ResponseHeaderTimeout: time.Duration(cfg.HeaderTimeout) * time.Second, TLSHandshakeTimeout: time.Duration(cfg.ConnectTimeout) * time.Second, TLSClientConfig: g.TLSConfig}
	connections := newRequestConnections(g.Dialer, time.Duration(cfg.ConnectTimeout)*time.Second, time.Duration(cfg.IdleTimeout)*time.Second)
	// net/http can close a non-reusable connection on its read goroutine after
	// ReverseProxy returns. Keep admission occupied through the serial CLOSE
	// barrier so a following turn cannot claim a slot that is still in use.
	defer connections.close()
	tr.DialContext = connections.dial
	defer tr.CloseIdleConnections()
	p := &httputil.ReverseProxy{Transport: tr, FlushInterval: -1, BufferPool: buffers{}, ErrorLog: log.New(failureWriter{g}, "", 0), Rewrite: func(pr *httputil.ProxyRequest) {
		path := pr.In.URL.Path
		base := strings.TrimRight(target.Path, "/")
		if base != "" {
			path = base + strings.TrimPrefix(path, "/v1")
		}
		pr.Out.URL.Scheme = target.Scheme
		pr.Out.URL.Host = target.Host
		pr.Out.URL.Path = path
		pr.Out.URL.RawPath = pr.In.URL.RawPath
		if base != "" && pr.In.URL.RawPath != "" {
			pr.Out.URL.RawPath = strings.TrimRight(target.EscapedPath(), "/") + strings.TrimPrefix(pr.In.URL.EscapedPath(), "/v1")
		}
		pr.Out.Host = target.Host
		pr.Out.Header.Set("Authorization", "Bearer "+key)
		pr.Out.Header.Del("Cookie")
		pr.Out.Header.Del("Proxy-Authorization")
		pr.Out.Header.Del("X-Forwarded-For")
		pr.Out.Header.Del("Forwarded")
		pr.Out.Header.Del("X-Forwarded-Host")
		pr.Out.Header.Del("X-Forwarded-Proto")
		pr.Out.GetBody = nil
	}, ModifyResponse: func(response *http.Response) error {
		if o.chat && response.StatusCode >= 200 && response.StatusCode < 300 && response.Header.Get("Content-Encoding") == "" {
			kind := response.Header.Get("Content-Type")
			if strings.Contains(kind, "text/event-stream") || strings.Contains(kind, "application/json") {
				response.Body = &usageBody{ReadCloser: response.Body, o: o, sse: strings.Contains(kind, "text/event-stream")}
			}
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		g.Failures.Add(1)
		if g.LogFailure != nil {
			g.LogFailure("upstream connection failed before response headers")
		}
		status := 502
		code := "upstream_transport_error"
		if errors.Is(err, context.DeadlineExceeded) {
			status = 504
			code = "upstream_timeout"
		}
		WriteError(w, status, code, "upstream connection failed; inspect local diagnostics")
	}}
	// Limit inactivity at the local client too. There is intentionally no total request timeout.
	rc := http.NewResponseController(w)
	// HTTP/1 must not drain/close the incoming body while the upstream writer
	// is finishing it and an early streaming response is already flowing.
	if e := rc.EnableFullDuplex(); e != nil && !errors.Is(e, http.ErrNotSupported) {
		WriteError(w, 500, "duplex_unavailable", "cannot enable streaming duplex transfer")
		return
	}
	r.Body = &idleBody{ReadCloser: &countedBody{ReadCloser: r.Body, o: o}, rc: rc, idle: time.Duration(cfg.IdleTimeout) * time.Second}
	p.ServeHTTP(&idleWriter{ResponseWriter: w, idle: time.Duration(cfg.IdleTimeout) * time.Second}, r)
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}
func (c *idleConn) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 1024 {
			chunk = chunk[:1024]
		}
		_ = c.Conn.SetDeadline(time.Now().Add(c.idle))
		n, e := c.Conn.Write(chunk)
		total += n
		b = b[n:]
		if e != nil {
			return total, e
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

type failureWriter struct{ g *Gateway }

func (f failureWriter) Write(p []byte) (int, error) {
	f.g.Failures.Add(1)
	if f.g.LogFailure != nil {
		f.g.LogFailure("upstream response stream ended unexpectedly; client response is incomplete")
	}
	return len(p), nil
}

type idleBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	return b.ReadCloser.Read(p)
}

type idleWriter struct {
	http.ResponseWriter
	idle time.Duration
}

func (w *idleWriter) Write(b []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.idle))
	return w.ResponseWriter.Write(b)
}
func (w *idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type buffers struct{}

func (buffers) Get() []byte { return make([]byte, 16*1024) }
func (buffers) Put([]byte)  {}
