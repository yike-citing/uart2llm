package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uart2llm/internal/config"
	"uart2llm/internal/credentials"
	"uart2llm/internal/device"
	"uart2llm/internal/gateway"
)

type fakeDevice struct{}

func (fakeDevice) Status() map[string]any                           { return map[string]any{"connected": false, "paired": false} }
func (fakeDevice) Connect(context.Context, string, int, bool) error { return nil }
func (fakeDevice) Disconnect()                                      {}
func (fakeDevice) Pair(context.Context, string, string) error       { return nil }
func (fakeDevice) AutoPair(context.Context) error                   { return credentials.ErrNotFound }
func (fakeDevice) RPC(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (fakeDevice) StageApply(context.Context, map[string]any) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func testAdmin(t *testing.T) *Server {
	c, e := config.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s := New(c, &credentials.Memory{}, fakeDevice{}, &gateway.Gateway{}, "admin-secret")
	s.refresh(context.Background())
	return s
}
func callAdmin(s *Server, method, path, body, token, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:8766/admin/v1"+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestAdminAuthOriginAndSession(t *testing.T) {
	s := testAdmin(t)
	if w := callAdmin(s, "GET", "/state", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := callAdmin(s, "GET", "/state", "", "admin-secret", "http://evil.invalid"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := callAdmin(s, "POST", "/session", `{}`, "", "http://localhost:8766")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure cookie")
	}
	r := httptest.NewRequest("GET", "http://localhost:8766/admin/v1/state", nil)
	r.AddCookie(cookie)
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatal(out.Code)
	}
	cookie.Value += "tampered"
	r = httptest.NewRequest("GET", "http://localhost:8766/admin/v1/state", nil)
	r.AddCookie(cookie)
	out = httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 401 {
		t.Fatal("tampered cookie accepted")
	}
}
func TestDisconnectedStateNotFabricated(t *testing.T) {
	s := testAdmin(t)
	w := callAdmin(s, "GET", "/state", "", "admin-secret", "")
	var v map[string]any
	json.Unmarshal(w.Body.Bytes(), &v)
	if v["connected"] != false || v["paired"] != false || v["device"] != nil {
		t.Fatalf("fabricated state: %v", v)
	}
}
func TestSecretsNeverReturnedByConfig(t *testing.T) {
	s := testAdmin(t)
	w := callAdmin(s, "POST", "/credentials", `{"upstream_key":"sensitive-key"}`, "admin-secret", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = callAdmin(s, "GET", "/config", "", "admin-secret", "")
	if strings.Contains(w.Body.String(), "sensitive-key") {
		t.Fatal("credential leaked")
	}
	if !strings.Contains(w.Body.String(), `"upstream_key_set":true`) {
		t.Fatal("presence not reported")
	}
}
func TestAdminRejectsOversizeAndUnknownFields(t *testing.T) {
	s := testAdmin(t)
	for _, b := range []string{`{"host":{"max_concurrent":9}}`, `{"unexpected":true}`, `{"host":{"serial_port":"` + strings.Repeat("x", 70000) + `"}}`} {
		w := callAdmin(s, "PATCH", "/config", b, "admin-secret", "")
		if w.Code != 400 {
			t.Fatalf("accepted invalid request %d", w.Code)
		}
	}
}

func TestListenerChangeKeepsCurrentManagementReachable(t *testing.T) {
	s := testAdmin(t)
	w := callAdmin(s, "PATCH", "/config", `{"host":{"admin_listen":"localhost:9876"}}`, "admin-secret", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = callAdmin(s, "GET", "/config", "", "admin-secret", "http://localhost:8766")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restart_required":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestConfigurationMaintenanceReservesGateway(t *testing.T) {
	s := testAdmin(t)
	w := callAdmin(s, "PATCH", "/config", `{"device":{"uart.baud":230400}}`, "admin-secret", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.Gateway.Pause() {
		t.Fatal("gateway not reserved during pending confirmation")
	}
	w = callAdmin(s, "POST", "/firmware", "x", "admin-secret", "")
	if w.Code != 409 {
		t.Fatal("OTA accepted during config migration", w.Code)
	}
	w = callAdmin(s, "POST", "/config/confirm", "", "admin-secret", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !s.Gateway.Pause() {
		t.Fatal("gateway not resumed after confirmation")
	}
	s.Gateway.Resume()
}

type stagingDevice struct {
	fakeDevice
	err   error
	calls int
}

func (d *stagingDevice) StageApply(context.Context, map[string]any) (json.RawMessage, error) {
	d.calls++
	return json.RawMessage(`{"pending":true}`), d.err
}

func TestRejectedDeviceStageAllowsImmediateCorrectedPatch(t *testing.T) {
	s := testAdmin(t)
	d := &stagingDevice{err: fmt.Errorf("%w: %w", device.ErrConfigNotApplied, &device.RPCRejection{Method: "config.stage", Code: "device_error", Message: "invalid value"})}
	s.Device = d
	w := callAdmin(s, "PATCH", "/config", `{"device":{"invalid":true}}`, "admin-secret", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid value") {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.configTimer != nil || !s.Gateway.Pause() {
		t.Fatal("known rejected stage retained gateway maintenance")
	}
	s.Gateway.Resume()
	d.err = nil
	w = callAdmin(s, "PATCH", "/config", `{"device":{"wifi.ssid":"corrected"}}`, "admin-secret", "")
	if w.Code != 200 || d.calls != 2 || s.configTimer == nil {
		t.Fatal("corrected patch did not execute immediately", w.Code, d.calls, w.Body.String())
	}
	if s.Gateway.Pause() {
		t.Fatal("successful apply did not reserve gateway until confirmation")
	}
	w = callAdmin(s, "POST", "/config/confirm", "", "admin-secret", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAmbiguousConfigurationFailureKeepsMaintenance(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		errors.New("invalid device JSON response"),
		&device.RPCRejection{Method: "config.apply", Code: "device_error", Message: "NVS staging failed"},
	} {
		t.Run(err.Error(), func(t *testing.T) {
			s := testAdmin(t)
			d := &stagingDevice{err: err}
			s.Device = d
			defer s.endConfigMaintenance()
			w := callAdmin(s, "PATCH", "/config", `{"device":{"wifi.ssid":"new"}}`, "admin-secret", "")
			if w.Code != 400 || s.configTimer == nil {
				t.Fatal("ambiguous failure did not retain recovery lease", w.Code)
			}
			if s.Gateway.Pause() {
				t.Fatal("ambiguous failure resumed gateway")
			}
			w = callAdmin(s, "PATCH", "/config", `{"device":{"wifi.ssid":"retry"}}`, "admin-secret", "")
			if w.Code != 409 || d.calls != 1 {
				t.Fatal("allowed another apply during uncertain transaction", w.Code, d.calls)
			}
		})
	}
}

func TestStateChangesRetainedSeparatelyFromSamples(t *testing.T) {
	s := testAdmin(t)
	first := len(s.Snapshot()["recent_changes"].([]map[string]any))
	s.refresh(context.Background())
	if got := len(s.Snapshot()["recent_changes"].([]map[string]any)); got != first {
		t.Fatal("unchanged sample created events")
	}
	if _, e := s.Config.Update(map[string]json.RawMessage{"idle_timeout_seconds": json.RawMessage(`60`)}); e != nil {
		t.Fatal(e)
	}
	s.refresh(context.Background())
	if got := len(s.Snapshot()["recent_changes"].([]map[string]any)); got != first+1 {
		t.Fatal("configuration transition not recorded")
	}
}

type recoveryDevice struct {
	fakeDevice
	bauds  []int
	paired bool
}

func (d *recoveryDevice) Connect(ctx context.Context, port string, baud int, flow bool) error {
	d.bauds = append(d.bauds, baud)
	if baud != 115200 {
		return errors.New("probe failed")
	}
	return nil
}
func (d *recoveryDevice) AutoPair(context.Context) error { d.paired = true; return nil }
func TestReconnectTriesConfirmedModeThenRecovery(t *testing.T) {
	s := testAdmin(t)
	d := &recoveryDevice{}
	s.Device = d
	_, e := s.Config.Update(map[string]json.RawMessage{"serial_port": json.RawMessage(`"COM12"`), "baud": json.RawMessage(`921600`), "flow_control": json.RawMessage(`true`)})
	if e != nil {
		t.Fatal(e)
	}
	s.Reconnect.Store(true)
	s.ReconnectOnce(context.Background())
	if len(d.bauds) != 2 || d.bauds[0] != 921600 || d.bauds[1] != 115200 || !d.paired {
		t.Fatalf("wrong recovery modes: %v", d.bauds)
	}
}
