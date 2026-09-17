//go:build windows

package platform

import (
	"os/exec"
	"syscall"
)

func Detached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000200 | 0x08000000}
}
