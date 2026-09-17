package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type scriptedDevice struct{ calls int }

func (d *scriptedDevice) Status() map[string]any {
	return map[string]any{"connected": true, "paired": true, "link": map[string]any{"session": 17}}
}

func (d *scriptedDevice) RPC(ctx context.Context, method string, params any) (json.RawMessage, error) {
	d.calls++
	if d.calls == 1 {
		return json.RawMessage(`{"session":17,"sample_time_ms":1000,"network":{"connected":true,"reconnects":4}}`), nil
	}
	return json.RawMessage(`{"session":17,"sample_time_ms":2000,"network":{"connected":true,"reconnects":5}}`), nil
}
func TestWaitNetworkRejectsCachedConnectedSnapshot(t *testing.T) {
	d := &scriptedDevice{}
	var r result
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := waitNetwork(ctx, d, &r, 4, true)
	if err != nil || s.Network.Reconnects != 5 || d.calls != 2 {
		t.Fatalf("accepted stale pre-fault Wi-Fi state: %+v, %v", s, err)
	}
}

type staleSessionDevice struct{ scriptedDevice }

func (d *staleSessionDevice) RPC(ctx context.Context, method string, params any) (json.RawMessage, error) {
	d.calls++
	if d.calls == 1 {
		return json.RawMessage(`{"session":99,"sample_time_ms":800,"network":{"connected":true,"reconnects":2},"link":{"resets":7}}`), nil
	}
	return json.RawMessage(`{"session":17,"sample_time_ms":1800,"network":{"connected":true,"reconnects":2},"link":{"resets":8}}`), nil
}
func TestInitialSnapshotWaitsForCurrentHelloSession(t *testing.T) {
	d := &staleSessionDevice{}
	var r result
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := waitNetwork(ctx, d, &r, 0, false)
	if err != nil || s.Session != 17 || s.Link.Resets != 8 || d.calls != 2 || r.StaleSessionSamples != 1 {
		t.Fatalf("previous HELLO session accepted as baseline: session=%d resets=%d calls=%d ignored=%d err=%v", s.Session, s.Link.Resets, d.calls, r.StaleSessionSamples, err)
	}
}
func TestConfigurationVerificationIncludesAllViews(t *testing.T) {
	original := configSnapshot{Revision: 2, Desired: map[string]any{"wifi.hostname": "original"}, Effective: map[string]any{"wifi.hostname": "original"}, Persisted: map[string]any{"wifi.hostname": "original"}}
	candidate := original
	candidate.Effective = map[string]any{"wifi.hostname": "changed"}
	if sameConfig(original, candidate) {
		t.Fatal("missed changed effective configuration")
	}
	candidate = original
	candidate.Pending = true
	if sameConfig(original, candidate) {
		t.Fatal("missed pending transaction")
	}
	candidate = original
	if !sameConfig(original, candidate) {
		t.Fatal("unchanged config rejected")
	}
	// Public result carries booleans only; complete configuration stays in memory.
	b, _ := json.Marshal(result{ConfigUnchanged: true})
	if strings.Contains(string(b), "hostname") || strings.Contains(string(b), "password") {
		t.Fatal("configuration leaked to report")
	}
}
