//go:build windows

package platform

import (
	"fmt"
	"os"
	"testing"
)

func TestSingletonLifecycle(t *testing.T) {
	name := fmt.Sprintf("uart2llm-test-%d", os.Getpid())
	release, e := Singleton(name)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := Singleton(name); e == nil {
		second()
		release()
		t.Fatal("duplicate instance accepted")
	}
	release()
	again, e := Singleton(name)
	if e != nil {
		t.Fatal(e)
	}
	again()
}
