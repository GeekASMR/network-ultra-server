//go:build !windows

package main

import (
	"errors"
	"log/slog"
	"net"
	"time"
)

// portIsFree: same semantics as the Windows version. Tries an actual bind.
func portIsFree(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	time.Sleep(50 * time.Millisecond)
	return true
}

// reclaimPort is a no-op on non-Windows: bridge.exe ships only on Windows
// in the production installer, but we still build for Linux/macOS in CI
// and dev environments. If the port is busy on one of those, surface the
// problem rather than silently kill anything.
func reclaimPort(addr string, log *slog.Logger) error {
	return errors.New("port reclaim not implemented on this platform")
}
