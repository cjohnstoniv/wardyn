// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// breakGlassReasonFlag registers --break-glass-reason on cmd and returns client wrapped to send it as
// X-Wardyn-Break-Glass-Reason on every request the command makes (the launch, or its --dry-run
// preflight). Without the flag the client is unchanged.
func breakGlassReasonFlag(cmd *cobra.Command, client clientFn) clientFn {
	var reason string
	cmd.Flags().StringVar(&reason, "break-glass-reason", "", "with the admin token: the reason this run gives as a break-glass run, sent as X-Wardyn-Break-Glass-Reason (required while the Wardyn boundary is in hard_limits and governance profiles exist; the boundary still binds the run)")
	return func() *sdk.Client {
		c := client()
		if reason != "" {
			c.BreakGlassReason = reason
		}
		return c
	}
}
