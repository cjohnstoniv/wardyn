// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/version"
)

// versionCmd prints this CLI's version. --json adds the SHA-256 of the runner
// the release built beside it (empty when unstamped), which the release job
// compares against the runner's SHA256SUMS line.
func versionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !asJSON {
				fmt.Fprintln(cmd.OutOrStdout(), version.String())
				return nil
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
				"version":        version.String(),
				"runnerd_sha256": pinnedRunnerdSHA256,
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the version and the pinned runner SHA-256 as JSON")
	return cmd
}
