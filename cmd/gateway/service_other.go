//go:build !windows

package main

import (
	"context"
	"errors"
)

func isWindowsService() bool {
	return false
}

func runWindowsService(name string, runner func(ctx context.Context, elog EventLogger) error) error {
	return errors.New("windows services are not supported on this platform")
}
