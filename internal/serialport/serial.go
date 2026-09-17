package serialport

import "io"

type Port interface {
	io.ReadWriteCloser
	SetMode(baud int, flow bool) error
}
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
