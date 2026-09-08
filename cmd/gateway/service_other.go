//go:build !windows

package main

import (
	"context"
	"errors"

	"golang.org/x/sys/windows/svc/eventlog"
)

func isWindowsService() bool {
	return false
}

func runWindowsService(name string, runner func(ctx context.Context, elog *eventlog.Log) error) error {
	return errors.New("windows services are not supported on this platform")
}
