//go:build windows

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// portIsFree returns true if we can bind a TCP listener on `addr` right now.
// We rely on actual bind+close semantics rather than enumerating sockets,
// because Windows has multiple address families and cached state.
func portIsFree(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	// Give Windows a beat to actually free the socket — TIME_WAIT can
	// re-block immediately if we sprint into the next listen.
	time.Sleep(50 * time.Millisecond)
	return true
}

// reclaimPort tries to identify the process holding `addr` and, if it's our
// own binary (network-ultra-bridge.exe — common after DAW crash leaves a
// zombie), kills it. Refuses to touch any other process kind so we never
// accidentally murder some user's SSH server or whatever happens to live
// on 18900.
func reclaimPort(addr string, log *slog.Logger) error {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse addr: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return fmt.Errorf("invalid port: %s", portStr)
	}

	pids, err := pidsHoldingPort(port)
	if err != nil {
		return fmt.Errorf("enumerate port holders: %w", err)
	}
	if len(pids) == 0 {
		return errors.New("port appears busy but no holder found (maybe TIME_WAIT)")
	}

	myExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	myBase := strings.ToLower(filepath.Base(myExe))
	myPid := os.Getpid()

	var killed []int
	for _, pid := range pids {
		if pid == myPid {
			continue
		}
		exe, err := exePathOfPID(pid)
		if err != nil {
			log.Warn("can't read exe for pid; skipping",
				"pid", pid, "err", err)
			continue
		}
		base := strings.ToLower(filepath.Base(exe))
		if base != myBase {
			return fmt.Errorf(
				"port %d held by %q (pid %d), not ours (%q); refusing to kill",
				port, exe, pid, myBase)
		}
		// Same binary name → safe to kill.
		log.Info("killing stale bridge instance", "pid", pid, "exe", exe)
		if err := killPID(pid); err != nil {
			log.Warn("kill failed", "pid", pid, "err", err)
			continue
		}
		killed = append(killed, pid)
	}

	if len(killed) == 0 {
		return errors.New("no stale bridge process killed")
	}
	return nil
}

// pidsHoldingPort uses PowerShell's Get-NetTCPConnection to find PIDs
// that have the given local port in LISTEN state. PowerShell is always
// present on Windows 10+ which is our minimum supported platform, and
// avoids us having to vendor a syscall-level socket enumerator.
func pidsHoldingPort(port int) ([]int, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(
			"(Get-NetTCPConnection -LocalPort %d -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess) -join ','",
			port))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	var pids []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		pids = append(pids, n)
	}
	return pids, nil
}

// exePathOfPID fetches the full path of a process binary. Falls back to
// just the name if the path can't be retrieved (rare; usually means the
// process exited between enumerate and inspect).
func exePathOfPID(pid int) (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("(Get-Process -Id %d -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path)", pid))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("no Path for pid %d", pid)
	}
	return s, nil
}

// killPID issues a force-kill via taskkill (sends WM_CLOSE first, then
// terminates). Equivalent to `taskkill /F /PID xxx`.
func killPID(pid int) error {
	out, err := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
