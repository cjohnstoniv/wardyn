// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
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

	cmd.AddCommand(revoke, sessionsListCmd(client))
	return subcommandGroup(cmd)
}

// sessionsListCmd lists every API token in the deployment — the nearest
// enumerable stand-in for "active sessions" that exists: an OIDC session
// cookie is a stateless signed value with no server-side row (see this file's
// own doc comment), so there is nothing to list there, but `revoke`'s own
// --all already treats a token as "a human's session in another form" and
// GET /api/v1/tokens is the one place that population is actually listable.
//
// Raw HTTP, not the SDK: pkg/client's own package doc (client.go) lists
// /api/v1/tokens among the admin-tier families it deliberately does not
// wrap. attach.go's mintAttachTicket follows the same pattern for its own
// deliberately-unwrapped family.
func sessionsListCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every API token in the deployment (the nearest enumerable analog to a session)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			toks, err := listAllAPITokens(cmd.Context(), client())
			if err != nil {
				return err
			}
			if asJSON {
				if toks == nil {
					toks = []types.APIToken{}
				}
				return emitJSON(cmd.OutOrStdout(), toks)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "PRINCIPAL\tROLE\tNAME\tCREATED\tLAST_USED\tSTATE")
			for _, t := range toks {
				state := "active"
				if t.RevokedAt != nil {
					state = "revoked"
				}
				lastUsed := "-"
				if t.LastUsedAt != nil {
					lastUsed = t.LastUsedAt.Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.Principal, t.Role, t.Name, t.CreatedAt.Format(time.RFC3339), lastUsed, state)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

// listAllAPITokens fetches GET /api/v1/tokens directly — see sessionsListCmd's
// doc comment for why this bypasses the SDK. Mirrors mintAttachTicket's own
// direct-request shape (attach.go), but a non-2xx here is always decisive
// (this route exists on every deployment new enough to have `sessions`), so
// it is always returned as an *sdk.APIError rather than treated as
// inconclusive.
func listAllAPITokens(ctx context.Context, c *sdk.Client) ([]types.APIToken, error) {
	target := strings.TrimRight(c.BaseURL, "/") + "/api/v1/tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &sdk.APIError{Status: resp.StatusCode, Body: string(body)}
	}
	var toks []types.APIToken
	if err := json.Unmarshal(body, &toks); err != nil {
		return nil, fmt.Errorf("decode /api/v1/tokens response: %w", err)
	}
	return toks, nil
}
