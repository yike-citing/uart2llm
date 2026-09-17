// Package tray supplies a daemon-owned Windows notification icon. Its state
// model stays portable; later desktop ports can implement the same interface.
package tray

import (
	"context"
	"fmt"
	"strings"
	"time"

	"uart2llm/internal/gateway"
)

type Action uint32

const (
	OpenWeb Action = iota + 100
	OpenNative
	CopyAPI
	TogglePause
	Connect
	Disconnect
	Reboot
	Refresh
	Details
	OpenConfig
	Quit
)

type Snapshot struct {
	API, Admin, ConfigDir string
	SampledAt             time.Time `json:"sampled_at"`
	Host                  struct {
		Uptime int64 `json:"uptime_seconds"`
	} `json:"host"`
	Connection struct {
		Connected bool   `json:"connected"`
		Paired    bool   `json:"paired"`
		Port      string `json:"port"`
		Baud      int    `json:"baud"`
		Link      struct {
			Session uint32 `json:"session"`
			Retries uint64 `json:"retries"`
			Invalid uint64 `json:"invalid_frames"`
		} `json:"link"`
	} `json:"connection"`
	Device *struct {
		Transport   string   `json:"transport"`
		Session     uint32   `json:"session"`
		Uptime      float64  `json:"uptime_ms"`
		Temperature *float64 `json:"temperature_celsius"`
		Memory      struct {
			Free  float64 `json:"internal_free"`
			PSRAM float64 `json:"psram_free"`
			Tasks float64 `json:"tasks"`
		} `json:"memory"`
		Network struct {
			Connected bool     `json:"connected"`
			RSSI      *float64 `json:"rssi_dbm"`
			IP        string   `json:"address"`
		} `json:"network"`
	} `json:"device"`
	LLM gateway.Statistics `json:"llm"`
}
type Options struct {
	Snapshot func() Snapshot
	Action   func(context.Context, Action) error
	Log      func(string)
}
type Item struct {
	Label    string
	Action   Action
	Enabled  bool
	Children []Item
}

func (s Snapshot) Fresh() bool {
	return s.Connection.Connected && s.Connection.Paired && s.Device != nil && s.Device.Session == s.Connection.Link.Session && !s.SampledAt.IsZero() && time.Since(s.SampledAt) < 5*time.Second
}
func (s Snapshot) Status() string {
	if s.LLM.Paused {
		return "已暂停接受新请求"
	}
	if !s.Connection.Connected {
		return "设备未连接"
	}
	if !s.Connection.Paired {
		return "设备未配对"
	}
	if !s.Fresh() {
		return "等待设备状态"
	}
	if !s.Device.Network.Connected {
		return "Wi-Fi 未连接"
	}
	if s.LLM.Active > 0 {
		return fmt.Sprintf("正在处理 %d 个请求", s.LLM.Active)
	}
	return "已就绪"
}
func row(s string) Item { return Item{Label: s} }
func (s Snapshot) Telemetry() []Item {
	transport := "传输方式：等待设备确认"
	if s.Fresh() {
		if strings.Contains(s.Device.Transport, "usb") {
			transport = "传输方式：原生 USB（不受 UART 波特率限制）"
		} else if s.Device.Transport == "uart" {
			transport = fmt.Sprintf("UART：%d baud", s.Connection.Baud)
		}
	}
	rows := []Item{row("设备：" + s.Status()), row("端口：" + s.Connection.Port), row(transport), row(fmt.Sprintf("重传 / 无效帧：%d / %d", s.Connection.Link.Retries, s.Connection.Link.Invalid)), row(fmt.Sprintf("后台运行：%s", (time.Duration(s.Host.Uptime) * time.Second).String()))}
	if !s.Fresh() {
		return append(rows, row("设备遥测不可用或已过期"))
	}
	wifi := "未连接"
	if s.Device.Network.Connected {
		wifi = "已连接"
	}
	rows = append(rows, row("Wi-Fi："+wifi))
	if s.Device.Network.RSSI != nil {
		rows = append(rows, row(fmt.Sprintf("信号：%.0f dBm", *s.Device.Network.RSSI)))
	}
	if s.Device.Network.IP != "" {
		rows = append(rows, row("IP："+s.Device.Network.IP))
	}
	rows = append(rows, row(fmt.Sprintf("内部可用内存：%.1f KiB", s.Device.Memory.Free/1024)), row(fmt.Sprintf("PSRAM 可用：%.2f MiB", s.Device.Memory.PSRAM/1048576)), row(fmt.Sprintf("设备任务数：%.0f", s.Device.Memory.Tasks)))
	if s.Device.Temperature != nil {
		rows = append(rows, row(fmt.Sprintf("芯片温度：%.1f °C", *s.Device.Temperature)))
	}
	return append(rows, row("采样："+s.SampledAt.Local().Format("15:04:05")))
}
func (s Snapshot) Statistics() []Item {
	v := s.LLM
	model := v.LastModel
	if model == "" {
		model = "尚无模型响应"
	}
	return []Item{row("范围：本次后台运行（重启清零）"), row(fmt.Sprintf("请求：%d   活跃：%d", v.Requests, v.Active)), row(fmt.Sprintf("成功 / 失败：%d / %d", v.Succeeded, v.Failed)), row(fmt.Sprintf("拒绝 / 中断：%d / %d", v.Rejected, v.Cancelled)), row(fmt.Sprintf("最近 60 秒请求：%d", v.RecentRequests)), row("最近模型：" + model), row(fmt.Sprintf("输入 / 输出 Token：%d / %d", v.PromptTokens, v.CompletionTokens)), row(fmt.Sprintf("总 Token / 缓存命中：%d / %d", v.TotalTokens, v.CachedTokens)), row(fmt.Sprintf("usage 已报告 / 缺失：%d / %d", v.UsageReported, v.UsageMissing)), row(fmt.Sprintf("平均首响应字节：%.0f ms", v.AverageFirstByteMS)), row(fmt.Sprintf("平均请求时长：%.0f ms", v.AverageDurationMS)), row(fmt.Sprintf("已读请求 / 已写响应：%.1f / %.1f KiB", float64(v.RequestBytes)/1024, float64(v.ResponseBytes)/1024)), row("费用：未设置价格，不估算账单")}
}
func (s Snapshot) Menu(busy bool) []Item {
	action := func(label string, id Action) Item { return Item{Label: label, Action: id, Enabled: !busy} }
	pause := "暂停接受新请求"
	if s.LLM.Paused {
		pause = "恢复代理服务"
	}
	connect := action("连接配置的设备", Connect)
	disconnect := action("断开设备", Disconnect)
	disconnect.Enabled = disconnect.Enabled && s.Connection.Connected
	reboot := action("重启 ESP32…", Reboot)
	reboot.Enabled = reboot.Enabled && s.Connection.Paired
	return []Item{row("uart2llm · " + s.Status()), action("打开 Web 管理", OpenWeb), action("打开管理窗口", OpenNative), action("复制 API 地址", CopyAPI), {Label: "设备状态与遥测", Enabled: true, Children: s.Telemetry()}, {Label: "LLM 统计", Enabled: true, Children: s.Statistics()}, action("查看完整状态摘要", Details), row(""), action(pause, TogglePause), connect, disconnect, reboot, action("刷新状态", Refresh), action("打开配置目录", OpenConfig), row(""), action("退出代理…", Quit)}
}
func (s Snapshot) Summary() string {
	lines := []string{"uart2llm · " + s.Status(), "API：" + s.API, "管理：" + s.Admin, ""}
	for _, r := range s.Telemetry() {
		lines = append(lines, r.Label)
	}
	lines = append(lines, "", "LLM 统计")
	for _, r := range s.Statistics() {
		lines = append(lines, r.Label)
	}
	return strings.Join(lines, "\n")
}
