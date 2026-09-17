//go:build windows

package tray

import (
	"testing"
	"unsafe"
)

func TestWindowsTrayABI(t *testing.T) {
	if unsafe.Sizeof(notifyData{}) != 976 || unsafe.Sizeof(windowClass{}) != 80 || unsafe.Sizeof(message{}) != 48 {
		t.Fatalf("unexpected x64 Win32 ABI: %d %d %d", unsafe.Sizeof(notifyData{}), unsafe.Sizeof(windowClass{}), unsafe.Sizeof(message{}))
	}
}
