// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/spf13/cobra"

// runInFlag registers --run-in on cmd into unit: the unit, by name or id, the request names as run_in.
func runInFlag(cmd *cobra.Command, unit *string) {
	cmd.Flags().StringVar(unit, "run-in", "", "Run in one of your units (default: your home unit)")
}
