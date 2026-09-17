package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"uart2llm/internal/admin"
	"uart2llm/internal/config"
	"uart2llm/internal/credentials"
	"uart2llm/internal/device"
	"uart2llm/internal/gateway"
	"uart2llm/internal/notices"
	"uart2llm/internal/platform"
	"uart2llm/internal/serialport"
	"uart2llm/internal/tray"
	"uart2llm/internal/ui"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		platform.AttachConsole()
	}
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "uart2llm:", e)
		if len(os.Args) == 1 {
			platform.ShowError("uart2llm", e.Error())
		}
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return launchDesktop(run, tray.Open)
	}
	if args[0] == "licenses" {
		return notices.WriteTo(os.Stdout)
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if args[0] == "help" || args[0] == "--help" {
		usage()
		return nil
	}
	if args[0] == "tray" {
		if len(args) > 1 && args[1] == "status" {
			return tray.ShowStatus()
		}
		return tray.ShowMenu()
	}
	dir := config.DefaultDir()
	cfg, e := config.Open(dir)
	if e != nil {
		return e
	}
	secrets, e := credentials.New(dir)
	if e != nil {
		return e
	}
	switch args[0] {
	case "init":
		if _, e = credentials.Ensure(secrets, "admin-token"); e != nil {
			return e
		}
		if _, e = credentials.Ensure(secrets, "api-token"); e != nil {
			return e
		}
		_, e = cfg.Update(map[string]json.RawMessage{})
		fmt.Println("Configuration initialized:", dir)
		return e
	case "serve":
		return serve(cfg, secrets)
	case "start":
		if r, e := adminRequest(cfg, secrets, "GET", "/state", nil, 0); e == nil {
			r.Body.Close()
			fmt.Println("Daemon already running")
			return nil
		}
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		cmd := exec.Command(exe, "serve")
		platform.Detached(cmd)
		if e = cmd.Start(); e != nil {
			return e
		}
		_ = cmd.Process.Release()
		for i := 0; i < 100; i++ {
			time.Sleep(100 * time.Millisecond)
			if r, e := adminRequest(cfg, secrets, "GET", "/state", nil, 0); e == nil {
				r.Body.Close()
				fmt.Println("Daemon ready")
				return nil
			}
		}
		return errors.New("daemon did not start; run serve for diagnostics")
	case "token":
		if len(args) != 2 || (args[1] != "api" && args[1] != "admin") {
			return errors.New("usage: token api|admin")
		}
		t, e := secrets.Get(args[1] + "-token")
		if e == nil {
			fmt.Println(t)
		}
		return e
	case "credentials":
		if len(args) < 2 || args[1] != "set-upstream" {
			return errors.New("usage: credentials set-upstream < key.txt (reads stdin; never put keys on command line)")
		}
		b, e := io.ReadAll(io.LimitReader(os.Stdin, 2561))
		if e != nil {
			return e
		}
		v := strings.TrimSpace(string(b))
		if v == "" || len(v) > 2560 {
			return errors.New("invalid credential length")
		}
		return secrets.Set("upstream-key", v)
	case "ports":
		p, e := serialport.List()
		if e == nil {
			b, _ := json.MarshalIndent(p, "", "  ")
			fmt.Println(string(b))
		}
		return e
	case "status":
		return printRequest(cfg, secrets, "GET", "/state", nil, 0)
	case "stop":
		return printRequest(cfg, secrets, "POST", "/shutdown", nil, 0)
	case "connect":
		f := flag.NewFlagSet("connect", flag.ContinueOnError)
		port := f.String("port", cfg.Get().SerialPort, "COM port")
		baud := f.Int("baud", 115200, "baud")
		flow := f.Bool("rtscts", false, "hardware flow control")
		if e = f.Parse(args[1:]); e != nil {
			return e
		}
		b, _ := json.Marshal(map[string]any{"port": *port, "baud": *baud, "flow_control": *flow})
		return printRequest(cfg, secrets, "POST", "/connect", bytes.NewReader(b), int64(len(b)))
	case "pair":
		if len(args) != 2 {
			return errors.New("usage: pair pairing.json")
		}
		f, e := os.Open(args[1])
		if e != nil {
			return e
		}
		defer f.Close()
		var p struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if e = json.NewDecoder(io.LimitReader(f, 8192)).Decode(&p); e != nil {
			return e
		}
		b, _ := json.Marshal(p)
		return printRequest(cfg, secrets, "POST", "/pair", bytes.NewReader(b), int64(len(b)))
	case "firmware":
		if len(args) != 2 {
			return errors.New("usage: firmware firmware.bin")
		}
		f, e := os.Open(args[1])
		if e != nil {
			return e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return e
		}
		return printRequest(cfg, secrets, "POST", "/firmware", f, st.Size())
	case "config":
		if len(args) == 1 || args[1] == "get" {
			return printRequest(cfg, secrets, "GET", "/config", nil, 0)
		}
		if args[1] == "schema" {
			return printRequest(cfg, secrets, "GET", "/config/schema", nil, 0)
		}
		if args[1] == "confirm" || args[1] == "rollback" {
			return printRequest(cfg, secrets, "POST", "/config/"+args[1], nil, 0)
		}
		if args[1] == "set" && len(args) == 3 {
			b, e := os.ReadFile(args[2])
			if e != nil {
				return e
			}
			return printRequest(cfg, secrets, "PATCH", "/config", bytes.NewReader(b), int64(len(b)))
		}
		return errors.New("usage: config get|schema|confirm|rollback|set patch.json")
	case "admin":
		if len(args) < 3 {
			return errors.New("usage: admin METHOD /path [json-file|-]")
		}
		var body io.Reader
		var size int64
		if len(args) > 3 {
			var b []byte
			if args[3] == "-" {
				b, e = io.ReadAll(io.LimitReader(os.Stdin, 65537))
			} else {
				b, e = os.ReadFile(args[3])
			}
			if e != nil {
				return e
			}
			body = bytes.NewReader(b)
			size = int64(len(b))
		}
		return printRequest(cfg, secrets, args[1], args[2], body, size)
	default:
		return errors.New("unknown command; run uart2llm help")
	}
}
func usage() {
	fmt.Println(`uart2llm - UART OpenAI gateway
Commands:
  init | serve | start | stop | status | tray | ports | version | licenses
  connect --port COM3 --baud 115200 [--rtscts]
  pair pairing.json
  token api|admin
  credentials set-upstream < key.txt
  config get|schema|confirm|rollback|set patch.json
  firmware firmware.bin
  admin METHOD /path [json-file|-]
Configuration directory: UART2LLM_DATA_DIR or %APPDATA%/uart2llm.
API defaults to http://localhost:8765/v1; management to http://localhost:8766.`)
}

// The same executable is the desktop launcher, detached daemon and CLI.
func launchDesktop(command func([]string) error, open func(string) error) error {
	if err := command([]string{"start"}); err != nil {
		return err
	}
	cfg, err := config.Open(config.DefaultDir())
	if err != nil {
		return err
	}
	return open("http://" + activeAdmin(cfg))
}
func activeAdmin(c *config.Store) string {
	address := c.Get().AdminListen
	b, e := os.ReadFile(filepath.Join(config.DefaultDir(), "runtime.json"))
	if e != nil {
		return address
	}
	var state struct {
		Admin string `json:"admin_listen"`
	}
	if json.Unmarshal(b, &state) == nil {
		h := c.Get()
		h.AdminListen = state.Admin
		if h.Validate() == nil {
			return state.Admin
		}
	}
	return address
}
func adminRequest(c *config.Store, s credentials.Store, method, path string, body io.Reader, size int64) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.Contains(path, "://") {
		return nil, errors.New("invalid admin path")
	}
	token, e := s.Get("admin-token")
	if e != nil {
		return nil, e
	}
	r, e := http.NewRequest(method, "http://"+activeAdmin(c)+"/admin/v1"+path, body)
	if e != nil {
		return nil, e
	}
	r.ContentLength = size
	r.Header.Set("Authorization", "Bearer "+token)
	if path == "/firmware" {
		r.Header.Set("Content-Type", "application/octet-stream")
	} else {
		r.Header.Set("Content-Type", "application/json")
	}
	headerTimeout := 2 * time.Minute
	if path == "/firmware" {
		headerTimeout = 0
	}
	cl := http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: headerTimeout}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, e := cl.Do(r)
	if e != nil {
		return nil, e
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, b)
	}
	return res, nil
}
func printRequest(c *config.Store, s credentials.Store, method, path string, body io.Reader, size int64) error {
	r, e := adminRequest(c, s, method, path, body, size)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	_, e = io.Copy(os.Stdout, r.Body)
	return e
}
func listen(address string) ([]net.Listener, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	hosts := []string{host}
	if host == "localhost" {
		hosts = []string{"127.0.0.1", "::1"}
	}
	var out []net.Listener
	for _, h := range hosts {
		l, e := net.Listen("tcp", net.JoinHostPort(h, port))
		if e != nil {
			for _, p := range out {
				p.Close()
			}
			return nil, e
		}
		out = append(out, l)
	}
	return out, nil
}
func serve(c *config.Store, s credentials.Store) error {
	u, e := user.Current()
	if e != nil {
		return e
	}
	sum := sha256.Sum256([]byte(u.Uid))
	release, e := platform.Singleton(fmt.Sprintf("uart2llm-%x", sum[:12]))
	if e != nil {
		return e
	}
	defer release()

	at, e := credentials.Ensure(s, "admin-token")
	if e != nil {
		return e
	}
	kt, e := credentials.Ensure(s, "api-token")
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	d := device.New(s)
	defer d.Disconnect()
	g := &gateway.Gateway{Config: c.Get, Dialer: d, Key: func() (string, error) { return s.Get("upstream-key") }, Token: kt}
	a := admin.New(c, s, d, g, at)
	a.Shutdown = cancel
	g.LogFailure = func(message string) { a.Log("error", message) }
	api := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 * 1024}
	mux := http.NewServeMux()
	mux.Handle("/admin/v1/", a)
	mux.Handle("/", ui.Handler())
	management := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 * 1024}
	al, e := listen(c.Get().APIListen)
	if e != nil {
		return e
	}
	defer func() {
		for _, l := range al {
			l.Close()
		}
	}()
	ml, e := listen(c.Get().AdminListen)
	if e != nil {
		return e
	}
	defer func() {
		for _, l := range ml {
			l.Close()
		}
	}()
	runtimePath := filepath.Join(config.DefaultDir(), "runtime.json")
	runtimeData, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "admin_listen": c.Get().AdminListen, "api_listen": c.Get().APIListen})
	if e = os.WriteFile(runtimePath, runtimeData, 0600); e != nil {
		return e
	}
	defer os.Remove(runtimePath)
	var wg sync.WaitGroup
	errorsC := make(chan error, 4)
	for _, pair := range []struct {
		s  *http.Server
		ls []net.Listener
	}{{api, al}, {management, ml}} {
		for _, l := range pair.ls {
			wg.Add(1)
			go func(server *http.Server, l net.Listener) {
				defer wg.Done()
				if e := server.Serve(l); e != nil && !errors.Is(e, http.ErrServerClosed) {
					errorsC <- e
				}
			}(pair.s, l)
		}
	}
	go a.Poll(ctx)
	trayDone := make(chan struct{})
	go func() { defer close(trayDone); runTray(ctx, a, c, s, d, g) }()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			cc, stop := context.WithTimeout(ctx, 35*time.Second)
			a.ReconnectOnce(cc)
			stop()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	a.Log("info", "daemon ready")
	fmt.Println("API:", c.Get().APIListen, "Management:", c.Get().AdminListen)
	fmt.Println("Configuration:", filepath.Join(config.DefaultDir(), "config.json"))
	select {
	case <-ctx.Done():
	case e = <-errorsC:
	}
	cancel()
	d.Disconnect()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = api.Shutdown(shutdown)
	_ = management.Shutdown(shutdown)
	_ = api.Close()
	_ = management.Close()
	wg.Wait()
	select {
	case <-trayDone:
	case <-time.After(time.Second):
	}
	return e
}
