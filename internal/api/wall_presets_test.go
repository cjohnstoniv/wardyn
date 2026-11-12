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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The wall preset routes (wall_presets.go) over an in-memory store: the built-ins, the templates'
// authority on every scope, the one owner subject, the missing-id 404 and the audit rows.

// fakeWallPresets is the in-memory half of store.WallPresetStore. It shares the template store's lock
// and grants, as the Postgres store shares the grant table.
type fakeWallPresets struct {
	ts      *fakeTemplateStore
	presets map[string]types.WallPreset
}

func (f *fakeWallPresets) ListWallPresetsVisible(_ context.Context, person string, groups []string) ([]types.WallPreset, error) {
	f.ts.mu.Lock()
	defer f.ts.mu.Unlock()
	var out []types.WallPreset
	for _, p := range f.presets {
		if p.Scope == types.WallPresetScopeOrg || (p.Scope == types.WallPresetScopePerson && p.OwnerID == person) ||
			(p.Scope == types.WallPresetScopeGroup && slices.Contains(groups, p.GroupID)) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b types.WallPreset) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func (f *fakeWallPresets) GetWallPreset(_ context.Context, id uuid.UUID) (types.WallPreset, error) {
	f.ts.mu.Lock()
	defer f.ts.mu.Unlock()
	p, ok := f.presets[id.String()]
	if !ok {
		return p, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeWallPresets) nameTaken(p types.WallPreset) bool {
	for _, o := range f.presets {
		if o.ID != p.ID && o.Owner() == p.Owner() && strings.EqualFold(o.Name, p.Name) {
			return true
		}
	}
	return false
}

func (f *fakeWallPresets) CreateWallPreset(_ context.Context, p types.WallPreset, grant *store.TemplateGrantCheck) (types.WallPreset, error) {
	f.ts.mu.Lock()
	defer f.ts.mu.Unlock()
	switch {
	case !f.ts.grantHeld(grant):
		return p, store.ErrTemplateGrantGone
	case f.nameTaken(p):
		return p, store.ErrTemplateNameTaken
	}
	now := time.Now().UTC()
	p.Version, p.UpdatedAt = 1, &now
	f.presets[p.ID] = p
	return p, nil
}

func (f *fakeWallPresets) UpdateWallPreset(_ context.Context, p types.WallPreset, expected int, grant *store.TemplateGrantCheck) (types.WallPreset, error) {
	f.ts.mu.Lock()
	defer f.ts.mu.Unlock()
	cur, ok := f.presets[p.ID]
	switch {
	case !f.ts.grantHeld(grant):
		return p, store.ErrTemplateGrantGone
	case !ok:
		return p, store.ErrNotFound
	case cur.Version != expected:
		return p, &store.TemplateRevisionConflictError{Current: cur.Version}
	case f.nameTaken(p):
		return p, store.ErrTemplateNameTaken
	}
	now := time.Now().UTC()
	cur.Name, cur.Description, cur.Spec, cur.Version, cur.UpdatedAt, cur.UpdatedBy = p.Name, p.Description, p.Spec, cur.Version+1, &now, p.UpdatedBy
	f.presets[p.ID] = cur
	return cur, nil
}

func (f *fakeWallPresets) DeleteWallPreset(_ context.Context, id uuid.UUID, grant *store.TemplateGrantCheck) (types.WallPreset, error) {
	f.ts.mu.Lock()
	defer f.ts.mu.Unlock()
	p, ok := f.presets[id.String()]
	switch {
	case !f.ts.grantHeld(grant):
		return p, store.ErrTemplateGrantGone
	case !ok:
		return p, store.ErrNotFound
	}
	delete(f.presets, id.String())
	return p, nil
}

type wallPresetTestStore struct {
	templateTestStore
	*fakeWallPresets
}

var _ store.WallPresetStore = wallPresetTestStore{}

type wallPresetEnv struct {
	*templateEnv
	wp *fakeWallPresets
}

func newWallPresetEnv(t *testing.T) *wallPresetEnv {
	t.Helper()
	h := newHarness(t)
	ts := newFakeTemplateStore()
	wp := &fakeWallPresets{ts: ts, presets: map[string]types.WallPreset{}}
	cfg := baseTestConfig(h, wallPresetTestStore{templateTestStore{fakeTemplateStore: ts}, wp})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	return &wallPresetEnv{templateEnv: &templateEnv{t: t, srv: New(cfg), ts: ts, audit: h.audit}, wp: wp}
}

// save POSTs a preset and answers it, failing the test on anything but 201.
func (e *wallPresetEnv) save(c *http.Cookie, body string) types.WallPreset {
	e.t.Helper()
	code, out := e.call(c, http.MethodPost, "/api/v1/wall-presets", body)
	if code != http.StatusCreated {
		e.t.Fatalf("POST %s = %d %s", body, code, out)
	}
	var p types.WallPreset
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// expect calls and checks the status and, when reason is set, the wire reason.
func (e *wallPresetEnv) expect(c *http.Cookie, method, path, body string, status int, reason string) string {
	e.t.Helper()
	code, out := e.call(c, method, path, body)
	var b errorBody
	_ = json.Unmarshal([]byte(out), &b)
	if code != status || (reason != "" && b.Reason != reason) {
		e.t.Errorf("%s %s = %d %s, want %d %s", method, path, code, out, status, reason)
	}
	return out
}

const tightSpec = `"spec":{"first_use_approval":"always_deny","denied_domains":["pastebin.com"]}`

func TestWallPresetBuiltinsPassTheWallsValidator(t *testing.T) {
	for _, p := range types.BuiltinWallPresets() {
		if err := validatePersonalWalls(p.Spec); err != nil {
			t.Errorf("built-in %s fails the walls validator: %v", p.ID, err)
		}
		if p.ID != types.WallPresetOpen && p.Spec.Empty() {
			t.Errorf("built-in %s names no limit", p.ID)
		}
	}
}

// TestWallPresetRoutesAnswer501WithoutTheStore: a store with no wall preset seam answers 501 on every
// route, and stores nothing.
func TestWallPresetRoutesAnswer501WithoutTheStore(t *testing.T) {
	e := newTemplateEnv(t)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	for _, rt := range [][2]string{{http.MethodGet, "/api/v1/wall-presets"}, {http.MethodPost, "/api/v1/wall-presets"},
		{http.MethodGet, "/api/v1/wall-presets/sealed"}, {http.MethodPut, "/api/v1/wall-presets/sealed"}, {http.MethodDelete, "/api/v1/wall-presets/sealed"}} {
		code, body := e.call(alice, rt[0], rt[1], `{}`)
		if code != http.StatusNotImplemented || !strings.Contains(body, reasonWallPresetsUnavailable) {
			t.Errorf("%s %s = %d %s", rt[0], rt[1], code, body)
		}
	}
}

// TestWallPresetListAndBuiltins: the list is the built-ins first, then what the caller may read, with
// where they may save; a built-in reads by its name; a missing or malformed id is 404. The admin token
// and a member may both read; only a signed-in person writes.
func TestWallPresetListAndBuiltins(t *testing.T) {
	e := newWallPresetEnv(t)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	bob := templateCookie(t, "bob", oidc.RoleUser, false, []string{}, false)
	mine := e.save(alice, `{"scope":"person","name":"Mine",`+tightSpec+`}`)

	for _, who := range []*http.Cookie{alice, bob} {
		_, body := e.call(who, http.MethodGet, "/api/v1/wall-presets", "")
		var l client.WallPresetList
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, p := range l.Presets {
			ids = append(ids, p.ID)
		}
		want := []string{types.WallPresetOpen, types.WallPresetReadOnly, types.WallPresetSealed, types.WallPresetNoNetwork}
		if who == alice {
			want = append(want, mine.ID)
		}
		if !slices.Equal(ids, want) || l.Publish.Org || l.Publish.Groups == nil {
			t.Errorf("list = %v publish %+v, want %v", ids, l.Publish, want)
		}
	}
	if body := e.expect(bob, http.MethodGet, "/api/v1/wall-presets/sealed", "", http.StatusOK, ""); !strings.Contains(body, `"scope":"builtin"`) {
		t.Errorf("GET sealed = %s", body)
	}
	missing := e.expect(bob, http.MethodGet, "/api/v1/wall-presets/"+uuid.NewString(), "", http.StatusNotFound, reasonWallPresetNotFound)
	foreign := e.expect(bob, http.MethodGet, "/api/v1/wall-presets/"+mine.ID, "", http.StatusNotFound, reasonWallPresetNotFound)
	if missing != foreign {
		t.Errorf("someone else's preset answers %q, a missing one %q: the id is an existence oracle", foreign, missing)
	}
	e.expect(bob, http.MethodGet, "/api/v1/wall-presets/Sealed", "", http.StatusNotFound, reasonWallPresetNotFound)

	if w := do(t, e.srv, http.MethodGet, "/api/v1/wall-presets", adminToken, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"sealed"`) {
		t.Errorf("admin token list = %d %s", w.Code, w.Body)
	}
	if w := do(t, e.srv, http.MethodPost, "/api/v1/wall-presets", adminToken, `{"scope":"org","name":"x",`+tightSpec+`}`); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonWallsNeedPerson {
		t.Errorf("admin token POST = %d %s, want 400 %s", w.Code, w.Body, reasonWallsNeedPerson)
	}
}

// TestWallPresetPersonScope: a person preset is keyed on the caller's one owner subject, readable and
// writable by them alone (an organisation administrator included), never sends who saved it, and
// audits knobs and sizes but never its lists.
func TestWallPresetPersonScope(t *testing.T) {
	e := newWallPresetEnv(t)
	alice := templateCookie(t, "Alice", oidc.RoleUser, false, []string{}, false)
	root := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	mine := e.save(alice, `{"scope":"person","name":"Mine",`+tightSpec+`}`)
	if mine.Version != 1 || mine.UpdatedBy != "" || mine.Scope != types.WallPresetScopePerson {
		t.Fatalf("created = %+v", mine)
	}
	stored := e.wp.presets[mine.ID]
	if stored.OwnerID == "" || stored.OwnerID != types.CanonicalUserSubject(stored.OwnerID) || stored.UpdatedBy != stored.OwnerID {
		t.Fatalf("stored owner %q, writer %q: want one canonical subject", stored.OwnerID, stored.UpdatedBy)
	}
	path := "/api/v1/wall-presets/" + mine.ID
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		e.expect(root, m, path, `{"name":"x","expected_version":1,`+tightSpec+`}`, http.StatusNotFound, reasonWallPresetNotFound)
	}
	e.expect(alice, http.MethodPut, path, `{"name":"Mine",`+tightSpec+`}`, http.StatusUnprocessableEntity, reasonWallPresetInvalid)
	e.expect(alice, http.MethodPut, path, `{"scope":"org","name":"Mine","expected_version":1,`+tightSpec+`}`, http.StatusUnprocessableEntity, reasonWallPresetInvalid)
	e.expect(alice, http.MethodPut, path, `{"name":"Mine 2","expected_version":1,`+tightSpec+`}`, http.StatusOK, "")
	e.expect(alice, http.MethodPut, path, `{"name":"Mine 3","expected_version":1,`+tightSpec+`}`, http.StatusConflict, reasonWallPresetVersionConflict)
	e.expect(alice, http.MethodDelete, path, "", http.StatusNoContent, "")

	actions := map[string]json.RawMessage{}
	for _, ev := range e.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "wall_preset.") {
			if ev.Target != mine.ID {
				t.Errorf("%s target = %q", ev.Action, ev.Target)
			}
			actions[ev.Action] = ev.Data
		}
	}
	for _, a := range []string{auditWallPresetCreate, auditWallPresetUpdate, auditWallPresetDelete} {
		data := string(actions[a])
		if data == "" || strings.Contains(data, "pastebin") || strings.Contains(data, `"spec"`) || !strings.Contains(data, `"scope":"person"`) {
			t.Errorf("%s data = %s", a, data)
		}
	}
	if !strings.Contains(string(actions[auditWallPresetUpdate]), `"denied_domains":1`) || strings.Contains(string(actions[auditWallPresetDelete]), "knobs") {
		t.Errorf("update data %s, delete data %s", actions[auditWallPresetUpdate], actions[auditWallPresetDelete])
	}
}

// TestWallPresetSharedScopes: the organisation's presets are written by the effective organisation
// administrator only (never from the User view); a group's by a verified member holding the template
// group-admin grant; any member reads them. A shared write audits its whole spec.
func TestWallPresetSharedScopes(t *testing.T) {
	e := newWallPresetEnv(t)
	root := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	rootAsUser := templateCookie(t, "root", oidc.RoleAdmin, true, []string{}, false)
	gary := templateCookie(t, "gary", oidc.RoleUser, false, []string{"eng"}, false)
	gina := templateCookie(t, "gina", oidc.RoleUser, false, []string{"eng"}, false)
	garyStale := templateCookie(t, "gary", oidc.RoleUser, false, []string{"eng"}, true)
	bob := templateCookie(t, "bob", oidc.RoleUser, false, []string{}, false)

	org := `{"scope":"org","name":"Org tight",` + tightSpec + `}`
	e.expect(gary, http.MethodPost, "/api/v1/wall-presets", org, http.StatusForbidden, reasonWallPresetScopeForbidden)
	e.expect(rootAsUser, http.MethodPost, "/api/v1/wall-presets", org, http.StatusForbidden, reasonWallPresetScopeForbidden)
	op := e.save(root, org)
	if op.UpdatedBy == "" {
		t.Error("an org preset does not say who saved it")
	}
	e.expect(bob, http.MethodGet, "/api/v1/wall-presets/"+op.ID, "", http.StatusOK, "")
	e.expect(bob, http.MethodDelete, "/api/v1/wall-presets/"+op.ID, "", http.StatusForbidden, reasonWallPresetScopeForbidden)

	grp := `{"scope":"group","group_id":"eng","name":"Eng tight",` + tightSpec + `}`
	e.expect(gary, http.MethodPost, "/api/v1/wall-presets", grp, http.StatusForbidden, reasonWallPresetScopeForbidden)
	e.grant("eng", "gary")
	e.expect(garyStale, http.MethodPost, "/api/v1/wall-presets", grp, http.StatusForbidden, reasonWallPresetScopeForbidden)
	gp := e.save(gary, grp)
	gpath := "/api/v1/wall-presets/" + gp.ID
	e.expect(gina, http.MethodGet, gpath, "", http.StatusOK, "")
	e.expect(gina, http.MethodPut, gpath, `{"name":"x","expected_version":1,`+tightSpec+`}`, http.StatusForbidden, reasonWallPresetScopeForbidden)
	e.expect(bob, http.MethodGet, gpath, "", http.StatusNotFound, reasonWallPresetNotFound)
	e.expect(bob, http.MethodPut, gpath, `{"name":"x","expected_version":1,`+tightSpec+`}`, http.StatusNotFound, reasonWallPresetNotFound)
	e.expect(garyStale, http.MethodGet, gpath, "", http.StatusNotFound, reasonWallPresetNotFound)
	e.expect(garyStale, http.MethodDelete, gpath, "", http.StatusForbidden, reasonWallPresetScopeForbidden)
	e.expect(root, http.MethodPut, gpath, `{"name":"x","expected_version":1,`+tightSpec+`}`, http.StatusNotFound, reasonWallPresetNotFound)
	e.expect(gary, http.MethodPut, gpath, `{"name":"Eng tighter","expected_version":1,`+tightSpec+`}`, http.StatusOK, "")

	for _, ev := range e.audit.snapshot() {
		if ev.Action == auditWallPresetCreate && ev.Target == gp.ID {
			if d := string(ev.Data); !strings.Contains(d, `"group_id":"eng"`) || !strings.Contains(d, "pastebin.com") {
				t.Errorf("group create data = %s", d)
			}
		}
	}
}

// TestWallPresetWriteRefusals: built-ins are never written; a built-in's name, an empty spec, a
// widening spec and a duplicate name in one audience are refused, and nothing is stored.
func TestWallPresetWriteRefusals(t *testing.T) {
	e := newWallPresetEnv(t)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		e.expect(alice, m, "/api/v1/wall-presets/read-only", `{"name":"x","expected_version":1,`+tightSpec+`}`, http.StatusForbidden, reasonWallPresetBuiltin)
	}
	post := func(body string, status int, reason string) {
		t.Helper()
		e.expect(alice, http.MethodPost, "/api/v1/wall-presets", body, status, reason)
	}
	post(`{"scope":"person","name":"SEALED",`+tightSpec+`}`, http.StatusConflict, reasonWallPresetNameTaken)
	post(`{"scope":"person","name":"x","spec":{}}`, http.StatusUnprocessableEntity, reasonWallPresetInvalid)
	post(`{"scope":"person","name":"x","spec":{"allow_all_egress":true}}`, http.StatusUnprocessableEntity, reasonWallPresetInvalid)
	post(`{"scope":"person","name":" ",`+tightSpec+`}`, http.StatusUnprocessableEntity, reasonWallPresetInvalid)
	post(`{"scope":"builtin","name":"x",`+tightSpec+`}`, http.StatusUnprocessableEntity, reasonWallPresetInvalid)
	post(`{"scope":"person","name":"x","spec":{"nope":1}}`, http.StatusBadRequest, "")
	if len(e.wp.presets) != 0 {
		t.Fatalf("a refused write stored %d presets", len(e.wp.presets))
	}
	e.save(alice, `{"scope":"person","name":"Tight",`+tightSpec+`}`)
	post(`{"scope":"person","name":"tight",`+tightSpec+`}`, http.StatusConflict, reasonWallPresetNameTaken)
	if n := len(e.wp.presets); n != 1 {
		t.Fatalf("stored %d presets, want 1", n)
	}
}

// TestWallPresetPersonalIsInvisibleToEveryoneButItsOwner: someone's personal preset is read by its owner
// alone, with no administrator exception (the design gives an administrator no read of a person's walls).
// To anyone else its GET is the 404 a missing id gets, byte for byte, so the id confirms nothing, and no
// list of theirs holds it, by id, name or spec.
func TestWallPresetPersonalIsInvisibleToEveryoneButItsOwner(t *testing.T) {
	e := newWallPresetEnv(t)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	mine := e.save(alice, `{"scope":"person","name":"Alice private",`+tightSpec+`}`)
	path := "/api/v1/wall-presets/" + mine.ID
	e.expect(alice, http.MethodGet, path, "", http.StatusOK, "")

	readers := map[string]*http.Cookie{
		"another user":   templateCookie(t, "bob", oidc.RoleUser, false, []string{}, false),
		"an admin":       templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false),
		"security admin": templateCookie(t, "sec", oidc.RoleSecurityAdmin, false, []string{}, false),
	}
	for who, c := range readers {
		missing := e.expect(c, http.MethodGet, "/api/v1/wall-presets/"+uuid.NewString(), "", http.StatusNotFound, reasonWallPresetNotFound)
		if got := e.expect(c, http.MethodGet, path, "", http.StatusNotFound, reasonWallPresetNotFound); got != missing {
			t.Errorf("%s: someone's personal preset answers %q, a missing id %q: the id is an existence oracle", who, got, missing)
		}
		_, list := e.call(c, http.MethodGet, "/api/v1/wall-presets", "")
		if strings.Contains(list, mine.ID) || strings.Contains(list, "Alice private") || strings.Contains(list, "pastebin") {
			t.Errorf("%s: the preset list holds someone else's personal preset: %s", who, list)
		}
	}
}

// TestWallPresetLaunchRefusesSomeoneElsesPersonalPreset: the one place a launch resolves its wall preset
// (launchWallPreset, behind the policy preview, Review and create) answers a non-owner naming someone's
// personal preset exactly as it answers an id that does not exist (422 wall_preset_unknown, the id echoed
// and nothing more), applies nothing, and shows none of the preset's rows; its owner applies it.
func TestWallPresetLaunchRefusesSomeoneElsesPersonalPreset(t *testing.T) {
	e := newWallPresetEnv(t)
	mine := tightPreset(types.WallPresetScopePerson, "Alice private")
	mine.OwnerID = "alice"
	e.wp.presets[mine.ID] = mine
	launch := func(ctx context.Context, id string) (*types.WallPresetOnRun, *runRefusal) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		return e.srv.launchWallPreset(ctx, r, id)
	}
	owner := withHumanIdentity(context.Background(), "alice", "alice@corp.example", oidc.RoleUser, types.UserTypeStandard, []string{}, false)
	if p, refusal := launch(owner, mine.ID); refusal != nil || p == nil || p.ID != mine.ID || p.Spec.Empty() {
		t.Fatalf("the owner: copy %+v refusal %+v, want the preset applied", p, refusal)
	}
	for who, role := range map[string]string{"another user": oidc.RoleUser, "an admin": oidc.RoleAdmin, "security admin": oidc.RoleSecurityAdmin} {
		ctx := withHumanIdentity(context.Background(), "bob", "bob@corp.example", role, types.UserTypeStandard, []string{}, false)
		p, refusal := launch(ctx, mine.ID)
		if p != nil || refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonWallPresetUnknown {
			t.Errorf("%s: copy %+v refusal %+v, want 422 %s", who, p, refusal, reasonWallPresetUnknown)
			continue
		}
		gone := uuid.NewString()
		_, missing := launch(ctx, gone)
		if missing == nil || strings.ReplaceAll(refusal.body.Error, mine.ID, "ID") != strings.ReplaceAll(missing.body.Error, gone, "ID") {
			t.Errorf("%s: someone's preset is refused %q, a missing one %+v: the sentence differs", who, refusal.body.Error, missing)
		}
		if b, _ := json.Marshal(refusal.body); strings.Contains(string(b), "pastebin") || strings.Contains(string(b), "Alice private") || strings.Contains(string(b), "ssh_key") {
			t.Errorf("%s: the refusal leaks the preset: %s", who, b)
		}
	}
}
