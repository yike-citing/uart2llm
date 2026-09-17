package admin

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

type blockingProbeDevice struct {
	fakeDevice
	statusEntered  chan struct{}
	statusRelease  chan struct{}
	connectEntered chan struct{}
	connectRelease chan struct{}
	connected      bool
	connects       atomic.Int32
	statusCalls    atomic.Int32
}

func (d *blockingProbeDevice) Status() map[string]any {
	if d.statusCalls.Add(1) == 1 && d.statusEntered != nil {
		close(d.statusEntered)
		<-d.statusRelease
	}
	return map[string]any{"connected": d.connected, "paired": d.connected}
}
func (d *blockingProbeDevice) Connect(context.Context, string, int, bool) error {
	d.connects.Add(1)
	if d.connectEntered != nil {
		close(d.connectEntered)
		<-d.connectRelease
	}
	return nil
}

func configureReconnect(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.Config.Update(map[string]json.RawMessage{"serial_port": json.RawMessage(`"COM12"`)}); err != nil {
		t.Fatal(err)
	}
	s.Reconnect.Store(true)
}
func awaitProbe(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("probe did not reach expected state")
	}
}

func TestIdleConnectedProbeDoesNotBlockManagementMutation(t *testing.T) {
	s := testAdmin(t)
	d := &blockingProbeDevice{connected: true, statusEntered: make(chan struct{}), statusRelease: make(chan struct{})}
	s.Device = d
	configureReconnect(t, s)
	done := make(chan struct{})
	go func() { defer close(done); s.ReconnectOnce(context.Background()) }()
	awaitProbe(t, d.statusEntered)
	// Keep Status blocked while a real mutating API operation takes its lock.
	w := callAdmin(s, "POST", "/diagnostics", `{"action":"check_memory"}`, "admin-secret", "")
	close(d.statusRelease)
	awaitProbe(t, done)
	if w.Code != 200 {
		t.Fatalf("idle probe interfered with management: %d %s", w.Code, w.Body.String())
	}
	if d.connects.Load() != 0 {
		t.Fatal("healthy device was reconnected")
	}
}

func TestActualReconnectRemainsSerializedWithManagement(t *testing.T) {
	s := testAdmin(t)
	d := &blockingProbeDevice{connectEntered: make(chan struct{}), connectRelease: make(chan struct{})}
	s.Device = d
	configureReconnect(t, s)
	done := make(chan struct{})
	go func() { defer close(done); s.ReconnectOnce(context.Background()) }()
	awaitProbe(t, d.connectEntered)
	w := callAdmin(s, "POST", "/diagnostics", `{"action":"check_memory"}`, "admin-secret", "")
	s.ReconnectOnce(context.Background())
	close(d.connectRelease)
	awaitProbe(t, done)
	if w.Code != 409 || d.connects.Load() != 1 {
		t.Fatalf("actual reconnect not serialized: status=%d connects=%d", w.Code, d.connects.Load())
	}
}

func TestReconnectRechecksDisabledStateAfterProbe(t *testing.T) {
	s := testAdmin(t)
	d := &blockingProbeDevice{statusEntered: make(chan struct{}), statusRelease: make(chan struct{})}
	s.Device = d
	configureReconnect(t, s)
	done := make(chan struct{})
	go func() { defer close(done); s.ReconnectOnce(context.Background()) }()
	awaitProbe(t, d.statusEntered)
	s.Reconnect.Store(false)
	close(d.statusRelease)
	awaitProbe(t, done)
	if d.connects.Load() != 0 {
		t.Fatal("reconnected after reconnect was disabled during probe")
	}
}
