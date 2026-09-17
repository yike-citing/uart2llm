package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"uart2llm/internal/credentials"
	"uart2llm/internal/link"
	"uart2llm/internal/security"
	"uart2llm/internal/serialport"
)

// ErrConfigNotApplied means staging was explicitly rejected and a subsequent
// authenticated snapshot confirmed no pending transaction. No apply was sent.
// Transport failures or a prior pending transaction never imply this guarantee.
var ErrConfigNotApplied = errors.New("configuration was not applied")

// RPCRejection is an authenticated device response with the expected request ID,
// as distinct from a timeout or malformed response with an unknown outcome.
type RPCRejection struct {
	Method  string
	Code    any
	Message string
}

func (e *RPCRejection) Error() string { return fmt.Sprintf("device %v: %s", e.Code, e.Message) }

type secureSession interface {
	Ready() bool
	Call(context.Context, string, []byte) ([]byte, error)
}

type Manager struct {
	mu          sync.RWMutex
	op          sync.Mutex
	l           *link.Link
	s           secureSession
	p           serialport.Port
	store       credentials.Store
	lastError   string
	port        string
	baud        int
	flow        bool
	rpcID       atomic.Uint64
	recovery    *time.Timer
	configEpoch uint64
}

func New(store credentials.Store) *Manager { return &Manager{store: store} }
func (m *Manager) Connect(ctx context.Context, name string, baud int, flow bool) error {
	m.op.Lock()
	defer m.op.Unlock()
	m.disconnect()
	p, e := serialport.Open(name, baud, flow)
	if e != nil {
		m.fail(e)
		return e
	}
	l := link.New(p, link.Options{})
	if e = l.Handshake(ctx); e != nil {
		l.Close()
		m.fail(e)
		return e
	}
	m.mu.Lock()
	m.l = l
	m.p = p
	m.port = name
	m.baud = baud
	m.flow = flow
	m.lastError = ""
	m.mu.Unlock()
	return nil
}
func (m *Manager) disconnect() {
	m.configEpoch++
	if m.recovery != nil {
		m.recovery.Stop()
		m.recovery = nil
	}
	m.mu.Lock()
	l := m.l
	m.l = nil
	m.s = nil
	m.p = nil
	m.mu.Unlock()
	if l != nil {
		_ = l.Close()
	}
}
func (m *Manager) Disconnect()  { m.op.Lock(); defer m.op.Unlock(); m.disconnect() }
func (m *Manager) fail(e error) { m.mu.Lock(); m.lastError = e.Error(); m.mu.Unlock() }
func (m *Manager) Pair(ctx context.Context, username, password string) error {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.RLock()
	l := m.l
	m.mu.RUnlock()
	if l == nil {
		return errors.New("device disconnected")
	}
	s, e := security.New(username, password)
	if e == nil {
		e = s.Handshake(ctx, l.RPC)
	}
	if e != nil {
		m.fail(e)
		m.disconnect()
		return e
	}
	if e = m.store.Set("device-username", username); e == nil {
		e = m.store.Set("device-password", password)
	}
	if e != nil {
		m.disconnect()
		return e
	}
	m.mu.Lock()
	m.s = s
	m.lastError = ""
	m.mu.Unlock()
	_, e = m.rpc(ctx, "device.self_test", map[string]any{})
	return e
}
func (m *Manager) AutoPair(ctx context.Context) error {
	u, e := m.store.Get("device-username")
	if e != nil {
		return e
	}
	p, e := m.store.Get("device-password")
	if e != nil {
		return e
	}
	return m.Pair(ctx, u, p)
}
func (m *Manager) Status() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	connected := m.l != nil && m.l.Err() == nil
	paired := connected && m.s != nil && m.s.Ready()
	out := map[string]any{"connected": connected, "paired": paired, "port": m.port, "baud": m.baud, "flow_control": m.flow, "last_error": m.lastError}
	if m.l != nil {
		out["link"] = m.l.Stats()
	}
	return out
}
func (m *Manager) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	m.op.Lock()
	m.mu.RLock()
	l, s := m.l, m.s
	m.mu.RUnlock()
	if l == nil || s == nil || !s.Ready() {
		m.op.Unlock()
		return nil, errors.New("device disconnected or unpaired")
	}
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		m.op.Unlock()
		return nil, e
	}
	var number int
	if _, e = fmt.Sscanf(port, "%d", &number); e != nil {
		m.op.Unlock()
		return nil, e
	}
	if _, e = m.rpc(ctx, "target.set", map[string]any{"host": host, "port": number}); e != nil {
		m.op.Unlock()
		return nil, e
	}
	m.op.Unlock()
	return l.DialContext(ctx, network, address)
}
func (m *Manager) RPC(ctx context.Context, method string, params any) (json.RawMessage, error) {
	m.op.Lock()
	defer m.op.Unlock()
	r, e := m.rpc(ctx, method, params)
	if e == nil && method == "config.confirm" && m.recovery != nil {
		m.configEpoch++
		m.recovery.Stop()
		m.recovery = nil
	}
	if e == nil && (method == "config.rollback" || method == "config.reset") {
		m.configEpoch++
		if m.recovery != nil {
			m.recovery.Stop()
			m.recovery = nil
		}
		time.Sleep(1200 * time.Millisecond)
		e = m.setMode(115200, false)
		if e == nil && method == "config.reset" {
			r, e = m.rpc(ctx, "config.confirm", map[string]any{})
		}
	}
	return r, e
}
func (m *Manager) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	m.mu.RLock()
	s := m.s
	m.mu.RUnlock()
	if s == nil {
		return nil, errors.New("device not paired")
	}
	id := m.rpcID.Add(1)
	b, e := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if e != nil {
		return nil, e
	}
	switch {
	case strings.HasPrefix(method, "ota."):
		ctx = link.WithRPCChannel(ctx, link.ChannelOTA)
	case method == "logs.get":
		ctx = link.WithRPCChannel(ctx, link.ChannelLogs)
	case strings.HasPrefix(method, "state.") || method == "tasks.get":
		ctx = link.WithRPCChannel(ctx, link.ChannelTelemetry)
	}
	b, e = s.Call(ctx, "rpc", b)
	if e != nil {
		m.fail(e)
		m.disconnect()
		return nil, e
	}
	var r struct {
		ID     uint64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return nil, e
	}
	if r.ID != id {
		return nil, errors.New("device RPC response id mismatch")
	}
	if r.Error != nil {
		return nil, &RPCRejection{Method: method, Code: r.Error.Code, Message: r.Error.Message}
	}
	if r.Result == nil {
		r.Result = json.RawMessage("null")
	}
	return r.Result, nil
}

// Config changes own the RPC lock, so telemetry cannot interrupt UART migration.
func (m *Manager) StageApply(ctx context.Context, values map[string]any) (json.RawMessage, error) {
	m.op.Lock()
	defer m.op.Unlock()
	if _, e := m.rpc(ctx, "config.stage", map[string]any{"values": values}); e != nil {
		var rejection *RPCRejection
		if errors.As(e, &rejection) {
			// A restarted host can meet a transaction applied by its predecessor.
			// A stage rejection alone therefore cannot safely resume API traffic.
			if snapshot, queryErr := m.rpc(ctx, "config.get", map[string]any{}); queryErr == nil {
				var state struct {
					Pending *bool `json:"pending"`
				}
				if json.Unmarshal(snapshot, &state) == nil && state.Pending != nil && !*state.Pending {
					return nil, fmt.Errorf("%w: %w", ErrConfigNotApplied, e)
				}
			}
		}
		return nil, e
	}
	result, e := m.rpc(ctx, "config.apply", map[string]any{})
	if e != nil {
		return nil, e
	}
	if m.recovery != nil {
		m.recovery.Stop()
	}
	m.configEpoch++
	epoch := m.configEpoch
	m.recovery = time.AfterFunc(32*time.Second, func() {
		m.op.Lock()
		defer m.op.Unlock()
		if epoch != m.configEpoch {
			return
		}
		_ = m.setMode(115200, false)
		m.recovery = nil
	})
	m.mu.RLock()
	baud, flow, p := m.baud, m.flow, m.p
	m.mu.RUnlock()
	changed := false
	if b, ok := values["uart.baud"].(float64); ok {
		baud = int(b)
		changed = true
	}
	if b, ok := values["uart.flow_control"].(bool); ok {
		flow = b
		changed = true
	}
	if changed {
		select {
		case <-time.After(1200 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if p == nil {
			return nil, errors.New("serial disconnected during mode change")
		}
		if e = p.SetMode(baud, flow); e != nil {
			m.fail(e)
			return nil, e
		}
		m.mu.Lock()
		m.baud = baud
		m.flow = flow
		m.mu.Unlock()
	}
	return result, nil
}

func (m *Manager) setMode(baud int, flow bool) error {
	m.mu.RLock()
	p := m.p
	m.mu.RUnlock()
	if p == nil {
		return errors.New("serial disconnected")
	}
	if e := p.SetMode(baud, flow); e != nil {
		return e
	}
	m.mu.Lock()
	m.baud = baud
	m.flow = flow
	m.mu.Unlock()
	return nil
}
