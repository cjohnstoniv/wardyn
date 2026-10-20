// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// meIdentity is the part of GET /api/v1/me the CLI prints.
type meIdentity struct {
	Principal string `json:"principal"`
	Email     string `json:"email"`
	Role      string `json:"role"`
}

// name is the person as the CLI names them: email, else principal.
func (m meIdentity) name() string {
	if m.Email != "" {
		return m.Email
	}
	return m.Principal
}

// whoamiCmd is `wardyn whoami`: the organisation, the person, the role in effect and where the credential
// came from, from GET /api/v1/me. It never prints a token.
func whoamiCmd(client func() *sdk.Client, sess *sessionState) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who this computer's CLI acts as",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client()
			if c.Token == "" {
				if sess.err != nil {
					return sess.err
				}
				return fmt.Errorf(errNotSignedIn, c.BaseURL)
			}
			raw, err := c.Me(cmd.Context())
			if err != nil {
				return err
			}
			var me meIdentity
			if err := json.Unmarshal(raw, &me); err != nil {
				return err
			}
			origin, role, from := c.BaseURL, me.Role, fmt.Sprintf(copyWhoToken, authHintFor.tokenFrom)
			if authHintFor.tokenFrom == tokenFromLogin {
				it, now := sess.s.item, time.Now()
				origin = sess.s.origin
				switch {
				case sess.s.tokenKind == types.APITokenKindCLIAdmin:
					role = fmt.Sprintf(copyRoleAdmin, me.Role, inWords(it.AdminExpiresAt.Sub(now)))
				case me.Role == "user":
					role = copyRoleUser
				}
				from = fmt.Sprintf(copyWhoStore, sess.s.kind, inWords(it.SessionExpiresAt.Sub(now)), inWords(it.IdleExpiresAt.Sub(now)))
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n%s (%s)\n%s\n", origin, me.name(), role, from)
			return err
		},
	}
}

// inWords is a time left as the terminal says it: days, hours or minutes, rounded down.
func inWords(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 24*time.Hour:
		return "1 day"
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= time.Hour:
		return "1 hour"
	}
	return fmt.Sprintf("%d min", max(int(d/time.Minute), 1))
}
