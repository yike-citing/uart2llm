//go:build windows

package serialport

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var getCommState = kernel.NewProc("GetCommState")
var setCommState = kernel.NewProc("SetCommState")
var setCommTimeouts = kernel.NewProc("SetCommTimeouts")
var setupComm = kernel.NewProc("SetupComm")
var createEvent = kernel.NewProc("CreateEventW")
var resetEvent = kernel.NewProc("ResetEvent")
var getOverlapped = kernel.NewProc("GetOverlappedResult")
var cancelIO = kernel.NewProc("CancelIoEx")
var queryDosDevice = kernel.NewProc("QueryDosDeviceW")

type dcb struct {
	Length, Baud, Flags                            uint32
	Reserved, XonLim, XoffLim                      uint16
	ByteSize, Parity, StopBits                     byte
	XonChar, XoffChar, ErrorChar, EofChar, EvtChar byte
	Reserved1                                      uint16
}
type timeouts struct{ ReadInterval, ReadMultiplier, ReadConstant, WriteMultiplier, WriteConstant uint32 }
type port struct {
	h               syscall.Handle
	mu              sync.Mutex
	closed          bool
	readMu, writeMu sync.Mutex
}

func Open(name string, baud int, flow bool) (Port, error) {
	if !strings.HasPrefix(strings.ToUpper(name), "COM") {
		return nil, errors.New("expected Windows COM port")
	}
	for _, c := range name[3:] {
		if c < '0' || c > '9' {
			return nil, errors.New("invalid COM port")
		}
	}
	if len(name) <= 3 {
		return nil, errors.New("missing COM port number")
	}
	p, e := syscall.UTF16PtrFromString(`\\.\` + name)
	if e != nil {
		return nil, e
	}
	h, e := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED, 0)
	if e != nil {
		return nil, e
	}
	s := &port{h: h}
	if e = s.SetMode(baud, flow); e != nil {
		s.Close()
		return nil, e
	}
	setupComm.Call(uintptr(h), 65536, 65536)
	// MAXDWORD for both fields selects Windows' first-byte return mode.
	// With a zero multiplier short ACK/DATA frames can wait for the 100 ms
	// total timeout, serializing every stop-and-wait protocol exchange.
	t := timeouts{0xffffffff, 0xffffffff, 100, 0, 5000}
	r, _, e := setCommTimeouts.Call(uintptr(h), uintptr(unsafe.Pointer(&t)))
	if r == 0 {
		s.Close()
		return nil, fmt.Errorf("SetCommTimeouts: %w", e)
	}
	return s, nil
}
func (p *port) SetMode(baud int, flow bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return io.ErrClosedPipe
	}
	var d dcb
	d.Length = uint32(unsafe.Sizeof(d))
	r, _, e := getCommState.Call(uintptr(p.h), uintptr(unsafe.Pointer(&d)))
	if r == 0 {
		return e
	}
	d.Baud = uint32(baud)
	d.ByteSize = 8
	d.Parity = 0
	d.StopBits = 0
	d.Flags = 1
	if flow {
		d.Flags |= 8 | (2 << 12)
	}
	r, _, e = setCommState.Call(uintptr(p.h), uintptr(unsafe.Pointer(&d)))
	if r == 0 {
		return fmt.Errorf("SetCommState: %w", e)
	}
	return nil
}
func (p *port) transfer(b []byte, write bool) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	h := p.h
	p.mu.Unlock()
	ev, _, e := createEvent.Call(0, 1, 0, 0)
	if ev == 0 {
		return 0, e
	}
	defer syscall.CloseHandle(syscall.Handle(ev))
	ov := syscall.Overlapped{HEvent: syscall.Handle(ev)}
	var n uint32
	// Submit while holding the close-state lock. Otherwise Close could cancel
	// all pending operations just before this call submits a new one, leaving
	// shutdown waiting on an operation that missed cancellation.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if write {
		e = syscall.WriteFile(h, b, &n, &ov)
	} else {
		e = syscall.ReadFile(h, b, &n, &ov)
	}
	p.mu.Unlock()
	if e == syscall.ERROR_IO_PENDING {
		r, _, we := getOverlapped.Call(uintptr(h), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 1)
		if r == 0 {
			return int(n), we
		}
		e = nil
	}
	return int(n), e
}
func (p *port) Read(b []byte) (int, error) {
	p.readMu.Lock()
	defer p.readMu.Unlock()
	for {
		n, e := p.transfer(b, false)
		if n > 0 || e != nil {
			return n, e
		}
	}
}
func (p *port) Write(b []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.transfer(b, true)
}
func (p *port) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	cancelIO.Call(uintptr(p.h), 0)
	p.readMu.Lock()
	defer p.readMu.Unlock()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return syscall.CloseHandle(p.h)
}
func List() ([]Info, error) {
	b := make([]uint16, 65536)
	n, _, e := queryDosDevice.Call(0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	if n == 0 {
		return nil, e
	}
	out := []Info{}
	start := 0
	for i := 0; i < int(n); i++ {
		if b[i] != 0 {
			continue
		}
		s := syscall.UTF16ToString(b[start:i])
		start = i + 1
		if strings.HasPrefix(s, "COM") && len(s) > 3 {
			valid := true
			for _, c := range s[3:] {
				if c < '0' || c > '9' {
					valid = false
				}
			}
			if valid {
				out = append(out, Info{s, "Windows serial port"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
