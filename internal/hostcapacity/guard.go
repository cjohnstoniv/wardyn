// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package hostcapacity refuses new runs while the host is short of memory or
// over a load ceiling, so a burst of launches cannot push the machine that
// hosts the daemon (and everything else on it) into swap or an OOM kill.
package hostcapacity

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Limits are the admission thresholds. A zero field disables that check, and
// the zero value disables the guard entirely.
type Limits struct {
	MinAvailableMiB int // refuse while MemAvailable is below this many MiB
	MaxLoad1        int // refuse while the 1-minute load average is above this
}

// ErrRefused is returned by Admit when the host is over a limit. Reason names
// the measured value and the limit it crossed, e.g. "load1=112.40 > 100".
type ErrRefused struct{ Reason string }

func (e ErrRefused) Error() string { return "host capacity refused: " + e.Reason }

// Guard checks the host against Limits. A nil *Guard admits everything.
type Guard struct {
	limits Limits
	read   func() (availMiB int, load1 float64, err error)
}

// New returns a Guard over limits that measures the host with read; the daemon
// passes ReadProc.
func New(limits Limits, read func() (availMiB int, load1 float64, err error)) *Guard {
	return &Guard{limits: limits, read: read}
}

var warnOnce sync.Once

// Admit returns nil when the guard is disabled or the host is within limits,
// else an ErrRefused naming every limit crossed.
func (g *Guard) Admit() error {
	if g == nil || g.limits == (Limits{}) {
		return nil
	}
	avail, load1, err := g.read()
	if err != nil {
		// Fail OPEN: this guard protects the host from overload, it is not a
		// security gate, and it must never wedge the daemon because /proc is
		// unreadable or changed shape. Warn once so the log is not flooded.
		warnOnce.Do(func() {
			slog.Warn("hostcapacity: cannot read host capacity; admitting runs unchecked", slog.Any("err", err))
		})
		return nil
	}
	var reasons []string
	if floor := g.limits.MinAvailableMiB; floor > 0 && avail < floor {
		reasons = append(reasons, fmt.Sprintf("mem_available_mib=%d < %d", avail, floor))
	}
	if ceiling := g.limits.MaxLoad1; ceiling > 0 && load1 > float64(ceiling) {
		reasons = append(reasons, fmt.Sprintf("load1=%.2f > %d", load1, ceiling))
	}
	if len(reasons) == 0 {
		return nil
	}
	return ErrRefused{Reason: strings.Join(reasons, ", ")}
}

// ReadProc reads MemAvailable from /proc/meminfo and the 1-minute load average
// from /proc/loadavg. In a container or a kind node these describe the kernel
// the daemon runs on (on WSL2, the whole VM), not a cgroup's share of it.
func ReadProc() (availMiB int, load1 float64, err error) {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	availKiB := -1
	for line := range bytes.Lines(meminfo) {
		if f := strings.Fields(string(line)); len(f) >= 2 && f[0] == "MemAvailable:" {
			if availKiB, err = strconv.Atoi(f[1]); err != nil {
				return 0, 0, fmt.Errorf("parse MemAvailable: %w", err)
			}
		}
	}
	if availKiB < 0 {
		return 0, 0, fmt.Errorf("no MemAvailable line in /proc/meminfo")
	}
	loadavg, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(loadavg))
	if len(f) == 0 {
		return 0, 0, fmt.Errorf("empty /proc/loadavg")
	}
	if load1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return 0, 0, fmt.Errorf("parse load1: %w", err)
	}
	return availKiB / 1024, load1, nil
}
