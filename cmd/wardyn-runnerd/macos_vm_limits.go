// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// macOSVMLimitsName is the file in the runner's state directory that keeps the organisation's per-run
// limits for `macos-vm prepare`, which runs in its own process and never sees the connect.
const macOSVMLimitsName = "macos-vm-limits.json"

func macOSVMLimitsFile(stateDir string) string { return filepath.Join(stateDir, macOSVMLimitsName) }

type savedLimits struct {
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMiB int64 `json:"memory_mib"`
}

// saveLimits keeps the limits the organisation sent, when it sent both, and drops the saved ones when it
// did not (it cleared them). A failed write or removal is logged once and never fails the connect.
func (m *macOSVMs) saveLimits(cpuMillis, memoryMiB int64) {
	if m.limitsFile == "" {
		return
	}
	var err error
	if cpuMillis <= 0 || memoryMiB <= 0 {
		if err = os.Remove(m.limitsFile); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = writeLimits(m.limitsFile, savedLimits{CPUMillis: cpuMillis, MemoryMiB: memoryMiB})
	}
	if err != nil {
		m.limitsWarn.Do(func() {
			slog.Warn("macOS VM sandboxes: the per-run limits were not saved for `macos-vm prepare`", "error", err.Error())
		})
	}
}

// writeLimits replaces the file whole: a temporary file in the same directory, then a rename.
func writeLimits(path string, l savedLimits) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), macOSVMLimitsName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// prepareLimits is the per-run CPUs and memory (MiB) `macos-vm prepare` sizes its VM at: a flag that is
// set wins, and an unset one comes from the limits the organisation last sent, else from the managed
// preferences PerRunCPU and PerRunMemoryGB.
func prepareLimits(file string, cpu float64, memGB int64) (cpus int, memMiB uint64, err error) {
	cpus, mem := int(cpu), memGB<<10
	known := false
	if cpu < 1 || memGB < 1 {
		var l savedLimits
		if data, rerr := os.ReadFile(file); rerr == nil && json.Unmarshal(data, &l) == nil {
			known = true
			if cpu < 1 {
				cpus = int(l.CPUMillis / 1000)
			}
			if memGB < 1 {
				mem = l.MemoryMiB // in MiB, as a run is sized
			}
		}
	}
	if cpus < 1 {
		if v, perr := strconv.ParseFloat(managedPref("PerRunCPU"), 64); perr == nil && v >= 1 {
			cpus = int(v)
		}
	}
	if mem < 1<<10 {
		if v, perr := strconv.ParseFloat(managedPref("PerRunMemoryGB"), 64); perr == nil && v >= 1 {
			mem = int64(v) << 10
		}
	}
	if cpus < 1 || mem < 1<<10 {
		msg := "pass --per-run-cpu and --per-run-memory: the runner's per-run limits, which prepare's own VM uses and never raises"
		if !known {
			msg += " (the organisation has sent no per-run limits)"
		}
		return 0, 0, errors.New(msg)
	}
	return cpus, uint64(mem), nil
}
