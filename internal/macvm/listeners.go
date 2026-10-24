// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

// ToolsPort is the tools-mode vsock port the guest agent dials. The helper
// listens on it only for a stage-2 boot (prepare and a tools update), never
// for a run.
const ToolsPort uint32 = 1026

// ErrToolsModeRun fails a run, before Init, whose configuration or vsock
// listeners are a stage-2 boot's.
var ErrToolsModeRun = errors.New("macvm: This macOS VM started in tools mode")

// runExtraPorts are vsock ports a run boot registers beyond its own: none.
// Tests set it to show the listener check fails closed.
var runExtraPorts []uint32

// checkRunListeners is the listener-set check before Init: a run boot's VM
// listens on the agent port and at most the desktop port, and nothing else.
func checkRunListeners(ports []uint32) error {
	if !slices.Contains(ports, AgentPort) {
		return fmt.Errorf("macvm: the run's VM has no guest agent listener (vsock %v)", ports)
	}
	for _, p := range ports {
		if p != AgentPort && p != DesktopPort {
			return fmt.Errorf("%w (vsock listeners %v)", ErrToolsModeRun, ports)
		}
	}
	return nil
}

// Classes of the guest agent's own result for its dial of the tools port,
// as it reports them: tools_dial in ctl.sock status (a run boot) and in
// prepare's log (a stage-2 boot).
const (
	ToolsDialConnected = "connected"
	ToolsDialRefused   = "refused"
	ToolsDialUnsupp    = "unsupported"
	ToolsDialTimeout   = "timeout"
	ToolsDialTransport = "transport failure"
)

// ValidToolsDial reports whether s is one of the five classes.
func ValidToolsDial(s string) bool {
	switch s {
	case ToolsDialConnected, ToolsDialRefused, ToolsDialUnsupp, ToolsDialTimeout, ToolsDialTransport:
		return true
	}
	return false
}

// makeRunSockDir makes the run's socket directory, new, after sweeping the
// socket directories of this login's runs that are gone.
func makeRunSockDir(o RunOptions, cfg RunConfig) (string, error) {
	sc := SockConfig(cfg.SockRoot)
	sc.Fstat = o.SockFstat
	key := RunKey(cfg.RunID)
	dir := filepath.Join(sc.UserDir(), key)
	if err := checkSocketPaths(dir); err != nil {
		return "", err
	}
	runs := filepath.Dir(filepath.Clean(o.RunDir))
	if removed, err := sc.SweepRunDirs(func(k string) bool { return k != key && sockResidue(runs, sc.UserDir(), k) }); err != nil {
		o.Log.Warn("macvm: sweeping socket directories of gone runs", "err", err.Error())
	} else if len(removed) > 0 {
		o.Log.Info("macvm: swept socket directories of gone runs", "keys", removed)
	}
	return sc.MakeRunDir(key)
}

// sockResidue says whether the socket directory key is a gone run's: its run
// directory is gone and nothing answers on its ctl.sock.
func sockResidue(runsDir, userDir, key string) bool {
	if _, err := os.Lstat(filepath.Join(runsDir, key)); !os.IsNotExist(err) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", filepath.Join(userDir, key, ControlSock))
	if err == nil {
		_ = c.Close()
		return false
	}
	return true
}

// RunSockDir is a run's socket directory, checked as on every open (owner,
// mode 0700, no link, through u<uid>'s descriptor walk). fstat replaces
// fstat(2) in tests; nil is the real one.
func RunSockDir(cfg RunConfig, fstat func(fd int, name string) (unix.Stat_t, error)) (string, error) {
	sc := SockConfig(cfg.SockRoot)
	sc.Fstat = fstat
	return sc.CheckRunDir(RunKey(cfg.RunID))
}

// SweepSockResidue removes the socket directories of this login's runs that
// are gone, under layout's runs dir: runnerd's call wherever it sweeps a dead
// run directory. It returns the run keys removed.
func SweepSockResidue(l Layout, sockRoot string) ([]string, error) {
	sc := SockConfig(sockRoot)
	return sc.SweepRunDirs(func(k string) bool { return sockResidue(l.RunsDir(), sc.UserDir(), k) })
}
