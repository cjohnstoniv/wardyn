// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// "Sign out everywhere" (sessions_only) ends the person's browser sessions on every instance and leaves
// their API token and SSH key authenticating: the cut is not the credential cutoff those two read.
func TestRevokeSessions_SessionsOnlyKeepsTokensAndKeysAuthenticating(t *testing.T) {
	e := newSCIMEnv(t)
	ctx := context.Background()
	const sub, email = "sub-alice", "alice@corp.example"
	cookie, _, token := e.mintTokenAs(e.a, entrafake.Identity{Username: email, Subject: sub})
	key, err := e.st.AddSSHKey(ctx, types.SSHPublicKey{Fingerprint: "SHA256:alice-key", Principal: sub, Name: "laptop",
		PublicKey: "ssh-ed25519 AAAA alice", Role: oidc.RoleUser, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*scimNode{e.a, e.b} {
		if !e.cookieWorks(n, cookie) || !e.tokenWorks(n, token) || n.srv.sshKeyRevocationRefusal(ctx, key) != "" {
			t.Fatal("a credential did not work before the sign-out")
		}
	}
	time.Sleep(10 * time.Millisecond)

	// The directory the drawer re-reads after the sign-out: a live session before it, none after.
	activeSessions := func() int {
		t.Helper()
		w := doSSO(t, e.a.srv, http.MethodGet, "/api/v1/people?q="+sub, accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{}), "")
		var page struct {
			People []struct {
				Principal      string `json:"principal"`
				ActiveSessions int    `json:"active_sessions"`
			} `json:"people"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.People) != 1 {
			t.Fatalf("GET /people?q=%s = %d %s", sub, w.Code, w.Body.String())
		}
		return page.People[0].ActiveSessions
	}
	if got := activeSessions(); got != 1 {
		t.Fatalf("active_sessions before the sign-out = %d, want 1", got)
	}

	admin := accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})
	if w := doSSO(t, e.a.srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"`+sub+`","sessions_only":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("sessions_only revoke = %d %s", w.Code, w.Body.String())
	}

	if got := activeSessions(); got != 0 {
		t.Errorf("active_sessions after the sign-out = %d, want 0", got)
	}
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if e.cookieWorks(n, cookie) {
			t.Errorf("instance %s: the session issued before the sign-out still works", name)
		}
		if !e.tokenWorks(n, token) {
			t.Errorf("instance %s: the API token stopped authenticating", name)
		}
		if why := n.srv.sshKeyRevocationRefusal(ctx, key); why != "" {
			t.Errorf("instance %s: the SSH key is refused: %s", name, why)
		}
	}
	if st, err := oidc.CheckSession(ctx, e.rev, sub, email, time.Now().Add(-time.Minute), -1); err != nil || st != oidc.SessionLive {
		t.Errorf("token/key owner check = %v, %v; want live", st, err)
	}
	toks, err := e.st.ListAPITokensByPrincipal(ctx, sub)
	if err != nil || len(toks) != 1 || toks[0].RevokedAt != nil {
		t.Errorf("API token rows = %+v, %v; want one, unrevoked", toks, err)
	}
	if got := e.keyCount(sub); got != 1 {
		t.Errorf("SSH key rows = %d, want 1", got)
	}
}
