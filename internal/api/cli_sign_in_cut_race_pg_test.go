// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A cut that commits after the token call's late check (mintSessionLive) and before CreateCLISession's
// insert still ends the new CLI session: the cut's sweep (endCLISessionsSQL) ends only the rows that
// exist when it runs, so the insert has to be ordered against the cut itself. The person's CLI session
// from before the cut is the control the sweep does reach; the sessions-only cut keeps the personal API
// token working.
//
// Guarded by WARDYN_TEST_PG (throwawayBarePGPool): skipped cleanly when unset, must PASS when set.
func TestPG_CLISignIn_CutDuringTokenCallEndsTheNewSession(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, body string }{
		{"revoke", `{"sub":"sub-user"}`},
		{"Sign out everywhere", `{"sub":"sub-user","sessions_only":true}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCLIEnv(t)
			personal := decodeJSON[types.APIToken](t, e.call(e.user, http.MethodPost, "/api/v1/me/tokens", `{"name":"ci"}`))
			_, old := e.signIn(e.user, nil)
			cl := e.client()
			si := cl.create(nil)
			ap := decodeJSON[types.CLISignInApproveResponse](t, e.approve(e.user, si.ID))
			// The hook runs once, after the token call's late check has answered "not cut".
			e.rev.setLateCheck(func() {
				if w := e.admin(http.MethodPost, "/api/v1/sessions/revoke", c.body); w.Code != http.StatusNoContent {
					t.Errorf("revoke = %d %s", w.Code, w.Body)
				}
			})
			w := cl.token(si.ID, ap.Code)

			if code, _ := e.me(old.AccessToken); code != http.StatusUnauthorized {
				t.Errorf("the access token of the session from before the %s = %d, want 401", c.name, code)
			}
			if got, _ := e.pg.GetCLISession(ctx, old.SessionID); got.EndedAt == nil {
				t.Errorf("the session from before the %s = %+v, want ended", c.name, got)
			}
			if w.Code == http.StatusCreated {
				g := decodeJSON[types.CLISessionGrant](t, w)
				if code, _ := e.me(g.AccessToken); code != http.StatusUnauthorized {
					t.Errorf("the access token the token call wrote across the %s = %d, want 401", c.name, code)
				}
				if got, _ := e.pg.GetCLISession(ctx, g.SessionID); got.EndedAt == nil {
					t.Errorf("the session the token call wrote across the %s = live (%+v), want ended or never written", c.name, got)
				}
				if r := cl.refresh(g.SessionID, g.Secret); r.Code != http.StatusUnauthorized {
					t.Errorf("refresh of the session written across the %s = %d %s, want 401", c.name, r.Code, r.Body)
				}
			}
			if c.name == "Sign out everywhere" {
				if code, _ := e.me(personal.Token); code != http.StatusOK {
					t.Errorf("the personal API token after Sign out everywhere = %d, want it to keep working", code)
				}
			}
		})
	}
}
