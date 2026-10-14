// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An API token is not ended by its person signing out of the browser (a browser-only cut), so what it
// mints after a sign-out is checked as the token is. These tests run one person with a live token and a
// running interactive run of their own.

const lateSub, lateEmail = "sub-late", "late@corp.example"

// afterSessionStatus fires a hook once, right after the next owner check (SessionStatus) answers: a
// revoke that lands after the token authenticated and before the mint's late re-check.
type afterSessionStatus struct {
	*pgTestRevocations
	mu   sync.Mutex
	hook func()
}

func (a *afterSessionStatus) SessionStatus(ctx context.Context, sub, email string, issue oidc.Issue, epoch int64) (oidc.SessionStatus, error) {
	st, err := a.pgTestRevocations.SessionStatus(ctx, sub, email, issue, epoch)
	a.mu.Lock()
	f := a.hook
	a.hook = nil
	a.mu.Unlock()
	if f != nil {
		f()
	}
	return st, err
}

func (a *afterSessionStatus) arm(f func()) { a.mu.Lock(); a.hook = f; a.mu.Unlock() }

type lateEnv struct {
	*scimEnv
	hooked *afterSessionStatus
	token  string
	run    uuid.UUID
	epoch  int64
}

func newLateEnv(t *testing.T) *lateEnv {
	t.Helper()
	hooked := &afterSessionStatus{}
	e := newSCIMEnv(t, func(c *Config) { c.SessionRevocations = hooked })
	hooked.pgTestRevocations = e.rev
	row := e.seedEntra(lateSub, lateEmail, oidLeaver)
	w := doSSO(t, e.a.srv, http.MethodPost, "/api/v1/me/tokens", lateCookie(e, row.AuthorityEpoch), `{"name":"cli"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint the token = %d %s", w.Code, w.Body.String())
	}
	return &lateEnv{scimEnv: e, hooked: hooked, token: decodeToken(t, w).Token, run: e.seedRun(lateSub, types.RunRunning), epoch: row.AuthorityEpoch}
}

// lateCookie is a browser session the person signs in to now.
func lateCookie(e *scimEnv, epoch int64) *http.Cookie {
	return e.orderedCookieAt(lateSub, lateEmail, epoch, time.Now().UTC())
}

// signOut signs the person out of a fresh browser session: a browser-only cut, which leaves the token working.
func (e *lateEnv) signOut(t *testing.T) {
	t.Helper()
	cookie := lateCookie(e.scimEnv, e.epoch)
	if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/auth/logout", cookie, ""); w.Code != http.StatusFound {
		t.Fatalf("sign-out = %d %s", w.Code, w.Body.String())
	}
	if e.cookieWorks(e.a, cookie) {
		t.Fatal("premise: the browser session still works after the sign-out")
	}
	if !e.tokenWorks(e.a, e.token) {
		t.Fatal("premise: the sign-out ended the API token")
	}
}

func (e *lateEnv) ticket(t *testing.T) string {
	t.Helper()
	w := do(t, e.a.srv, http.MethodPost, "/api/v1/runs/"+e.run.String()+"/attach-ticket", e.token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("mint an attach ticket with the token = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil || body.Ticket == "" {
		t.Fatalf("decode the ticket: %v", err)
	}
	return body.Ticket
}

func (e *lateEnv) redeem(t *testing.T, ticket string) entryVerdict {
	t.Helper()
	_, v, err := e.a.srv.redeemAttachTicket(context.Background(), ticket, e.run)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *lateEnv) addKey(t *testing.T) int {
	t.Helper()
	_, pub := mustSSHKeypair(t)
	return do(t, e.a.srv, http.MethodPost, "/api/v1/me/ssh-keys", e.token, addKeyBody(t, pub)).Code
}

// TestPG_TokenMintsAfterAnOlderBrowserSignOut: after the person signed out of the browser, their token still
// mints an attach ticket for their own running run, the ticket redeems, and the token registers an SSH key.
func TestPG_TokenMintsAfterAnOlderBrowserSignOut(t *testing.T) {
	e := newLateEnv(t)
	e.signOut(t)
	if v := e.redeem(t, e.ticket(t)); v != verdictOK {
		t.Fatalf("redeem a ticket the token minted after the sign-out = %q, want ok", v)
	}
	if code := e.addKey(t); code != http.StatusCreated {
		t.Fatalf("register an SSH key with the token after the sign-out = %d, want 201", code)
	}
}

// TestPG_TokenTicketEndsAtALaterSignOut: a ticket the token minted is ended by a sign-out after its mint, as a
// browser session's ticket is, though an older sign-out did not stop the mint.
func TestPG_TokenTicketEndsAtALaterSignOut(t *testing.T) {
	e := newLateEnv(t)
	e.signOut(t)
	first, second := e.ticket(t), e.ticket(t)
	if v := e.redeem(t, first); v != verdictOK {
		t.Fatalf("redeem before the later sign-out = %q, want ok", v)
	}
	e.signOut(t)
	if v := e.redeem(t, second); v != verdictRevoked {
		t.Fatalf("redeem after the later sign-out = %q, want %q", v, verdictRevoked)
	}
}

// TestPG_TokenMintRefusesARevokeInFlight: a revoke of the person's sessions and tokens that lands after the
// token authenticated and before the mint's late re-check leaves no ticket and no key behind.
func TestPG_TokenMintRefusesARevokeInFlight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mint   func(t *testing.T, e *lateEnv) (int, string)
		reason string
	}{
		{"attach ticket", func(t *testing.T, e *lateEnv) (int, string) {
			w := do(t, e.a.srv, http.MethodPost, "/api/v1/runs/"+e.run.String()+"/attach-ticket", e.token, "")
			return w.Code, w.Body.String()
		}, reasonAttachTicketNotYourRun},
		{"ssh key", func(t *testing.T, e *lateEnv) (int, string) {
			_, pub := mustSSHKeypair(t)
			w := do(t, e.a.srv, http.MethodPost, "/api/v1/me/ssh-keys", e.token, addKeyBody(t, pub))
			return w.Code, w.Body.String()
		}, reasonSSHKeyRevokedSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLateEnv(t)
			e.signOut(t)
			e.hooked.arm(func() {
				if err := e.rev.RevokeSub(context.Background(), lateSub); err != nil {
					t.Error(err)
				}
			})
			code, body := tc.mint(t, e)
			if code != http.StatusForbidden || !strings.Contains(body, tc.reason) {
				t.Fatalf("mint with a revoke in flight = %d %s, want 403 %s", code, body, tc.reason)
			}
			if n := e.keyCount(lateSub); n != 0 {
				t.Fatalf("%d SSH keys registered, want none", n)
			}
		})
	}
}
