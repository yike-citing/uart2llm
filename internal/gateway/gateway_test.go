package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"uart2llm/internal/config"
)

type testDial struct{ net.Dialer }

func testGateway(t *testing.T, handler http.Handler) (*Gateway, *httptest.Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewTLSServer(handler)
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	cfg := config.Default()
	cfg.UpstreamURL = up.URL + "/v1"
	cfg.IdleTimeout = 2
	g := &Gateway{Config: func() config.Host { return cfg }, Dialer: &testDial{}, Key: func() (string, error) { return "upstream-secret", nil }, Token: "local-secret", TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	proxy := httptest.NewServer(g)
	t.Cleanup(up.Close)
	t.Cleanup(proxy.Close)
	return g, proxy, up
}
func request(method, url string, body io.Reader) *http.Request {
	r, _ := http.NewRequest(method, url, body)
	r.Header.Set("Authorization", "Bearer local-secret")
	return r
}
func TestStreamingPreservesEvents(t *testing.T) {
	release := make(chan struct{})
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n"
	rest := "data: {\"choices\":[],\"usage\":{\"total_tokens\":2}}\n\ndata: [DONE]\n\n"
	_, proxy, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("wrong upstream credential")
		}
		if r.URL.RawQuery != "test=yes" {
			t.Error("query lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, rest)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := request("POST", proxy.URL+"/v1/chat/completions?test=yes", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	res, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b := make([]byte, len(first))
	if _, e = io.ReadFull(res.Body, b); e != nil {
		t.Fatal(e)
	}
	if string(b) != first {
		t.Fatal("first event changed")
	}
	close(release)
	b, e = io.ReadAll(res.Body)
	if e != nil || string(b) != rest {
		t.Fatalf("remaining event mismatch: %s %v", b, e)
	}
}
func TestFilesMultipartAndBinary(t *testing.T) {
	data := bytes.Repeat([]byte{0, 255, 128, '\n'}, 256*1024)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, _ := mw.CreateFormFile("file", "payload.bin")
	p.Write(data)
	_ = mw.WriteField("purpose", "user_data")
	mw.Close()
	original := append([]byte(nil), body.Bytes()...)
	_, proxy, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, e := io.ReadAll(r.Body)
		if e != nil || !bytes.Equal(b, original) {
			t.Error("multipart body changed")
		}
		if r.Header.Get("Content-Type") != mw.FormDataContentType() {
			t.Error("boundary changed")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Request-Id", "test-request")
		w.WriteHeader(201)
		w.Write(data)
	}))
	r := request("POST", proxy.URL+"/v1/files", bytes.NewReader(original))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	res, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil || !bytes.Equal(b, data) || res.StatusCode != 201 || res.Header.Get("X-Request-Id") != "test-request" {
		t.Fatal("binary response changed")
	}
}
func TestFourSlotsAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 4)
	_, proxy, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := http.DefaultClient.Do(request("POST", proxy.URL+"/v1/chat/completions", nil).WithContext(ctx))
			if res != nil {
				res.Body.Close()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("four requests did not start")
		}
	}
	res, e := http.DefaultClient.Do(request("GET", proxy.URL+"/v1/models", nil))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 503 || res.Header.Get("Retry-After") == "" {
		t.Fatal("fifth request not rejected")
	}
	cancel()
	wg.Wait()
}
func TestAuthenticationAndPathBoundary(t *testing.T) {
	calls := 0
	_, proxy, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	for _, tc := range []struct {
		path, token string
		want        int
	}{{"/v1/models", "", 401}, {"/v1/responses", "local-secret", 404}, {"/jobs", "local-secret", 404}, {"/v1/filesevil", "local-secret", 404}, {"/v1/files/../responses", "local-secret", 404}} {
		r := request("GET", proxy.URL+tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		res, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s: %d", tc.path, res.StatusCode)
		}
	}
	if calls != 0 {
		t.Fatal("unsupported requests reached upstream")
	}
}
func TestErrorAndRedirectPreserved(t *testing.T) {
	for _, status := range []int{401, 429, 500, 307} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			_, p, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://other.invalid/")
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"upstream"}}`)
			}))
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, e := client.Do(request("POST", p.URL+"/v1/fine_tuning/jobs", nil))
			if e != nil {
				t.Fatal(e)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode != status || !strings.Contains(string(b), "upstream") {
				t.Fatal("upstream error changed")
			}
		})
	}
}
func TestNoReplayAfterConnectionLoss(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	_, p, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		c, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			c.Close()
		}
	}))
	res, e := http.DefaultClient.Do(request("POST", p.URL+"/v1/fine_tuning/jobs", strings.NewReader("{}")))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 || res.StatusCode != 502 {
		t.Fatalf("calls=%d status=%d", calls, res.StatusCode)
	}
}
func TestTLSHostVerification(t *testing.T) {
	g, p, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request should not reach HTTP") }))
	g.TLSConfig = &tls.Config{RootCAs: x509.NewCertPool()}
	res, e := http.DefaultClient.Do(request("GET", p.URL+"/v1/models", nil))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestEscapedModelIdentifierPreserved(t *testing.T) {
	const path = "/v1/models/ft%3Agpt-model%3Aorg"
	_, proxy, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != path {
			t.Errorf("escaped identifier changed: %s", r.URL.EscapedPath())
		}
		w.Write([]byte(`{"id":"ok"}`))
	}))
	res, e := http.DefaultClient.Do(request("GET", proxy.URL+path, nil))
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
}
