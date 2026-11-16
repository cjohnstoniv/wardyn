// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// workload_pin.go — `wardyn workload pin` and `unpin`: hold your workload's launches to the launch policy
// digest a pipeline evaluated, which `wardyn apply --dry-run` prints (apply.go).

func workloadPinCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "pin NAME DIGEST",
		Short: "Pin your workload to a launch policy digest; a launch that resolves to another is refused",
		Long: "Pin your workload to a launch policy digest, the one `wardyn apply --dry-run` prints. A later launch\n" +
			"whose policy resolves to another digest, for example after a stored policy, a workspace or a component\n" +
			"it uses, or your organisation's limits changed, is refused before its run starts. Pin the new digest to\n" +
			"accept the change. The digest does not cover what is added as the run starts, the confinement class\n" +
			"above the policy's minimum or the run's lifetime limit.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := client().PinWorkloadPolicy(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s pinned to launch policy digest %s\n", w.Name, w.PolicyPin)
			return nil
		},
	}
}

func workloadUnpinCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "unpin NAME",
		Short: "Let your workload launch with whatever policy it resolves to",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := client().UnpinWorkloadPolicy(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s unpinned\n", w.Name)
			return nil
		},
	}
}
