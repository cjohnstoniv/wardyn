// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
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
			// Renew a spent token first (this takes and releases the lock itself).
			_, loadErr := loadSession(cmd.Context(), origin)
			beforeLogoutLock()
			unlock, ok, err := lockRefresh(dir)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf(errRenewBusy, origin)
			}
			defer unlock()
			// The item as stored now, under the lock: another command may have rotated it since.
			var serverErr error
			if it, found, err := storeFor(kind, dir).get(origin); err == nil && found {
				now := time.Now()
				switch tok, _ := it.bearer(now); {
				case tok != "":
					serverErr = signOutAt(cmd.Context(), origin, it.SessionID, tok)
				case it.endedWhy(now) == "":
					// A live session this command could not renew to sign out.
					serverErr = loadErr
					if serverErr == nil {
						serverErr = errors.New("no live token to sign it out with")
					}
				}
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

// beforeLogoutLock runs between logout's renewal and its lock; a test runs another command's refresh there.
var beforeLogoutLock = func() {}

// tokenRefusedReasons are the 401 reasons a bearer the organisation no longer knows gets: an ended
// session's tokens are revoked, so its token is refused as unknown (the admin-token fallback's reasons).
var tokenRefusedReasons = []string{"cli_session_invalid", "admin_token_not_configured", "invalid_admin_token"}

// signOutAt ends session at origin with token. A session already gone (404, or its token refused as
// unknown) is not an error; any other refusal, such as a stale role stamp, is.
func signOutAt(ctx context.Context, origin string, session uuid.UUID, token string) error {
	return sessionGone(cliCall(ctx, origin, http.MethodDelete, "/api/v1/me/cli-sessions/"+session.String(), token, nil, nil))
}

// sessionGone is err, or nil when it says the session or its token is already gone.
func sessionGone(err error) error {
	var ae *sdk.APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound ||
		ae.Status == http.StatusUnauthorized && slices.Contains(tokenRefusedReasons, ae.Reason)) {
		return nil
	}
	return err
}
