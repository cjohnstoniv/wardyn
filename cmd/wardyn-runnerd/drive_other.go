// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// newDriveCommand is `wardyn-runnerd drive` where there are no macOS VM drives.
func newDriveCommand() *cobra.Command {
	return &cobra.Command{Use: "drive", Short: "Disk drives of macOS VMs (a Mac only)", Args: cobra.NoArgs, Hidden: true,
		RunE: func(*cobra.Command, []string) error { return errors.New("macOS VM drives exist only on a Mac") }}
}
