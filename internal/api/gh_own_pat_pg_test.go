// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// A run holds one GitHub credential: github.com repositories decided by two
// own-token rows, or by an own-token row beside a shared one, are refused at
// launch with the reason, the shared row's grant is never rewritten to a
// person's token, and the mixed refusal names both rows (never a secret).
func TestPG_GitHubOwnToken_RunRefusesMoreThanOneDeciderRow(t *testing.T) {
	e := newOwnerOnlyPG(t)
	ctx := context.Background()
	other := ghOwnTestRow()
	other.ID, other.BaseURLs = "gh-own-two", []string{"https://github.com/beta"}
	shared := types.GitProvider{ID: "gh-shared", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/gamma"},
		Lanes: []types.GitLane{types.GitLanePAT}}
	policy := e.storePolicyFor(t, "gh-own", "github.com", "git-pat-github-com", false)
	refusal := func(t *testing.T, rows ...types.GitProvider) string {
		t.Helper()
		if _, err := e.h.srv.cfg.Store.PutSiteConfig(ctx, adoSite(rows...)); err != nil {
			t.Fatalf("PutSiteConfig: %v", err)
		}
		w := e.createRun(t, "alice", policy)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"reason":"`+reasonGitHubOwnTokenUndeliverable+`"`) {
			t.Fatalf("create = %d %s, want 422 %s", w.Code, w.Body.String(), reasonGitHubOwnTokenUndeliverable)
		}
		return w.Body.String()
	}
	t.Run("two own-token rows: refused", func(t *testing.T) {
		if body := refusal(t, ghOwnTestRow(), other); !strings.Contains(body, "two GitHub rows") {
			t.Fatalf("body = %s, want the two-rows refusal", body)
		}
	})
	t.Run("an own-token row beside a shared one: refused, naming neither row to a member", func(t *testing.T) {
		body := refusal(t, ghOwnTestRow(), shared)
		if !strings.Contains(body, "a shared GitHub provider and your own token") || strings.Contains(body, shared.ID) ||
			strings.Contains(body, ghOwnRowID) || strings.Contains(body, "github_pat_") {
			t.Fatalf("body = %s, want the member form: no row id and no token", body)
		}
	})
}

// An own-token git_pat grant on a local placement (the person's own runner)
// is refused at create: the token is not handed to a runner.
func TestGitHubOwnPATRun_LocalPlacementRefused(t *testing.T) {
	grant := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"github.com","secret_name":"git-pat-github-com"}`)}
	s := &Server{cfg: Config{Now: time.Now}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	_, ok := s.ownGitHubTokenGrants(r.Context(), w, r, uuid.New(), []types.GrantSpec{grant}, adoSite(ghOwnTestRow()), nil, types.PlacementLocal)
	if ok || w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), gitHubOwnTokenLocalRefusal) ||
		!strings.Contains(w.Body.String(), `"reason":"`+reasonGitHubOwnTokenUndeliverable+`"`) {
		t.Fatalf("local placement: ok=%v %d %s, want 422 %s", ok, w.Code, w.Body.String(), reasonGitHubOwnTokenUndeliverable)
	}
}
