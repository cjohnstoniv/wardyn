// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	leaverSub = "leaver-sub"
	leaverBot = "leaver-bot"
)

// liveTokens counts the unrevoked token rows of a principal.
func (e probeEnv) liveTokens(t *testing.T, principal string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM api_tokens WHERE principal = $1 AND revoked_at IS NULL`, principal).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestF1508Probe_DeactivatedPersonMintsNothing: a person whose identity carries deactivated_at mints no token
// through any path (self-mint, invitation claim, sponsor mint of a service token), whether or not the cut also
// raised their authority epoch, which is what a SCIM suspension does. The controls first: the same three calls
// succeed for the same person before the deactivation, so a refusal is the deactivation and not the setup.
func TestF1508Probe_DeactivatedPersonMintsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, set string
	}{
		{"deactivated_at only", `deactivated_at = now()`},
		{"deactivated_at and the epoch raised, as a SCIM suspension does", `deactivated_at = now(), authority_epoch = authority_epoch + 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newProbeEnv(t)
			e.bind(t, leaverSub, "https://idp.example", "", "")
			e.mustCreate(t, leaverBot, leaverSub)
			leaver := accessSession(t, leaverSub, "leaver@corp.example", oidc.RoleUser, []string{})
			inv := inviteEnv{peoplePG: e.peoplePG}

			newInvitation := func() types.Invitation {
				w := inv.invite(t, e.sec, leaverSub, `{}`)
				var rec types.Invitation
				if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &rec) != nil {
					t.Fatalf("invite the leaver: %d %s", w.Code, w.Body.String())
				}
				return rec
			}
			paths := []struct {
				name      string
				principal string // whose token rows a mint would add to
				call      func(invitation types.Invitation) *httptest.ResponseRecorder
			}{
				{"POST /me/tokens", leaverSub, func(types.Invitation) *httptest.ResponseRecorder {
					return e.ask(t, leaver, "", http.MethodPost, "/api/v1/me/tokens", `{"name":"x","ttl_seconds":3600}`)
				}},
				{"POST /me/invitations/{id}/claim", leaverSub, func(i types.Invitation) *httptest.ResponseRecorder {
					return e.ask(t, leaver, "", http.MethodPost, "/api/v1/me/invitations/"+i.ID.String()+"/claim", `{}`)
				}},
				{"POST /me/service-identities/{name}/tokens", "service:" + leaverBot, func(types.Invitation) *httptest.ResponseRecorder {
					return e.ask(t, leaver, "", http.MethodPost, "/api/v1/me/service-identities/"+leaverBot+"/tokens", `{"ttl_seconds":3600}`)
				}},
			}
			for _, p := range paths {
				w := p.call(newInvitation())
				if w.Code != http.StatusCreated {
					t.Fatalf("control: %s before the deactivation: %d %s, want 201", p.name, w.Code, w.Body.String())
				}
				var tok types.APIToken
				if json.Unmarshal(w.Body.Bytes(), &tok) != nil {
					t.Fatal(w.Body.String())
				}
				e.ask(t, nil, tok.Token, http.MethodGet, "/api/v1/me", "") // the control token is real
			}
			// Retire what the controls minted, so the count below is the refusals' alone.
			for _, principal := range []string{leaverSub, "service:" + leaverBot} {
				if _, err := e.pool.Exec(context.Background(), `UPDATE api_tokens SET revoked_at = now() WHERE principal = $1`, principal); err != nil {
					t.Fatal(err)
				}
			}

			pending := newInvitation() // addressed before the deactivation, so the claim is the only thing the cut changes
			if _, err := e.pool.Exec(context.Background(), `UPDATE principal_identities SET `+tc.set+` WHERE principal = $1`, leaverSub); err != nil {
				t.Fatal(err)
			}
			for _, p := range paths {
				t.Run(p.name, func(t *testing.T) {
					w := p.call(pending)
					t.Logf("%s after the deactivation: %d %s", p.name, w.Code, respReason(w))
					if w.Code/100 == 2 || bearerShaped.MatchString(w.Body.String()) {
						t.Errorf("a deactivated person minted through %s: %d %s", p.name, w.Code, w.Body.String())
					}
					if n := e.liveTokens(t, p.principal); n != 0 {
						t.Errorf("%s left %d live tokens for %s after the deactivation", p.name, n, p.principal)
					}
				})
			}
		})
	}
}
