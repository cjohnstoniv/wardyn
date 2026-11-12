// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// approvalListWaitingOnMe lists the PENDING approvals the caller may decide now (`approval list
// --waiting-on-me`). The server pages nothing there and lists only pending rows, so --state, --limit and --offset would be ignored; this command takes none of them, nor --run.
// REQUESTED_BY is the run's creator, set when the caller decides as someone other than the creator.
func approvalListWaitingOnMe(cmd *cobra.Command, client clientFn, asJSON bool) error {
	aps, truncated, err := client().ListApprovalsDecidable(cmd.Context())
	if err != nil {
		return err
	}
	if truncated {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: approval list truncated at %d row(s): more were waiting on you than the server returns\n", len(aps))
	}
	if asJSON {
		return emitJSON(cmd.OutOrStdout(), aps)
	}
	tw := newTab(cmd.OutOrStdout())
	fmt.Fprintln(tw, "ID\tRUN\tKIND\tSTATE\tHOST\tREQUESTED\tREQUESTED_BY")
	for _, a := range aps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.ID, short(a.RunID.String()), a.Kind, a.State, approvalHost(a),
			a.RequestedAt.Format(time.RFC3339), a.RequestedBy)
	}
	return tw.Flush()
}
