// link-bench measures the serial TCP tunnel independently of model generation.
// Stop the daemon before running: this program exclusively owns the COM port.
package main

import (
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
	"sync"
	"time"

	"uart2llm/internal/config"
	"uart2llm/internal/credentials"
	"uart2llm/internal/device"
)

type streamResult struct {
	Index         int     `json:"index"`
	Sent          int64   `json:"sent_bytes"`
	Received      int64   `json:"received_bytes"`
	SendSHA256    string  `json:"send_sha256"`
	ReceiveSHA256 string  `json:"receive_sha256"`
	DurationMS    float64 `json:"duration_ms"`
	Matched       bool    `json:"matched"`
	Error         string  `json:"error,omitempty"`
}
type telemetryResult struct {
	Samples             int     `json:"samples"`
	Failures            int     `json:"failures"`
	MaximumMS           float64 `json:"maximum_ms"`
	LastError           string  `json:"last_error,omitempty"`
	MinimumInternalFree uint64  `json:"minimum_internal_free_bytes"`
	MinimumPSRAMFree    uint64  `json:"minimum_psram_free_bytes"`
	MaximumTasks        uint64  `json:"maximum_tasks"`
}
type cancelResult struct {
	CloseMS       float64      `json:"close_ms"`
	ReadUnblocked bool         `json:"read_unblocked"`
	SameSession   bool         `json:"same_session"`
	Recovery      streamResult `json:"recovery"`
	Error         string       `json:"error,omitempty"`
}
type report struct {
	Started                   time.Time       `json:"started"`
	Port                      string          `json:"port"`
	Target                    string          `json:"target"`
	BytesPerStream            int64           `json:"bytes_per_stream"`
	Parallel                  int             `json:"parallel"`
	DurationMS                float64         `json:"duration_ms"`
	EchoPayloadBytesPerSecond float64         `json:"echo_payload_bytes_per_second"`
	DuplexBytesPerSecond      float64         `json:"duplex_bytes_per_second"`
	Streams                   []streamResult  `json:"streams"`
	Telemetry                 telemetryResult `json:"telemetry"`
	Cancellation              cancelResult    `json:"cancellation"`
	Before                    map[string]any  `json:"before,omitempty"`
	After                     map[string]any  `json:"after,omitempty"`
	Passed                    bool            `json:"passed"`
	Error                     string          `json:"error,omitempty"`
}

// pattern is a deterministic byte stream with no allocation proportional to load.
// Keeping state independent of Read boundaries catches truncation/reordering.
type pattern struct{ state uint64 }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		p.state ^= p.state << 13
		p.state ^= p.state >> 7
		p.state ^= p.state << 17
		b[i] = byte(p.state >> 24)
	}
	return len(b), nil
}
func seed(index int) *pattern { return &pattern{state: 0x6a09e667f3bcc909 ^ uint64(index+1)} }

func transfer(ctx context.Context, conn net.Conn, index int, count int64) streamResult {
	r := streamResult{Index: index}
	started := time.Now()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	type written struct {
		n   int64
		sum string
		err error
	}
	writes := make(chan written, 1)
	go func() {
		h := sha256.New()
		p := seed(index)
		buf := make([]byte, 16*1024)
		var total int64
		var err error
		for total < count {
			b := buf[:min(int64(len(buf)), count-total)]
			_, _ = p.Read(b)
			_ = conn.SetWriteDeadline(time.Now().Add(120 * time.Second))
			var n int
			n, err = conn.Write(b)
			_, _ = h.Write(b[:n])
			total += int64(n)
			if err != nil {
				break
			}
			if n != len(b) {
				err = io.ErrShortWrite
				break
			}
		}
		writes <- written{total, hex.EncodeToString(h.Sum(nil)), err}
	}()
	h := sha256.New()
	buf := make([]byte, 16*1024)
	var readErr error
	for r.Received < count {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := conn.Read(buf[:min(int64(len(buf)), count-r.Received)])
		_, _ = h.Write(buf[:n])
		r.Received += int64(n)
		if err != nil && r.Received < count {
			readErr = err
			break
		}
		if n == 0 && err == nil {
			readErr = io.ErrNoProgress
			break
		}
	}
	if readErr != nil {
		_ = conn.Close()
	}
	w := <-writes
	r.Sent, r.SendSHA256 = w.n, w.sum
	r.ReceiveSHA256 = hex.EncodeToString(h.Sum(nil))
	r.Matched = r.Sent == count && r.Received == count && r.SendSHA256 == r.ReceiveSHA256 && readErr == nil && w.err == nil
	if err := errors.Join(w.err, readErr); err != nil {
		r.Error = err.Error()
	}
	r.DurationMS = float64(time.Since(started).Microseconds()) / 1000
	return r
}

func session(m *device.Manager) uint32 {
	b, _ := json.Marshal(m.Status())
	var s struct {
		Link struct {
			Session uint32 `json:"session"`
		} `json:"link"`
	}
	_ = json.Unmarshal(b, &s)
	return s.Link.Session
}
func cancellation(ctx context.Context, m *device.Manager, target string) cancelResult {
	var r cancelResult
	initial := session(m)
	c, err := m.DialContext(ctx, "tcp", target)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	read := make(chan error, 1)
	go func() { var b [1]byte; _, err := c.Read(b[:]); read <- err }()
	started := time.Now()
	_ = c.Close()
	r.CloseMS = float64(time.Since(started).Microseconds()) / 1000
	select {
	case err = <-read:
		r.ReadUnblocked = err != nil
	case <-time.After(5 * time.Second):
		r.Error = "cancelled connection reader remained blocked"
	}
	c, err = m.DialContext(ctx, "tcp", target)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Recovery = transfer(ctx, c, 101, 4096)
	r.SameSession = initial != 0 && initial == session(m)
	return r
}

func execute(ctx context.Context, r *report) error {
	store, err := credentials.New(config.DefaultDir())
	if err != nil {
		return err
	}
	m := device.New(store)
	defer m.Disconnect()
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = m.Connect(setup, r.Port, 115200, false); err != nil {
		return err
	}
	if err = m.AutoPair(setup); err != nil {
		return err
	}
	r.Before = m.Status()
	defer func() { r.After = m.Status() }()
	connections := make([]net.Conn, 0, r.Parallel)
	defer func() {
		for _, c := range connections {
			_ = c.Close()
		}
	}()
	for i := 0; i < r.Parallel; i++ {
		c, err := m.DialContext(setup, "tcp", r.Target)
		if err != nil {
			return err
		}
		connections = append(connections, c)
	}
	// Stop polling cleanly before the cancel/recovery phase. Only scalar timing
	// is retained; device replies and credential material never enter the report.
	pollCtx, stopPoll := context.WithCancel(ctx)
	pollDone := make(chan telemetryResult, 1)
	go func() {
		var result telemetryResult
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				pollDone <- result
				return
			case <-ticker.C:
				// Do not cancel an already transmitted Security2 RPC when data
				// finishes: its response must drain to preserve cipher ordering.
				probe, cancel := context.WithTimeout(ctx, 4*time.Second)
				start := time.Now()
				raw, err := m.RPC(probe, "state.get", map[string]any{})
				cancel()
				result.Samples++
				result.MaximumMS = max(result.MaximumMS, float64(time.Since(start).Microseconds())/1000)
				if err != nil {
					result.Failures++
					result.LastError = err.Error()
				} else {
					var sample struct {
						Memory struct {
							Internal uint64 `json:"internal_free"`
							PSRAM    uint64 `json:"psram_free"`
							Tasks    uint64 `json:"tasks"`
						} `json:"memory"`
					}
					if json.Unmarshal(raw, &sample) == nil {
						if result.MinimumInternalFree == 0 || sample.Memory.Internal < result.MinimumInternalFree {
							result.MinimumInternalFree = sample.Memory.Internal
						}
						if result.MinimumPSRAMFree == 0 || sample.Memory.PSRAM < result.MinimumPSRAMFree {
							result.MinimumPSRAMFree = sample.Memory.PSRAM
						}
						result.MaximumTasks = max(result.MaximumTasks, sample.Memory.Tasks)
					}
				}
			}
		}
	}()
	start := time.Now()
	r.Streams = make([]streamResult, r.Parallel)
	var wg sync.WaitGroup
	for i, c := range connections {
		wg.Add(1)
		go func(i int, c net.Conn) { defer wg.Done(); r.Streams[i] = transfer(ctx, c, i, r.BytesPerStream) }(i, c)
	}
	wg.Wait()
	elapsed := time.Since(start)
	stopPoll()
	r.Telemetry = <-pollDone
	r.DurationMS = float64(elapsed.Microseconds()) / 1000
	var received, duplex int64
	r.Passed = true
	for _, s := range r.Streams {
		received += s.Received
		duplex += s.Sent + s.Received
		r.Passed = r.Passed && s.Matched
	}
	r.EchoPayloadBytesPerSecond = float64(received) / elapsed.Seconds()
	r.DuplexBytesPerSecond = float64(duplex) / elapsed.Seconds()
	r.Cancellation = cancellation(ctx, m, r.Target)
	r.Passed = r.Passed && r.Telemetry.Failures == 0 && r.Cancellation.ReadUnblocked && r.Cancellation.SameSession && r.Cancellation.Recovery.Matched && r.Cancellation.CloseMS < 5000 && r.Cancellation.Error == ""
	return nil
}

func run() error {
	r := report{Started: time.Now().UTC()}
	flag.StringVar(&r.Port, "port", "COM4", "exclusive serial port (stop daemon first)")
	flag.StringVar(&r.Target, "target", "", "LAN TCP echo server host:port, reached by ESP32")
	flag.Int64Var(&r.BytesPerStream, "bytes", 256*1024, "bytes to echo per connection (bounded memory)")
	flag.IntVar(&r.Parallel, "parallel", 4, "simultaneous connections, 1..4")
	path := flag.String("report", "link-bench-report.json", "JSON report path")
	timeout := flag.Duration("timeout", 30*time.Minute, "whole diagnostic timeout")
	flag.Parse()
	if _, _, err := net.SplitHostPort(r.Target); err != nil {
		return errors.New("--target must be a LAN TCP echo host:port")
	}
	if r.Parallel < 1 || r.Parallel > 4 || r.BytesPerStream < 1 || r.BytesPerStream > 1<<32 || *timeout <= 0 {
		return errors.New("invalid parallel, byte count or timeout")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if err := execute(ctx, &r); err != nil {
		r.Error = err.Error()
		r.Passed = false
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(*path), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(*path, append(b, '\n'), 0600); err != nil {
		return err
	}
	fmt.Printf("passed=%t echo_payload=%.0f bytes/s duplex=%.0f bytes/s report=%s\n", r.Passed, r.EchoPayloadBytesPerSecond, r.DuplexBytesPerSecond, *path)
	if !r.Passed {
		return errors.New("link benchmark failed; see report")
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
