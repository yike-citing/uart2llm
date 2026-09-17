package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"uart2llm/internal/config"
	"uart2llm/internal/gateway"
)

type modelDialer struct{ calls atomic.Int32 }

func (d *modelDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.calls.Add(1)
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func TestModelsCheckUsesGatewayAuthenticationAndDialer(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer upstream-test" || r.Header.Get("Cookie") != "" {
			t.Error("model check leaked browser headers or changed target")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"test-model"}]}`))
	}))
	defer up.Close()
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	cfg := config.Default()
	cfg.UpstreamURL = up.URL
	dial := &modelDialer{}
	s := testAdmin(t)
	s.Gateway = &gateway.Gateway{Config: func() config.Host { return cfg }, Dialer: dial, Token: "api-test", Key: func() (string, error) { return "upstream-test", nil }, TLSConfig: &tls.Config{RootCAs: roots}}
	if w := callAdmin(s, "GET", "/models", "", "", ""); w.Code != 401 || dial.calls.Load() != 0 {
		t.Fatal("unauthenticated request reached upstream")
	}
	w := callAdmin(s, "GET", "/models?ignored=yes", "", "admin-secret", "http://localhost:8766")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "test-model") || dial.calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), dial.calls.Load())
	}
	s.Gateway.SetUserPaused(true)
	if w := callAdmin(s, "GET", "/models", "", "admin-secret", ""); w.Code != 503 || dial.calls.Load() != 1 {
		t.Fatal("model check bypassed pause/admission")
	}
}
