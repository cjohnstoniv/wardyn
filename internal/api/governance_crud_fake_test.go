// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// govCrudStore is the governance half of store.Store over memory, with one
// injectable error per call, so the profile and assignment handlers run with no
// Postgres. Every method it does not define panics through the nil embedded
// Store, which would flag a handler reaching past what these tests set up.
type govCrudStore struct {
	store.Store
	profiles    []types.GovernanceProfile
	assignments []types.GovernanceAssignment

	listProfilesErr, listAssignmentsErr  error
	upsertProfileErr, deleteProfileErr   error
	upsertAssignmentErr, deleteAssignErr error

	// lastProfile is what UpsertGovernanceProfile was last asked to store.
	lastProfile types.GovernanceProfile
	// existing makes UpsertGovernanceAssignment answer an already-stored row's id,
	// which is how the real store says "repointed, not created".
	existing uuid.UUID
}

func (s *govCrudStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, s.listProfilesErr
}

func (s *govCrudStore) ListGovernanceAssignments(context.Context) ([]types.GovernanceAssignment, error) {
	return s.assignments, s.listAssignmentsErr
}

func (s *govCrudStore) UpsertGovernanceProfile(_ context.Context, p types.GovernanceProfile) (types.GovernanceProfile, error) {
	s.lastProfile = p
	return p, s.upsertProfileErr
}

// WriteGovernanceProfile is the composed-profile write: build decides the row from every stored
// profile, as the real store does under its graph lock, and the row is then stored at id.
func (s *govCrudStore) WriteGovernanceProfile(ctx context.Context, id uuid.UUID, build store.GovernanceProfileBuild) (types.GovernanceProfile, error) {
	p, err := build(s.profiles)
	if err != nil {
		return types.GovernanceProfile{}, err
	}
	p.ID = id
	return s.UpsertGovernanceProfile(ctx, p)
}

func (s *govCrudStore) DeleteGovernanceProfile(context.Context, uuid.UUID) error {
	return s.deleteProfileErr
}

func (s *govCrudStore) UpsertGovernanceAssignment(_ context.Context, a types.GovernanceAssignment) (types.GovernanceAssignment, error) {
	if s.existing != uuid.Nil {
		a.ID = s.existing
	}
	return a, s.upsertAssignmentErr
}

func (s *govCrudStore) DeleteGovernanceAssignment(context.Context, uuid.UUID) error {
	return s.deleteAssignErr
}

func govCrudServer(t *testing.T, st *govCrudStore) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	return New(baseTestConfig(h, st)), h
}

func govAuditActions(h *harness) []string {
	var out []string
	for _, ev := range h.audit.snapshot() {
		out = append(out, ev.Action)
	}
	return out
}

func TestGovernanceCRUD_GetAnswersBothListsAndNamesTheFailingRead(t *testing.T) {
	pid := uuid.New()
	st := &govCrudStore{
		profiles:    []types.GovernanceProfile{{ID: pid, Name: "interns"}},
		assignments: []types.GovernanceAssignment{{ID: uuid.New(), SubjectType: types.CapabilitySubjectAll, ProfileID: pid}},
	}
	srv, _ := govCrudServer(t, st)

	w := do(t, srv, http.MethodGet, "/api/v1/governance", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /governance = %d %s", w.Code, w.Body.String())
	}
	var got governanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Name != "interns" || len(got.Assignments) != 1 || got.Assignments[0].ProfileID != pid {
		t.Errorf("body = %+v, want the one profile and the one assignment pointing at it", got)
	}

	for name, mutate := range map[string]func(*govCrudStore){
		"profiles":    func(s *govCrudStore) { s.listProfilesErr = errors.New("db down") },
		"assignments": func(s *govCrudStore) { s.listAssignmentsErr = errors.New("db down") },
	} {
		t.Run("a failing "+name+" read", func(t *testing.T) {
			bad := &govCrudStore{}
			mutate(bad)
			srv, _ := govCrudServer(t, bad)
			w := do(t, srv, http.MethodGet, "/api/v1/governance", adminToken, "")
			if w.Code != http.StatusInternalServerError {
				t.Errorf("GET /governance = %d, want 500", w.Code)
			}
			if strings.Contains(w.Body.String(), "db down") {
				t.Errorf("the 500 leaked the driver error: %s", w.Body.String())
			}
		})
	}
}

func TestGovernanceCRUD_ProfileWriteRefusals(t *testing.T) {
	srv, h := govCrudServer(t, &govCrudStore{})
	long := strings.Repeat("n", maxGovernanceProfileNameLen+1)
	for _, c := range []struct{ name, body string }{
		{"an unknown field", `{"name":"x","surprise":1}`},
		{"a blank name", `{"name":"   "}`},
		{"an over-long name", `{"name":"` + long + `"}`},
		{"a control character in the name", `{"name":"a\u0007b"}`},
		{"an eligible grant the deployment never provisioned", `{"name":"x","ceiling":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"github_token"}]}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/api/v1/governance/profiles", adminToken, c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d %s, want 400", w.Code, w.Body.String())
			}
		})
	}
	// The grant bound has its own reason: it is the one refusal a caller fixes
	// by changing the deployment, not the body.
	w := do(t, srv, http.MethodPost, "/api/v1/governance/profiles", adminToken, `{"name":"x","ceiling":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"github_token"}]}}`)
	if got := errorReason(w); got != reasonGovernanceCeilingInvalid {
		t.Errorf("reason = %q, want %q", got, reasonGovernanceCeilingInvalid)
	}
	if a := govAuditActions(h); len(a) != 0 {
		t.Errorf("a refused write left audit rows %v", a)
	}
}

func TestGovernanceCRUD_ProfileWriteCreatesUpdatesAndAudits(t *testing.T) {
	st := &govCrudStore{}
	srv, h := govCrudServer(t, st)

	w := do(t, srv, http.MethodPost, "/api/v1/governance/profiles", adminToken, `{"name":"  interns  ","ceiling":{"min_confinement_class":"CC2"},"limits":{"max_concurrent_runs":2}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d %s, want 201", w.Code, w.Body.String())
	}
	var created governanceProfileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Profile.Name != "interns" || created.Profile.Limits.MaxConcurrentRuns != 2 || created.Profile.ID == uuid.Nil {
		t.Errorf("created = %+v, want the trimmed name, the cap and a server-minted id", created.Profile)
	}
	if st.lastProfile.CreatedBy == "" {
		t.Error("the store was not told who wrote the profile")
	}

	// PUT names its id in the path: the row stored is that id, answered 200.
	id := uuid.New()
	w = do(t, srv, http.MethodPut, "/api/v1/governance/profiles/"+id.String(), adminToken, `{"name":"interns-2","ceiling":{"min_confinement_class":"CC2"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", w.Code, w.Body.String())
	}
	if st.lastProfile.ID != id || st.lastProfile.Name != "interns-2" {
		t.Errorf("stored %+v, want id %s renamed interns-2", st.lastProfile, id)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/governance/profiles/not-a-uuid", adminToken, `{"name":"x"}`); w.Code != http.StatusBadRequest || errorReason(w) != reasonInvalidIDParam {
		t.Errorf("PUT with a bad id = %d %s, want 400 %s", w.Code, w.Body.String(), reasonInvalidIDParam)
	}

	var wrote int
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "governance.profile.write" && ev.Outcome == "success" {
			wrote++
		}
	}
	if wrote != 2 {
		t.Errorf("governance.profile.write rows = %d, want one per successful write (2)", wrote)
	}
}

func TestGovernanceCRUD_ProfileWriteStoreFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"a taken name is the caller's to fix", store.ErrConflict, http.StatusConflict},
		{"anything else is the server's", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, h := govCrudServer(t, &govCrudStore{upsertProfileErr: c.err})
			w := do(t, srv, http.MethodPost, "/api/v1/governance/profiles", adminToken, `{"name":"x","ceiling":{"min_confinement_class":"CC2"}}`)
			if w.Code != c.want {
				t.Errorf("POST = %d %s, want %d", w.Code, w.Body.String(), c.want)
			}
			if c.want == http.StatusConflict && errorReason(w) != reasonGovernanceProfileNameConflict {
				t.Errorf("reason = %q, want %q", errorReason(w), reasonGovernanceProfileNameConflict)
			}
			if a := govAuditActions(h); len(a) != 0 {
				t.Errorf("a failed write left audit rows %v", a)
			}
		})
	}
}

func TestGovernanceCRUD_ProfileDelete(t *testing.T) {
	id := uuid.New()
	path := "/api/v1/governance/profiles/" + id.String()
	for _, c := range []struct {
		name   string
		err    error
		want   int
		reason string
		audit  bool
	}{
		{"deleted", nil, http.StatusNoContent, "", true},
		{"unknown", store.ErrNotFound, http.StatusNotFound, reasonGovernanceProfileNotFoundByID, false},
		{"still assigned", store.ErrConflict, http.StatusConflict, reasonGovernanceProfileInUse, false},
		{"store failure", errors.New("boom"), http.StatusInternalServerError, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, h := govCrudServer(t, &govCrudStore{deleteProfileErr: c.err})
			w := do(t, srv, http.MethodDelete, path, adminToken, "")
			if w.Code != c.want || (c.reason != "" && errorReason(w) != c.reason) {
				t.Errorf("DELETE = %d %s, want %d %s", w.Code, w.Body.String(), c.want, c.reason)
			}
			got := strings.Join(govAuditActions(h), ",")
			if c.audit != (got == "governance.profile.delete") {
				t.Errorf("audit rows = %q, want delete row: %v", got, c.audit)
			}
		})
	}
	srv, _ := govCrudServer(t, &govCrudStore{})
	if w := do(t, srv, http.MethodDelete, "/api/v1/governance/profiles/nope", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("DELETE with a bad id = %d, want 400", w.Code)
	}
}

func TestGovernanceCRUD_AssignmentWrites(t *testing.T) {
	pid := uuid.New()
	body := func(subjectType, subject string) string {
		b, _ := json.Marshal(map[string]any{"subject_type": subjectType, "subject": subject, "profile_id": pid, "priority": 3})
		return string(b)
	}

	t.Run("refusals", func(t *testing.T) {
		srv, h := govCrudServer(t, &govCrudStore{})
		for name, b := range map[string]string{
			"an unknown field":                      `{"subject_type":"user","subject":"a@b.example","profile_id":"` + pid.String() + `","x":1}`,
			"a subject type outside the vocabulary": body("planet", "mars"),
			"no profile":                            `{"subject_type":"user","subject":"a@b.example"}`,
			"a user with no subject":                body("user", "   "),
			"a group that cannot match":             body("group", "café"),
			"an over-long subject":                  body("user", strings.Repeat("a", maxCapabilityGrantFieldLen+1)),
			"a malformed user type id":              body("user_type", "Not A Type!"),
		} {
			w := do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, b)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: POST = %d %s, want 400", name, w.Code, w.Body.String())
			}
		}
		if a := govAuditActions(h); len(a) != 0 {
			t.Errorf("refused assignments left audit rows %v", a)
		}
	})

	t.Run("all drops a caller-supplied subject", func(t *testing.T) {
		srv, h := govCrudServer(t, &govCrudStore{})
		w := do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, body("all", "everyone-but-really-nobody"))
		if w.Code != http.StatusCreated {
			t.Fatalf("POST = %d %s, want 201", w.Code, w.Body.String())
		}
		var got types.GovernanceAssignment
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got.Subject != "" || got.SubjectType != types.CapabilitySubjectAll {
			t.Errorf("saved %+v, want the all type with an empty subject", got)
		}
		if a := govAuditActions(h); len(a) != 1 || a[0] != "governance.assignment.write" {
			t.Errorf("audit = %v, want one governance.assignment.write", a)
		}
	})

	t.Run("a new binding is 201 and a repointed one is 200", func(t *testing.T) {
		srv, _ := govCrudServer(t, &govCrudStore{})
		if w := do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, body("group", "Eng")); w.Code != http.StatusCreated {
			t.Errorf("new = %d %s, want 201", w.Code, w.Body.String())
		}
		srv, _ = govCrudServer(t, &govCrudStore{existing: uuid.New()})
		if w := do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, body("group", "Eng")); w.Code != http.StatusOK {
			t.Errorf("repoint = %d %s, want 200", w.Code, w.Body.String())
		}
	})

	t.Run("store failures", func(t *testing.T) {
		srv, _ := govCrudServer(t, &govCrudStore{upsertAssignmentErr: store.ErrNotFound})
		w := do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, body("group", "Eng"))
		if w.Code != http.StatusNotFound || errorReason(w) != reasonGovernanceProfileNotFoundByID {
			t.Errorf("unknown profile = %d %s, want 404 %s", w.Code, w.Body.String(), reasonGovernanceProfileNotFoundByID)
		}
		srv, _ = govCrudServer(t, &govCrudStore{upsertAssignmentErr: errors.New("boom")})
		if w := do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, body("group", "Eng")); w.Code != http.StatusInternalServerError {
			t.Errorf("store failure = %d, want 500", w.Code)
		}
	})
}

func TestGovernanceCRUD_AssignmentDelete(t *testing.T) {
	path := "/api/v1/governance/assignments/" + uuid.New().String()
	for _, c := range []struct {
		name  string
		err   error
		want  int
		audit bool
	}{
		{"deleted", nil, http.StatusNoContent, true},
		{"unknown", store.ErrNotFound, http.StatusNotFound, false},
		{"store failure", errors.New("boom"), http.StatusInternalServerError, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, h := govCrudServer(t, &govCrudStore{deleteAssignErr: c.err})
			if w := do(t, srv, http.MethodDelete, path, adminToken, ""); w.Code != c.want {
				t.Errorf("DELETE = %d %s, want %d", w.Code, w.Body.String(), c.want)
			}
			if got := strings.Join(govAuditActions(h), ","); c.audit != (got == "governance.assignment.delete") {
				t.Errorf("audit rows = %q, want delete row: %v", got, c.audit)
			}
		})
	}
	srv, _ := govCrudServer(t, &govCrudStore{})
	if w := do(t, srv, http.MethodDelete, "/api/v1/governance/assignments/nope", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("DELETE with a bad id = %d, want 400", w.Code)
	}
}
