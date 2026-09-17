//go:build !windows

package tray

import (
	"context"
	"errors"
)

func ShowStatus() error { return errors.New("tray status is available on Windows") }

func ShowMenu() error                          { return errors.New("tray menu is available on Windows") }
func Run(ctx context.Context, _ Options) error { <-ctx.Done(); return nil }
func Open(string) error                        { return errors.New("desktop integration is available on Windows") }
