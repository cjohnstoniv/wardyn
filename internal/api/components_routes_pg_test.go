// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// componentsPG is the server over a real Postgres store, so the capability
// resolver, the restriction rows and the component rows are the real ones.
type componentsPG struct {
	h      *harness
	st     store.PG
	admin  *http.Cookie
	member *http.Cookie
}

func newComponentsPG(t *testing.T) componentsPG {
	t.Helper()
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.srv.cfg.Store = store.NewPG(pool)
	h.srv.cfg.Secrets = sec
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	if err := sec.For("").Put(context.Background(), "shared-key", []byte("v")); err != nil {
		t.Fatal(err)
	}
	return componentsPG{
		h: h, st: store.NewPG(pool),
		admin:  ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin),
		member: ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser),
	}
}

func (e componentsPG) do(t *testing.T, method, path string, who *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, method, path, who, body)
}

// putOrg has the admin create an org component and returns its id.
func (e componentsPG) putOrg(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	w := e.do(t, http.MethodPut, "/api/v1/components/"+id.String(), e.admin, saveComponentBody(name, stripeDef))
	if w.Code != http.StatusCreated {
		t.Fatalf("admin creates org component %q = %d: %s", name, w.Code, w.Body.String())
	}
	return id
}

// grantTo has the security tier write an allow/deny row for the member.
func (e componentsPG) grant(t *testing.T, subjectType, subject, kind, value, effect string) {
	t.Helper()
	body := fmt.Sprintf(`{"subject_type":%q,"subject":%q,"capability":%q,"value":%q,"effect":%q}`, subjectType, subject, kind, value, effect)
	if w := e.do(t, http.MethodPost, "/api/v1/permissions/grants", e.admin, body); w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("grant %s %s %s: %d %s", kind, value, effect, w.Code, w.Body.String())
	}
}

// memberDoor asks the door a run's attach asks, as the member, on the real store.
func (e componentsPG) memberDoor(t *testing.T, orgRowID string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).
		WithContext(withOIDCGroups(operatorCtx("sub-member", "member@corp.example", oidc.RoleUser), nil))
	ref := e.h.srv.componentAttachRefusal(r, orgRowID)
	if ref == nil {
		return 0, ""
	}
	w := httptest.NewRecorder()
	ref.write(e.h.srv, w, r)
	return w.Code, strings.TrimSpace(w.Body.String())
}

func (e componentsPG) restricted(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	got, err := e.st.ListCapabilityRestrictions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got[capComponent][id.String()]
}

func (e componentsPG) memberList(t *testing.T) client.MyComponents {
	t.Helper()
	w := e.do(t, http.MethodGet, "/api/v1/me/components", e.member, "")
	var list client.MyComponents
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		t.Fatalf("member list = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	return list
}

// TestPG_Components_A19NewOrgComponentIsAttachableByNobody: a created org
// component carries its restriction from the moment it exists. An ungranted
// member is refused with the bytes every ungranted id gets, a wildcard allow does
// not admit them, and an allow naming the id does. A create the store refuses
// leaves no restriction behind.
func TestPG_Components_A19NewOrgComponentIsAttachableByNobody(t *testing.T) {
	e := newComponentsPG(t)
	id := e.putOrg(t, "Org Stripe")

	if !e.restricted(t, id) {
		t.Fatal("the component exists but its restriction row does not")
	}
	if code, body := e.memberDoor(t, id.String()); code != http.StatusForbidden || body != componentRefusalBody {
		t.Fatalf("ungranted member = %d %s, want 403 %s", code, body, componentRefusalBody)
	}
	if len(e.memberList(t).Org) != 0 {
		t.Fatal("an ungranted org component is in the member's list")
	}

	e.grant(t, "all", "", capComponent, capWildcard, "allow")
	if code, _ := e.memberDoor(t, id.String()); code != http.StatusForbidden {
		t.Fatalf("a wildcard allow admitted the member (%d); only an allow naming the id may", code)
	}
	e.grant(t, "user", "sub-member", capComponent, id.String(), "allow")
	if code, body := e.memberDoor(t, id.String()); code != 0 {
		t.Fatalf("an allow naming the id: refused %d %s", code, body)
	}
	if got := e.memberList(t).Org; len(got) != 1 || got[0].ID != id {
		t.Fatalf("granted member's org list = %+v, want the one row", got)
	}

	// A refused create (same name, new id) leaves no restriction.
	loser := uuid.New()
	w := e.do(t, http.MethodPut, "/api/v1/components/"+loser.String(), e.admin, saveComponentBody("org stripe", stripeDef))
	if w.Code != http.StatusConflict || errorReason(w) != "component_name_conflict" {
		t.Fatalf("duplicate name = %d %s, want 409 component_name_conflict", w.Code, w.Body.String())
	}
	if e.restricted(t, loser) {
		t.Fatal("a create that was refused left its restriction behind")
	}
	// An id that is already someone's personal row is refused too, whole.
	mine := decodeSaved(t, e.do(t, http.MethodPost, "/api/v1/me/components", e.member, saveComponentBody("Mine", stripeDef)))
	if w := e.do(t, http.MethodPut, "/api/v1/components/"+mine.ID.String(), e.admin, saveComponentBody("Hijack", stripeDef)); w.Code != http.StatusConflict {
		t.Fatalf("an org PUT on a personal row's id = %d, want 409", w.Code)
	}
	if e.restricted(t, mine.ID) {
		t.Fatal("the refused org PUT restricted a personal row's id")
	}
	if got, err := e.st.GetComponent(context.Background(), mine.ID, "sub-member"); err != nil || got.Name != "Mine" {
		t.Fatalf("personal row after the refused org PUT = %+v, %v; want it untouched", got, err)
	}
}

// TestPG_Components_A11DeleteKeepsTheRestriction: deleting an org component
// removes the row and the grants naming it and keeps the restriction, so the
// id's door is never open.
func TestPG_Components_A11DeleteKeepsTheRestriction(t *testing.T) {
	e := newComponentsPG(t)
	id := e.putOrg(t, "Doomed")
	e.grant(t, "user", "sub-member", capComponent, id.String(), "allow")
	if code, _ := e.memberDoor(t, id.String()); code != 0 {
		t.Fatalf("granted member refused before the delete (%d)", code)
	}

	if w := e.do(t, http.MethodDelete, "/api/v1/components/"+id.String(), e.admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if w := e.do(t, http.MethodDelete, "/api/v1/components/"+id.String(), e.admin, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", w.Code)
	}
	if !e.restricted(t, id) {
		t.Fatal("deleting the component lifted its restriction (A11)")
	}
	grants, err := e.st.ListCapabilityGrants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		if g.Capability == capComponent && g.Value == id.String() {
			t.Errorf("grant %+v survived the delete", g)
		}
	}
	if code, body := e.memberDoor(t, id.String()); code != http.StatusForbidden || body != componentRefusalBody {
		t.Fatalf("member after the delete = %d %s, want the refusal bytes", code, body)
	}
	if rows, err := e.st.ListComponents(context.Background(), ""); err != nil || len(rows) != 0 {
		t.Fatalf("org rows after delete = %+v, %v", rows, err)
	}
	var del map[string]any
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "component.delete" {
			_ = json.Unmarshal(ev.Data, &del)
		}
	}
	if del["restriction_kept"] != true || del["grants_removed"] != float64(1) || del["name"] != "Doomed" {
		t.Errorf("component.delete datum = %v, want restriction_kept, one grant removed, and the org name", del)
	}
}

// TestPG_Components_RowCaps: 32 saved rows per person and 256 org rows; the cap
// refuses the next create and an update of an existing row still goes through.
func TestPG_Components_RowCaps(t *testing.T) {
	e := newComponentsPG(t)
	ctx := context.Background()

	for i := 0; i < componentsMaxPerPerson; i++ {
		if _, err := e.st.CreateComponent(ctx, types.Component{ID: uuid.New(), Owner: "sub-member", Name: fmt.Sprintf("p%02d", i)}); err != nil {
			t.Fatalf("seed person row %d: %v", i, err)
		}
	}
	w := e.do(t, http.MethodPost, "/api/v1/me/components", e.member, saveComponentBody("one too many", stripeDef))
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != "component_cap_reached" {
		t.Fatalf("33rd personal row = %d %s, want 422 component_cap_reached", w.Code, w.Body.String())
	}
	rows, _ := e.st.ListComponents(ctx, "sub-member")
	if w := e.do(t, http.MethodPut, "/api/v1/me/components/"+rows[0].ID.String(), e.member, saveComponentBody("p00 renamed", stripeDef)); w.Code != http.StatusOK {
		t.Fatalf("update at the cap = %d: %s", w.Code, w.Body.String())
	}
	if w := e.do(t, http.MethodDelete, "/api/v1/me/components/"+rows[1].ID.String(), e.member, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete at the cap = %d", w.Code)
	}
	if w := e.do(t, http.MethodPost, "/api/v1/me/components", e.member, saveComponentBody("fits now", stripeDef)); w.Code != http.StatusCreated {
		t.Fatalf("create after a delete = %d: %s", w.Code, w.Body.String())
	}
	// Another person's rows do not count against this one.
	other := ssoSession(t, "sub-other", "other@corp.example", oidc.RoleUser)
	if w := e.do(t, http.MethodPost, "/api/v1/me/components", other, saveComponentBody("first", stripeDef)); w.Code != http.StatusCreated {
		t.Fatalf("another person's first row = %d", w.Code)
	}

	var firstOrg uuid.UUID
	for i := 0; i < componentsMaxOrg; i++ {
		c, err := e.st.CreateComponent(ctx, types.Component{ID: uuid.New(), Name: fmt.Sprintf("o%03d", i)})
		if err != nil {
			t.Fatalf("seed org row %d: %v", i, err)
		}
		if i == 0 {
			firstOrg = c.ID
		}
	}
	w = e.do(t, http.MethodPut, "/api/v1/components/"+uuid.NewString(), e.admin, saveComponentBody("org one too many", stripeDef))
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != "component_cap_reached" {
		t.Fatalf("257th org row = %d %s, want 422 component_cap_reached", w.Code, w.Body.String())
	}
	if w := e.do(t, http.MethodPut, "/api/v1/components/"+firstOrg.String(), e.admin, saveComponentBody("o000 renamed", stripeDef)); w.Code != http.StatusOK {
		t.Fatalf("org update at the cap = %d: %s", w.Code, w.Body.String())
	}
}

// TestPG_Components_MembersAreIsolatedAndCannotWriteOrgRows: one person cannot
// read, change or delete another's row; no member can reach the org routes; the
// admin's org listing carries org rows only.
func TestPG_Components_MembersAreIsolatedAndCannotWriteOrgRows(t *testing.T) {
	e := newComponentsPG(t)
	other := ssoSession(t, "sub-other", "other@corp.example", oidc.RoleUser)
	theirs := decodeSaved(t, e.do(t, http.MethodPost, "/api/v1/me/components", other, saveComponentBody("Theirs", stripeDef)))
	org := e.putOrg(t, "Org")

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		w := e.do(t, method, "/api/v1/me/components/"+theirs.ID.String(), e.member, saveComponentBody("Taken", stripeDef))
		a := e.do(t, method, "/api/v1/me/components/"+uuid.NewString(), e.member, saveComponentBody("Taken", stripeDef))
		if w.Code != http.StatusNotFound || w.Body.String() != a.Body.String() {
			t.Errorf("%s on another person's row = %d %s, want an absent id's bytes %s", method, w.Code, w.Body.String(), a.Body.String())
		}
		// An org row's id is not a personal row of anyone's: also absent here.
		if w := e.do(t, method, "/api/v1/me/components/"+org.String(), e.member, saveComponentBody("Taken", stripeDef)); w.Code != http.StatusNotFound {
			t.Errorf("%s on an org row through /me = %d, want 404", method, w.Code)
		}
		if w := e.do(t, method, "/api/v1/me/components/"+org.String(), e.admin, saveComponentBody("Taken", stripeDef)); w.Code != http.StatusNotFound {
			t.Errorf("admin %s on an org row through /me = %d, want 404", method, w.Code)
		}
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/components"},
		{http.MethodPut, "/api/v1/components/" + uuid.NewString()},
		{http.MethodPut, "/api/v1/components/" + org.String()},
		{http.MethodDelete, "/api/v1/components/" + org.String()},
	} {
		if w := e.do(t, c.method, c.path, e.member, saveComponentBody("x", stripeDef)); w.Code != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403", c.method, c.path, w.Code)
		}
	}
	if got, err := e.st.GetComponent(context.Background(), org, ""); err != nil || got.Name != "Org" {
		t.Fatalf("org row after the member's attempts = %+v, %v", got, err)
	}
	if got, err := e.st.GetComponent(context.Background(), theirs.ID, "sub-other"); err != nil || got.Name != "Theirs" {
		t.Fatalf("the other person's row after the attempts = %+v, %v", got, err)
	}
	if len(e.memberList(t).Mine) != 0 {
		t.Error("a member's list carries another person's row")
	}
	var all []types.Component
	_ = json.Unmarshal(e.do(t, http.MethodGet, "/api/v1/components", e.admin, "").Body.Bytes(), &all)
	if len(all) != 1 || all[0].ID != org {
		t.Errorf("admin /components = %+v, want the org row only", all)
	}
}

// TestPG_Components_RefusedWhenCustomComponentIsDenied: with the
// custom_component feature value denied, a member can neither save nor edit
// their own component, is told so in their list, and can still delete what they
// saved; an admin is exempt.
func TestPG_Components_RefusedWhenCustomComponentIsDenied(t *testing.T) {
	e := newComponentsPG(t)
	mine := decodeSaved(t, e.do(t, http.MethodPost, "/api/v1/me/components", e.member, saveComponentBody("Before", stripeDef)))

	e.grant(t, "all", "", capFeature, featureCustomComponent, "deny")

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/me/components"},
		{http.MethodPut, "/api/v1/me/components/" + mine.ID.String()},
	} {
		w := e.do(t, c.method, c.path, e.member, saveComponentBody("After", stripeDef))
		if w.Code != http.StatusForbidden || errorReason(w) != "capability_feature" ||
			!strings.Contains(w.Body.String(), "Custom components aren't turned on for you. Ask your admin.") {
			t.Errorf("%s %s with the value denied = %d %s, want 403 capability_feature", c.method, c.path, w.Code, w.Body.String())
		}
	}
	list := e.memberList(t)
	if list.MayDefine || len(list.Mine) != 1 {
		t.Errorf("list = %+v, want may_define false and the existing row still listed", list)
	}
	if rows, _ := e.st.ListComponents(context.Background(), "sub-member"); len(rows) != 1 || rows[0].Name != "Before" {
		t.Errorf("rows after the refusals = %+v, want the one untouched row", rows)
	}
	if w := e.do(t, http.MethodDelete, "/api/v1/me/components/"+mine.ID.String(), e.member, ""); w.Code != http.StatusNoContent {
		t.Errorf("deleting one's own row with the value denied = %d, want 204", w.Code)
	}
	if w := e.do(t, http.MethodPost, "/api/v1/me/components", e.admin, saveComponentBody("Admin's own", stripeDef)); w.Code != http.StatusCreated {
		t.Errorf("an admin with the value denied = %d %s, want 201 (exempt)", w.Code, w.Body.String())
	}
}

// TestPG_Components_OwnerIsTheSubjectSecretsAreKeyedBy pins C9-I2 from this side:
// the owner a saved row carries is the string the person's own secret namespace
// is keyed by, which is the string erasure resolves a person to, so erasing by it
// reaches the row.
func TestPG_Components_OwnerIsTheSubjectSecretsAreKeyedBy(t *testing.T) {
	e := newComponentsPG(t)
	ctx := context.Background()
	if w := e.do(t, http.MethodPut, "/api/v1/secrets/my-token", e.member, `{"value":"a-long-enough-value"}`); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("member secret = %d: %s", w.Code, w.Body.String())
	}
	names, err := e.h.srv.cfg.Secrets.For("sub-member").List(ctx)
	if err != nil || len(names) != 1 || names[0] != "my-token" {
		t.Fatalf("secrets under the subject = %v, %v; want the member's own", names, err)
	}
	saved := decodeSaved(t, e.do(t, http.MethodPost, "/api/v1/me/components", e.member, saveComponentBody("Mine", stripeDef)))
	if saved.Owner != "sub-member" {
		t.Fatalf("owner = %q, want the subject the secrets are keyed by", saved.Owner)
	}
	if n, err := e.st.DeleteComponentsByOwner(ctx, "sub-member"); err != nil || n != 1 {
		t.Fatalf("erasing by that subject removed %d rows (%v), want 1", n, err)
	}
}

// TestPG_Components_ASavedRowIsFoundByTheRunGate: the owner a row is saved under
// is the owner the gate looks it up by, so a person's own saved component attaches
// by id at the policy-preview door and its hosts reach the run; another person's
// row is refused as absent, and an unknown id answers with the same bytes.
func TestPG_Components_ASavedRowIsFoundByTheRunGate(t *testing.T) {
	e := newComponentsPG(t)
	mine := decodeSaved(t, e.do(t, http.MethodPost, "/api/v1/me/components", e.member,
		saveComponentBody("Mine", `{"hosts":["saved-host.example.com"]}`)))
	other := ssoSession(t, "sub-other", "other@corp.example", oidc.RoleUser)
	theirs := decodeSaved(t, e.do(t, http.MethodPost, "/api/v1/me/components", other,
		saveComponentBody("Theirs", `{"hosts":["their-host.example.com"]}`)))

	ask := func(id uuid.UUID) *httptest.ResponseRecorder {
		return e.do(t, http.MethodPost, "/api/v1/runs/policy-preview", e.member,
			`{"agent":"claude-code","task":"t","components":[{"id":"`+id.String()+`"}]}`)
	}
	w := ask(mine.ID)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "saved-host.example.com") {
		t.Fatalf("attaching the saved row = %d %s, want 200 with its host in the preview", w.Code, w.Body.String())
	}
	foreign, absent := ask(theirs.ID), ask(uuid.New())
	if foreign.Code != http.StatusForbidden || errorReason(foreign) != "capability_component" || foreign.Body.String() != absent.Body.String() {
		t.Fatalf("another person's row = %d %s, an absent id = %d %s; want the same 403 capability_component bytes (A18)",
			foreign.Code, foreign.Body.String(), absent.Code, absent.Body.String())
	}
	if strings.Contains(foreign.Body.String(), "their-host") {
		t.Fatal("the refusal leaks the other person's host")
	}
}

// TestPG_Components_ConcurrentCaseVariantsOfANameSaveOnce: eight writers save
// case variants of one name at once, for a person and for the organisation.
// The handler's name check reads before it writes, so it cannot hold this; the
// unique index on (owner, lower(name)) does, and its violation answers 409
// component_name_conflict. A losing org create leaves no restriction behind.
func TestPG_Components_ConcurrentCaseVariantsOfANameSaveOnce(t *testing.T) {
	e := newComponentsPG(t)
	variants := []string{"Stripe", "stripe", "STRIPE", "sTripe", "StRiPe", "strIPE", "stripE", "STRipe"}
	orgIDs := make([]uuid.UUID, len(variants))
	for i := range orgIDs {
		orgIDs[i] = uuid.New()
	}
	for owner, save := range map[string]func(i int) *httptest.ResponseRecorder{
		"sub-member": func(i int) *httptest.ResponseRecorder {
			return e.do(t, http.MethodPost, "/api/v1/me/components", e.member, saveComponentBody(variants[i], stripeDef))
		},
		"": func(i int) *httptest.ResponseRecorder {
			return e.do(t, http.MethodPut, "/api/v1/components/"+orgIDs[i].String(), e.admin, saveComponentBody(variants[i], stripeDef))
		},
	} {
		got := make([]*httptest.ResponseRecorder, len(variants))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range variants {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[i] = save(i)
			}()
		}
		close(start)
		wg.Wait()

		rows, err := e.st.ListComponents(context.Background(), owner)
		if err != nil || len(rows) != 1 {
			t.Fatalf("owner %q: %d rows (%v) from eight concurrent case variants of one name, want exactly 1: %+v", owner, len(rows), err, rows)
		}
		created := 0
		for i, w := range got {
			switch {
			case w.Code == http.StatusCreated:
				created++
			case w.Code != http.StatusConflict || errorReason(w) != "component_name_conflict":
				t.Errorf("owner %q: %q = %d %s, want 201 or 409 component_name_conflict", owner, variants[i], w.Code, w.Body.String())
			}
		}
		if created != 1 {
			t.Errorf("owner %q: %d creates answered 201, want exactly 1", owner, created)
		}
	}
	for _, id := range orgIDs {
		if _, err := e.st.GetComponent(context.Background(), id, ""); err != nil && e.restricted(t, id) {
			t.Errorf("the refused org create of %s left its restriction behind", id)
		}
	}
}
