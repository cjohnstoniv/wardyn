// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// auditRetentionCmd is `wardyn audit retention`: the audit log's retention policy and every partition's
// state and eligibility (GET /audit/retention), with `drop` beneath it. The runbook is export, check, drop
// (docs/OPERATIONS.md, "Audit retention"): export-partition writes the archive and its digest, the operator
// verifies the archive, and drop submits that digest, which the database recomputes before it removes anything.
func auditRetentionCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "retention",
		Short: "Show the audit retention policy and every partition's eligibility (security admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := client().AuditRetention(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), st)
			}
			out := cmd.OutOrStdout()
			window := "forever"
			if st.Policy.EffectiveDays > 0 {
				window = fmt.Sprintf("%d days", st.Policy.EffectiveDays)
			}
			fmt.Fprintf(out, "retention: %s", window)
			if st.Policy.PendingDays != nil && st.Policy.PendingEffectiveAt != nil {
				pending := "forever"
				if *st.Policy.PendingDays > 0 {
					pending = fmt.Sprintf("%d days", *st.Policy.PendingDays)
				}
				fmt.Fprintf(out, " (a decrease to %s takes effect %s)", pending, st.Policy.PendingEffectiveAt.Format(time.RFC3339))
			}
			fmt.Fprintf(out, "\ncutover: %s    partitions ahead: %d months\n", st.Cutover.Format(time.RFC3339), st.MonthsAhead)
			tw := newTab(out)
			fmt.Fprintln(tw, "PARTITION\tROWS\tSTATE\tDROPPABLE\tREFUSAL")
			for _, p := range st.Partitions {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%t\t%s\n", p.Name, p.Rows, p.State, p.Eligible, p.Refusal)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	cmd.AddCommand(auditRetentionDropCmd(client))
	return cmd
}

// auditRetentionDropCmd is `wardyn audit retention drop <partition> --digest <hex>`.
func auditRetentionDropCmd(client clientFn) *cobra.Command {
	var digest string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "drop <partition>",
		Short: "Drop the oldest closed audit partition past the retention window, checked against an export's digest (security admin)",
		Long: "Drops one audit partition through the database function that recomputes its digest, refuses on a mismatch, " +
			"records a chained audit.retention.partition_dropped event and an anchor, and only then removes it. " +
			"Export it first (`wardyn audit export-partition <partition> --raw -o file`), check the archive, " +
			"and pass the digest in the footer line. A partition that is not the oldest, is not closed, is inside " +
			"the retention window or holds a live run's rows is refused, and the refusal names why.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if digest == "" {
				return fmt.Errorf("--digest is required: the digest from the footer line of the export you checked")
			}
			d, err := client().DropAuditPartition(cmd.Context(), args[0], digest)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), d)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dropped %s: %d rows (seq %d to %d), digest %s, chained event at seq %d\n",
				d.Partition, d.Rows, d.SeqLo, d.SeqHi, d.Digest, d.EventSeq)
			return nil
		},
	}
	cmd.Flags().StringVar(&digest, "digest", "", "the partition's digest, from the footer of its export")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}
