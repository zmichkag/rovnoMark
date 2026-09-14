//go:build windows

package main

import (
	"golang.org/x/sys/windows/svc/eventlog"
)

func logWindowsEvent(elog any, eid uint32, msg string) {
	if l, ok := elog.(*eventlog.Log); ok && l != nil {
		_ = l.Error(eid, msg)
	}
}
