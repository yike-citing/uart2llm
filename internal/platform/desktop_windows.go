//go:build windows

package platform

import (
	"os"
	"syscall"
	"unsafe"
)

// GUI-subsystem builds attach only for explicit CLI commands. Preserve pipes
// supplied by callers, so version/token/automation keep working without a TTY.
func AttachConsole() {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	kernel.NewProc("AttachConsole").Call(uintptr(^uint32(0)))
	for _, stream := range []struct {
		file **os.File
		id   int
		name string
	}{{&os.Stdin, -10, "stdin"}, {&os.Stdout, -11, "stdout"}, {&os.Stderr, -12, "stderr"}} {
		if _, err := (*stream.file).Stat(); err == nil {
			continue
		}
		h, _, _ := kernel.NewProc("GetStdHandle").Call(uintptr(int32(stream.id)))
		if h != 0 && h != ^uintptr(0) {
			*stream.file = os.NewFile(h, stream.name)
		}
	}
}

func ShowError(title, message string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(message)
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0,
		uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
}
