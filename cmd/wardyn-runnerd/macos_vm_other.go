// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newMacOSVMCommand is `wardyn-runnerd macos-vm` where there are no macOS VMs.
func newMacOSVMCommand() *cobra.Command {
	return &cobra.Command{Use: "macos-vm", Short: "macOS VM sandboxes (a Mac only)", Args: cobra.NoArgs, Hidden: true,
		RunE: func(*cobra.Command, []string) error { return errors.New("macOS VM sandboxes run only on a Mac") }}
}

// Setup's macOS VM steps, where there are none.
func checkMacOSVMFlag(v, _ string) error {
	if v == "on" {
		return errors.New("--macos-vm on needs an Apple silicon Mac")
	}
	return nil
}

func askMacOSVM(*cobra.Command, string, *setupPlan, *types.RunnerLimits) error { return nil }

func prepareMacOSVMBase(*cobra.Command, setupPlan, *types.RunnerLimits) {}

// macOSVMDoctorStages is doctor's macOS VM stage, where there is none.
func macOSVMDoctorStages() []doctor.Stage { return nil }
