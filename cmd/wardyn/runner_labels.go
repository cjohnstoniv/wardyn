// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// runnerLabelCmd is a security operator's `runner label`: the runner's whole operator label set, which is what
// audit rows and the export carry. No arguments clears it.
func runnerLabelCmd(client clientFn) *cobra.Command {
	var acceptProposed bool
	cmd := &cobra.Command{Use: "label <runner-id> [key=value ...]", Short: "Set a runner's confirmed labels (security operator): the whole set, none clears it", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := uuid.Parse(args[0])
		if err != nil {
			return fmt.Errorf("runner id %q is not a UUID", args[0])
		}
		given := map[string]string{}
		for _, a := range args[1:] {
			k, v, ok := strings.Cut(a, "=")
			if _, dup := given[k]; !ok || dup {
				return fmt.Errorf("label %q: want key=value, each key once", a)
			}
			given[k] = v
		}
		c := client()
		labels := given
		if acceptProposed {
			runner, err := c.GetRunner(cmd.Context(), id)
			if err != nil {
				return err
			}
			labels = maps.Clone(runner.AssertedLabels)
			if labels == nil {
				labels = map[string]string{}
			}
			maps.Copy(labels, given)
		}
		view, err := c.SetRunnerLabels(cmd.Context(), id, labels)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Runner %s (%s) labels: %s\n", view.Name, view.ID, labelsLine(view.Labels))
		return nil
	}}
	cmd.Flags().BoolVar(&acceptProposed, "accept-proposed", false, "start from the labels the runner proposed at registration, then apply the arguments")
	return cmd
}

// labelsLine is a label set as sorted key=value pairs, comma-joined, or "none".
func labelsLine(labels map[string]string) string {
	if len(labels) == 0 {
		return "none"
	}
	pairs := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		pairs = append(pairs, k+"="+labels[k])
	}
	return strings.Join(pairs, ",")
}
