// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"fmt"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// macOSVMArithmetic is setup's worked sum (design 3.10): a runner's macOS guests together get at most
// its total memory, which cannot pass Docker's, while the host's budget for them is what Docker and
// macOS leave. Bytes throughout.
type macOSVMArithmetic struct {
	host, docker, total, perRun uint64
}

// max is how many macOS VMs this Mac can run at once at the per-run memory: Apple's cap, the runner's
// total and the host budget, whichever holds fewest.
func (a macOSVMArithmetic) max() int {
	if a.perRun == 0 {
		return 0
	}
	n := min(admission.LicenceCap, int(a.total/a.perRun))
	return min(n, int(a.budget()/(a.perRun+admission.DefaultOverheadBytes)))
}

func (a macOSVMArithmetic) budget() uint64 {
	if a.host <= a.docker+admission.MacOSReserveBytes {
		return 0
	}
	return a.host - a.docker - admission.MacOSReserveBytes
}

// lines are setup's words for the sum, with this Mac's numbers, and what to change when it holds none.
func (a macOSVMArithmetic) lines() []string {
	gb := func(b uint64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', -1, 64) }
	out := []string{
		fmt.Sprintf("macOS VMs on this Mac: %s GB of memory, less %s GB for Docker and 4 GB for macOS, leaves %s GB for macOS guests.", gb(a.host), gb(a.docker), gb(a.budget())),
		fmt.Sprintf("Each guest takes the per-run memory (%s GB) and about 1 GB more, and the runner's total memory (%s GB) holds all of them at once.", gb(a.perRun), gb(a.total)),
		fmt.Sprintf("So this Mac runs at most %d macOS VM(s) at once (Apple's licence allows 2).", a.max()),
	}
	if a.max() == 0 {
		out = append(out, "To run one: give Docker less memory, or lower --per-run-memory (a guest needs at least the macOS base's minimum, which prepare reports), or raise --total-memory up to what Docker has.")
	}
	return out
}
