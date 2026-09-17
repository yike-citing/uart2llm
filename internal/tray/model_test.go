package tray

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTraySnapshotFreshnessAndSecretFreeSummary(t *testing.T) {
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"connection":{"connected":true,"paired":true,"port":"COM4","link":{"session":7}},"device":{"session":7,"network":{"connected":true,"address":"192.0.2.2","rssi_dbm":-45},"memory":{"internal_free":81920,"psram_free":8388608,"tasks":32}},"upstream_key":"must-not-appear"}`), &s); err != nil {
		t.Fatal(err)
	}
	s.SampledAt = time.Now()
	s.Device.Transport = "usb_serial_jtag_validation"
	text := s.Summary()
	if !strings.Contains(text, "原生 USB") || strings.Contains(text, "115200 baud") {
		t.Fatal("USB incorrectly presented as baud-limited UART")
	}
	if !strings.Contains(text, "80.0 KiB") || !strings.Contains(text, "192.0.2.2") || strings.Contains(text, "must-not-appear") {
		t.Fatal(text)
	}
	s.SampledAt = time.Now().Add(-6 * time.Second)
	if s.Fresh() || strings.Contains(s.Summary(), "80.0 KiB") {
		t.Fatal("stale telemetry presented as current")
	}
}
func TestPauseLabelAndDisconnectedActions(t *testing.T) {
	s := Snapshot{}
	s.LLM.Paused = true
	for _, item := range s.Menu(false) {
		if item.Action == TogglePause && !strings.Contains(item.Label, "恢复") {
			t.Fatal("wrong pause label")
		}
		if (item.Action == Disconnect || item.Action == Reboot) && item.Enabled {
			t.Fatal("offline action enabled")
		}
	}
	for _, item := range s.Menu(true) {
		if item.Action != 0 && item.Enabled {
			t.Fatal("busy action enabled")
		}
	}
}
