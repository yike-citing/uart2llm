// Package link implements the bounded UART byte tunnel described in protocol.md.
package link

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const MaxPayload = 4096
const DataPayload = 1024
const ChannelCount = 8
const (
	ChannelManagement uint8 = 0
	ChannelTelemetry  uint8 = 5
	ChannelLogs       uint8 = 6
	ChannelOTA        uint8 = 7
)
const (
	Hello byte = iota + 1
	HelloAck
	Ack
	Open
	Opened
	Data
	CloseFrame
	RPCFrame
	RPCReply
	ErrorFrame
	Busy
	Abort
)

type Frame struct {
	Kind     byte
	Session  uint32
	Channel  byte
	Sequence uint32
	Payload  []byte
}

func Encode(f Frame) ([]byte, error) {
	if len(f.Payload) > MaxPayload || f.Channel >= ChannelCount {
		return nil, errors.New("invalid frame size or channel")
	}
	b := make([]byte, 18+len(f.Payload)+4)
	binary.LittleEndian.PutUint16(b, 0x3255)
	b[2] = 1
	b[3] = f.Kind
	binary.LittleEndian.PutUint32(b[4:], f.Session)
	b[8] = f.Channel
	binary.LittleEndian.PutUint32(b[10:], f.Sequence)
	binary.LittleEndian.PutUint16(b[14:], uint16(len(f.Payload)))
	copy(b[18:], f.Payload)
	binary.LittleEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
	out := make([]byte, 1, len(b)+len(b)/254+2)
	codeAt, code := 0, byte(1)
	for _, v := range b {
		if v == 0 {
			out[codeAt] = code
			codeAt = len(out)
			out = append(out, 0)
			code = 1
		} else {
			out = append(out, v)
			code++
			if code == 255 {
				out[codeAt] = code
				codeAt = len(out)
				out = append(out, 0)
				code = 1
			}
		}
	}
	out[codeAt] = code
	return append(out, 0), nil
}

// Decode accepts encoded bytes excluding the trailing delimiter.
func Decode(encoded []byte) (Frame, error) {
	bad := errors.New("invalid UART frame")
	if len(encoded) > MaxPayload+64 {
		return Frame{}, bad
	}
	b := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); {
		code := int(encoded[i])
		i++
		if code == 0 || i+code-1 > len(encoded) {
			return Frame{}, bad
		}
		b = append(b, encoded[i:i+code-1]...)
		i += code - 1
		if code < 255 && i < len(encoded) {
			b = append(b, 0)
		}
	}
	if len(b) < 22 || binary.LittleEndian.Uint16(b) != 0x3255 || b[2] != 1 || b[8] >= ChannelCount || b[9] != 0 || binary.LittleEndian.Uint16(b[16:]) != 0 {
		return Frame{}, bad
	}
	n := int(binary.LittleEndian.Uint16(b[14:]))
	if n > MaxPayload || len(b) != 22+n || crc32.ChecksumIEEE(b[:len(b)-4]) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return Frame{}, bad
	}
	if b[3] < Hello || b[3] > Abort {
		return Frame{}, bad
	}
	return Frame{b[3], binary.LittleEndian.Uint32(b[4:]), b[8], binary.LittleEndian.Uint32(b[10:]), b[18 : 18+n]}, nil
}
