// link-recovery owns the serial port and deliberately reconnects device Wi-Fi.
// Its echo traffic is bounded, non-secret, and reaches the LAN through ESP32.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"time"

	"uart2llm/internal/config"
	"uart2llm/internal/credentials"
	"uart2llm/internal/device"
)

type deviceAPI interface {
	RPC(context.Context, string, any) (json.RawMessage, error)
	Status() map[string]any
}
type snapshot struct {
	Session  uint32 `json:"session"`
	SampleMS int64  `json:"sample_time_ms"`
	Link     struct {
		Resets uint64 `json:"resets"`
	} `json:"link"`
	Network struct {
		Connected   bool   `json:"connected"`
		Reconnects  uint64 `json:"reconnects"`
		Connections []struct {
			Channel int    `json:"channel"`
			Open    bool   `json:"open"`
			RX      uint64 `json:"rx_bytes"`
		} `json:"connections"`
	} `json:"network"`
}
type configSnapshot struct {
	Revision                      uint64 `json:"revision"`
	Pending                       bool   `json:"pending"`
	Desired, Effective, Persisted map[string]any
}
type result struct {
	Started                      time.Time `json:"started"`
	Fault                        string    `json:"fault"`
	AttemptedSlowBytes           int       `json:"attempted_slow_bytes"`
	SlowWrittenBytes             int       `json:"slow_written_bytes"`
	BackpressureObserved         bool      `json:"backpressure_observed"`
	IndependentEcho              bool      `json:"independent_echo"`
	RecoveryEcho                 bool      `json:"recovery_echo"`
	EchoSHA256                   string    `json:"echo_sha256,omitempty"`
	StateSamples                 int       `json:"state_samples"`
	MaxStateMS                   float64   `json:"maximum_state_ms"`
	ReconnectAccepted            bool      `json:"reconnect_accepted"`
	ReconnectCounterAdvanced     bool      `json:"reconnect_counter_advanced"`
	OldConnectionEnded           bool      `json:"old_connection_ended"`
	OldConnectionTimeout         bool      `json:"old_connection_timeout"`
	OldConnectionUnexpectedBytes int       `json:"old_connection_unexpected_bytes"`
	SlowCloseMS                  float64   `json:"slow_close_ms"`
	SameSerialSession            bool      `json:"same_serial_session"`
	StaleSessionSamples          int       `json:"stale_session_samples_ignored"`
	HostStartSession             uint32    `json:"host_start_session"`
	HostEndSession               uint32    `json:"host_end_session"`
	HostCleanupSession           uint32    `json:"host_cleanup_session"`
	HostEndConnected             bool      `json:"host_end_connected"`
	DeviceStartSession           uint32    `json:"device_start_session"`
	DeviceEndSession             uint32    `json:"device_end_session"`
	DeviceCleanupSession         uint32    `json:"device_cleanup_session"`
	DeviceStartSampleMS          int64     `json:"device_start_sample_ms"`
	DeviceEndSampleMS            int64     `json:"device_end_sample_ms"`
	DeviceCleanupSampleMS        int64     `json:"device_cleanup_sample_ms"`
	DeviceStartResets            uint64    `json:"device_start_resets"`
	DeviceEndResets              uint64    `json:"device_end_resets"`
	DeviceCleanupResets          uint64    `json:"device_cleanup_resets"`
	CleanupReconnected           bool      `json:"cleanup_reconnected"`
	FinallyNetworkReady          bool      `json:"finally_network_ready"`
	ConfigUnchanged              bool      `json:"config_unchanged"`
	Passed                       bool      `json:"passed"`
	Error                        string    `json:"error,omitempty"`
	CleanupError                 string    `json:"cleanup_error,omitempty"`
}

func call(ctx context.Context, m deviceAPI, method string, params any, out any) error {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := m.RPC(probe, method, params)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func currentLink(m deviceAPI) (uint32, bool) {
	var s struct {
		Connected bool `json:"connected"`
		Paired    bool `json:"paired"`
		Link      struct {
			Session uint32 `json:"session"`
		} `json:"link"`
	}
	b, _ := json.Marshal(m.Status())
	_ = json.Unmarshal(b, &s)
	return s.Link.Session, s.Connected && s.Paired
}
func state(ctx context.Context, m deviceAPI, r *result) (snapshot, error) {
	for {
		var s snapshot
		start := time.Now()
		err := call(ctx, m, "state.get", map[string]any{}, &s)
		r.StateSamples++
		r.MaxStateMS = max(r.MaxStateMS, float64(time.Since(start).Microseconds())/1000)
		if err != nil {
			return s, err
		}
		host, connected := currentLink(m)
		if !connected || host == 0 {
			return s, errors.New("host serial session disconnected while sampling")
		}
		// state.get may cache the previous HELLO session for up to one second.
		// Never compare or accept network data belonging to another session.
		if s.Session == host && s.SampleMS > 0 {
			return s, nil
		}
		r.StaleSessionSamples++
		if err = waitTick(ctx); err != nil {
			return s, err
		}
	}
}
func waitTick(ctx context.Context) error {
	select {
	case <-time.After(250 * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func waitNetwork(ctx context.Context, m deviceAPI, r *result, previous uint64, requireAdvance bool) (snapshot, error) {
	for {
		s, err := state(ctx, m, r)
		if err != nil {
			return s, err
		}
		if s.Network.Connected && (!requireAdvance || s.Network.Reconnects > previous) {
			return s, nil
		}
		if err = waitTick(ctx); err != nil {
			return s, err
		}
	}
}
func configuration(ctx context.Context, m deviceAPI) (configSnapshot, error) {
	var c configSnapshot
	err := call(ctx, m, "config.get", map[string]any{}, &c)
	return c, err
}
func sameConfig(a, b configSnapshot) bool {
	return a.Revision == b.Revision && a.Pending == b.Pending &&
		reflect.DeepEqual(a.Desired, b.Desired) && reflect.DeepEqual(a.Effective, b.Effective) && reflect.DeepEqual(a.Persisted, b.Persisted)
}
func echo(ctx context.Context, m *device.Manager, target string) (bool, string, error) {
	c, err := m.DialContext(ctx, "tcp", target)
	if err != nil {
		return false, "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i*37 + 19)
	}
	written := make(chan error, 1)
	go func() { _, e := io.Copy(c, bytes.NewReader(data)); written <- e }()
	received := make([]byte, len(data))
	_, readErr := io.ReadFull(c, received)
	if readErr != nil {
		_ = c.Close()
	}
	writeErr := <-written
	digest := sha256.Sum256(received)
	return readErr == nil && writeErr == nil && bytes.Equal(data, received), hex.EncodeToString(digest[:]), errors.Join(readErr, writeErr)
}
func highestOpenChannel(s snapshot) int {
	n := 0
	for _, c := range s.Network.Connections {
		if c.Open {
			n = max(n, c.Channel)
		}
	}
	return n
}
func received(s snapshot, channel int) uint64 {
	for _, c := range s.Network.Connections {
		if c.Channel == channel {
			return c.RX
		}
	}
	return 0
}

func execute(ctx context.Context, r *result, port, target string) (err error) {
	store, err := credentials.New(config.DefaultDir())
	if err != nil {
		return err
	}
	m := device.New(store)
	defer m.Disconnect()
	connect := func(ctx context.Context) error {
		if e := m.Connect(ctx, port, 115200, false); e != nil {
			return e
		}
		return m.AutoPair(ctx)
	}
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = connect(setup); err != nil {
		return err
	}
	r.HostStartSession, _ = currentLink(m)
	original, err := configuration(setup, m)
	if err != nil {
		return err
	}
	if original.Pending {
		return errors.New("refusing diagnostic while a user configuration transaction is pending")
	}
	var slow, old net.Conn
	var writes chan int
	// Cleanup owns a fresh context, including on timeout or Ctrl+C. It never
	// changes configuration and verifies the complete redacted snapshot in memory.
	defer func() {
		if slow != nil {
			_ = slow.Close()
		}
		if old != nil {
			_ = old.Close()
		}
		if writes != nil {
			select {
			case r.SlowWrittenBytes = <-writes:
			case <-time.After(5 * time.Second):
				r.CleanupError = "slow writer did not exit"
			}
		}
		r.HostEndSession, r.HostEndConnected = currentLink(m)
		cleanup, done := context.WithTimeout(context.Background(), 45*time.Second)
		defer done()
		if ok, _ := m.Status()["paired"].(bool); !ok {
			r.CleanupReconnected = true
			if e := connect(cleanup); e != nil {
				r.CleanupError = "cannot reconnect for final verification"
				return
			}
		}
		finalState, e := waitNetwork(cleanup, m, r, 0, false)
		if e != nil {
			r.CleanupError = "Wi-Fi did not recover during cleanup"
			return
		}
		r.HostCleanupSession, _ = currentLink(m)
		r.DeviceCleanupSession = finalState.Session
		r.DeviceCleanupSampleMS = finalState.SampleMS
		r.DeviceCleanupResets = finalState.Link.Resets
		r.SameSerialSession = r.HostStartSession != 0 && r.HostEndConnected && !r.CleanupReconnected &&
			r.HostStartSession == r.HostEndSession && r.HostEndSession == r.HostCleanupSession &&
			r.HostStartSession == r.DeviceStartSession && r.DeviceStartSession == r.DeviceEndSession &&
			r.DeviceEndSession == r.DeviceCleanupSession && r.DeviceStartResets == r.DeviceEndResets &&
			r.DeviceEndResets == r.DeviceCleanupResets
		r.FinallyNetworkReady = true
		final, e := configuration(cleanup, m)
		if e != nil {
			r.CleanupError = "cannot verify final configuration"
			return
		}
		r.ConfigUnchanged = sameConfig(original, final)
		if !r.ConfigUnchanged {
			r.CleanupError = "configuration changed during diagnostic; no configuration mutation was requested by this tool"
		}
	}()
	before, err := waitNetwork(setup, m, r, 0, false)
	if err != nil {
		return err
	}
	r.DeviceStartSession = before.Session
	r.DeviceStartSampleMS = before.SampleMS
	r.DeviceStartResets = before.Link.Resets
	slow, err = m.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	// state.get is sampled once per second; diagnostics snapshot is immediate.
	var fresh snapshot
	if err = call(ctx, m, "diagnostics", map[string]any{"action": "snapshot"}, &fresh); err != nil {
		return err
	}
	if fresh.Session != r.HostStartSession || fresh.SampleMS < before.SampleMS {
		return errors.New("immediate device snapshot belongs to a different or older session")
	}
	channel := highestOpenChannel(fresh)
	if channel == 0 {
		return errors.New("new slow connection not observable")
	}
	initialBytes := received(fresh, channel)
	writes = make(chan int, 1)
	go func(c net.Conn) {
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		payload := make([]byte, 64*1024)
		for i := range payload {
			payload[i] = byte(i*13 + 7)
		}
		n, _ := c.Write(payload)
		writes <- n
	}(slow)
	r.AttemptedSlowBytes = 64 * 1024
	// Never read slow. A rising then stable ESP TCP receive count beyond the
	// host's 8 KiB queue proves backpressure reached ESP with an echo server.
	congestion, stopCongestion := context.WithTimeout(ctx, 12*time.Second)
	var previous uint64
	var previousSample int64
	stable := 0
	for {
		s, e := state(congestion, m, r)
		if e != nil {
			stopCongestion()
			return e
		}
		value := received(s, channel)
		if s.SampleMS != previousSample {
			if value-initialBytes >= 8*1024 && value == previous {
				stable++
			} else {
				stable = 0
			}
			previous, previousSample = value, s.SampleMS
		}
		if stable >= 1 {
			r.BackpressureObserved = true
			break
		}
		if e = waitTick(congestion); e != nil {
			stopCongestion()
			return errors.New("did not observe bounded slow-consumer backpressure")
		}
	}
	stopCongestion()
	r.IndependentEcho, _, err = echo(ctx, m, target)
	if err != nil || !r.IndependentEcho {
		return errors.New("independent stream failed during backpressure")
	}
	old, err = m.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	oldEnded := make(chan struct {
		n   int
		err error
	}, 1)
	_ = old.SetReadDeadline(time.Now().Add(12 * time.Second))
	go func(c net.Conn) {
		var b [1]byte
		n, e := c.Read(b[:])
		oldEnded <- struct {
			n   int
			err error
		}{n, e}
	}(old)
	var accepted struct {
		Accepted bool `json:"accepted"`
	}
	if err = call(ctx, m, "diagnostics", map[string]any{"action": "wifi_reconnect"}, &accepted); err != nil {
		return err
	}
	r.ReconnectAccepted = accepted.Accepted
	recovering, stopRecovery := context.WithTimeout(ctx, 20*time.Second)
	after, waitErr := waitNetwork(recovering, m, r, before.Network.Reconnects, true)
	stopRecovery()
	if waitErr != nil {
		return waitErr
	}
	r.ReconnectCounterAdvanced = after.Network.Reconnects > before.Network.Reconnects
	r.DeviceEndSession = after.Session
	r.DeviceEndSampleMS = after.SampleMS
	r.DeviceEndResets = after.Link.Resets
	select {
	case v := <-oldEnded:
		r.OldConnectionUnexpectedBytes = v.n
		var timeout net.Error
		r.OldConnectionTimeout = errors.As(v.err, &timeout) && timeout.Timeout()
		r.OldConnectionEnded = v.err != nil && !r.OldConnectionTimeout && v.n == 0
	case <-ctx.Done():
		return ctx.Err()
	}
	start := time.Now()
	_ = slow.Close()
	r.SlowCloseMS = float64(time.Since(start).Microseconds()) / 1000
	slow = nil
	_ = old.Close()
	old = nil
	r.RecoveryEcho, r.EchoSHA256, err = echo(ctx, m, target)
	return err
}
func run() error {
	port := flag.String("port", "COM4", "exclusive serial port; stop daemon first")
	target := flag.String("target", "", "LAN TCP echo host:port reached by ESP32")
	reportPath := flag.String("report", "link-recovery-report.json", "non-secret JSON results")
	timeout := flag.Duration("timeout", 2*time.Minute, "diagnostic timeout plus independent cleanup budget")
	flag.Parse()
	if _, _, err := net.SplitHostPort(*target); err != nil {
		return errors.New("--target must be a LAN echo host:port")
	}
	if *timeout < 30*time.Second {
		return errors.New("timeout must be at least 30 seconds")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	r := result{Started: time.Now().UTC(), Fault: "authenticated software Wi-Fi disconnect/reconnect; no physical unplug or configuration mutation"}
	if err := execute(ctx, &r, *port, *target); err != nil {
		r.Error = err.Error()
	}
	r.Passed = r.Error == "" && r.CleanupError == "" && r.BackpressureObserved && r.IndependentEcho && r.RecoveryEcho &&
		r.SlowWrittenBytes == r.AttemptedSlowBytes && r.AttemptedSlowBytes >= 64*1024 &&
		r.ReconnectAccepted && r.ReconnectCounterAdvanced && r.OldConnectionEnded && r.SameSerialSession &&
		r.SlowCloseMS < 5000 && r.FinallyNetworkReady && r.ConfigUnchanged
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(*reportPath), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(*reportPath, append(raw, '\n'), 0600); err != nil {
		return err
	}
	fmt.Printf("passed=%t report=%s\n", r.Passed, *reportPath)
	if !r.Passed {
		return errors.New("recovery diagnostic failed; see report")
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
