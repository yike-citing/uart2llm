//go:build !windows

package serialport

import "errors"

func Open(string, int, bool) (Port, error) {
	return nil, errors.New("UART hardware access currently supports Windows only")
}
func List() ([]Info, error) { return []Info{}, nil }
