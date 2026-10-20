// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// logoutCmd is `wardyn logout`: it ends this computer's CLI session at the server (DELETE
// /api/v1/me/cli-sessions/{id}, with the session's own token) and deletes the stored item from whichever
// store holds it. The item is deleted even when the server cannot be told, which the error then says.
func logoutCmd(serverURL func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign this computer's CLI out of your organisation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			origin, err := cliprofile.Normalize(serverURL())
			if err != nil {
				return err
			}
			dir, err := cliprofile.Dir()
			if err != nil {
				return err
			}
			kind, err := cliprofile.ReadSessionStore(dir, origin)
			if err != nil {
				return err
			}
			if kind == "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), copyNotSignedIn+"\n", origin)
				return err
			}
			s, loadErr := loadSession(cmd.Context(), origin)
			var serverErr error
			switch {
			case s.token != "":
				serverErr = cliCall(cmd.Context(), origin, http.MethodDelete, "/api/v1/me/cli-sessions/"+s.item.SessionID.String(), s.token, nil, nil)
				// 404: the session already ended; 401: its tokens no longer authenticate, so neither does it.
				var ae *sdk.APIError
				if errors.As(serverErr, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusUnauthorized) {
					serverErr = nil
				}
			case loadErr != nil && s.item.endedWhy(time.Now()) == "":
				serverErr = loadErr // a live session this command could not renew to sign out
			}
			if err := storeFor(kind, dir).del(origin); err != nil {
				return err
			}
			if err := cliprofile.WriteSessionStore(dir, origin, ""); err != nil {
				return err
			}
			if serverErr != nil {
				return fmt.Errorf(errLogoutServer, origin, serverErr)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), copyLoggedOut+"\n", origin)
			return err
		},
	}
}
