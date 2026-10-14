// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// clockStepBack is the modelled step of the wall clock between a session's issue and a cut-off: when the
// cut-off is written, the session's issue time reads this far in the future.
const clockStepBack = 2 * time.Second

// steppedAhead re-signs c as a session issued just before the wall clock stepped back by clockStepBack. Its
// issue time moves; its revocation sequence, its place in the order, does not.
func steppedAhead(t *testing.T, c *http.Cookie) *http.Cookie {
	t.Helper()
	at := time.Now().UTC().Add(clockStepBack)
	return reissued(t, c, func(s *oidc.Session) { s.IssuedAt = at })
}

// A session issued in the same sub-second window as a later cut-off, with the wall clock stepping back
// between the two, is cut off like any other session issued before it.
func TestPG_SessionCutOffOrderingUnderABackwardClockStep(t *testing.T) {
	e := newSCIMEnv(t)
	const sub, email = "sub-clockstep", "clockstep@corp.example"
	// The issue time reads 600ms ahead of now: the clock stepped back after the session was issued.
	c := e.orderedCookieAt(sub, email, -1, time.Now().UTC().Add(600*time.Millisecond))
	if !e.cookieWorks(e.a, c) {
		t.Fatal("control: the session does not work before the cut-off")
	}
	if err := e.rev.RevokeSub(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // the stepped clock has passed the session's issue time
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if r := doSSO(t, n.srv, http.MethodGet, "/api/v1/me", c, ""); r.Code == http.StatusOK {
			t.Errorf("instance %s: a session issued before the cut-off still works after it", name)
		}
	}
}

// Every writer of a cut-off ends the sessions issued before it on both instances, though their issue times
// read after the cut-off's: the order is the revocation sequence each session recorded at sign-in.
func TestPG_EveryCutOffWriterEndsAnEarlierSessionUnderABackwardClockStep(t *testing.T) {
	// cut is the writer, run on instance b; it receives the sessions as they are when it runs.
	type cut func(t *testing.T, e *scimEnv, sessions map[string]*http.Cookie)
	revoke := func(body string) cut {
		return func(t *testing.T, e *scimEnv, _ map[string]*http.Cookie) {
			admin := accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})
			if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/sessions/revoke", admin, body); w.Code != http.StatusNoContent {
				t.Fatalf("revoke %s = %d %s", body, w.Code, w.Body.String())
			}
		}
	}
	signedIn := func(t *testing.T, e *scimEnv) map[string]*http.Cookie {
		c := sessionCookieOf(e.signIn(e.a, "sub-alice", "alice@corp.example"))
		if c == nil {
			t.Fatal("sign-in gave no session")
		}
		return map[string]*http.Cookie{"signed-in": c}
	}
	// leaver is a person bound by object id, with a session under each form the suspension cuts: the sub,
	// the entra form (no identity row of its own), and another sub carrying the address.
	leaver := func(e *scimEnv) (string, map[string]*http.Cookie) {
		const email = "leaver@corp.example"
		e.seedEntra("sub-leaver", email, oidLeaver)
		id := e.postUserID(e.a, oidLeaver, email, email)
		now := time.Now().UTC()
		return id, map[string]*http.Cookie{
			"sub":   e.orderedCookieAt("sub-leaver", email, 0, now),
			"entra": e.orderedCookieAt(entraPrincipalPrefix+e.tenant+":"+oidLeaver, "x@corp.example", 0, now),
			"email": e.orderedCookieAt("idp-other-sub", "LEAVER@Corp.Example", 0, now),
		}
	}
	cases := []struct {
		name string
		// issue builds the instances and the sessions, and returns the writer that must end them.
		issue func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut)
	}{
		{"revoke", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			e := newSCIMEnv(t)
			return e, signedIn(t, e), revoke(`{"sub":"sub-alice"}`)
		}},
		{"sign out everywhere", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			e := newSCIMEnv(t)
			return e, signedIn(t, e), revoke(`{"sub":"sub-alice","sessions_only":true}`)
		}},
		{"revoke all", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			e := newSCIMEnv(t)
			return e, signedIn(t, e), revoke(`{"all":true}`)
		}},
		{"sign out", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			e := newSCIMEnv(t)
			return e, signedIn(t, e), func(t *testing.T, e *scimEnv, sessions map[string]*http.Cookie) {
				if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/auth/logout", sessions["signed-in"], ""); w.Code != http.StatusFound {
					t.Fatalf("sign-out = %d %s", w.Code, w.Body.String())
				}
			}
		}},
		{"SCIM suspension", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			e := newSCIMEnv(t)
			id, sessions := leaver(e)
			return e, sessions, func(t *testing.T, e *scimEnv, _ map[string]*http.Cookie) {
				if w := e.patch(e.b, id, patchOf(`"False"`)); w.Code != http.StatusOK {
					t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
				}
			}
		}},
		{"SCIM delete", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			e := newSCIMEnv(t)
			id, sessions := leaver(e)
			return e, sessions, func(t *testing.T, e *scimEnv, _ map[string]*http.Cookie) {
				if w := e.scim(e.b, http.MethodDelete, "/scim/v2/Users/"+id, ""); w.Code != http.StatusNoContent {
					t.Fatalf("delete = %d %s", w.Code, w.Body.String())
				}
			}
		}},
		{"SCIM group removal", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			w := newMoverWorld(t)
			sessions := map[string]*http.Cookie{}
			for i, c := range w.moverCookies {
				sessions["mover "+strconv.Itoa(i)] = c
			}
			return w.e, sessions, func(t *testing.T, e *scimEnv, _ map[string]*http.Cookie) {
				if r := e.patchGroup(e.b, w.group.ID, groupRemovePatch(t, "entra-doc-patch-group-remove-members.json", w.moverID)); r.Code != http.StatusOK {
					t.Fatalf("remove member = %d %s", r.Code, r.Body.String())
				}
			}
		}},
		{"unbind", func(t *testing.T) (*scimEnv, map[string]*http.Cookie, cut) {
			e := newSCIMEnv(t)
			const sub, email = "sub-unbound", "unbound@corp.example"
			row := e.seedEntra(sub, email, uuid.NewString())
			return e, map[string]*http.Cookie{"sub": e.orderedCookieAt(sub, email, row.AuthorityEpoch, time.Now().UTC())},
				func(t *testing.T, e *scimEnv, _ map[string]*http.Cookie) {
					super := accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})
					if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/admin/identities/"+row.ID.String()+"/unbind", super, ""); w.Code/100 != 2 {
						t.Fatalf("unbind = %d %s", w.Code, w.Body.String())
					}
				}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, sessions, cutOff := c.issue(t)
			for name, s := range sessions {
				sessions[name] = steppedAhead(t, s)
			}
			for name, s := range sessions {
				if !e.cookieWorks(e.a, s) || !e.cookieWorks(e.b, s) {
					t.Fatalf("control: the %s session does not work before the cut-off", name)
				}
			}
			cutOff(t, e, sessions)
			for name, s := range sessions {
				for node, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
					if e.cookieWorks(n, s) {
						t.Errorf("instance %s: the %s session, issued before the cut-off, still works after it", node, name)
					}
				}
			}
		})
	}
}

// A session that recorded no revocation sequence (signed in before the sequence existed, or on a replica
// that predates it) is ordered by its issue time, as before: a cut-off after it ends it on both instances,
// and one before it does not.
func TestPG_SessionWithoutASequenceIsOrderedByItsIssueTime(t *testing.T) {
	e := newSCIMEnv(t)
	const sub, email = "sub-legacy", "legacy@corp.example"
	before := scimCookieAt(t, sub, email, 0, time.Now().UTC().Add(-clockStepMargin))
	for _, n := range []*scimNode{e.a, e.b} {
		if !e.cookieWorks(n, before) {
			t.Fatal("control: the session does not work before the cut-off")
		}
	}
	if err := e.rev.RevokeSub(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	after := scimCookieAt(t, sub, email, 0, time.Now().UTC().Add(clockStepMargin))
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if e.cookieWorks(n, before) {
			t.Errorf("instance %s: a session issued before the cut-off still works after it", name)
		}
		if !e.cookieWorks(n, after) {
			t.Errorf("instance %s: a session issued after the cut-off was refused", name)
		}
	}
}
