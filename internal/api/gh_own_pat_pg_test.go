// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's own GitHub token end to end, over a real run store, secret store
// and broker: a stored policy's git_pat grant for github.com, decided by an
// own-token row, needs no shared secret; it is refused at launch until the
// person has a usable token; then the run's grant reads that person's own
// sealed token, owner_only (never the operator's row of the same name), the
// mint answers it with the person's login, and with the PAT broker off the
// mint door refuses it rather than hand it into the sandbox.
func TestPG_GitHubOwnToken_RunUsesTheOwnersToken(t *testing.T) {
	e := newOwnerOnlyPG(t)
	ctx := context.Background()
	if _, err := e.h.srv.cfg.Store.PutSiteConfig(ctx, adoSite(ghOwnTestRow())); err != nil {
		t.Fatalf("PutSiteConfig: %v", err)
	}
	policy := e.storePolicyFor(t, "gh-own", "github.com", "git-pat-github-com", false)
	name := types.GitHubOwnTokenSecretName(ghOwnRowID)
	blob := func(token string, expires time.Time) []byte {
		raw, _ := json.Marshal(types.GitHubOwnToken{Token: token, AccountID: 101, Login: "alice-gh", ExpiresOn: expires, StoredAt: time.Now()})
		return raw
	}
	if err := e.sec.Put(ctx, name, blob("github_pat_operator", time.Now().AddDate(0, 0, 5))); err != nil {
		t.Fatal(err)
	}

	refused := func(t *testing.T, sub, reason string) {
		t.Helper()
		w := e.createRun(t, sub, policy)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"reason":"`+reason+`"`) {
			t.Fatalf("create as %s = %d %s, want 422 %s", sub, w.Code, w.Body.String(), reason)
		}
	}
	t.Run("no token of the person's own: refused at launch", func(t *testing.T) {
		refused(t, "alice", reasonGitHubOwnTokenRequired)
	})
	t.Run("a token past the expiry the person entered: refused at launch", func(t *testing.T) {
		if err := e.sec.For("bob").Put(ctx, name, blob("github_pat_bob", time.Now().Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
		refused(t, "bob", reasonGitHubOwnTokenExpired)
	})

	if err := e.sec.For("alice").Put(ctx, name, blob("github_pat_alice", time.Now().AddDate(0, 0, 5))); err != nil {
		t.Fatal(err)
	}
	created := mustCreate(t, e.createRun(t, "alice", policy))
	t.Run("the grant reads the owner's own token, owner_only", func(t *testing.T) {
		var run createRunResponse
		if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		grants, err := store.NewPG(e.pool).ListGrantsByRun(ctx, run.ID)
		if err != nil || len(grants) != 1 {
			t.Fatalf("grants = %+v (%v)", grants, err)
		}
		pat, err := types.DecodeGitPATScope(grants[0].Spec.Scope)
		if err != nil || pat.SecretName != name || pat.Host != "github.com" || !grants[0].Spec.OwnerOnly {
			t.Fatalf("grant = %+v %s, want github.com reading %s, owner_only", grants[0].Spec, grants[0].Spec.Scope, name)
		}
		uses := slices.DeleteFunc(e.h.audit.snapshot(), func(ev types.AuditEvent) bool { return ev.Action != gitHubPATAuditOwnUse })
		if len(uses) != 1 || uses[0].RunID == nil || *uses[0].RunID != run.ID || uses[0].Actor != "alice" ||
			strings.Contains(string(uses[0].Data), "github_pat_") {
			t.Fatalf("use rows = %+v, want one for the run by alice, without the token", uses)
		}
	})
	t.Run("the mint answers the owner's token and login", func(t *testing.T) {
		w := e.mint(t, "alice", created)
		var m mintResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &m) != nil || m.Token != "github_pat_alice" || m.Username != "alice-gh" {
			t.Fatalf("mint = %d %s, want alice's own token as alice-gh", w.Code, w.Body.String())
		}
	})
	t.Run("with the PAT broker off the mint door refuses it", func(t *testing.T) {
		e.h.srv.cfg.DisableGitPATBroker = true
		defer func() { e.h.srv.cfg.DisableGitPATBroker = false }()
		w := e.mint(t, "alice", created)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonGitHubOwnTokenResident) || strings.Contains(w.Body.String(), "github_pat_") {
			t.Fatalf("mint with the broker off = %d %s, want 403 %s and no token", w.Code, w.Body.String(), reasonGitHubOwnTokenResident)
		}
		refused(t, "alice", reasonGitHubOwnTokenUndeliverable)
	})
}
