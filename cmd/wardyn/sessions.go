// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// sessionsCmd is D16's admin surface for "revoke a human now" — see
// pkg/client's RevokeSessions doc comment and internal/api/sessions.go's
// handleRevokeSessions for what actually happens server-side (a stateless
// OIDC session cookie has no row to delete, so this stamps a cutoff the
// server checks on every authenticated request going forward).
func sessionsCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage active OIDC/console sessions",
	}

	var sub string
	var all bool
	revoke := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke active sessions — logout-all for one principal (--sub) or everyone (--all)",
		Long: "Revoke active OIDC console sessions. A session cookie is stateless (no server-side\n" +
			"row to delete), so this stamps a cutoff time: any session for the target issued\n" +
			"at or before that moment stops authenticating on its VERY NEXT request, rather\n" +
			"than lingering until the cookie's own expiry.\n\n" +
			"Both arms ALSO revoke API tokens: --sub revokes every unrevoked wdn_ token that\n" +
			"principal holds; --all revokes EVERY unrevoked token in the deployment — the\n" +
			"calling admin's own included (a token is a human's session in another form).\n\n" +
			"Requires an OIDC deployment with the session-revocation store wired (404 otherwise).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case all && sub != "":
				return fmt.Errorf("pass exactly one of --sub or --all, not both")
			case all:
				if err := client().RevokeSessions(cmd.Context(), "", true); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "revoked every active session")
			case sub != "":
				if err := client().RevokeSessions(cmd.Context(), sub, false); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked active sessions for %q\n", sub)
			default:
				return fmt.Errorf("pass --sub <principal> or --all")
			}
			return nil
		},
	}
	revoke.Flags().StringVar(&sub, "sub", "", "revoke this principal's active sessions (the OIDC sub/email)")
	revoke.Flags().BoolVar(&all, "all", false, "revoke every active session, for every principal")

	cmd.AddCommand(revoke)
	return cmd
}
