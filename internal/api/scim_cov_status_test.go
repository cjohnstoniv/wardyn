// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const scimCovStatusPath = "/api/v1/scim/status"

type scimCovStatusBody struct {
	Configured        bool   `json:"configured"`
	LastTokenSlot     string `json:"last_token_slot"`
	PurgeAfterSeconds int64  `json:"purge_after_seconds"`
	KeepWorkspaces    bool   `json:"keep_workspaces"`
	Deactivated       []struct {
		Person        string  `json:"person"`
		DeactivatedAt string  `json:"deactivated_at"`
		PurgeAfter    *string `json:"purge_after"`
	} `json:"deactivated"`
	Pending []struct {
		Person    string `json:"person"`
		Step      string `json:"step"`
		LastError string `json:"last_error"`
	} `json:"pending"`
	Drives []struct {
		Person   string `json:"person"`
		Drive    string `json:"drive"`
		PurgedAt string `json:"purged_at"`
	} `json:"drives"`
}

func (e *scimCovEnv) status() (*httptest.ResponseRecorder, scimCovStatusBody) {
	e.t.Helper()
	w := do(e.t, e.srv, http.MethodGet, scimCovStatusPath, adminToken, "")
	var body scimCovStatusBody
	if w.Code == http.StatusOK {
		body = scimCovDecode[scimCovStatusBody](e.t, w)
	}
	return w, body
}

func scimCovAudit(action, target string, at time.Time, data string) types.AuditEvent {
	return types.AuditEvent{
		ID: uuid.New(), Time: at, ActorType: types.ActorSystem, Actor: scimActor, Action: action, Target: target, Outcome: "success",
		Data: json.RawMessage(data),
	}
}

func TestSCIMCovStatusWhenSCIMIsNotConfigured(t *testing.T) {
	for name, e := range map[string]*scimCovEnv{
		"no SCIM configuration": newSCIMCovEnv(t, newSCIMCovStore(), func(cfg *Config) { cfg.SCIM = nil }),
		"no SCIM store backend": newSCIMCovEnv(t, rbacStore{}),
	} {
		t.Run(name, func(t *testing.T) {
			w, body := e.status()
			if w.Code != http.StatusOK || body.Configured || body.PurgeAfterSeconds != 0 || body.LastTokenSlot != "" {
				t.Fatalf("status = %d %s", w.Code, w.Body.String())
			}
			for _, field := range []string{`"deactivated":[]`, `"pending":[]`, `"drives":[]`} {
				if !strings.Contains(w.Body.String(), field) {
					t.Errorf("body %s lacks %s: the console reads these as arrays", w.Body.String(), field)
				}
			}
		})
	}
}

func TestSCIMCovStatusRequiresASecurityAdmin(t *testing.T) {
	e := newSCIMCovEnv(t, newSCIMCovStore())
	for _, c := range []struct{ name, bearer, reason string }{
		{"anonymous", "", "missing_bearer_token"},
		{"the SCIM bearer", scimCovToken, "invalid_admin_token"},
	} {
		w := do(t, e.srv, http.MethodGet, scimCovStatusPath, c.bearer, "")
		if w.Code != http.StatusUnauthorized || errorReason(w) != c.reason {
			t.Errorf("%s = %d reason %q (%s), want 401 %s: the SCIM bearer is not a console credential", c.name, w.Code, errorReason(w), w.Body.String(), c.reason)
		}
	}
}

func TestSCIMCovStatusListsDeactivatedAndPending(t *testing.T) {
	st := newSCIMCovStore()
	gone := time.Now().UTC().Truncate(time.Second).In(time.FixedZone("x", 2*3600))
	purge := gone.Add(72 * time.Hour)
	scimName := st.addIdentity(store.PrincipalIdentity{Principal: "sub-a", ScimUserName: "Ann", EmailLower: "ann@corp.example", DeactivatedAt: &gone, PurgeAfter: &purge})
	st.addIdentity(store.PrincipalIdentity{Principal: "sub-b", EmailLower: "bob@corp.example", DeactivatedAt: &gone})
	st.addIdentity(store.PrincipalIdentity{Principal: "sub-c", DeactivatedAt: &gone})
	st.addIdentity(store.PrincipalIdentity{ObjectID: scimCovOID, DeactivatedAt: &gone})
	byID := st.addIdentity(store.PrincipalIdentity{DeactivatedAt: &gone})
	st.addIdentity(store.PrincipalIdentity{Principal: "sub-purged", DeactivatedAt: &gone, PurgedAt: &gone})
	unlisted := st.addIdentity(store.PrincipalIdentity{Principal: "sub-live", EmailLower: "live@corp.example"})
	st.failures = []store.DeprovisionFailure{
		{IdentityID: scimName.ID, Kind: store.JobKindSuspend, Step: "sweep", LastError: "revocation store unavailable"},
		{IdentityID: unlisted.ID, Kind: store.JobKindPurge, Step: "grants", LastError: "boom"},
		{IdentityID: unlisted.ID, Kind: store.JobKindPurge, Step: "workspaces", LastError: "boom too"},
	}
	e := newSCIMCovEnv(t, st, func(cfg *Config) {
		cfg.SCIM.PurgeAfter = 36 * time.Hour
		cfg.SCIM.KeepWorkspaces = true
	})

	w, body := e.status()
	if w.Code != http.StatusOK || !body.Configured || body.PurgeAfterSeconds != 129600 || !body.KeepWorkspaces {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	var people []string
	for _, d := range body.Deactivated {
		people = append(people, d.Person)
	}
	if want := []string{"Ann", "bob@corp.example", "sub-c", scimCovOID, byID.ID.String()}; fmt.Sprint(people) != fmt.Sprint(want) {
		t.Errorf("deactivated = %v, want %v: the SCIM name, else the email, the principal, the object id, the row id (a purged person is not listed)", people, want)
	}
	if got := body.Deactivated[0]; got.DeactivatedAt != gone.UTC().Format(time.RFC3339) || got.PurgeAfter == nil || *got.PurgeAfter != purge.UTC().Format(time.RFC3339) {
		t.Errorf("first row = %+v, want both instants in UTC", got)
	}
	if body.Deactivated[1].PurgeAfter != nil {
		t.Errorf("a person with no purge scheduled shows %v", *body.Deactivated[1].PurgeAfter)
	}
	if len(body.Pending) != 3 || body.Pending[0].Person != "Ann" || body.Pending[0].Step != "sweep" || body.Pending[0].LastError != "revocation store unavailable" ||
		body.Pending[1].Person != "live@corp.example" || body.Pending[2].Step != "workspaces" {
		t.Errorf("pending = %+v, want the failures named by person, step and error", body.Pending)
	}
	if got := st.callCount("GetIdentity"); got != 1 {
		t.Errorf("GetIdentity ran %d times, want once: the listed person's label is reused and an unlisted one is read once", got)
	}
}

func TestSCIMCovStatusStoreFailures(t *testing.T) {
	for _, c := range []struct {
		name, method string
		arm          func(*scimCovStore)
	}{
		{"listing the deactivated", "ListDeactivatedIdentities", nil},
		{"listing the failures", "ListDeprovisionFailures", nil},
		{"naming a person with a failure", "GetIdentity", func(s *scimCovStore) {
			s.failures = []store.DeprovisionFailure{{IdentityID: uuid.New(), Step: "sweep", LastError: "x"}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			if c.arm != nil {
				c.arm(st)
			}
			st.failNext(c.method, errSCIMCovBoom, -1)
			e := newSCIMCovEnv(t, st)
			w, _ := e.status()
			if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "secret-dsn") {
				t.Errorf("status = %d %s, want a 500 that does not leak the driver error", w.Code, w.Body.String())
			}
		})
	}
}

func scimCovPagedEnv(t *testing.T, st *scimCovStore, audit []types.AuditEvent) (*scimCovEnv, *scimCovPaged) {
	paged := &scimCovPaged{scimCovStore: st, audit: audit}
	return newSCIMCovEnv(t, paged), paged
}

func TestSCIMCovStatusLastTokenSlotIsTheNewestWriteThatNamesOne(t *testing.T) {
	t0 := scimCovNow
	rows := []types.AuditEvent{
		scimCovAudit("scim.user.deactivate", "x", t0, `{"slot":"sweeper"}`),
		scimCovAudit("scim.user.write", "x", t0.Add(-time.Minute), `not json`),
		scimCovAudit("scim.user.write", "x", t0.Add(-2*time.Minute), `{"slot":"next"}`),
		scimCovAudit("scim.user.write", "x", t0.Add(-3*time.Minute), `{"slot":"primary"}`),
		{ID: uuid.New(), Time: t0, ActorType: types.ActorHuman, Actor: "admin", Action: "scim.user.write", Data: json.RawMessage(`{"slot":"primary"}`)},
	}
	e, paged := scimCovPagedEnv(t, newSCIMCovStore(), rows)

	w, body := e.status()
	if w.Code != http.StatusOK || body.LastTokenSlot != "next" {
		t.Fatalf("status = %d %s, want last_token_slot next", w.Code, w.Body.String())
	}
	first := paged.filters[0]
	if first.ActionPrefix != "scim.user." || first.Actor != scimActor || first.ActorType != types.ActorSystem || first.Origin != store.AuditOriginOrganisation {
		t.Errorf("slot filter = %+v, want the SCIM caller's own organisation rows of scim.user.*", first)
	}

	paged.audit = rows[:2]
	if _, body := e.status(); body.LastTokenSlot != "" {
		t.Errorf("last_token_slot = %q, want empty when no row names a bearer", body.LastTokenSlot)
	}
}

func TestSCIMCovStatusAuditReadFailures(t *testing.T) {
	for _, c := range []struct{ name, action string }{
		{"the slot read", ""},
		{"the purge rows", "person.deprovision"},
		{"the reclaim rows", "drive.reclaim"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			id := st.addIdentity(store.PrincipalIdentity{Principal: "sub-p", EmailLower: "p@corp.example"})
			st.drives = []types.UserDriveListItem{{UserDrive: types.UserDrive{Name: "home"}}}
			rows := []types.AuditEvent{scimCovAudit("person.deprovision", id.ID.String(), scimCovNow, `{"kind":"purge","drives":["home"]}`)}
			e, paged := scimCovPagedEnv(t, st, rows)
			paged.err, paged.errAction = errSCIMCovBoom, c.action
			w, _ := e.status()
			if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "secret-dsn") {
				t.Errorf("status = %d %s, want a 500 that does not leak the driver error", w.Code, w.Body.String())
			}
		})
	}
}

func TestSCIMCovStatusDrivesToReclaim(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(store.PrincipalIdentity{Principal: "sub-pat", EmailLower: "pat@corp.example", ScimUserName: "Pat"})
	sam := st.addIdentity(store.PrincipalIdentity{Principal: "sub-sam", EmailLower: "sam@corp.example"})
	st.drives = []types.UserDriveListItem{
		{UserDrive: types.UserDrive{Name: "alpha"}}, {UserDrive: types.UserDrive{Name: "zeta"}}, {UserDrive: types.UserDrive{Name: "omega"}},
	}
	t1 := time.Now().UTC().Truncate(time.Second).In(time.FixedZone("x", 3600))
	t2 := t1.Add(-24 * time.Hour)
	purge := func(target string, at time.Time, drives string) types.AuditEvent {
		return scimCovAudit("person.deprovision", target, at, `{"kind":"purge","drives":`+drives+`}`)
	}
	reclaim := func(at time.Time, drive, subject, subjectType string) types.AuditEvent {
		return types.AuditEvent{
			ID: uuid.New(), Time: at, ActorType: types.ActorHuman, Actor: "admin", Action: "drive.reclaim", Outcome: "success",
			Data: json.RawMessage(fmt.Sprintf(`{"drive":%q,"subject":%q,"subject_type":%q}`, drive, subject, subjectType)),
		}
	}
	rows := []types.AuditEvent{
		purge(pat.ID.String(), t1, `["alpha","zeta","deleted-drive"]`),
		purge(sam.ID.String(), t2, `["omega","alpha"]`),
		scimCovAudit("person.deprovision", pat.ID.String(), t1, `{"kind":"suspend","drives":["omega"]}`),
		purge("not-a-uuid", t1, `["omega"]`),
		scimCovAudit("person.deprovision", pat.ID.String(), t1, `{"kind":"purge","drives":"nope"}`),
		// Reclaims: pat's alpha after the purge (done); pat's zeta before it, by another subject, of another type
		// or by a person named differently (none of them count); sam's omega after his purge by his principal (done).
		reclaim(t1.Add(time.Hour), "alpha", "PAT@corp.example", "user"),
		reclaim(t1.Add(-time.Hour), "zeta", "pat@corp.example", "user"),
		reclaim(t1.Add(time.Hour), "zeta", "sam@corp.example", "user"),
		reclaim(t1.Add(time.Hour), "zeta", "pat@corp.example", "group"),
		reclaim(t2.Add(time.Hour), "omega", "sub-sam", "user"),
	}
	e, paged := scimCovPagedEnv(t, st, rows)

	w, body := e.status()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	type drive struct{ person, drive, at string }
	var got []drive
	for _, d := range body.Drives {
		got = append(got, drive{d.Person, d.Drive, d.PurgedAt})
	}
	want := []drive{{"Pat", "zeta", t1.UTC().Format(time.RFC3339)}, {"sam@corp.example", "alpha", t2.UTC().Format(time.RFC3339)}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("drives = %v, want %v: still-live drives of each purge, newest purge first, minus what a later reclaim by that person freed", got, want)
	}
	var sawPurgeFilter, sawReclaimFilter bool
	for _, f := range paged.filters {
		switch f.Action {
		case "person.deprovision":
			sawPurgeFilter = f.DataContains == `{"kind":"purge"}` && f.Actor == scimActor
		case "drive.reclaim":
			sawReclaimFilter = f.Outcome == "success" && f.Origin == store.AuditOriginOrganisation && f.Actor == ""
		}
	}
	if !sawPurgeFilter || !sawReclaimFilter {
		t.Errorf("filters = %+v, want the purge rows of the SCIM caller and the operators' successful reclaims", paged.filters)
	}
}

func TestSCIMCovStatusDrivesNeedsNothingWhenNoPurgeListedDrives(t *testing.T) {
	st := newSCIMCovStore()
	e, _ := scimCovPagedEnv(t, st, nil)
	w, body := e.status()
	if w.Code != http.StatusOK || len(body.Drives) != 0 || !strings.Contains(w.Body.String(), `"drives":[]`) {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	if st.callCount("ListUserDrives") != 0 {
		t.Error("the live drives were read although no purge listed any")
	}

	// A purge row whose person Wardyn cannot read fails the whole read rather than dropping the drive.
	st.drives = []types.UserDriveListItem{{UserDrive: types.UserDrive{Name: "home"}}}
	e2, _ := scimCovPagedEnv(t, st, []types.AuditEvent{scimCovAudit("person.deprovision", uuid.NewString(), scimCovNow, `{"kind":"purge","drives":["home"]}`)})
	if w, _ := e2.status(); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a purge of a person that cannot be read", w.Code)
	}
	st.failNext("ListUserDrives", errSCIMCovBoom, -1)
	if w, _ := e2.status(); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the live drives cannot be read", w.Code)
	}

	// The person is read twice, for the card's label and for the reclaim match; either read failing fails the card.
	st3 := newSCIMCovStore()
	pat := st3.addIdentity(store.PrincipalIdentity{Principal: "sub-pat"})
	st3.drives = st.drives
	e3, _ := scimCovPagedEnv(t, st3, []types.AuditEvent{scimCovAudit("person.deprovision", pat.ID.String(), scimCovNow, `{"kind":"purge","drives":["home"]}`)})
	st3.failAfter("GetIdentity", errSCIMCovBoom, 1)
	if w, _ := e3.status(); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "secret-dsn") {
		t.Errorf("status = %d %s, want 500 when the person cannot be read for the reclaim match", w.Code, w.Body.String())
	}
}
