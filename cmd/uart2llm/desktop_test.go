package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopReusesRunningDaemonAddress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UART2LLM_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), []byte(`{"admin_listen":"[::1]:9876"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var commands int
	var opened string
	err := launchDesktop(func(args []string) error {
		commands++
		if len(args) != 1 || args[0] != "start" {
			t.Fatalf("unexpected command: %v", args)
		}
		return nil
	}, func(url string) error { opened = url; return nil })
	if err != nil || commands != 1 || opened != "http://[::1]:9876" {
		t.Fatalf("desktop launch: %v, commands=%d, url=%q", err, commands, opened)
	}
}

func TestDesktopDoesNotOpenOnStartupFailure(t *testing.T) {
	want := errors.New("port unavailable")
	err := launchDesktop(func([]string) error { return want }, func(string) error {
		t.Fatal("browser must not open after failed startup")
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("lost startup error: %v", err)
	}
}

func TestDesktopRejectsRemoteRuntimeURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UART2LLM_DATA_DIR", dir)
	os.WriteFile(filepath.Join(dir, "runtime.json"), []byte(`{"admin_listen":"example.com:9876"}`), 0600)
	var opened string
	err := launchDesktop(func([]string) error { return nil }, func(url string) error { opened = url; return nil })
	if err != nil || opened != "http://localhost:8766" {
		t.Fatalf("untrusted runtime address used: %s %v", opened, err)
	}
}
