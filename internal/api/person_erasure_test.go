// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// erasureTestStore is the identity directory plus the person-erasure reads and
// writes, over memory.
type erasureTestStore struct {
	secretOwnerDirectory
	runs      map[string][]uuid.UUID
	blanked   []string
	failBlank error
}

func (s *erasureTestStore) RunIDsCreatedBy(_ context.Context, by string) ([]uuid.UUID, error) {
	return s.runs[by], nil
}

func (s *erasureTestStore) BlankRunTasks(_ context.Context, by string) (int, error) {
	if s.failBlank != nil {
		return 0, s.failBlank
	}
	s.blanked = append(s.blanked, by)
	return len(s.runs[by]), nil
}

func (s *erasureTestStore) PrincipalForName(_ context.Context, name string) (string, error) {
	return name, nil
}

func (s *erasureTestStore) PrincipalAliases(context.Context, string) ([]string, error) {
	return nil, nil
}

func (s *erasureTestStore) EraseGovernanceChangePersonalFields(context.Context, []string) (int, error) {
	return 0, nil
}

func (s *erasureTestStore) SubjectKeyDestroyedSince(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func erasureRig(t *testing.T) (*harness, *Server, *erasureTestStore, secretstore.Store) {
	t.Helper()
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := eraseFixture(t, sec)
	st := &erasureTestStore{
		secretOwnerDirectory: secretOwnerDirectory{toks: []types.APIToken{
			{ID: uuid.New(), Principal: "bob", Email: "bob@corp.example"},
			{ID: uuid.New(), Principal: "sec-1", Email: "sec@corp.example"},
		}},
		runs: map[string][]uuid.UUID{"bob": {uuid.New(), uuid.New()}},
	}
	srv.cfg.Store = st
	if err := sec.For("sec-1").Put(t.Context(), "anthropic-api-key", []byte("seeded-credential-value-0000")); err != nil {
		t.Fatal(err)
	}
	return h, srv, st, sec
}

func erasureBody(scopes ...string) string {
	b, _ := json.Marshal(map[string]any{"scopes": scopes})
	return string(b)
}

func erasureRows(h *harness) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.audit.events {
		if ev.Action == "person.erasure" {
			out = append(out, ev)
		}
	}
	return out
}

func TestErasePerson_RefusesABadScopeListBeforeErasingAnything(t *testing.T) {
	h, srv, st, sec := erasureRig(t)
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	for name, body := range map[string]string{
		"empty list":    erasureBody(),
		"missing":       `{}`,
		"unknown scope": erasureBody("credentials", "everything"),
	} {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", sa, body)
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonErasureScopeUnknown {
			t.Errorf("%s: %d %s, want 400 %s", name, w.Code, w.Body, reasonErasureScopeUnknown)
		}
	}
	if len(st.blanked) != 0 || len(namesOf(t, sec, "bob")) != 2 {
		t.Fatalf("a refused request erased something: blanked %v, bob holds %v", st.blanked, namesOf(t, sec, "bob"))
	}
	if rows := erasureRows(h); len(rows) != 3 || rows[0].Outcome != "failure" || rows[0].Target != "bob" {
		t.Errorf("person.erasure rows = %+v, want a failure row per refusal", rows)
	}
}

func TestErasePerson_RefusesTheOperatorNamespaceBeforeAnything(t *testing.T) {
	_, srv, st, sec := erasureRig(t)
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/people/%20/erasure", sa, erasureBody("credentials", "run_tasks"))
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonErasureOperatorNamespace {
		t.Fatalf("operator namespace = %d %s, want 400 %s", w.Code, w.Body, reasonErasureOperatorNamespace)
	}
	if len(st.blanked) != 0 || !slices.Equal(namesOf(t, sec, ""), []string{"npm-token"}) {
		t.Fatalf("a refused request erased something: blanked %v, operator holds %v", st.blanked, namesOf(t, sec, ""))
	}
}

func TestErasePerson_OnlyTheNamedScopesRun(t *testing.T) {
	h, srv, st, sec := erasureRig(t)
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/people/bob@corp.example/erasure", sa, erasureBody("run_tasks"))
	if w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	if !slices.Equal(st.blanked, []string{"bob"}) {
		t.Errorf("blanked %v, want bob's tasks (resolved from his email)", st.blanked)
	}
	if got := namesOf(t, sec, "bob"); len(got) != 2 {
		t.Errorf("bob's credentials = %v: a scope that was not asked for ran", got)
	}
	rows := erasureRows(h)
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Target != "bob" {
		t.Fatalf("person.erasure rows = %+v, want one success for bob", rows)
	}
	var data map[string]any
	_ = json.Unmarshal(rows[0].Data, &data)
	if oc, _ := data["outcome"].(map[string]any); oc["run_tasks"] != "done" || len(oc) != 1 {
		t.Errorf("row outcome = %v, want run_tasks done and nothing else", data["outcome"])
	}
}

func TestErasePerson_ASecurityAdminCannotEraseThemselfExceptTheirCredentials(t *testing.T) {
	h, srv, st, sec := erasureRig(t)
	self := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	for _, who := range []string{"sec-1", "sec@corp.example"} {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+who+"/erasure", self, erasureBody("credentials", "run_tasks"))
		if w.Code != http.StatusForbidden || errorReason(w) != reasonErasureSelfRefused {
			t.Fatalf("self erasure as %s = %d %s, want 403 %s", who, w.Code, w.Body, reasonErasureSelfRefused)
		}
	}
	if len(st.blanked) != 0 || len(namesOf(t, sec, "sec-1")) != 1 {
		t.Fatalf("a refused self erasure erased something: blanked %v, holds %v", st.blanked, namesOf(t, sec, "sec-1"))
	}
	rows := erasureRows(h)
	if len(rows) != 2 || rows[1].Outcome != "failure" || !strings.Contains(string(rows[1].Data), reasonErasureSelfRefused) {
		t.Fatalf("person.erasure rows = %+v, want a failure row naming %s", rows, reasonErasureSelfRefused)
	}
	// Their own credentials they may erase.
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/sec-1/erasure", self, erasureBody("credentials")); w.Code != http.StatusOK {
		t.Fatalf("self credentials erasure = %d %s, want 200", w.Code, w.Body)
	}
	if got := namesOf(t, sec, "sec-1"); len(got) != 0 {
		t.Errorf("their credentials survive their own credentials erasure: %v", got)
	}
}

// The admin token is no person, so it may erase anyone, every scope it can.
func TestErasePerson_TheAdminTokenMayErase(t *testing.T) {
	_, srv, st, sec := erasureRig(t)
	w := do(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", adminToken, erasureBody("credentials", "run_tasks"))
	if w.Code != http.StatusOK {
		t.Fatalf("admin-token erasure = %d %s", w.Code, w.Body)
	}
	if !slices.Equal(st.blanked, []string{"bob"}) || len(namesOf(t, sec, "bob")) != 0 {
		t.Fatalf("blanked %v, bob holds %v, want both erased", st.blanked, namesOf(t, sec, "bob"))
	}
}

// A scope that fails stops the run: the answer names what is done and what is
// left, nothing after it ran, and a retry finishes the rest.
func TestErasePerson_APartialFailureNamesTheScopesLeftAndARetryCompletes(t *testing.T) {
	h, srv, st, sec := erasureRig(t)
	st.failBlank = errors.New("store down")
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", sa, erasureBody("credentials", "run_tasks"))
	var body erasureIncompleteBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusInternalServerError || body.Reason != reasonErasureIncomplete {
		t.Fatalf("erasure = %d %s, want 500 %s", w.Code, w.Body, reasonErasureIncomplete)
	}
	if !slices.Equal(body.Remaining, []string{"run_tasks", "credentials"}) || len(body.Done) != 0 {
		t.Errorf("done %v, remaining %v, want everything left", body.Done, body.Remaining)
	}
	if len(namesOf(t, sec, "bob")) != 2 {
		t.Error("credentials were erased after the scope before them failed")
	}
	rows := erasureRows(h)
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("person.erasure rows = %+v, want one failure row", rows)
	}
	var data map[string]any
	_ = json.Unmarshal(rows[0].Data, &data)
	if oc, _ := data["outcome"].(map[string]any); oc["run_tasks"] != "failed" || oc["credentials"] != "not_run" {
		t.Errorf("row outcome = %v, want run_tasks failed and credentials not_run", data["outcome"])
	}

	st.failBlank = nil
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", sa, erasureBody("credentials", "run_tasks")); w.Code != http.StatusOK {
		t.Fatalf("retry = %d %s, want 200", w.Code, w.Body)
	}
	if len(namesOf(t, sec, "bob")) != 0 || !slices.Equal(st.blanked, []string{"bob"}) {
		t.Errorf("after the retry: blanked %v, bob holds %v", st.blanked, namesOf(t, sec, "bob"))
	}
}

// A scope this server has no backing for is refused, never reported erased.
func TestErasePerson_AScopeWithNoBackingIsNotReportedErased(t *testing.T) {
	_, srv, _, _ := erasureRig(t)
	w := do(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", adminToken, erasureBody("mask_copies"))
	if w.Code != http.StatusInternalServerError || errorReason(w) != reasonErasureIncomplete {
		t.Fatalf("mask_copies with no manifests = %d %s, want 500 %s", w.Code, w.Body, reasonErasureIncomplete)
	}
}

// The credential route is the credentials scope alone: it never reaches run
// tasks, and shares the orchestrator's one credentials path.
func TestErasePersonCredentialsRouteLeavesEverythingElseAlone(t *testing.T) {
	_, srv, st, sec := erasureRig(t)
	w := do(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("credentials erase = %d %s", w.Code, w.Body)
	}
	if len(namesOf(t, sec, "bob")) != 0 || len(st.blanked) != 0 {
		t.Fatalf("credentials route: bob holds %v, blanked %v; want credentials only", namesOf(t, sec, "bob"), st.blanked)
	}
}

// fakeUnsealer stands in for audit.Sealer.
type fakeUnsealer struct{ err error }

func (f fakeUnsealer) Unseal(_ context.Context, evs []types.AuditEvent) ([]types.AuditEvent, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := append([]types.AuditEvent(nil), evs...)
	for i := range out {
		out[i].Data = json.RawMessage(strings.ReplaceAll(string(out[i].Data), "seal1.sealed", "opened"))
	}
	return out, nil
}

var _ audit.Unsealer = fakeUnsealer{}

func TestAuditReadsOpenSealedFieldsAndFailOnAKeyStoreOutage(t *testing.T) {
	evs := []types.AuditEvent{{ID: uuid.New(), Action: "approval.decide", Data: json.RawMessage(`{"reason":"seal1.sealed"}`)}}
	s := &Server{cfg: Config{AuditUnsealer: fakeUnsealer{}}}
	got, err := s.unsealed(t.Context(), evs, nil)
	if err != nil || !strings.Contains(string(got[0].Data), "opened") {
		t.Fatalf("unsealed = %s, %v, want the field opened", got[0].Data, err)
	}
	if string(evs[0].Data) != `{"reason":"seal1.sealed"}` {
		t.Error("the caller's rows were changed in place")
	}
	s.cfg.AuditUnsealer = fakeUnsealer{err: errors.New("key store down")}
	if _, err := s.unsealed(t.Context(), evs, nil); err == nil {
		t.Error("an outage of the key store answered with the rows as stored")
	}
	if got, err := (&Server{}).unsealed(t.Context(), evs, nil); err != nil || string(got[0].Data) != string(evs[0].Data) {
		t.Errorf("with no unsealer rows must pass through: %v %v", got, err)
	}
}
