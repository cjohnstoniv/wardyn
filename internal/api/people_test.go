// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mintRemovedBody is the refusal's body, byte for byte (#1477, ROLE-01): the
// approved console packet's wording, the same for every caller and subject.
const mintRemovedBody = `{"error":"No one can create a token that acts as another person. They sign in and create their own.","reason":"person_token_mint_removed"}`

// TestPeople_MintRouteRefusesEveryCaller is #1477's contract at the HTTP
// boundary: no role can mint a token that acts as another person. A security
// admin and a super admin are both refused with the same 403 and the same
// body, for a person who exists and for one who does not, and the refusal
// writes one denied audit row and no api_tokens row.
func TestPeople_MintRouteRefusesEveryCaller(t *testing.T) {
	h := newHarness(t)
	tokenRows := newTokenMemStore()
	st := &personMintFakeStore{Store: tokenRows, people: map[string]types.Person{
		"synthetic-person": {Principal: "synthetic-person", Email: "person@corp.example"},
	}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = newAccessAuth(t, map[string]string{
		"sec@corp.example": oidc.RoleSecurityAdmin, "root@corp.example": oidc.RoleAdmin,
	}, oidc.RoleUser, nil, nil)
	srv := New(cfg)
	audit := func() (denied int, other int) {
		for _, ev := range h.audit.snapshot() {
			if ev.Action != "person.token.create" {
				continue
			}
			if ev.Outcome == "denied" {
				denied++
			} else {
				other++
			}
		}
		return
	}
	for _, c := range []struct {
		name string
		as   *http.Cookie
	}{
		{"security admin", accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})},
		{"super admin", accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})},
	} {
		t.Run(c.name, func(t *testing.T) {
			before, _ := audit()
			var bodies []string
			for _, target := range []string{"synthetic-person", "nobody-at-all"} {
				w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+target+"/tokens", c.as, `{"name":"x"}`)
				if w.Code != http.StatusForbidden || errorReason(w) != "person_token_mint_removed" {
					t.Fatalf("mint for %s: %d %s, want 403 person_token_mint_removed", target, w.Code, w.Body.String())
				}
				bodies = append(bodies, strings.TrimSpace(w.Body.String()))
			}
			if bodies[0] != bodies[1] || bodies[0] != mintRemovedBody {
				t.Fatalf("bodies differ between a known and an unknown person, or from the canon: %q", bodies)
			}
			if got, other := audit(); got-before != 2 || other != 0 {
				t.Errorf("denied rows added = %d, other rows = %d, want 2 and 0", got-before, other)
			}
		})
	}
	if n := len(tokenRows.byID); n != 0 {
		t.Fatalf("%d api_tokens rows exist after the refusals, want none", n)
	}
	// The denied row names the target and the reason, and the target is cut.
	long := strings.Repeat("a", 400)
	root := accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+long+"/tokens", root, `{"name":"x"}`); w.Code != http.StatusForbidden {
		t.Fatalf("long target: %d", w.Code)
	}
	evs := h.audit.snapshot()
	last := evs[len(evs)-1]
	if last.Action != "person.token.create" || last.Outcome != "denied" || !strings.Contains(string(last.Data), `"reason":"person_token_mint_removed"`) ||
		strings.Contains(string(last.Data), long) || !strings.Contains(string(last.Data), strings.Repeat("a", 256)) {
		t.Errorf("last audit row = %+v data=%s, want denied, the reason, and the target cut to 256 bytes", last, last.Data)
	}
}

// TestPeople_MintRouteNoStoreGuardsStayFirst pins the order: the two no-store
// guards answer before the removal refusal, so an API-token caller keeps
// api_token_from_api_token and the admin token keeps mint_no_human, and a
// non-admin caller never reaches the handler (no row).
func TestPeople_MintRouteNoStoreGuardsStayFirst(t *testing.T) {
	h := newHarness(t)
	tokenRows := newTokenMemStore()
	st := &personMintFakeStore{Store: tokenRows, people: map[string]types.Person{}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = newAccessAuth(t, map[string]string{"root@corp.example": oidc.RoleAdmin}, oidc.RoleUser, nil, nil)
	srv := New(cfg)
	root := accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})
	tok, _ := mintToken(t, srv, root, "ci")
	for _, c := range []struct {
		name, bearer, reason string
	}{
		{"an API token", tok, "api_token_from_api_token"},
		{"the admin token", adminToken, "mint_no_human"},
	} {
		w := do(t, srv, http.MethodPost, "/api/v1/people/x/tokens", c.bearer, `{"name":"x"}`)
		if w.Code != http.StatusForbidden || errorReason(w) != c.reason {
			t.Errorf("%s: %d %s, want 403 %s", c.name, w.Code, w.Body.String(), c.reason)
		}
	}
	mem := accessSession(t, "mem", "mem@corp.example", oidc.RoleUser, []string{})
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/x/tokens", mem, `{"name":"x"}`); w.Code != http.StatusForbidden || errorReason(w) == "person_token_mint_removed" {
		t.Errorf("member: %d %s, want the existing 403 from the route's tier", w.Code, w.Body.String())
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "person.token.create" {
			t.Errorf("a refused-before-the-handler call wrote %+v", ev)
		}
	}
}

// TestListAllAPITokens_MintedForOthersInventory pins the #1477 inventory: with
// ?minted_for_others=true GET /tokens lists only live tokens whose minter is
// someone other than their owner, never a plaintext; without the parameter the
// route answers exactly as before, and a person can still mint their own.
func TestListAllAPITokens_MintedForOthersInventory(t *testing.T) {
	h := newHarness(t)
	st := newTokenMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = newAccessAuth(t, map[string]string{"sec@corp.example": oidc.RoleSecurityAdmin}, oidc.RoleUser, nil, nil)
	srv := New(cfg)
	sec := accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})
	now := time.Now().UTC()
	seed := func(principal, mintedBy string, revoked bool) types.APIToken {
		t.Helper()
		row := types.APIToken{ID: uuid.New(), Principal: principal, Role: oidc.RoleUser, Name: "n", MintedBy: mintedBy, CreatedAt: now}
		if revoked {
			row.RevokedAt = &now
		}
		created, err := st.CreateAPIToken(context.Background(), row, "wdn_"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	legacy := seed("pat", "admin-1", false)
	seed("pat", "admin-1", true) // revoked: gone from the inventory
	seed("pat", "pat", false)    // self-minted: not for others
	seed("pat", "", false)       // owner-minted: not for others
	_, own := mintToken(t, srv, sec, "mine")

	w := doSSO(t, srv, http.MethodGet, "/api/v1/tokens?minted_for_others=true", sec, "")
	if w.Code != http.StatusOK {
		t.Fatalf("inventory: %d %s", w.Code, w.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["id"] != legacy.ID.String() || rows[0]["minted_by"] != "admin-1" {
		t.Fatalf("inventory = %s, want exactly the live admin-minted token", w.Body.String())
	}
	if _, has := rows[0]["token"]; has {
		t.Errorf("inventory row carries a token: %v", rows[0])
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/tokens", sec, ""); !strings.Contains(w.Body.String(), own.ID.String()) || !strings.Contains(w.Body.String(), legacy.ID.String()) {
		t.Errorf("without the parameter the route lists everything, got %s", w.Body.String())
	}
}
