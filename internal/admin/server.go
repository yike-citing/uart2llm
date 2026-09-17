package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"uart2llm/internal/config"
	"uart2llm/internal/credentials"
	"uart2llm/internal/device"
	"uart2llm/internal/gateway"
	"uart2llm/internal/serialport"
)

type Device interface {
	Status() map[string]any
	Connect(context.Context, string, int, bool) error
	Disconnect()
	Pair(context.Context, string, string) error
	AutoPair(context.Context) error
	RPC(context.Context, string, any) (json.RawMessage, error)
	StageApply(context.Context, map[string]any) (json.RawMessage, error)
}
type Server struct {
	Config      *config.Store
	Secrets     credentials.Store
	Device      Device
	Gateway     *gateway.Gateway
	Token       string
	Shutdown    func()
	mu          sync.RWMutex
	snapshot    map[string]any
	logs        []map[string]any
	started     time.Time
	upload      atomic.Bool
	events      atomic.Int32
	changes     sync.Mutex
	Reconnect   atomic.Bool
	ActualAdmin string
	ActualAPI   string
	configTimer *time.Timer
	configEpoch uint64
	history     []map[string]any
	observed    map[string]string
	eventID     uint64
}

func New(c *config.Store, s credentials.Store, d Device, g *gateway.Gateway, token string) *Server {
	x := &Server{Config: c, Secrets: s, Device: d, Gateway: g, Token: token, started: time.Now(), snapshot: map[string]any{}, ActualAdmin: c.Get().AdminListen, ActualAPI: c.Get().APIListen}
	x.Reconnect.Store(c.Get().SerialPort != "")
	return x
}
func (s *Server) Log(level, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, map[string]any{"time": time.Now().UTC(), "level": level, "message": message})
	if len(s.logs) > 256 {
		s.logs = s.logs[len(s.logs)-256:]
	}
}
func (s *Server) Poll(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) holdConfigMaintenance() {
	s.configEpoch++
	epoch := s.configEpoch
	if s.configTimer != nil {
		s.configTimer.Stop()
	}
	s.configTimer = time.AfterFunc(33*time.Second, func() {
		s.changes.Lock()
		defer s.changes.Unlock()
		if epoch != s.configEpoch {
			return
		}
		s.configTimer = nil
		s.Gateway.Resume()
	})
}
func (s *Server) endConfigMaintenance() {
	s.configEpoch++
	if s.configTimer != nil {
		s.configTimer.Stop()
		s.configTimer = nil
	}
	s.Gateway.Resume()
}
func (s *Server) ReconnectOnce(ctx context.Context) {
	// An idle health check must not reserve the mutation lock. Status may wait
	// behind a device operation even when the existing connection is healthy.
	if !s.Reconnect.Load() || s.upload.Load() || s.Config.Get().SerialPort == "" || s.Device.Status()["connected"] == true {
		return
	}
	if !s.changes.TryLock() {
		return
	}
	defer s.changes.Unlock()
	if !s.Reconnect.Load() || s.Device.Status()["connected"] == true || s.upload.Load() {
		return
	}
	h := s.Config.Get()
	if h.SerialPort == "" {
		return
	}
	modes := []struct {
		baud int
		flow bool
	}{{h.Baud, h.FlowControl}}
	if h.Baud != 115200 || h.FlowControl {
		modes = append(modes, struct {
			baud int
			flow bool
		}{115200, false})
	}
	for _, mode := range modes {
		probe, cancel := context.WithTimeout(ctx, 6*time.Second)
		e := s.Device.Connect(probe, h.SerialPort, mode.baud, mode.flow)
		cancel()
		if e == nil {
			_ = s.Device.AutoPair(ctx)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}
func (s *Server) firmwareRPC(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return s.Device.RPC(c, method, params)
}
func (s *Server) refresh(ctx context.Context) {
	status := s.Device.Status()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	d := any(nil)
	if status["paired"] == true && !s.upload.Load() {
		budget := 4 * time.Second
		if baud, ok := status["baud"].(int); ok && baud > 0 {
			wire := time.Duration(81920/baud+2) * time.Second
			if wire > budget {
				budget = wire
			}
		}
		call, cancel := context.WithTimeout(ctx, budget)
		r, e := s.Device.RPC(call, "state.get", map[string]any{})
		cancel()
		if e == nil {
			_ = json.Unmarshal(r, &d)
		} else {
			status["telemetry_error"] = e.Error()
		}
	}
	snap := map[string]any{"host": map[string]any{"uptime_seconds": int(time.Since(s.started).Seconds()), "active_requests": s.Gateway.Active(), "requests": s.Gateway.Requests.Load(), "failures": s.Gateway.Failures.Load(), "config_revision": s.Config.Revision(), "firmware_upload": s.upload.Load(), "heap_bytes": mem.HeapAlloc, "heap_system_bytes": mem.HeapSys, "goroutines": runtime.NumGoroutine()}, "device": d, "connected": status["connected"], "paired": status["paired"], "link": status, "sampled_at": time.Now().UTC()}
	s.mu.Lock()
	facts := map[string]any{"connected": status["connected"], "paired": status["paired"], "host_config_revision": s.Config.Revision()}
	if device, ok := d.(map[string]any); ok {
		facts["device_config_revision"] = device["revision"]
		if network, ok := device["network"].(map[string]any); ok {
			facts["wifi_connected"] = network["connected"]
		}
	}
	if s.observed == nil {
		s.observed = map[string]string{}
	}
	for key, value := range facts {
		current := fmt.Sprint(value)
		previous, exists := s.observed[key]
		if !exists || previous != current {
			s.eventID++
			s.history = append(s.history, map[string]any{"id": s.eventID, "time": time.Now().UTC(), "field": key, "value": value})
			s.observed[key] = current
		}
	}
	if len(s.history) > 256 {
		s.history = s.history[len(s.history)-256:]
	}
	snap["recent_changes"] = append([]map[string]any{}, s.history...)
	snap["llm"] = s.Gateway.Statistics()
	s.snapshot = snap
	s.mu.Unlock()
}
func (s *Server) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]any, len(s.snapshot))
	for k, v := range s.snapshot {
		out[k] = v
	}
	return out
}
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errReply(w http.ResponseWriter, status int, e error) {
	gateway.WriteError(w, status, "management_error", e.Error())
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("one JSON value required")
	}
	return nil
}
func (s *Server) originAllowed(r *http.Request) bool {
	// Cross-site browser fetches must never bootstrap or use local management.
	// CLI clients omit Fetch Metadata; normal same-origin fetches use same-origin.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	_, port, e := net.SplitHostPort(s.ActualAdmin)
	if e != nil {
		return false
	}
	host, hp, e := net.SplitHostPort(r.Host)
	if e != nil || hp != port {
		return false
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, e := url.Parse(origin)
	return e == nil && u.Scheme == "http" && u.Host == r.Host && u.Path == "" && u.RawQuery == ""
}
func (s *Server) cookie() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	v := strconv.FormatInt(time.Now().Add(8*time.Hour).Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte(s.Token))
	mac.Write([]byte(v))
	return v + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) authorized(r *http.Request) bool {
	if gateway.Authorized(r.Header.Get("Authorization"), s.Token) {
		return true
	}
	c, e := r.Cookie("uart2llm_session")
	if e != nil {
		return false
	}
	p := strings.Split(c.Value, ".")
	if len(p) != 3 {
		return false
	}
	expiry, e := strconv.ParseInt(p[0], 10, 64)
	if e != nil || time.Now().Unix() > expiry {
		return false
	}
	v := p[0] + "." + p[1]
	mac := hmac.New(sha256.New, []byte(s.Token))
	mac.Write([]byte(v))
	signature, e := base64.RawURLEncoding.DecodeString(p[2])
	return e == nil && hmac.Equal(signature, mac.Sum(nil))
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if !s.originAllowed(r) {
		errReply(w, 403, errors.New("invalid local origin or host"))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/v1")
	if path == "/session" && r.Method == "POST" {
		kind, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || kind != "application/json" {
			errReply(w, 415, errors.New("local browser session requires application/json"))
			return
		}
		var in struct {
			// Accepted only for old clients; browser users no longer enter a token.
			Token string `json:"token"`
		}
		if e := decode(w, r, &in); e != nil {
			errReply(w, 400, e)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "uart2llm_session", Value: s.cookie(), Path: "/admin/v1", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 28800})
		send(w, 200, map[string]bool{"authenticated": true})
		return
	}
	if !s.authorized(r) {
		errReply(w, 401, errors.New("administration authentication required"))
		return
	}
	if path == "/session" && r.Method == "DELETE" {
		http.SetCookie(w, &http.Cookie{Name: "uart2llm_session", Path: "/admin/v1", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		send(w, 200, map[string]bool{"authenticated": false})
		return
	}
	if path == "/events" && r.Method == "GET" {
		s.streamEvents(w, r)
		return
	}
	if r.Method != "GET" {
		if !s.changes.TryLock() {
			errReply(w, 409, errors.New("another management operation is in progress"))
			return
		}
		defer s.changes.Unlock()
	}
	if path == "/firmware" && r.Method == "POST" {
		s.firmware(w, r)
		return
	}
	if s.upload.Load() && r.Method != "GET" {
		errReply(w, 409, errors.New("configuration and lifecycle changes are unavailable during firmware maintenance"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	var result any
	var e error
	switch r.Method + " " + path {
	case "GET /openapi.json":
		result = json.RawMessage(openAPIDocument)
	case "GET /state":
		result = s.Snapshot()
		result.(map[string]any)["connection"] = s.Device.Status()
		result.(map[string]any)["llm"] = s.Gateway.Statistics()
	case "GET /metrics":
		result = s.Gateway.Statistics()
	case "GET /models":
		// Reuse the complete authenticated gateway path: capacity, TLS and the
		// device dialer. Never use a desktop HTTP client as a network fallback.
		upstream := r.Clone(r.Context())
		endpoint := *r.URL
		endpoint.Path, endpoint.RawPath, endpoint.RawQuery = "/v1/models", "", ""
		upstream.URL = &endpoint
		upstream.RequestURI = "/v1/models"
		upstream.Header = make(http.Header)
		upstream.Header.Set("Authorization", "Bearer "+s.Gateway.Token)
		s.Gateway.ServeHTTP(w, upstream)
		return
	case "POST /proxy/pause":
		var p struct {
			Paused *bool `json:"paused"`
		}
		if e = decode(w, r, &p); e != nil {
			break
		}
		if p.Paused == nil {
			e = errors.New("paused boolean required")
			break
		}
		s.Gateway.SetUserPaused(*p.Paused)
		result = map[string]any{"paused": s.Gateway.UserPaused(), "active": s.Gateway.Active()}
	case "GET /ports":
		result, e = serialport.List()
	case "GET /capabilities":
		result = map[string]any{"host": map[string]any{"platform": "windows-x64", "max_concurrent": 4, "ota_max_bytes": 4 * 1024 * 1024, "logical_channels": map[string]any{"management": 0, "tcp": []int{1, 2, 3, 4}, "telemetry": 5, "logs": 6, "ota": 7}, "transports": []string{"uart", "usb_serial_jtag_validation"}, "api_families": []string{"chat/completions", "fine_tuning/jobs", "files", "models"}}, "device": nil, "connection": s.Device.Status()}
		if s.Device.Status()["paired"] == true {
			result.(map[string]any)["device"], e = s.Device.RPC(ctx, "capabilities", map[string]any{})
		}
	case "GET /config/schema":
		result = map[string]any{"host": config.Schema(), "device": []any{}}
		if s.Device.Status()["paired"] == true {
			result.(map[string]any)["device"], e = s.Device.RPC(ctx, "config.schema", map[string]any{})
		}
	case "GET /state/schema":
		items := []any{}
		offset := 0
		for page := 0; page < 32 && s.Device.Status()["paired"] == true; page++ {
			var raw json.RawMessage
			raw, e = s.Device.RPC(ctx, "state.schema", map[string]any{"offset": offset, "limit": 12})
			if e != nil {
				break
			}
			var p struct {
				Items []any `json:"items"`
				Next  *int  `json:"next_offset"`
			}
			if e = json.Unmarshal(raw, &p); e != nil {
				break
			}
			items = append(items, p.Items...)
			if p.Next == nil {
				break
			}
			if *p.Next <= offset {
				e = errors.New("invalid state schema pagination")
				break
			}
			offset = *p.Next
		}
		result = map[string]any{"host": hostStateSchema(), "device": items}
	case "GET /tasks":
		items := []any{}
		offset := 0
		var afterID uint32
		cursor := false
		for page := 0; page < 32 && s.Device.Status()["paired"] == true; page++ {
			params := map[string]any{"offset": offset}
			if page == 0 || cursor {
				params["after_id"] = afterID
			}
			raw, re := s.Device.RPC(ctx, "tasks.get", params)
			if re != nil {
				e = re
				break
			}
			var p struct {
				Items     []any           `json:"items"`
				Next      *int            `json:"next_offset"`
				NextAfter json.RawMessage `json:"next_after_id"`
			}
			if e = json.Unmarshal(raw, &p); e != nil {
				break
			}
			items = append(items, p.Items...)
			if len(p.NextAfter) != 0 {
				cursor = true
				var next *uint32
				if e = json.Unmarshal(p.NextAfter, &next); e != nil {
					break
				}
				if next == nil {
					break
				}
				if *next <= afterID {
					e = errors.New("invalid task ID pagination")
					break
				}
				afterID = *next
			} else if cursor {
				e = errors.New("task ID cursor missing from response")
				break
			} else {
				if p.Next == nil {
					break
				}
				if *p.Next <= offset {
					e = errors.New("invalid task pagination")
					break
				}
				offset = *p.Next
			}
			if page == 31 {
				e = errors.New("task list exceeds pagination limit")
			}
		}
		result = map[string]any{"device": items}
	case "GET /config":
		key, ke := s.Secrets.Get("upstream-key")
		result = map[string]any{"host": s.Config.Get(), "device": nil, "secrets": map[string]bool{"upstream_key_set": ke == nil && key != "", "api_token_set": true}, "revision": s.Config.Revision(), "effective_listeners": map[string]string{"api_listen": s.ActualAPI, "admin_listen": s.ActualAdmin}, "restart_required": s.Config.Get().APIListen != s.ActualAPI || s.Config.Get().AdminListen != s.ActualAdmin}
		if s.Device.Status()["paired"] == true {
			result.(map[string]any)["device"], e = s.Device.RPC(ctx, "config.get", map[string]any{})
		}
	case "PATCH /config":
		var in struct {
			Host   map[string]json.RawMessage `json:"host"`
			Device map[string]any             `json:"device"`
		}
		if e = decode(w, r, &in); e != nil {
			break
		}
		if len(in.Host) > 0 && len(in.Device) > 0 {
			e = errors.New("update host and device separately; they have independent transactions")
			break
		}
		if len(in.Host) > 0 {
			result, e = s.Config.Update(in.Host)
		} else if len(in.Device) > 0 {
			if !s.Gateway.Pause() {
				errReply(w, 409, errors.New("finish active API requests or pending configuration before changing device settings"))
				return
			}
			result, e = s.Device.StageApply(ctx, in.Device)
			if errors.Is(e, device.ErrConfigNotApplied) {
				s.endConfigMaintenance()
			} else {
				s.holdConfigMaintenance() // An ambiguous apply may still be pending on the device.
			}
		} else {
			e = errors.New("empty configuration patch")
		}
	case "POST /config/confirm":
		result, e = s.Device.RPC(ctx, "config.confirm", map[string]any{})
		if e == nil {
			s.endConfigMaintenance()
			state := s.Device.Status()
			if baud, ok := state["baud"]; ok {
				b, _ := json.Marshal(baud)
				f, _ := json.Marshal(state["flow_control"])
				_, e = s.Config.Update(map[string]json.RawMessage{"baud": b, "flow_control": f})
			}
		}
	case "POST /config/rollback":
		if s.configTimer == nil && !s.Gateway.Pause() {
			errReply(w, 409, errors.New("finish active API requests before rollback"))
			return
		}
		result, e = s.Device.RPC(ctx, "config.rollback", map[string]any{})
		if e == nil {
			s.endConfigMaintenance()
		} else if s.configTimer == nil {
			s.holdConfigMaintenance()
		}
	case "POST /connect":
		s.Reconnect.Store(false)
		var in struct {
			Port        string `json:"port"`
			Baud        int    `json:"baud"`
			FlowControl bool   `json:"flow_control"`
		}
		if e = decode(w, r, &in); e != nil {
			break
		}
		if in.Baud == 0 {
			in.Baud = 115200
		}
		if in.Baud < 9600 || in.Baud > 3000000 {
			e = errors.New("invalid baud")
			break
		}
		e = s.Device.Connect(ctx, in.Port, in.Baud, in.FlowControl)
		if e == nil {
			ae := s.Device.AutoPair(ctx)
			if ae != nil && !errors.Is(ae, credentials.ErrNotFound) {
				s.Log("warning", "automatic pairing failed; reconnect and check device credentials")
			}
			result = s.Device.Status()
			if result.(map[string]any)["connected"] == true {
				bp, _ := json.Marshal(in.Port)
				bb, _ := json.Marshal(in.Baud)
				bf, _ := json.Marshal(in.FlowControl)
				_, e = s.Config.Update(map[string]json.RawMessage{"serial_port": bp, "baud": bb, "flow_control": bf})
				s.Reconnect.Store(true)
			}
		}
	case "POST /disconnect":
		s.Reconnect.Store(false)
		s.Device.Disconnect()
		s.endConfigMaintenance()
		result = map[string]bool{"connected": false}
	case "POST /pair":
		var in struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if e = decode(w, r, &in); e == nil {
			e = s.Device.Pair(ctx, in.Username, in.Password)
		}
		result = s.Device.Status()
	case "POST /credentials":
		var in struct {
			UpstreamKey string `json:"upstream_key"`
		}
		if e = decode(w, r, &in); e == nil {
			if strings.TrimSpace(in.UpstreamKey) == "" {
				e = errors.New("upstream key is empty")
			} else {
				e = s.Secrets.Set("upstream-key", in.UpstreamKey)
			}
		}
		result = map[string]bool{"saved": e == nil}
	case "GET /api-token":
		var token string
		token, e = s.Secrets.Get("api-token")
		result = map[string]string{"token": token}
	case "GET /logs":
		s.mu.RLock()
		hostLogs := append([]map[string]any{}, s.logs...)
		s.mu.RUnlock()
		result = map[string]any{"host": hostLogs, "device": nil}
		if s.Device.Status()["paired"] == true {
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			result.(map[string]any)["device"], e = s.Device.RPC(ctx, "logs.get", map[string]any{"after": after})
		}
	case "POST /diagnostics":
		var in struct {
			Action string `json:"action"`
		}
		if e = decode(w, r, &in); e == nil {
			if in.Action == "export" {
				result = map[string]any{"state": s.Snapshot(), "host_config": s.Config.Get(), "connection": s.Device.Status()}
			} else if in.Action == "crash_export" {
				result, e = s.crashExport(ctx)
			} else {
				result, e = s.Device.RPC(ctx, "diagnostics", map[string]any{"action": in.Action})
			}
		}
	case "POST /device/action":
		var in struct {
			Action string `json:"action"`
		}
		if e = decode(w, r, &in); e == nil {
			switch in.Action {
			case "clear_crash":
				result, e = s.Device.RPC(ctx, "crash.clear", map[string]any{})
			case "reboot":
				result, e = s.Device.RPC(ctx, "device.restart", map[string]any{})
			case "factory_reset":
				if !s.Gateway.Pause() {
					errReply(w, 409, errors.New("finish active API requests or configuration before reset"))
					return
				}
				result, e = s.Device.RPC(ctx, "config.reset", map[string]any{})
				if e == nil {
					s.Gateway.Resume()
				} else {
					s.holdConfigMaintenance()
				}
			default:
				e = errors.New("unsupported device action")
			}
		}
	case "POST /shutdown":
		result = map[string]bool{"stopping": true}
		if s.Shutdown != nil {
			defer func() { go s.Shutdown() }()
		}
	default:
		errReply(w, 404, errors.New("unknown administration endpoint or method"))
		return
	}
	if e != nil {
		errReply(w, 400, e)
		return
	}
	send(w, 200, result)
}
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	if s.events.Add(1) > 16 {
		s.events.Add(-1)
		errReply(w, 503, errors.New("too many event subscribers"))
		return
	}
	defer s.events.Add(-1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastEvent, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	for {
		s.mu.RLock()
		updates := append([]map[string]any{}, s.history...)
		s.mu.RUnlock()
		for _, event := range updates {
			eventID := event["id"].(uint64)
			if eventID > lastEvent {
				b, _ := json.Marshal(event)
				_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if _, e := fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", eventID, b); e != nil {
					return
				}
				lastEvent = eventID
			}
		}
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		b, _ := json.Marshal(s.Snapshot())
		if _, e := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); e != nil {
			return
		}
		if e := rc.Flush(); e != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) firmware(w http.ResponseWriter, r *http.Request) {
	if !s.upload.CompareAndSwap(false, true) {
		errReply(w, 409, errors.New("firmware update in progress"))
		return
	}
	defer s.upload.Store(false)
	if !s.Gateway.Pause() {
		errReply(w, 409, errors.New("finish active API requests before firmware maintenance"))
		return
	}
	defer s.Gateway.Resume()
	if r.ContentLength <= 0 || r.ContentLength > 4*1024*1024 {
		errReply(w, 400, errors.New("firmware requires Content-Length between 1 and 4194304"))
		return
	}
	ctx := r.Context()
	if _, e := s.firmwareRPC(ctx, "ota.begin", map[string]any{"size": r.ContentLength}); e != nil {
		errReply(w, 400, e)
		return
	}
	complete := false
	defer func() {
		if !complete {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.Device.RPC(c, "ota.abort", map[string]any{})
		}
	}()
	h := sha256.New()
	buf := make([]byte, 1024)
	offset := int64(0)
	rc := http.NewResponseController(w)
	for {
		_ = rc.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, re := r.Body.Read(buf)
		if n > 0 {
			if offset+int64(n) > r.ContentLength {
				errReply(w, 400, errors.New("firmware length mismatch"))
				return
			}
			h.Write(buf[:n])
			if _, e := s.firmwareRPC(ctx, "ota.write", map[string]any{"offset": offset, "data": base64.StdEncoding.EncodeToString(buf[:n])}); e != nil {
				errReply(w, 502, e)
				return
			}
			offset += int64(n)
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			errReply(w, 400, errors.New("firmware upload interrupted"))
			return
		}
	}
	if offset != r.ContentLength {
		errReply(w, 400, errors.New("firmware length mismatch"))
		return
	}
	res, e := s.firmwareRPC(ctx, "ota.end", map[string]any{"sha256": hex.EncodeToString(h.Sum(nil))})
	if e != nil {
		errReply(w, 400, e)
		return
	}
	complete = true
	send(w, 200, res)
}

// Coredumps are bounded by the 64 KiB diagnostic partition, never arbitrary files.
func (s *Server) crashExport(ctx context.Context) (any, error) {
	data := make([]byte, 0, 65536)
	for offset := 0; offset < 65536; {
		raw, e := s.Device.RPC(ctx, "crash.read", map[string]any{"offset": offset})
		if e != nil {
			return nil, e
		}
		var p struct {
			Offset int    `json:"offset"`
			Size   int    `json:"size"`
			Data   string `json:"data"`
			EOF    bool   `json:"eof"`
		}
		if e = json.Unmarshal(raw, &p); e != nil {
			return nil, e
		}
		b, e := base64.StdEncoding.DecodeString(p.Data)
		if e != nil {
			return nil, e
		}
		if p.Offset != offset || p.Size > 65536 || offset+len(b) > 65536 {
			return nil, errors.New("invalid crash data range")
		}
		data = append(data, b...)
		offset += len(b)
		if p.EOF {
			return map[string]any{"size": len(data), "encoding": "base64", "data": base64.StdEncoding.EncodeToString(data)}, nil
		}
		if len(b) == 0 {
			return nil, errors.New("crash download made no progress")
		}
	}
	return nil, errors.New("crash data exceeds diagnostic partition")
}

func hostStateSchema() []map[string]any {
	defs := []struct{ Name, Unit, Valid string }{
		{"host.uptime_seconds", "s", "daemon_running"}, {"host.active_requests", "requests", "daemon_running"}, {"host.requests", "requests", "daemon_running"}, {"host.failures", "requests", "daemon_running"}, {"host.config_revision", "revision", "daemon_running"}, {"host.firmware_upload", "boolean", "daemon_running"}, {"host.heap_bytes", "bytes", "daemon_running"}, {"host.heap_system_bytes", "bytes", "daemon_running"}, {"host.goroutines", "tasks", "daemon_running"},
		{"connected", "boolean", "daemon_running"}, {"paired", "boolean", "daemon_running"}, {"sampled_at", "UTC", "daemon_running"},
		{"link.connected", "boolean", "daemon_running"}, {"link.paired", "boolean", "daemon_running"}, {"link.port", "COM name", "last_connection_or_empty"}, {"link.baud", "baud", "last_connection_or_zero"}, {"link.flow_control", "boolean", "last_connection_or_default"}, {"link.last_error", "text", "empty_when_no_error"}, {"link.telemetry_error", "text", "present_when_sample_failed"},
		{"link.link.session", "id", "link_object_exists"}, {"link.link.connected", "boolean", "link_object_exists"}, {"link.link.received_frames", "frames", "link_object_exists"}, {"link.link.sent_frames", "frames", "link_object_exists"}, {"link.link.invalid_frames", "frames", "link_object_exists"}, {"link.link.retries", "frames", "link_object_exists"}, {"link.link.duplicate_frames", "frames", "link_object_exists"}, {"link.link.active_connections", "connections", "link_object_exists"},
		{"recent_changes[].id", "id", "retained_change"}, {"recent_changes[].time", "UTC", "retained_change"}, {"recent_changes[].field", "path", "retained_change"}, {"recent_changes[].value", "value", "retained_change"},
	}
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]any{"name": d.Name, "source": "daemon", "unit": d.Unit, "sample_time_field": "sampled_at", "valid_when": d.Valid, "unavailable_reason": "device disconnected, not sampled, or no applicable event"})
	}
	return append(out, gateway.StatisticsSchema()...)
}
