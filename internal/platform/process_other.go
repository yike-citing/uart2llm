//go:build !windows

package platform

import "os/exec"

func Detached(cmd *exec.Cmd) {}
