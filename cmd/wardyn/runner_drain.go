// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// runnerDrainCmds is `wardyn runner drain` and `undrain`: an administrator takes a runner out of placement for
// maintenance, and puts it back. Running runs are not touched.
func runnerDrainCmds(client clientFn) []*cobra.Command {
	var reason string
	drain := &cobra.Command{Use: "drain <runner-id>", Short: "Drain a runner for maintenance: it takes no new sandbox, and its running runs go on (admin)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := uuid.Parse(args[0])
		if err != nil {
			return fmt.Errorf("runner id %q is not a UUID", args[0])
		}
		view, err := client().DrainRunner(cmd.Context(), id, reason)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Runner %s (%s) is drained: it takes no new sandbox. %d running.\n", view.Name, view.ID, view.Usage.InUse.Runs)
		return nil
	}}
	drain.Flags().StringVar(&reason, "reason", "", "why, kept in the audit row (required)")
	_ = drain.MarkFlagRequired("reason")
	undrain := &cobra.Command{Use: "undrain <runner-id>", Short: "Undrain a runner, so placement may choose it again (admin)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := uuid.Parse(args[0])
		if err != nil {
			return fmt.Errorf("runner id %q is not a UUID", args[0])
		}
		view, err := client().UndrainRunner(cmd.Context(), id)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Runner %s (%s) takes runs again.\n", view.Name, view.ID)
		return nil
	}}
	return []*cobra.Command{drain, undrain}
}
