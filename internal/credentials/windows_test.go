//go:build windows

package credentials

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestWindowsCredentialRoundTrip(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	key := fmt.Sprintf("test-%d-%d", os.Getpid(), time.Now().UnixNano())
	defer s.Delete(key)
	if _, e = s.Get(key); !errors.Is(e, ErrNotFound) {
		t.Fatalf("expected missing: %v", e)
	}
	const value = "test-only-凭据-π"
	if e = s.Set(key, value); e != nil {
		t.Fatal(e)
	}
	got, e := s.Get(key)
	if e != nil || got != value {
		t.Fatal("credential roundtrip mismatch", e)
	}
	if e = s.Delete(key); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(key); !errors.Is(e, ErrNotFound) {
		t.Fatal("credential delete failed", e)
	}
}
