// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"strconv"
	"strings"
)

// Self returns pid's start time from h's process table, for an entry's
// OwnerStart or HelperStart.
func Self(h Host, pid int) (int64, error) {
	ps, err := h.Processes()
	if err != nil {
		return 0, err
	}
	for _, p := range ps {
		if p.PID == pid {
			return p.Start, nil
		}
	}
	return 0, errors.New("admission: process not in the process table")
}

// parseVMStat reads vm_stat's output: the page size from its first line,
// then free + inactive + speculative pages.
func parseVMStat(out string) (uint64, error) {
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return 0, errors.New("admission: empty vm_stat output")
	}
	_, after, ok := strings.Cut(lines[0], "page size of ")
	if !ok {
		return 0, errors.New("admission: vm_stat did not give a page size")
	}
	sz, _, _ := strings.Cut(after, " ")
	page, err := strconv.ParseUint(sz, 10, 64)
	if err != nil {
		return 0, errors.New("admission: vm_stat page size")
	}
	var pages uint64
	seen := 0
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages free", "Pages inactive", "Pages speculative":
			n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
			if err != nil {
				return 0, errors.New("admission: vm_stat " + k)
			}
			pages += n
			seen++
		}
	}
	if seen == 0 {
		return 0, errors.New("admission: vm_stat gave no free pages")
	}
	return pages * page, nil
}
