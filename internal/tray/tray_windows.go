//go:build windows

package tray

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	user32      = syscall.NewLazyDLL("user32.dll")
	shell32     = syscall.NewLazyDLL("shell32.dll")
	kernel32    = syscall.NewLazyDLL("kernel32.dll")
	notify      = shell32.NewProc("Shell_NotifyIconW")
	defWindow   = user32.NewProc("DefWindowProcW")
	postMessage = user32.NewProc("PostMessageW")
)

const (
	wmIcon   = 0x8001
	wmResult = 0x8002
	wmClose  = 0x0010
)

type point struct{ X, Y int32 }
type message struct {
	Window         uintptr
	ID             uint32
	WParam, LParam uintptr
	Time           uint32
	Point          point
	Private        uint32
}
type windowClass struct {
	Size, Style                        uint32
	Procedure                          uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
	SmallIcon                          uintptr
}
type notifyData struct {
	Size                uint32
	Window              uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Version             uint32
	Title               [64]uint16
	InfoFlags           uint32
	GUID                [16]byte
	BalloonIcon         uintptr
}

func wide(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return p
}
func copyWide(dst []uint16, s string) {
	v := utf16.Encode([]rune(s))
	if len(v) >= len(dst) {
		v = v[:len(dst)-1]
		if len(v) > 0 && v[len(v)-1] >= 0xD800 && v[len(v)-1] <= 0xDBFF {
			v = v[:len(v)-1]
		}
	}
	clear(dst)
	copy(dst, v)
}

type desktop struct {
	opts           Options
	ctx            context.Context
	window         uintptr
	data           notifyData
	icons          [3]uintptr
	busy           atomic.Bool
	resultMu       sync.Mutex
	result         string
	taskbarCreated uint32
	added          bool
}

func Run(ctx context.Context, opts Options) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	d := &desktop{opts: opts, ctx: ctx}
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	className := wide("uart2llm.NotificationWindow")
	proc := syscall.NewCallback(d.procedure)
	class := windowClass{Size: uint32(unsafe.Sizeof(windowClass{})), Procedure: proc, Instance: instance, ClassName: className}
	if value, _, err := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class))); value == 0 {
		return fmt.Errorf("register tray window: %v", err)
	}
	defer user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(className)), instance)
	hwnd, _, err := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(wide("uart2llm tray"))), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return fmt.Errorf("create tray window: %v", err)
	}
	d.window = hwnd
	defer user32.NewProc("DestroyWindow").Call(hwnd)
	for i, color := range []uint32{0xFF21966F, 0xFFE09A27, 0xFF738294} {
		d.icons[i] = makeIcon(color)
		defer user32.NewProc("DestroyIcon").Call(d.icons[i])
	}
	id, _, _ := user32.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(wide("TaskbarCreated"))))
	d.taskbarCreated = uint32(id)
	d.data = notifyData{Size: uint32(unsafe.Sizeof(notifyData{})), Window: hwnd, ID: 1, Flags: 1 | 2 | 4 | 128, Callback: wmIcon}
	d.update()
	if !d.added {
		return fmt.Errorf("Windows notification area unavailable")
	}
	defer notify.Call(2, uintptr(unsafe.Pointer(&d.data)))
	user32.NewProc("SetTimer").Call(hwnd, 1, 1000, 0)
	defer user32.NewProc("KillTimer").Call(hwnd, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			postMessage.Call(hwnd, wmClose, 0, 0)
		case <-done:
		}
	}()
	var msg message
	for {
		value, _, err := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(value) == -1 {
			return fmt.Errorf("tray message loop: %v", err)
		}
		if value == 0 {
			return nil
		}
		user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
		user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
	}
}
func makeIcon(color uint32) uintptr {
	var pixels [32 * 32]uint32
	var mask [128]byte
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			if (x-16)*(x-16)+(y-16)*(y-16) <= 225 {
				pixels[y*32+x] = color
				if (x >= 9 && x <= 12 && y >= 8 && y <= 21) || (x >= 20 && x <= 23 && y >= 8 && y <= 21) || (x >= 12 && x <= 20 && y >= 21 && y <= 24) {
					pixels[y*32+x] = 0xFFFFFFFF
				}
			}
		}
	}
	icon, _, _ := user32.NewProc("CreateIcon").Call(0, 32, 32, 1, 32, uintptr(unsafe.Pointer(&mask[0])), uintptr(unsafe.Pointer(&pixels[0])))
	runtime.KeepAlive(pixels)
	runtime.KeepAlive(mask)
	if icon == 0 {
		icon, _, _ = user32.NewProc("LoadIconW").Call(0, 32512)
	}
	return icon
}
func (d *desktop) update() {
	s := d.opts.Snapshot()
	i := 0
	if !s.Fresh() || !s.Device.Network.Connected {
		i = 2
	}
	if s.LLM.Paused {
		i = 1
	}
	d.data.Icon = d.icons[i]
	copyWide(d.data.Tip[:], fmt.Sprintf("uart2llm · %s\n活跃 %d · 请求 %d · Token %d", s.Status(), s.LLM.Active, s.LLM.Requests, s.LLM.TotalTokens))
	cmd := uintptr(1)
	if !d.added {
		cmd = 0
	}
	ok, _, _ := notify.Call(cmd, uintptr(unsafe.Pointer(&d.data)))
	if ok != 0 && !d.added {
		d.added = true
		d.data.Version = 4
		notify.Call(4, uintptr(unsafe.Pointer(&d.data)))
	}
}
func (d *desktop) procedure(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	if d.taskbarCreated != 0 && msg == d.taskbarCreated {
		d.added = false
		d.update()
		return 0
	}
	switch msg {
	case 0x0113:
		d.update()
		return 0
	case wmClose:
		user32.NewProc("PostQuitMessage").Call(0)
		return 0
	case wmIcon:
		switch uint32(lparam) & 0xffff {
		case 0x007B, 0x0205:
			d.popup()
		case 0x0203, 0x0401:
			d.command(OpenWeb)
		}
		return 0
	case 0x8003:
		d.command(Details)
		return 0
	case wmResult:
		d.resultMu.Lock()
		text := d.result
		d.resultMu.Unlock()
		d.update()
		if text != "" {
			user32.NewProc("MessageBoxW").Call(d.window, uintptr(unsafe.Pointer(wide(text))), uintptr(unsafe.Pointer(wide("uart2llm"))), 0x10)
		}
		return 0
	}
	value, _, _ := defWindow.Call(hwnd, uintptr(msg), wparam, lparam)
	return value
}
func createMenu(items []Item) uintptr {
	h, _, _ := user32.NewProc("CreatePopupMenu").Call()
	for _, item := range items {
		flags := uintptr(0)
		id := uintptr(item.Action)
		if item.Label == "" {
			flags = 0x800
		} else if len(item.Children) > 0 {
			flags = 0x10
			id = createMenu(item.Children)
		} else if !item.Enabled {
			flags = 1
		}
		user32.NewProc("AppendMenuW").Call(h, flags, id, uintptr(unsafe.Pointer(wide(item.Label))))
	}
	return h
}
func (d *desktop) popup() {
	h := createMenu(d.opts.Snapshot().Menu(d.busy.Load()))
	defer user32.NewProc("DestroyMenu").Call(h)
	var cursor point
	user32.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&cursor)))
	user32.NewProc("SetForegroundWindow").Call(d.window)
	id, _, _ := user32.NewProc("TrackPopupMenu").Call(h, 0x100|0x2, uintptr(cursor.X), uintptr(cursor.Y), 0, d.window, 0)
	postMessage.Call(d.window, 0, 0, 0)
	if id != 0 {
		d.command(Action(id))
	}
}
func (d *desktop) command(action Action) {
	s := d.opts.Snapshot()
	if action == Details {
		user32.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(wide(s.Summary()))), uintptr(unsafe.Pointer(wide("uart2llm 状态与 LLM 统计"))), 0)
		return
	}
	if action == CopyAPI {
		if err := clipboard(d.window, s.API); err != nil {
			d.opts.Log(err.Error())
		}
		return
	}
	if action == Refresh {
		d.update()
		return
	}
	if action == Reboot || action == Quit || ((action == Connect || action == Disconnect) && s.LLM.Active > 0) {
		prompt := "执行此操作将中断当前设备连接和请求，是否继续？"
		if action == Quit {
			prompt = "退出本地代理？托盘图标会消失，现有请求将中断。"
		}
		answer, _, _ := user32.NewProc("MessageBoxW").Call(d.window, uintptr(unsafe.Pointer(wide(prompt))), uintptr(unsafe.Pointer(wide("uart2llm"))), 0x4|0x30|0x100)
		if answer != 6 {
			return
		}
	}
	if !d.busy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		err := d.opts.Action(d.ctx, action)
		d.resultMu.Lock()
		d.result = ""
		if err != nil {
			d.result = err.Error()
		}
		d.resultMu.Unlock()
		d.busy.Store(false)
		postMessage.Call(d.window, wmResult, 0, 0)
	}()
}

func Open(target string) error {
	result, _, err := shell32.NewProc("ShellExecuteW").Call(0, uintptr(unsafe.Pointer(wide("open"))), uintptr(unsafe.Pointer(wide(target))), 0, 0, 1)
	if result <= 32 {
		return fmt.Errorf("无法打开管理入口：%v", err)
	}
	return nil
}

// ShowMenu makes the menu reachable when Windows hides the icon in overflow.
func ShowMenu() error {
	hwnd, _, _ := user32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(wide("uart2llm.NotificationWindow"))), 0)
	if hwnd == 0 {
		return fmt.Errorf("后台托盘尚未运行，请先启动代理")
	}
	ok, _, err := postMessage.Call(hwnd, wmIcon, 0, 0x007B)
	if ok == 0 {
		return fmt.Errorf("无法打开托盘菜单：%v", err)
	}
	return nil
}

func ShowStatus() error {
	hwnd, _, _ := user32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(wide("uart2llm.NotificationWindow"))), 0)
	if hwnd == 0 {
		return fmt.Errorf("后台托盘尚未运行，请先启动代理")
	}
	ok, _, err := postMessage.Call(hwnd, 0x8003, 0, 0)
	if ok == 0 {
		return fmt.Errorf("无法打开托盘状态：%v", err)
	}
	return nil
}

func clipboard(hwnd uintptr, text string) error {
	ok, _, _ := user32.NewProc("OpenClipboard").Call(hwnd)
	if ok == 0 {
		return fmt.Errorf("剪贴板正被占用")
	}
	defer user32.NewProc("CloseClipboard").Call()
	v := utf16.Encode([]rune(text))
	v = append(v, 0)
	h, _, _ := kernel32.NewProc("GlobalAlloc").Call(2, uintptr(len(v)*2))
	if h == 0 {
		return fmt.Errorf("无法分配剪贴板内存")
	}
	p, _, _ := kernel32.NewProc("GlobalLock").Call(h)
	if p == 0 {
		kernel32.NewProc("GlobalFree").Call(h)
		return fmt.Errorf("无法写入剪贴板")
	}
	// GlobalLock returns OS-owned memory, not a Go pointer. Copy through the
	// Windows ABI instead of constructing a Go slice from an integer address.
	kernel32.NewProc("RtlMoveMemory").Call(p, uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2))
	runtime.KeepAlive(v)
	kernel32.NewProc("GlobalUnlock").Call(h)
	user32.NewProc("EmptyClipboard").Call()
	ok, _, _ = user32.NewProc("SetClipboardData").Call(13, h)
	if ok == 0 {
		kernel32.NewProc("GlobalFree").Call(h)
		return fmt.Errorf("无法更新剪贴板")
	}
	return nil
}
