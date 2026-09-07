// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F341: an admin's cross-principal ?owner= took the raw query string as the
// namespace key, with no canonicalization and no resolution. Naming a real
// member by their EMAIL (or by a case-variant of their subject) answered 204
// with an outcome=success audit row while the PUT landed in a namespace nobody
// reads and the DELETE left the live secret in place.
//
// The shipped contract, pinned below: ?owner= names a HUMAN, and is resolved to
// the namespace key that human's own writes land in — matched case-insensitively
// against the principals this deployment knows, and mapped from the email form
// through the SAME (principal, email) pairing revokeAPITokensFor already matches
// on. An email form that pairs to no known principal is REFUSED, never
// answered 204, because it can only ever mint a namespace nothing reads.

// f341Store is authzStore plus the api_tokens directory the resolver reads —
// the one place this codebase holds a (principal, email) pairing.
type f341Store struct {
	*authzStore
	toks []types.APIToken
}

func (s *f341Store) ListAPITokens(context.Context) ([]types.APIToken, error) { return s.toks, nil }

// f341Server is secretsRBACServer with a store wired, so the owner resolver has
// a directory to consult.
func f341Server(t *testing.T, sec *memSecrets, toks []types.APIToken) (*harness, *Server) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, &f341Store{authzStore: newAuthzStore(), toks: toks})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = sec
	return h, New(cfg)
}

// f341Alice is the directory row pairing alice's subject with the email her IdP
// sends — exactly the pairing an admin naming her by email is relying on.
func f341Alice() []types.APIToken {
	return []types.APIToken{{
		ID: uuid.New(), Principal: "alice", Email: "alice@corp.example",
		Role: string(oidc.RoleMember), CreatedAt: time.Now().UTC(),
	}}
}

func f341Admin(t *testing.T) *http.Cookie {
	t.Helper()
	return ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
}

// TestF341_OwnerParamResolvesIdentityForms is the headline arm: an admin naming
// a member by EMAIL, or by a case-variant of their subject, must reach the same
// row the member's own self-service call does.
func TestF341_OwnerParamResolvesIdentityForms(t *testing.T) {
	for _, form := range []string{"alice@corp.example", "ALICE", "alice"} {
		t.Run(form, func(t *testing.T) {
			sec := &memSecrets{m: map[string][]byte{}}
			_, srv := f341Server(t, sec, f341Alice())
			admin := f341Admin(t)

			w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key?owner="+form, admin,
				`{"value":"sk-ant-alices-key-value-000"}`)
			if w.Code != http.StatusNoContent {
				t.Fatalf("admin PUT ?owner=%s = %d, want 204: %s", form, w.Code, w.Body.String())
			}
			got, err := sec.For("alice").Get(context.Background(), "anthropic-api-key")
			if err != nil || string(got) != "sk-ant-alices-key-value-000" {
				t.Fatalf("alice's OWN namespace after an admin PUT ?owner=%s = (%q, %v); the write landed somewhere she cannot read", form, got, err)
			}

			// And the delete half: it must remove the row the member can see.
			if w := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/anthropic-api-key?owner="+form, admin, ""); w.Code != http.StatusNoContent {
				t.Fatalf("admin DELETE ?owner=%s = %d, want 204: %s", form, w.Code, w.Body.String())
			}
			if names, _ := sec.For("alice").List(context.Background()); len(names) != 0 {
				t.Fatalf("alice's secret is STILL LIVE (%v) after the admin's revoke naming her %q", names, form)
			}
		})
	}
}

// TestF341_OwnerParamCaseCollision: an OIDC subject is opaque and
// case-SENSITIVE, so a deployment may legitimately hold two that differ only by
// case. An EXACT match must win outright, and a value that merely folds onto
// both must be REFUSED rather than resolved to whichever the directory happened
// to list first — guessing there writes a credential into the wrong human's
// namespace, which is worse than the miss this finding is about.
func TestF341_OwnerParamCaseCollision(t *testing.T) {
	collision := []types.APIToken{
		{ID: uuid.New(), Principal: "alice", Email: "alice@corp.example", CreatedAt: time.Now().UTC()},
		{ID: uuid.New(), Principal: "ALICE", Email: "alice.other@corp.example", CreatedAt: time.Now().UTC()},
	}

	t.Run("exact match wins", func(t *testing.T) {
		for _, want := range []string{"alice", "ALICE"} {
			t.Run(want, func(t *testing.T) {
				sec := &memSecrets{m: map[string][]byte{}}
				_, srv := f341Server(t, sec, collision)
				w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key?owner="+want, f341Admin(t),
					`{"value":"sk-ant-exact-match-value-00"}`)
				if w.Code != http.StatusNoContent {
					t.Fatalf("admin PUT ?owner=%s = %d, want 204: %s", want, w.Code, w.Body.String())
				}
				if _, err := sec.For(want).Get(context.Background(), "anthropic-api-key"); err != nil {
					t.Fatalf("the exactly-named subject %q did not receive the write (%v); a case-fold overrode an exact match", want, err)
				}
				for other := range sec.owned {
					if other != want {
						t.Fatalf("the write also reached %q; an exact ?owner= must resolve to nothing else", other)
					}
				}
			})
		}
	})

	t.Run("ambiguous fold is refused", func(t *testing.T) {
		sec := &memSecrets{m: map[string][]byte{}}
		h, srv := f341Server(t, sec, collision)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key?owner=AlIcE", f341Admin(t),
			`{"value":"sk-ant-ambiguous-value-0000"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("admin PUT ?owner=AlIcE (folds onto two known subjects) = %d, want 422: %s", w.Code, w.Body.String())
		}
		if len(sec.owned) != 0 {
			t.Errorf("an ambiguous ?owner= wrote into %v; it must write nothing", sec.owned)
		}
		for _, ev := range h.audit.events {
			if ev.Action == "secret.write" && ev.Outcome == "success" {
				t.Errorf("an ambiguous ?owner= recorded secret.write outcome=success: %s", ev.Data)
			}
		}
	})

	t.Run("an email folding onto two principals is refused too", func(t *testing.T) {
		shared := []types.APIToken{
			{ID: uuid.New(), Principal: "sub-a", Email: "shared@corp.example", CreatedAt: time.Now().UTC()},
			{ID: uuid.New(), Principal: "sub-b", Email: "SHARED@corp.example", CreatedAt: time.Now().UTC()},
		}
		sec := &memSecrets{m: map[string][]byte{}}
		_, srv := f341Server(t, sec, shared)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key?owner=shared@corp.example", f341Admin(t),
			`{"value":"sk-ant-shared-email-value00"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("admin PUT ?owner=shared@corp.example (two principals) = %d, want 422: %s", w.Code, w.Body.String())
		}
		if len(sec.owned) != 0 {
			t.Errorf("an ambiguous email ?owner= wrote into %v; it must write nothing", sec.owned)
		}
	})
}

// TestF341_UnpairedEmailOwnerRefused: an email form that pairs to no principal
// this deployment knows can only mint a namespace nothing reads. It must be
// refused — never 204 with a success audit row.
func TestF341_UnpairedEmailOwnerRefused(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			sec := &memSecrets{m: map[string][]byte{}}
			h, srv := f341Server(t, sec, nil) // empty directory
			admin := f341Admin(t)

			path := "/api/v1/secrets/anthropic-api-key?owner=nobody@corp.example"
			body := `{"value":"sk-ant-orphaned-key-value00"}`
			if method == http.MethodGet {
				path, body = "/api/v1/secrets?owner=nobody@corp.example", ""
			}
			if method == http.MethodDelete {
				body = ""
			}
			w := doSSO(t, srv, method, path, admin, body)
			if w.Code == http.StatusNoContent || w.Code == http.StatusOK {
				t.Fatalf("%s ?owner=nobody@corp.example = %d — an identity form that resolves to no principal answered success", method, w.Code)
			}
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s ?owner=nobody@corp.example = %d, want 422: %s", method, w.Code, w.Body.String())
			}
			if len(sec.owned) != 0 {
				t.Errorf("a refused ?owner= minted %d orphan namespace(s): %v", len(sec.owned), sec.owned)
			}
			for _, ev := range h.audit.events {
				if ev.Outcome == "success" && (ev.Action == "secret.write" || ev.Action == "secret.delete" || ev.Action == "secret.list") {
					t.Errorf("a refused ?owner= wrote %s outcome=success: %s", ev.Action, ev.Data)
				}
			}
		})
	}
}

// TestF341_CrossNamespaceDeleteThatRemovedNothingIsReported: the DELETE half's
// own closure, and it needs no directory. An admin who names a namespace that
// does not hold the row is told so, rather than being handed the 204 +
// outcome=success that made the miss invisible. (A MEMBER's own delete keeps its
// idempotent 204 — the no-existence-oracle posture is about members.)
func TestF341_CrossNamespaceDeleteThatRemovedNothingIsReported(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := f341Server(t, sec, f341Alice())
	admin := f341Admin(t)

	w := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/anthropic-api-key?owner=alice", admin, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("admin DELETE of a row the named namespace does not hold = %d, want 404: %s", w.Code, w.Body.String())
	}
	for _, ev := range h.audit.events {
		if ev.Action == "secret.delete" && ev.Outcome == "success" {
			t.Fatalf("a delete that removed nothing recorded outcome=success: %s", ev.Data)
		}
	}

	// The member's own idempotent delete is untouched: 204, no oracle.
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/anthropic-api-key", alice, ""); w.Code != http.StatusNoContent {
		t.Fatalf("member's own DELETE of a never-set name = %d, want the idempotent 204: %s", w.Code, w.Body.String())
	}
}
