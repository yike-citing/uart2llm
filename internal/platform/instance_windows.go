//go:build windows

package platform

import (
	"errors"
	"syscall"
	"unsafe"
)

// Hold a kernel object for the daemon lifetime; the OS releases it after a crash.
func Singleton(name string) (func(), error) {
	p, e := syscall.UTF16PtrFromString("Local\\" + name)
	if e != nil {
		return nil, e
	}
	kernel := syscall.NewLazyDLL("kernel32.dll")
	h, _, callErr := kernel.NewProc("CreateMutexW").Call(0, 0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return nil, callErr
	}
	if callErr == syscall.Errno(183) {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, errors.New("daemon already running for this user")
	}
	return func() { syscall.CloseHandle(syscall.Handle(h)) }, nil
}
