package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type Host struct {
	APIListen      string `json:"api_listen"`
	AdminListen    string `json:"admin_listen"`
	UpstreamURL    string `json:"upstream_url"`
	SerialPort     string `json:"serial_port"`
	Baud           int    `json:"baud"`
	FlowControl    bool   `json:"flow_control"`
	ConnectTimeout int    `json:"connect_timeout_seconds"`
	HeaderTimeout  int    `json:"header_timeout_seconds"`
	IdleTimeout    int    `json:"idle_timeout_seconds"`
	MaxConcurrent  int    `json:"max_concurrent"`
}

func Default() Host {
	return Host{"localhost:8765", "localhost:8766", "https://api.openai.com/v1", "", 115200, false, 30, 120, 120, 4}
}
func (h Host) Validate() error {
	for _, a := range []string{h.APIListen, h.AdminListen} {
		host, port, err := net.SplitHostPort(a)
		if err != nil || port == "0" || port == "" {
			return fmt.Errorf("invalid listen address %q", a)
		}
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return fmt.Errorf("listen port must be numeric 1..65535: %q", a)
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("listeners must bind loopback")
		}
	}
	if h.APIListen == h.AdminListen {
		return errors.New("API and admin listeners must differ")
	}
	u, err := url.Parse(h.UpstreamURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("upstream_url must be HTTPS without credentials, query or fragment")
	}
	if strings.Contains(u.Path, "..") {
		return errors.New("upstream path cannot contain dot segments")
	}
	if h.Baud < 9600 || h.Baud > 3000000 {
		return errors.New("baud must be 9600..3000000")
	}
	if h.ConnectTimeout < 1 || h.ConnectTimeout > 600 || h.HeaderTimeout < 1 || h.HeaderTimeout > 3600 || h.IdleTimeout < 1 || h.IdleTimeout > 86400 {
		return errors.New("invalid timeout")
	}
	if h.MaxConcurrent < 1 || h.MaxConcurrent > 4 {
		return errors.New("max_concurrent must be 1..4")
	}
	return nil
}

type Store struct {
	mu       sync.RWMutex
	path     string
	value    Host
	revision uint64
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "config.json"), value: Default()}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err = json.Unmarshal(b, &s.value); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, s.value.Validate()
}
func (s *Store) Get() Host        { s.mu.RLock(); defer s.mu.RUnlock(); return s.value }
func (s *Store) Revision() uint64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.revision }
func (s *Store) Update(values map[string]json.RawMessage) (Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.value
	b, _ := json.Marshal(v)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	for k, x := range values {
		if _, ok := m[k]; !ok {
			return v, fmt.Errorf("unknown host configuration %q", k)
		}
		if string(x) == "null" {
			return v, fmt.Errorf("%s cannot be null", k)
		}
		m[k] = x
	}
	b, _ = json.Marshal(m)
	if err := json.Unmarshal(b, &v); err != nil {
		return s.value, err
	}
	if err := v.Validate(); err != nil {
		return s.value, err
	}
	b, _ = json.MarshalIndent(v, "", "  ")
	f, err := os.CreateTemp(filepath.Dir(s.path), ".config-*")
	if err != nil {
		return s.value, err
	}
	name := f.Name()
	defer os.Remove(name)
	_ = f.Chmod(0600)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err != nil {
		return s.value, err
	}
	s.value = v
	s.revision++
	return v, nil
}
func DefaultDir() string {
	if d := os.Getenv("UART2LLM_DATA_DIR"); d != "" {
		return d
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return ".uart2llm"
	}
	return filepath.Join(d, "uart2llm")
}
func Schema() []map[string]any {
	defs := []struct {
		n, t, apply string
		min, max    int
	}{
		{"api_listen", "string", "restart", 0, 0}, {"admin_listen", "string", "restart", 0, 0}, {"upstream_url", "string", "next_request", 0, 0}, {"serial_port", "string", "reconnect", 0, 0}, {"baud", "integer", "reconnect", 9600, 3000000}, {"flow_control", "boolean", "reconnect", 0, 0}, {"connect_timeout_seconds", "integer", "next_request", 1, 600}, {"header_timeout_seconds", "integer", "next_request", 1, 3600}, {"idle_timeout_seconds", "integer", "next_request", 1, 86400}, {"max_concurrent", "integer", "next_request", 1, 4}}
	b, _ := json.Marshal(Default())
	var defaults map[string]any
	_ = json.Unmarshal(b, &defaults)
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		m := map[string]any{"name": d.n, "type": d.t, "default": defaults[d.n], "apply": d.apply, "owner": "host", "persistent": true, "persistence": "config.json", "dependencies": []string{}, "scope": "host_runtime", "secret": false}
		if d.max > 0 {
			m["minimum"] = d.min
			m["maximum"] = d.max
		}
		out = append(out, m)
	}
	return out
}
