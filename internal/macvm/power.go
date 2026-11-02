// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// PowerSource says whether the Mac is on AC power. Prepare's download pauses
// while it is not; the install, once started, never pauses.
type PowerSource interface {
	OnAC(ctx context.Context) (bool, error)
}

// PmsetPower reads `pmset -g batt`.
type PmsetPower struct{}

func (PmsetPower) OnAC(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "batt").Output()
	if err != nil {
		return false, err
	}
	return parsePmsetBatt(string(out))
}

// parsePmsetBatt reads pmset's first line, "Now drawing from 'AC Power'" (or
// 'Battery Power', 'UPS Power'). Only AC counts as AC.
func parsePmsetBatt(out string) (bool, error) {
	line, _, _ := strings.Cut(out, "\n")
	_, src, ok := strings.Cut(line, "Now drawing from '")
	if !ok {
		return false, errors.New("macvm: pmset did not say what power the Mac is on")
	}
	src, _, _ = strings.Cut(src, "'")
	return src == "AC Power", nil
}

// IdleSleepCommand is the command that holds an idle-sleep assertion for as
// long as pid lives: caffeinate -i -w <pid>. (Whether that is the assertion
// macOS honours for this is read back with `pmset -g assertions` on a Mac.)
func IdleSleepCommand(pid int) []string {
	return []string{"/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(pid)}
}
