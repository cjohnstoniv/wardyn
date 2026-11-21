// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// personListDoor is the real HTTP write of one admin-written person list, as the admin makes it. stored counts
// the stored rows that name a person, so a test reads back what the door kept.
type personListDoor struct {
	method string
	path   func(person string) string
	body   func(person, tag string) string
	stored func(person string) int
	// approver decides a held change of this door; nil is the second super admin.
	approver *http.Cookie
}

func (d personListDoor) send(t *testing.T, e *regPG, person, tag string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, d.method, d.path(person), e.admin, d.body(person, tag))
}

// personDoorRow is one row of the plan's table of admin-written person lists.
type personDoorRow struct {
	n      int
	list   string
	refuse bool
	// kind is the governance target kind of the door while the second-human switch is on; empty when the
	// door is not a governance-covered write.
	kind string
	// refusedAtProposal marks a held door whose proposal already runs the store write, so the refusal is the
	// answer to the write itself and no change is created.
	refusedAtProposal bool
	prep              func(t *testing.T, e *regPG, n int) personListDoor
}

func personListCount(t *testing.T, e *regPG, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func personListUnit(t *testing.T, e *regPG, n int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}','t')`,
		id, fmt.Sprintf("person-list-%d-%s", n, id)); err != nil {
		t.Fatal(err)
	}
	return id
}

// personListStoredSubject counts the rows of a table that name person as a user subject.
func personListStoredSubject(t *testing.T, e *regPG, table string) func(string) int {
	return func(person string) int {
		return personListCount(t, e, `SELECT count(*) FROM `+table+` WHERE subject_type = 'user' AND subject = $1`, person)
	}
}

func personListConst(path string) func(string) string { return func(string) string { return path } }

// personListPending reads the 202 of a held write and returns the change, asserting it is a pending change of
// kind.
func personListPending(t *testing.T, w *httptest.ResponseRecorder, kind string) types.GovernanceChange {
	t.Helper()
	if w.Code != http.StatusAccepted {
		t.Fatalf("held write = %d %s, want 202", w.Code, w.Body.String())
	}
	var body struct {
		Pending types.GovernanceChange `json:"pending_change"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Pending.ID == uuid.Nil {
		t.Fatalf("202 body %s carries no pending change: %v", w.Body.String(), err)
	}
	if body.Pending.TargetKind != kind || body.Pending.State != types.GovernanceChangePending {
		t.Fatalf("pending change = kind %q state %q, want %q pending", body.Pending.TargetKind, body.Pending.State, kind)
	}
	return body.Pending
}

func personListState(t *testing.T, e *regPG, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(t.Context(), `SELECT state FROM governance_changes WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func personDoorRows() []personDoorRow {
	return []personDoorRow{
		{n: 1, list: "capability grant, user subject", kind: govKindGrant,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				return personListDoor{http.MethodPost, personListConst("/api/v1/permissions/grants"),
					func(p, _ string) string {
						return fmt.Sprintf(`{"subject_type":"user","subject":%q,"capability":"egress_host","value":"example.com","effect":"allow"}`, p)
					}, personListStoredSubject(t, e, "capability_grants"), nil}
			}},
		{n: 2, list: "governance membership, user subject", kind: govKindMembership,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				unit := personListUnit(t, e, n)
				return personListDoor{http.MethodPost, personListConst("/api/v1/governance/memberships"),
					func(p, _ string) string {
						return fmt.Sprintf(`{"subject_type":"user","subject":%q,"unit_id":%q,"priority":1}`, p, unit)
					}, personListStoredSubject(t, e, "governance_memberships"), nil}
			}},
		{n: 3, list: "restriction members", kind: govKindRestriction,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				return personListDoor{http.MethodPost, personListConst("/api/v1/governance/restrictions"),
					func(p, tag string) string {
						allowed := []string{"b.example"}
						b, err := json.Marshal(types.RestrictionWrite{Name: fmt.Sprintf("person-list-%d-%s", n, tag),
							Spec:    types.RestrictionSpec{AllowedDomains: &allowed},
							Members: []types.RestrictionMember{{SubjectType: types.CapabilitySubjectUser, Subject: p}}})
						if err != nil {
							t.Fatal(err)
						}
						return string(b)
					}, personListStoredSubject(t, e, "governance_restriction_members"), nil}
			}},
		{n: 4, list: "drive grant subject",
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/drives", e.admin, `{"name":"person-list-drive","backend":"docker_volume","size_mib":10240}`)
				var drive struct {
					ID uuid.UUID `json:"id"`
				}
				if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &drive) != nil || drive.ID == uuid.Nil {
					t.Fatalf("create drive = %d %s, want 201 with an id", w.Code, w.Body.String())
				}
				return personListDoor{http.MethodPost, personListConst("/api/v1/drives/grants"),
					func(p, _ string) string {
						return fmt.Sprintf(`{"subject_type":"user","subject":%q,"drive_id":%q,"priority":1}`, p, drive.ID)
					}, personListStoredSubject(t, e, "user_drive_grants"), nil}
			}},
		{n: 5, list: "UI-app callers, user arm",
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				return personListDoor{http.MethodPost, personListConst("/api/v1/policies"),
					func(p, tag string) string {
						return fmt.Sprintf(`{"name":"person-list-%d-%s","spec":{"min_confinement_class":"CC2","allowed_domains":["example.com"],`+
							`"ui_apps":[{"name":"web","port":3000,"inbound":true,"callers":[%q]}]}}`, n, tag, "user:"+p)
					}, func(p string) int {
						return personListCount(t, e, `SELECT count(*) FROM run_policies WHERE strpos(spec::text, $1) > 0`, p)
					}, nil}
			}},
		{n: 6, list: "runner pool use-policy subjects", kind: govKindRunnerPoolUsePolicy,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runner-pools", e.admin, fmt.Sprintf(`{"name":"person-list-pool-%d"}`, n))
				var pool types.RunnerPool
				if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &pool) != nil || pool.ID == uuid.Nil {
					t.Fatalf("create pool = %d %s, want 201 with an id", w.Code, w.Body.String())
				}
				return personListDoor{http.MethodPut, personListConst("/api/v1/runner-pools/" + pool.ID.String() + "/use-policy"),
					func(p, _ string) string {
						return fmt.Sprintf(`{"subjects":[{"subject_type":"user","subject":%q}]}`, p)
					}, func(p string) int {
						return personListCount(t, e, `SELECT count(*) FROM runner_pool_use_policies WHERE pool_id = $1 AND strpos(subjects::text, $2) > 0`, pool.ID, p)
					}, nil}
			}},
		{n: 7, list: "remote runner people subjects", refuse: true, kind: govKindRemoteRunnerPeople,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				if _, err := e.pool.Exec(t.Context(), `INSERT INTO remote_runners (id, name) VALUES ('fake', 'test') ON CONFLICT DO NOTHING`); err != nil {
					t.Fatal(err)
				}
				return personListDoor{http.MethodPut, personListConst("/api/v1/remote-runners/fake/people-policy"),
					func(p, _ string) string {
						return fmt.Sprintf(`{"people_pools":"listed","subjects":[{"subject_type":"user","subject":%q}]}`, p)
					}, func(p string) int {
						return personListCount(t, e, `SELECT count(*) FROM remote_runners WHERE id = 'fake' AND strpos(people_subjects::text, $1) > 0`, p)
					}, nil}
			}},
		{n: 8, list: "unit admin grant", refuse: true, kind: govKindUnitAdmin,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				unit := personListUnit(t, e, n)
				return personListDoor{http.MethodPost, personListConst("/api/v1/governance/units/" + unit.String() + "/admins"),
					func(p, _ string) string { return fmt.Sprintf(`{"subject_type":"user","subject":%q}`, p) },
					personListStoredSubject(t, e, "governance_unit_admins"), nil}
			}},
		{n: 9, list: "template group admin person", refuse: true,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				return personListDoor{http.MethodPut, personListConst("/api/v1/admin/template-group-admins"),
					func(p, tag string) string {
						return fmt.Sprintf(`{"group_id":"person-list-%d-%s","person":%q}`, n, tag, p)
					},
					func(p string) int {
						return personListCount(t, e, `SELECT count(*) FROM template_group_admins WHERE person = $1`, p)
					}, nil}
			}},
		{n: 10, list: "role mapping value", refuse: true, kind: govKindRoleMapping,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				// The approval's lockout guard reads the approver's own sign-in, so the approver is a second
				// person whose address the chart maps to admin.
				approver := accessSession(t, "root3", "root@corp.example", oidc.RoleAdmin, []string{})
				return personListDoor{http.MethodPost, personListConst("/api/v1/access/mappings"),
					func(p, _ string) string { return fmt.Sprintf(`{"value":%q,"role":"user"}`, p) },
					func(p string) int {
						return personListCount(t, e, `SELECT count(*) FROM role_mappings WHERE value = $1`, p)
					}, approver}
			}},
		{n: 11, list: "service identity sponsor", refuse: true, kind: govKindServiceIdentity, refusedAtProposal: true,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				return personListDoor{http.MethodPost, personListConst("/api/v1/service-identities"),
					func(p, tag string) string { return fmt.Sprintf(`{"name":"person-list-%d-%s","sponsor":%q}`, n, tag, p) },
					func(p string) int {
						return personListCount(t, e, `SELECT count(*) FROM service_identities WHERE sponsor = $1`, p)
					}, nil}
			}},
		{n: 16, list: "key-domain assignment, user subject", refuse: true, kind: govKindKeyDomain,
			prep: func(t *testing.T, e *regPG, n int) personListDoor {
				e.h.srv.cfg.KeyDomains = keydomain.NewService(e.pool, []string{"a", "b"})
				return personListDoor{http.MethodPut, func(p string) string { return "/api/v1/key-domains/assignments/user/" + p },
					func(string, string) string { return `{"domain":"a"}` },
					personListStoredSubject(t, e, "key_domain_assignments"), nil}
			}},
	}
}

// Each admin-written person list is driven at its real HTTP door: an allow row stores the person under a
// bound issuer key and refuses one under an unbound key, a refuse row refuses any portal person, and a
// governance-covered door does the same through a held change and its approval by a second super admin.
func TestAdminPersonLists_DelegatePersonRuling_PG(t *testing.T) {
	e := newRegPG(t)
	// Registered before the switch goes on: with it on, a registration is itself held.
	bound := e.registered(t, map[string]any{"issuer": "https://person-lists.example.com"})
	person := func(key string, n int, tag string) string {
		return types.PortalPrincipal(key, fmt.Sprintf("00uRow%d%s", n, tag))
	}

	for _, row := range personDoorRows() {
		t.Run(fmt.Sprintf("row %d %s", row.n, row.list), func(t *testing.T) {
			door := row.prep(t, e, row.n)
			approver := door.approver
			if approver == nil {
				approver = e.admin2
			}
			approve := func(change types.GovernanceChange) *httptest.ResponseRecorder {
				return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/governance/changes/"+change.ID.String()+"/approve", approver, `{"reason":"ok"}`)
			}
			wantRefused := func(name string, w *httptest.ResponseRecorder, reason string) {
				t.Helper()
				if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reason {
					t.Errorf("%s = %d %s, want 422 %s", name, w.Code, w.Body.String(), reason)
				}
			}

			// The direct write, with the switch off.
			if row.refuse {
				p := person(bound.IssuerKey, row.n, "direct")
				wantRefused("write under a bound key", door.send(t, e, p, "direct"), reasonDelegatePersonNotAllowed)
				if n := door.stored(p); n != 0 {
					t.Errorf("a refused write stored %d rows", n)
				}
			} else {
				p := person(bound.IssuerKey, row.n, "direct")
				if w := door.send(t, e, p, "direct"); w.Code < 200 || w.Code > 299 {
					t.Errorf("write under a bound key = %d %s, want 2xx", w.Code, w.Body.String())
				}
				if n := door.stored(p); n != 1 {
					t.Errorf("stored rows naming %s = %d, want 1", p, n)
				}
				u := person(types.PortalIssuerKey(fmt.Sprintf("https://never-registered-%d.example.com", row.n)), row.n, "direct")
				wantRefused("write under an unbound key", door.send(t, e, u, "unbound"), reasonDelegatePersonUnknownPortal)
				if n := door.stored(u); n != 0 {
					t.Errorf("a refused write stored %d rows", n)
				}
			}
			if row.kind == "" {
				return
			}

			// The same write with the switch on.
			t.Setenv(envGovernanceSecondHuman, "true")
			p := person(bound.IssuerKey, row.n, "held")
			if row.refusedAtProposal {
				before := personListCount(t, e, `SELECT count(*) FROM governance_changes WHERE target_kind = $1`, row.kind)
				wantRefused("held write", door.send(t, e, p, "held"), reasonDelegatePersonNotAllowed)
				if n := personListCount(t, e, `SELECT count(*) FROM governance_changes WHERE target_kind = $1`, row.kind); n != before {
					t.Errorf("a refused proposal created %d changes", n-before)
				}
				return
			}
			change := personListPending(t, door.send(t, e, p, "held"), row.kind)
			if n := door.stored(p); n != 0 {
				t.Errorf("a held write stored %d rows before its approval", n)
			}
			w := approve(change)
			if row.refuse {
				wantRefused("approval", w, reasonDelegatePersonNotAllowed)
				if s := personListState(t, e, change.ID); s != types.GovernanceChangePending {
					t.Errorf("a refused approval left the change %s, want pending", s)
				}
				if n := door.stored(p); n != 0 {
					t.Errorf("a refused approval stored %d rows", n)
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("approval = %d %s, want 200", w.Code, w.Body.String())
			}
			if s := personListState(t, e, change.ID); s != types.GovernanceChangeApplied {
				t.Errorf("an approved change is %s, want applied", s)
			}
			if n := door.stored(p); n != 1 {
				t.Errorf("stored rows naming %s after approval = %d, want 1", p, n)
			}
		})
	}

	// Rows 12-14 refuse a portal person at their own doors by the reserved-principal rule: nothing is stored.
	person12 := person(bound.IssuerKey, 12, "door")
	person13 := person(bound.IssuerKey, 13, "door")
	person14 := person(bound.IssuerKey, 14, "door")
	t.Run("row 12 invitation target", func(t *testing.T) {
		w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people/"+person12+"/invitations", e.admin, `{}`)
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonInvitationTargetInvalid {
			t.Errorf("invitation = %d %s, want 422 %s", w.Code, w.Body.String(), reasonInvitationTargetInvalid)
		}
		if n := personListCount(t, e, `SELECT count(*) FROM person_invitations WHERE target = $1`, person12); n != 0 {
			t.Errorf("a refused invitation stored %d rows", n)
		}
	})
	t.Run("row 13 person create", func(t *testing.T) {
		w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people", e.admin, fmt.Sprintf(`{"principal":%q,"email":"row13@corp.example"}`, person13))
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonPersonPrincipalReserved {
			t.Errorf("person create = %d %s, want 422 %s", w.Code, w.Body.String(), reasonPersonPrincipalReserved)
		}
		if n := personListCount(t, e, `SELECT count(*) FROM people WHERE principal = $1`, person13); n != 0 {
			t.Errorf("a refused create stored %d rows", n)
		}
	})
	t.Run("row 14 token, ssh key and secret issued to a principal", func(t *testing.T) {
		// An admin minting a token for the person is refused for every principal, whoever the person is.
		w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people/"+person14+"/tokens", e.admin, `{}`)
		if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonPersonTokenMintRemoved {
			t.Errorf("mint for the person = %d %s, want 403 %s", w.Code, w.Body.String(), reasonPersonTokenMintRemoved)
		}
		// A correctly signed cookie for the person reaches no route: the reserved-principal check refuses it
		// before the handler runs, so none of the credential doors can issue to the person.
		cookie := accessSession(t, person14, "row14@corp.example", oidc.RoleUser, []string{})
		for _, door := range []struct{ method, path, body string }{
			{http.MethodPost, "/api/v1/me/tokens", `{"name":"row14"}`},
			{http.MethodPost, "/api/v1/me/ssh-keys", `{"name":"row14"}`},
			{http.MethodPut, "/api/v1/secrets/row14", `{"value":"x"}`},
			{http.MethodPut, "/api/v1/me/secrets/row14", `{"value":"x"}`},
		} {
			w := doSSO(t, e.h.srv, door.method, door.path, cookie, door.body)
			if w.Code != http.StatusUnauthorized || reasonOf(t, w) != reasonReservedPrincipal {
				t.Errorf("%s %s = %d %s, want 401 %s", door.method, door.path, w.Code, w.Body.String(), reasonReservedPrincipal)
			}
		}
		for _, q := range []string{
			`SELECT count(*) FROM api_tokens WHERE principal = $1`,
			`SELECT count(*) FROM ssh_public_keys WHERE principal = $1`,
			`SELECT count(*) FROM secrets WHERE owned_by = $1`,
		} {
			if n := personListCount(t, e, q, person14); n != 0 {
				t.Errorf("%s = %d, want 0", q, n)
			}
		}
	})
}
