//go:build windows

package credentials

import (
	"fmt"
	"syscall"
	"unsafe"
)

var advapi = syscall.NewLazyDLL("advapi32.dll")
var credRead = advapi.NewProc("CredReadW")
var credWrite = advapi.NewProc("CredWriteW")
var credDelete = advapi.NewProc("CredDeleteW")
var credFree = advapi.NewProc("CredFree")

type credential struct {
	Flags, Type             uint32
	TargetName, Comment     *uint16
	LastWritten             syscall.Filetime
	BlobSize                uint32
	Blob                    *byte
	Persist, AttributeCount uint32
	Attributes              uintptr
	TargetAlias, UserName   *uint16
}
type Windows struct{}

func New(string) (Store, error) { return Windows{}, nil }
func (Windows) Get(key string) (string, error) {
	p, e := syscall.UTF16PtrFromString("uart2llm/" + key)
	if e != nil {
		return "", e
	}
	var c *credential
	r, _, e := credRead.Call(uintptr(unsafe.Pointer(p)), 1, 0, uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		if e == syscall.Errno(1168) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("CredRead: %w", e)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(c)))
	if c.BlobSize == 0 {
		return "", nil
	}
	return string(unsafe.Slice(c.Blob, int(c.BlobSize))), nil
}
func (Windows) Set(key, value string) error {
	p, e := syscall.UTF16PtrFromString("uart2llm/" + key)
	if e != nil {
		return e
	}
	b := []byte(value)
	if len(b) > 2560 {
		return fmt.Errorf("credential exceeds Windows limit")
	}
	c := credential{Type: 1, TargetName: p, Persist: 2, BlobSize: uint32(len(b))}
	if len(b) > 0 {
		c.Blob = &b[0]
	}
	r, _, e := credWrite.Call(uintptr(unsafe.Pointer(&c)), 0)
	if r == 0 {
		return fmt.Errorf("CredWrite: %w", e)
	}
	return nil
}
func (Windows) Delete(key string) error {
	p, e := syscall.UTF16PtrFromString("uart2llm/" + key)
	if e != nil {
		return e
	}
	r, _, e := credDelete.Call(uintptr(unsafe.Pointer(p)), 1, 0)
	if r == 0 && e != syscall.Errno(1168) {
		return e
	}
	return nil
}
