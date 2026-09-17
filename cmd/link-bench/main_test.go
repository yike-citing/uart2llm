package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestEchoMatchesAcrossFragmentation(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go func() {
		buf := make([]byte, 113)
		for {
			n, err := b.Read(buf)
			if n > 0 {
				if _, e := b.Write(buf[:n]); e != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	r := transfer(ctx, a, 2, 65539)
	if !r.Matched || r.Sent != 65539 || r.Received != 65539 {
		t.Fatalf("unexpected result: %+v", r)
	}
}
func TestCorruptionFailsChecksum(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go func() {
		buf := make([]byte, 1024)
		_, err := io.ReadFull(b, buf)
		if err == nil {
			buf[200] ^= 1
			_, _ = b.Write(buf)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	r := transfer(ctx, a, 0, 1024)
	if r.Matched || r.SendSHA256 == r.ReceiveSHA256 {
		t.Fatal("corruption was not detected")
	}
}
func TestCancellationUnblocksBothDirections(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan streamResult, 1)
	go func() { done <- transfer(ctx, a, 0, 1024) }()
	select {
	case r := <-done:
		if r.Matched || r.Error == "" {
			t.Fatal("cancelled transfer reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("transfer hung")
	}
}
