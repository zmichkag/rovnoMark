//go:build !windows

package main

func logWindowsEvent(elog any, eid uint32, msg string) {
	// No-op на Linux / macOS
}
