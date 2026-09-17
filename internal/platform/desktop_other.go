//go:build !windows

package platform

func AttachConsole()           {}
func ShowError(string, string) {}
