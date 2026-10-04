// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// The Groups tests: the identity provider tells Wardyn a person left a group, and their sessions are cut and
// the API tokens whose login-time group snapshot holds that group are revoked, nobody else's. Everything is
// Postgres-backed (WARDYN_TEST_PG), two instances over one database, a fake Entra tenant as the provider.

const (
	// groupClaim is the group's object id as the id_token's groups claim carries it; the SCIM externalId is
	// the same id in another case, so a match that does not fold case revokes nothing.
	groupClaim    = "3f2a9c10-7B1D-4e6a-9C55-0a1b2c3d4e5f"
	otherGroup    = "9d8c7b6a-0000-4000-8000-00000000f00d"
	oidMover      = "bbbbbbbb-0000-4000-8000-0000000000b1"
	oidMember     = "bbbbbbbb-0000-4000-8000-0000000000b2"
	moverEmail    = "mover@corp.example"
	memberEmail   = "member@corp.example"
	bystanderMail = "bystander@corp.example"
)

var groupExternalID = strings.ToUpper(groupClaim)

type scimGroup struct {
	ID          string        `json:"id"`
	ExternalID  string        `json:"externalId"`
	DisplayName string        `json:"displayName"`
	Members     []scim.Member `json:"members"`
}

func (e *scimEnv) postGroup(n *scimNode, externalID, displayName string) (scimGroup, int) {
	e.t.Helper()
	raw, _ := json.Marshal(map[string]any{"schemas": []string{scim.SchemaGroup}, "externalId": externalID, "displayName": displayName})
	w := e.scim(n, http.MethodPost, "/scim/v2/Groups", string(raw))
	if w.Code != http.StatusCreated {
		return scimGroup{}, w.Code
	}
	return decodeSCIMGroup(e.t, w.Body.Bytes()), w.Code
}

func decodeSCIMGroup(t *testing.T, raw []byte) scimGroup {
	t.Helper()
	var g scimGroup
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode SCIM group: %v: %s", err, raw)
	}
	return g
}

func (e *scimEnv) patchGroup(n *scimNode, id, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.scim(n, http.MethodPatch, "/scim/v2/Groups/"+id, body)
}

const patchEnvelope = `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[%s]}`

func addMembersPatch(ids ...string) string {
	vals := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		vals = append(vals, map[string]any{"value": id})
	}
	raw, _ := json.Marshal(vals)
	return strings.Replace(patchEnvelope, "%s", `{"op":"Add","path":"members","value":`+string(raw)+`}`, 1)
}

// groupRemovePatch is a recorded member-remove fixture with its member pointed at the person memberID: the
// recorded payloads carry the identity provider's own sample ids.
func groupRemovePatch(t *testing.T, fixture, memberID string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(scimFixture(t, fixture)), &body); err != nil {
		t.Fatal(err)
	}
	op := body["Operations"].([]any)[0].(map[string]any)
	if path, _ := op["path"].(string); strings.HasPrefix(path, "members[") {
		op["path"] = `members[value eq "` + memberID + `"]`
	} else {
		op["value"].([]any)[0].(map[string]any)["value"] = memberID
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// mintTokenAs signs the person in with the claims id carries and mints an API token through the real
// self-service route, so the token's group snapshot is what login normalisation made of the claim.
func (e *scimEnv) mintTokenAs(n *scimNode, id entrafake.Identity) (*http.Cookie, uuid.UUID, string) {
	e.t.Helper()
	resp := e.signInAs(n, id)
	cookie := sessionCookieOf(resp)
	if cookie == nil {
		e.t.Fatalf("sign-in of %s gave no session: %d %s %s", id.Username, resp.Code, resp.Header().Get("Location"), resp.Body.String())
	}
	w := doSSO(e.t, n.srv, http.MethodPost, "/api/v1/me/tokens", cookie, `{"name":"t"}`)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("mint a token as %s = %d %s", id.Username, w.Code, w.Body.String())
	}
	var created types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Token == "" {
		e.t.Fatalf("decode token: %v %s", err, w.Body.String())
	}
	return cookie, created.ID, created.Token
}

type groupToken struct {
	id  uuid.UUID
	raw string
}

// moverWorld is a group with two members and the credentials around it:
//   - the mover holds three tokens (snapshot with the group, snapshot without it, truncated snapshot) and
//     three sessions;
//   - another member of the group holds a token whose snapshot holds the group;
//   - an unrelated person holds a token whose snapshot is truncated.
type moverWorld struct {
	e                               *scimEnv
	group                           scimGroup
	moverID, memberID               string
	moverCookies                    []*http.Cookie
	withGroup, without, truncated   groupToken
	otherMember, unrelatedTruncated groupToken
	memberCookie, unrelatedCookie   *http.Cookie
}

func newMoverWorld(t *testing.T, shape ...func(*Config)) *moverWorld {
	t.Helper()
	e := newSCIMEnv(t, shape...)
	w := &moverWorld{e: e}
	mint := func(dst *groupToken, id entrafake.Identity) *http.Cookie {
		c, tid, raw := e.mintTokenAs(e.a, id)
		*dst = groupToken{tid, raw}
		return c
	}
	var cookies [3]*http.Cookie
	cookies[0] = mint(&w.withGroup, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{groupClaim, "other-team"}})
	cookies[1] = mint(&w.without, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{"other-team"}})
	cookies[2] = mint(&w.truncated, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{"other-team"}, GroupsOverage: true})
	w.moverCookies = cookies[:]
	w.memberCookie = mint(&w.otherMember, entrafake.Identity{Username: memberEmail, Subject: "sub-member", Groups: []string{groupClaim}})
	w.unrelatedCookie = mint(&w.unrelatedTruncated, entrafake.Identity{Username: bystanderMail, Subject: "sub-bystander", GroupsOverage: true})

	w.moverID = e.postUserID(e.a, oidMover, moverEmail, moverEmail)
	w.memberID = e.postUserID(e.a, oidMember, memberEmail, memberEmail)
	g, code := e.postGroup(e.a, groupExternalID, "Engineering")
	if code != http.StatusCreated {
		t.Fatalf("POST /Groups = %d", code)
	}
	w.group = g
	if r := e.patchGroup(e.a, g.ID, addMembersPatch(w.moverID, w.memberID)); r.Code != http.StatusOK {
		t.Fatalf("add members = %d %s", r.Code, r.Body.String())
	}
	return w
}

// stillWorks asserts the credentials the removal must not touch, and that the ones it must are gone.
func (w *moverWorld) checkRemoved(t *testing.T, nodes ...*scimNode) {
	t.Helper()
	e := w.e
	for _, n := range nodes {
		for i, c := range w.moverCookies {
			if e.cookieWorks(n, c) {
				t.Errorf("the mover's session %d still works", i)
			}
		}
		if e.tokenWorks(n, w.withGroup.raw) || e.tokenWorks(n, w.truncated.raw) {
			t.Error("a token whose snapshot holds the group, or cannot prove it absent, still works")
		}
		if !e.tokenWorks(n, w.without.raw) {
			t.Error("the mover's token without the group stopped authenticating")
		}
		if !e.tokenWorks(n, w.otherMember.raw) || !e.cookieWorks(n, w.memberCookie) {
			t.Error("another member of the group lost a credential to the mover's removal")
		}
		if !e.tokenWorks(n, w.unrelatedTruncated.raw) || !e.cookieWorks(n, w.unrelatedCookie) {
			t.Error("an unrelated person's truncated-snapshot token or session was touched")
		}
	}
	for name, tok := range map[string]groupToken{"with the group": w.withGroup, "truncated": w.truncated} {
		if !e.tokenRevoked(tok.id) {
			t.Errorf("the token %s is not marked revoked", name)
		}
	}
	for name, tok := range map[string]groupToken{"without the group": w.without, "of another member": w.otherMember, "of an unrelated person": w.unrelatedTruncated} {
		if e.tokenRevoked(tok.id) {
			t.Errorf("the token %s was revoked", name)
		}
	}
}

func (e *scimEnv) groupJobsDone(identityID, groupID string) bool {
	e.t.Helper()
	jobs, err := e.st.ListDeprovisionJobs(context.Background(), uuid.MustParse(identityID), store.JobKindGroupRemove)
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, j := range jobs {
		if j.Target != groupID {
			continue
		}
		n++
		if !j.Done {
			return false
		}
	}
	return n == len(store.GroupRemovalSteps)
}

// Removing a person from a group, by each shape the identity provider is recorded sending, cuts their sessions
// and revokes exactly their tokens whose group snapshot holds the group or cannot say. The snapshots come from
// real sign-ins whose groups claim names the group in a different case from the SCIM externalId.
func TestSCIMGroupMemberRemoveRevokesTheMoversTokens(t *testing.T) {
	for _, fixture := range []string{
		"entra-doc-patch-group-remove-members.json",
		"entra-doc-patch-group-remove-members-value-array.json",
		"entra-doc-patch-group-remove-members-filter-path.json",
		"rfc-patch-group-remove-member-filter-path.json",
	} {
		t.Run(strings.TrimSuffix(fixture, ".json"), func(t *testing.T) {
			w := newMoverWorld(t)
			e := w.e
			for _, c := range w.moverCookies {
				if !e.cookieWorks(e.a, c) {
					t.Fatal("control: a session does not work before the removal")
				}
			}
			for _, tok := range []groupToken{w.withGroup, w.without, w.truncated, w.otherMember, w.unrelatedTruncated} {
				if !e.tokenWorks(e.a, tok.raw) {
					t.Fatal("control: a token does not work before the removal")
				}
			}
			// The mover's snapshots are what login normalisation made of the claim.
			toks, err := e.st.ListAPITokensByPrincipal(context.Background(), "sub-mover")
			if err != nil || len(toks) != 3 {
				t.Fatalf("tokens of the mover = %v, %v", toks, err)
			}
			for _, tk := range toks {
				if tk.ID == w.withGroup.id && !slices.Contains(tk.Groups, strings.ToLower(groupClaim)) {
					t.Fatalf("the snapshot is %v, want the lower-cased group id", tk.Groups)
				}
			}

			// The removal lands on the other instance than the one the credentials were minted on.
			r := e.patchGroup(e.b, w.group.ID, groupRemovePatch(t, fixture, w.moverID))
			if r.Code != http.StatusOK {
				t.Fatalf("remove member = %d %s", r.Code, r.Body.String())
			}
			if g := decodeSCIMGroup(t, r.Body.Bytes()); len(g.Members) != 1 || g.Members[0].Value != w.memberID {
				t.Errorf("members after the removal = %+v, want only the other member", g.Members)
			}
			w.checkRemoved(t, e.a, e.b)

			rows := e.rows(e.b, "scim.group.member_remove")
			if len(rows) != 1 || rows[0].Target != w.moverID || rows[0].Actor != scimActor {
				t.Fatalf("scim.group.member_remove rows = %+v, want one for the mover", rows)
			}
			if d := dataOf(t, rows[0]); d["tokens_revoked"] != float64(2) || d["group"] != w.group.ID || d["slot"] != scimSlotPrimary {
				t.Errorf("row data = %v, want 2 tokens revoked and the group", d)
			}
			if !e.groupJobsDone(w.moverID, w.group.ID) {
				t.Error("the ledger has pending group_remove steps")
			}
			if got := len(e.rows(e.b, "token.revoke")); got != 2 {
				t.Errorf("%d token.revoke rows, want one per revoked token", got)
			}

			// Their next sign-in carries a fresh snapshot, and a repeated request costs nothing: it does not
			// cut that new session or write the row again.
			fresh := sessionCookieOf(e.signInAs(e.a, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{"other-team"}}))
			if fresh == nil || !e.cookieWorks(e.a, fresh) {
				t.Fatal("the mover cannot sign in again")
			}
			if r := e.patchGroup(e.b, w.group.ID, groupRemovePatch(t, fixture, w.moverID)); r.Code != http.StatusOK {
				t.Fatalf("repeat = %d", r.Code)
			}
			if !e.cookieWorks(e.a, fresh) || len(e.rows(e.b, "scim.group.member_remove")) != 1 {
				t.Error("a repeated removal cut the fresh session or wrote its row again")
			}
		})
	}
}

// A removal that fails part way answers 5xx and is resumed by the retry: the steps that finished stay done,
// the audit row is written once, and its count is the whole removal's.
func TestSCIMGroupRemovalIsDurable(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	patch := groupRemovePatch(t, "entra-doc-patch-group-remove-members-value-array.json", w.moverID)

	e.rev.setFail(true)
	e.b.audit.fail("scim.group.member_remove", true)
	if r := e.patchGroup(e.b, w.group.ID, patch); r.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose session cut failed = %d, want 5xx so the provider retries", r.Code)
	}
	if e.groupJobsDone(w.moverID, w.group.ID) || len(e.rows(e.b, "scim.group.member_remove")) != 0 {
		t.Fatal("a failed removal is recorded as done")
	}
	if !e.tokenRevoked(w.withGroup.id) || !e.tokenRevoked(w.truncated.id) {
		t.Error("the token step did not run beside the failed session step")
	}

	e.rev.setFail(false)
	if r := e.patchGroup(e.b, w.group.ID, patch); r.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose audit row failed = %d, want 5xx", r.Code)
	}
	e.b.audit.fail("scim.group.member_remove", false)
	if r := e.patchGroup(e.a, w.group.ID, patch); r.Code != http.StatusOK {
		t.Fatalf("the retry = %d %s", r.Code, r.Body.String())
	}
	w.checkRemoved(t, e.a, e.b)
	if !e.groupJobsDone(w.moverID, w.group.ID) {
		t.Error("the ledger still has pending steps after the retry")
	}
	rows := e.rows(e.a, "scim.group.member_remove")
	if len(rows) != 1 || dataOf(t, rows[0])["tokens_revoked"] != float64(2) {
		t.Errorf("rows after the retry = %+v, want one counting both tokens", rows)
	}
}

// A person added to the group again after a removal is removed again for real.
func TestSCIMGroupReaddedMemberIsRemovedAgain(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	patch := groupRemovePatch(t, "entra-doc-patch-group-remove-members.json", w.moverID)
	if r := e.patchGroup(e.a, w.group.ID, patch); r.Code != http.StatusOK {
		t.Fatalf("remove = %d", r.Code)
	}
	_, _, raw := e.mintTokenAs(e.a, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{groupClaim}})
	if r := e.patchGroup(e.a, w.group.ID, addMembersPatch(w.moverID)); r.Code != http.StatusOK {
		t.Fatalf("re-add = %d", r.Code)
	}
	if !e.tokenWorks(e.a, raw) {
		t.Fatal("a member add revoked a token")
	}
	if r := e.patchGroup(e.a, w.group.ID, patch); r.Code != http.StatusOK {
		t.Fatalf("second remove = %d", r.Code)
	}
	if e.tokenWorks(e.a, raw) || len(e.rows(e.a, "scim.group.member_remove")) != 2 {
		t.Error("the second removal did not revoke the new token and write its row")
	}
}

// DELETE of a group removes every member as a mover, then the group; remove-all by PATCH is the same sweep.
func TestSCIMGroupDeleteAndRemoveAllMoveEveryone(t *testing.T) {
	t.Run("DELETE is not answered 204 when a token was revoked under the sweep", func(t *testing.T) {
		race := &revokeRaceStore{}
		w := newMoverWorld(t, func(c *Config) {
			race.PG = c.Store.(store.PG)
			c.Store = race
		})
		e := w.e
		race.arm(w.withGroup.id)
		if r := e.scim(e.a, http.MethodDelete, "/scim/v2/Groups/"+w.group.ID, ""); r.Code < 500 {
			t.Fatalf("DELETE with a token revoked under the sweep = %d %s, want 5xx so the provider retries", r.Code, r.Body.String())
		}
		if r := e.scim(e.a, http.MethodGet, "/scim/v2/Groups/"+w.group.ID, ""); r.Code != http.StatusOK {
			t.Errorf("the group is gone while its removal is unfinished: %d", r.Code)
		}
		if r := e.scim(e.b, http.MethodDelete, "/scim/v2/Groups/"+w.group.ID, ""); r.Code != http.StatusNoContent {
			t.Fatalf("the retried DELETE = %d %s", r.Code, r.Body.String())
		}
		if r := e.scim(e.a, http.MethodGet, "/scim/v2/Groups/"+w.group.ID, ""); r.Code != http.StatusNotFound {
			t.Errorf("GET of the deleted group = %d, want 404", r.Code)
		}
		if !e.tokenRevoked(w.truncated.id) || !e.tokenRevoked(w.withGroup.id) {
			t.Error("the retried DELETE left the mover's group-holding tokens unrevoked")
		}
		// Each instance keeps its own audit log; the pair holds one row per member, none repeated by the retry.
		if got := len(e.rows(e.a, "scim.group.member_remove")) + len(e.rows(e.b, "scim.group.member_remove")); got != 2 {
			t.Errorf("%d scim.group.member_remove rows, want one per member", got)
		}
	})
	t.Run("DELETE", func(t *testing.T) {
		w := newMoverWorld(t)
		e := w.e
		if r := e.scim(e.b, http.MethodDelete, "/scim/v2/Groups/"+w.group.ID, ""); r.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d %s", r.Code, r.Body.String())
		}
		if r := e.scim(e.a, http.MethodGet, "/scim/v2/Groups/"+w.group.ID, ""); r.Code != http.StatusNotFound {
			t.Errorf("GET of the deleted group = %d, want 404", r.Code)
		}
		if r := e.scim(e.a, http.MethodDelete, "/scim/v2/Groups/"+w.group.ID, ""); r.Code != http.StatusNotFound {
			t.Errorf("a second DELETE = %d, want 404", r.Code)
		}
		if e.tokenWorks(e.a, w.withGroup.raw) || e.tokenWorks(e.a, w.otherMember.raw) || !e.tokenWorks(e.a, w.without.raw) || !e.tokenWorks(e.a, w.unrelatedTruncated.raw) {
			t.Error("the deletion did not remove every member as a mover, or reached someone else")
		}
		if got := len(e.rows(e.b, "scim.group.member_remove")); got != 2 {
			t.Errorf("%d scim.group.member_remove rows, want one per member", got)
		}
	})
	t.Run("remove every member by PATCH", func(t *testing.T) {
		w := newMoverWorld(t)
		e := w.e
		if r := e.patchGroup(e.a, w.group.ID, scimFixture(t, "rfc-patch-group-remove-all-members.json")); r.Code != http.StatusOK {
			t.Fatalf("remove all = %d %s", r.Code, r.Body.String())
		}
		if e.tokenWorks(e.a, w.withGroup.raw) || e.tokenWorks(e.a, w.otherMember.raw) || !e.tokenWorks(e.a, w.without.raw) {
			t.Error("removing every member did not sweep each of them")
		}
		if r := e.scim(e.a, http.MethodGet, "/scim/v2/Groups/"+w.group.ID, ""); r.Code != http.StatusOK {
			t.Errorf("the group is gone after its members were removed: %d", r.Code)
		}
	})
}

// A group created and a member added are stored and grant nothing: no authorisation decision changes, no
// credential is touched, nothing is audited, and no code outside the store and the SCIM routes reads the tables.
func TestSCIMGroupAddGrantsNothing(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	snapshot := func() map[string]string {
		out := map[string]string{}
		for name, c := range map[string]*http.Cookie{"mover": w.moverCookies[0], "member": w.memberCookie} {
			r := doSSO(t, e.a.srv, http.MethodGet, "/api/v1/me", c, "")
			out["cookie "+name] = r.Body.String()
		}
		for name, tok := range map[string]groupToken{"with": w.withGroup, "without": w.without, "truncated": w.truncated} {
			r := do(t, e.a.srv, http.MethodGet, "/api/v1/me", tok.raw, "")
			out["token "+name] = r.Body.String()
		}
		return out
	}
	before := snapshot()
	var rolesBefore int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM capability_grants`).Scan(&rolesBefore); err != nil {
		t.Fatal(err)
	}

	other, code := e.postGroup(e.a, strings.ToUpper(otherGroup), "Admins")
	if code != http.StatusCreated {
		t.Fatalf("POST /Groups = %d", code)
	}
	if r := e.patchGroup(e.a, other.ID, addMembersPatch(w.moverID, w.memberID)); r.Code != http.StatusOK {
		t.Fatalf("add = %d", r.Code)
	}
	after := snapshot()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: what the person is changed after a group create and member add:\n before %s\n after  %s", k, v, after[k])
		}
	}
	var rolesAfter int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM capability_grants`).Scan(&rolesAfter); err != nil || rolesAfter != rolesBefore {
		t.Errorf("capability grants %d -> %d (%v)", rolesBefore, rolesAfter, err)
	}
	for _, ev := range e.a.h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "scim.group") {
			t.Errorf("a group create or member add was audited: %s", ev.Action)
		}
	}
	for _, tok := range []groupToken{w.withGroup, w.without, w.truncated, w.otherMember} {
		if e.tokenRevoked(tok.id) || !e.tokenWorks(e.a, tok.raw) {
			t.Error("a group create or member add touched a token")
		}
	}

	// Nothing but the SCIM routes and the store reads the group tables.
	allowed := map[string]bool{"internal/store/store_scim_groups.go": true, "internal/db/migrations/0119_scim_groups.sql": true}
	root := "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || n == ".git" || n == "ui" || n == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if allowed[rel] || strings.HasSuffix(rel, "_test.go") || !(strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, ".sql")) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "scim_group") {
			t.Errorf("%s reads the SCIM group tables; no authorisation decision may", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The routes: a filter is required, a filtered list is capped at 100 with paging ignored, an externalId is
// unique in any case, ids are never principals, and a member Wardyn does not know is ignored.
func TestSCIMGroupsRoutes(t *testing.T) {
	e := newSCIMEnv(t)
	n := e.a
	for name, path := range map[string]string{
		"no filter":           "/scim/v2/Groups",
		"an empty filter":     "/scim/v2/Groups?filter=",
		"an unsupported attr": "/scim/v2/Groups?filter=" + url.QueryEscape(`members eq "x"`),
		"an unsupported op":   "/scim/v2/Groups?filter=" + url.QueryEscape(`displayName co "x"`),
		"paging only":         "/scim/v2/Groups?startIndex=1&count=10",
		"a Users-only attr":   "/scim/v2/Groups?filter=" + url.QueryEscape(`userName eq "x"`),
		"a compound filter":   "/scim/v2/Groups?filter=" + url.QueryEscape(`displayName eq "x" and externalId eq "y"`),
	} {
		w := e.scim(n, http.MethodGet, path, "")
		if status, typ := scimErrorOf(t, w); w.Code != http.StatusBadRequest || status != "400" || typ != "invalidFilter" {
			t.Errorf("%s: %d %s, want 400 invalidFilter", name, w.Code, w.Body.String())
		}
	}
	if _, code := e.postGroup(n, "", "x"); code != http.StatusBadRequest {
		t.Errorf("POST with no externalId = %d, want 400", code)
	}

	g, code := e.postGroup(n, groupExternalID, "Engineering")
	if code != http.StatusCreated || g.ID == "" || g.ExternalID != groupExternalID {
		t.Fatalf("POST = %d %+v", code, g)
	}
	if _, code := e.postGroup(n, strings.ToLower(groupExternalID), "Again"); code != http.StatusConflict {
		t.Errorf("a second POST of the same externalId in another case = %d, want 409", code)
	}
	ids := func(attr, value string) []string {
		w := e.scim(n, http.MethodGet, "/scim/v2/Groups?filter="+url.QueryEscape(attr+` eq "`+value+`"`), "")
		var list struct {
			TotalResults int         `json:"totalResults"`
			Resources    []scimGroup `json:"Resources"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.TotalResults != len(list.Resources) {
			t.Fatalf("filter %s eq %q = %d %s", attr, value, w.Code, w.Body.String())
		}
		out := []string{}
		for _, r := range list.Resources {
			out = append(out, r.ID)
		}
		return out
	}
	for _, f := range [][2]string{{"externalId", strings.ToLower(groupExternalID)}, {"displayName", "ENGINEERING"}} {
		if got := ids(f[0], f[1]); !slices.Equal(got, []string{g.ID}) {
			t.Errorf("filter %s eq %q = %v, want [%s]", f[0], f[1], got, g.ID)
		}
	}
	if got := ids("displayName", "nobody"); len(got) != 0 {
		t.Errorf("a filter that matches nothing returned %v", got)
	}
	for _, id := range []string{"not-a-uuid", uuid.NewString(), "sub-mover"} {
		if w := e.scim(n, http.MethodGet, "/scim/v2/Groups/"+id, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET /Groups/%s = %d, want 404", id, w.Code)
		}
		if w := e.patchGroup(n, id, addMembersPatch(uuid.NewString())); w.Code != http.StatusNotFound {
			t.Errorf("PATCH /Groups/%s = %d, want 404", id, w.Code)
		}
	}

	// A displayName replace renames; an externalId change is refused; a member that names nobody is ignored.
	rename := strings.Replace(patchEnvelope, "%s", `{"op":"Replace","path":"displayName","value":"Platform"}`, 1)
	if w := e.patchGroup(n, g.ID, rename); w.Code != http.StatusOK || decodeSCIMGroup(t, w.Body.Bytes()).DisplayName != "Platform" {
		t.Errorf("rename = %d %s", w.Code, w.Body.String())
	}
	if got := ids("displayName", "platform"); !slices.Equal(got, []string{g.ID}) {
		t.Errorf("the renamed group is not found under its new name: %v", got)
	}
	moveExternal := strings.Replace(patchEnvelope, "%s", `{"op":"Replace","path":"externalId","value":"`+uuid.NewString()+`"}`, 1)
	if w := e.patchGroup(n, g.ID, moveExternal); w.Code != http.StatusBadRequest {
		t.Errorf("an externalId change = %d, want 400", w.Code)
	}
	if w := e.patchGroup(n, g.ID, addMembersPatch("u1091", uuid.NewString())); w.Code != http.StatusOK || len(decodeSCIMGroup(t, w.Body.Bytes()).Members) != 0 {
		t.Errorf("adding members Wardyn does not know = %d %s", w.Code, w.Body.String())
	}
	if w := e.patchGroup(n, g.ID, groupRemovePatch(t, "entra-doc-patch-group-remove-members.json", uuid.NewString())); w.Code != http.StatusOK {
		t.Errorf("removing a member Wardyn does not know = %d", w.Code)
	}
	if got := e.rows(n, "scim.group.member_remove"); len(got) != 0 {
		t.Errorf("a removal of nobody was audited: %+v", got)
	}

	// Filtered lists carry at most 100 groups, whatever startIndex and count say.
	for range 101 {
		if _, err := e.st.CreateScimGroup(context.Background(), uuid.NewString(), "bulk", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	w := e.scim(n, http.MethodGet, "/scim/v2/Groups?filter="+url.QueryEscape(`displayName eq "bulk"`)+"&startIndex=5&count=3", "")
	var list struct {
		TotalResults int         `json:"totalResults"`
		StartIndex   int         `json:"startIndex"`
		Resources    []scimGroup `json:"Resources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Resources) != 100 || list.TotalResults != 100 || list.StartIndex != 1 {
		t.Errorf("a filtered list = %d resources, total %d, start %d (%v), want 100, 100, 1", len(list.Resources), list.TotalResults, list.StartIndex, err)
	}

	// The recorded group POST has the shape Wardyn stores.
	var rec struct {
		ExternalID string `json:"externalId"`
	}
	post := scimFixture(t, "entra-doc-post-group.json")
	_ = json.Unmarshal([]byte(post), &rec)
	if w := e.scim(n, http.MethodPost, "/scim/v2/Groups", post); w.Code != http.StatusCreated || decodeSCIMGroup(t, w.Body.Bytes()).ExternalID != rec.ExternalID {
		t.Errorf("the recorded POST /Groups = %d %s", w.Code, w.Body.String())
	}
}

// revokeRaceStore revokes an armed token first, as the owner's own DELETE would between the sweep's listing and
// its revoke, so the sweep's revoke finds nothing live and answers store.ErrNotFound.
type revokeRaceStore struct {
	store.PG
	mu    sync.Mutex
	armed map[uuid.UUID]bool
}

func (r *revokeRaceStore) arm(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.armed == nil {
		r.armed = map[uuid.UUID]bool{}
	}
	r.armed[id] = true
}

func (r *revokeRaceStore) RevokeAPIToken(ctx context.Context, id uuid.UUID, principal string, now time.Time) (types.APIToken, error) {
	r.mu.Lock()
	race := r.armed[id]
	delete(r.armed, id)
	r.mu.Unlock()
	if race {
		if _, err := r.PG.RevokeAPIToken(ctx, id, principal, now); err != nil {
			return types.APIToken{}, err
		}
	}
	return r.PG.RevokeAPIToken(ctx, id, principal, now)
}
