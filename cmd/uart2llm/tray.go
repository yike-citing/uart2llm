package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"uart2llm/internal/admin"
	"uart2llm/internal/config"
	"uart2llm/internal/credentials"
	"uart2llm/internal/device"
	"uart2llm/internal/gateway"
	"uart2llm/internal/platform"
	"uart2llm/internal/tray"
)

func runTray(ctx context.Context, a *admin.Server, c *config.Store, s credentials.Store, d *device.Manager, g *gateway.Gateway) {
	apiAddress, adminAddress := c.Get().APIListen, c.Get().AdminListen
	opts := tray.Options{Snapshot: func() tray.Snapshot {
		data := a.Snapshot()
		data["connection"] = d.Status()
		data["llm"] = g.Statistics()
		raw, _ := json.Marshal(data)
		var snapshot tray.Snapshot
		_ = json.Unmarshal(raw, &snapshot)
		snapshot.API = "http://" + apiAddress + "/v1"
		snapshot.Admin = "http://" + adminAddress
		snapshot.ConfigDir = config.DefaultDir()
		return snapshot
	}, Log: func(message string) { a.Log("warning", message) }}
	opts.Action = func(ctx context.Context, action tray.Action) error {
		if action == tray.OpenWeb {
			return tray.Open("http://" + adminAddress)
		}
		if action == tray.OpenConfig {
			return tray.Open(config.DefaultDir())
		}
		if action == tray.OpenNative {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			path := filepath.Join(filepath.Dir(exe), "uart2llm-gui.exe")
			if _, err = os.Stat(path); err != nil {
				path = filepath.Join(filepath.Dir(exe), "..", "native", "build", "uart2llm-gui.exe")
				if _, err = os.Stat(path); err != nil {
					return tray.Open("http://" + adminAddress)
				}
			}
			cmd := exec.Command(path)
			platform.Detached(cmd)
			if err = cmd.Start(); err != nil {
				return err
			}
			return cmd.Process.Release()
		}
		path := ""
		var body any = map[string]any{}
		switch action {
		case tray.TogglePause:
			path = "/proxy/pause"
			body = map[string]any{"paused": !g.UserPaused()}
		case tray.Connect:
			cfg := c.Get()
			if cfg.SerialPort == "" {
				return fmt.Errorf("尚未配置串口，请先打开管理界面选择设备")
			}
			path = "/connect"
			body = map[string]any{"port": cfg.SerialPort, "baud": cfg.Baud, "flow_control": cfg.FlowControl}
		case tray.Disconnect:
			path = "/disconnect"
		case tray.Reboot:
			path = "/device/action"
			body = map[string]any{"action": "reboot"}
		case tray.Quit:
			path = "/shutdown"
		default:
			return nil
		}
		secret, err := s.Get("admin-token")
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", "http://"+adminAddress+"/admin/v1"+path, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		transport := &http.Transport{Proxy: nil}
		defer transport.CloseIdleConnections()
		client := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(req)
		if err != nil {
			if action == tray.Quit && ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("管理操作失败：%w", err)
		}
		defer response.Body.Close()
		reply, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		if response.StatusCode >= 300 {
			return fmt.Errorf("管理操作未完成（HTTP %d）：%s", response.StatusCode, reply)
		}
		return nil
	}
	if err := tray.Run(ctx, opts); err != nil {
		a.Log("warning", "托盘不可用，后台仍运行："+err.Error())
	}
}
