// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adorunpat"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// miscCovPersonStore is the identity directory, the person-erasure reads and writes, the run-output
// store and the sign-in end store, over memory, recording what it was asked and in what order.
type miscCovPersonStore struct {
	*memRunOutputs
	toks []types.APIToken

	mu    sync.Mutex
	calls []string

	runs       map[string][]uuid.UUID
	runsErr    error
	blanked    map[string]int
	blankErr   error
	principals map[string]string
	principErr error
	aliases    map[string][]string
	aliasErr   error
	govN       int
	govErr     error
	eraseErr   error
	beginErr   error
}

func newMiscCovPersonStore() *miscCovPersonStore {
	return &miscCovPersonStore{memRunOutputs: newMemRunOutputs(nil), runs: map[string][]uuid.UUID{}, blanked: map[string]int{},
		principals: map[string]string{}, aliases: map[string][]string{}}
}

func (s *miscCovPersonStore) note(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *miscCovPersonStore) callLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *miscCovPersonStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return s.toks, nil
}
func (s *miscCovPersonStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}

func (s *miscCovPersonStore) RunIDsCreatedBy(_ context.Context, by string) ([]uuid.UUID, error) {
	s.note("runs:" + by)
	return s.runs[by], s.runsErr
}

func (s *miscCovPersonStore) BlankRunTasks(_ context.Context, by string) (int, error) {
	s.note("blank:" + by)
	if s.blankErr != nil {
		return 0, s.blankErr
	}
	s.blanked[by]++
	return len(s.runs[by]), nil
}

func (s *miscCovPersonStore) PrincipalForName(_ context.Context, name string) (string, error) {
	s.note("principal:" + name)
	if s.principErr != nil {
		return "", s.principErr
	}
	if p, ok := s.principals[name]; ok {
		return p, nil
	}
	return name, nil
}

func (s *miscCovPersonStore) PrincipalAliases(_ context.Context, principal string) ([]string, error) {
	s.note("aliases:" + principal)
	return s.aliases[principal], s.aliasErr
}

func (s *miscCovPersonStore) EraseGovernanceChangePersonalFields(_ context.Context, names []string) (int, error) {
	s.note("governance:" + strings.Join(names, ","))
	return s.govN, s.govErr
}

func (s *miscCovPersonStore) SubjectKeyDestroyedSince(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func (s *miscCovPersonStore) EraseRunOutputs(ctx context.Context, ids []uuid.UUID) error {
	s.note(fmt.Sprintf("erase-outputs:%d", len(ids)))
	if s.eraseErr != nil {
		return s.eraseErr
	}
	return s.memRunOutputs.EraseRunOutputs(ctx, ids)
}

func (s *miscCovPersonStore) BeginADOSignInEnd(context.Context, string, string) error {
	s.note("begin-end")
	return s.beginErr
}

func (s *miscCovPersonStore) FinishADOSignInEnd(context.Context, string) error { return nil }

func (s *miscCovPersonStore) ADOSignInEnds(context.Context, string) (store.ADOSignInEndState, error) {
	return store.ADOSignInEndState{}, nil
}

// miscCovPersonRig is an erasure server over the in-memory person store.
func miscCovPersonRig(t *testing.T) (*harness, *Server, *miscCovPersonStore, *memSecrets) {
	t.Helper()
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := eraseFixture(t, sec)
	st := newMiscCovPersonStore()
	st.toks = []types.APIToken{{ID: uuid.New(), Principal: "bob", Email: "bob@corp.example"}}
	st.runs["bob"] = []uuid.UUID{
		uuid.MustParse("00000000-0000-4000-8000-0000000000c1"), uuid.MustParse("00000000-0000-4000-8000-0000000000c2"),
	}
	srv.cfg.Store = st
	return h, srv, st, sec
}

func miscCovErasureRow(t *testing.T, h *harness) (types.AuditEvent, map[string]any) {
	t.Helper()
	rows := erasureRows(h)
	if len(rows) != 1 {
		t.Fatalf("person.erasure rows = %+v, want exactly one", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return rows[0], data
}

// A request the server cannot read, or names nobody it can resolve, erases nothing.
func TestMiscCovErasePersonRefusesBeforeErasing(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
		reason           string
		wantRow          bool
	}{
		{"a body that is not JSON", "bob", `{"scopes":`, http.StatusBadRequest, reasonInvalidRequestBody, false},
		{"a body with an unknown field", "bob", `{"scopes":["run_tasks"],"force":true}`, http.StatusBadRequest, reasonInvalidRequestBody, false},
		{"an email nobody here has", "ghost@corp.example", erasureBody("run_tasks"), http.StatusUnprocessableEntity, reasonOwnerUnresolved, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, srv, st, _ := miscCovPersonRig(t)
			w := do(t, srv, http.MethodPost, "/api/v1/people/"+tc.path+"/erasure", adminToken, tc.body)
			if w.Code != tc.status || (tc.reason != "" && errorReason(w) != tc.reason) {
				t.Fatalf("erasure = %d %s (%s), want %d %s", w.Code, errorReason(w), w.Body, tc.status, tc.reason)
			}
			if len(st.callLog()) != 0 || len(st.blanked) != 0 {
				t.Errorf("a refused request reached the store: %v", st.callLog())
			}
			rows := erasureRows(h)
			if (len(rows) == 1) != tc.wantRow {
				t.Fatalf("person.erasure rows = %+v, want a row: %v", rows, tc.wantRow)
			}
			if tc.wantRow && (rows[0].Outcome != "denied" || !strings.Contains(string(rows[0].Data), tc.reason) || rows[0].Target != tc.path) {
				t.Errorf("refusal row = %+v, want a denied row for %s naming %s", rows[0], tc.path, tc.reason)
			}
		})
	}
}

func TestMiscCovErasePersonRefusesAnAmbiguousEmail(t *testing.T) {
	h, srv, st, _ := miscCovPersonRig(t)
	st.toks = []types.APIToken{{Principal: "Bob", Email: "bob@corp.example"}, {Principal: "bob", Email: "BOB@corp.example"}}
	w := do(t, srv, http.MethodPost, "/api/v1/people/bob@corp.example/erasure", adminToken, erasureBody("run_tasks", "credentials"))
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOwnerAmbiguous {
		t.Fatalf("erasure = %d %s (%s), want 422 %s", w.Code, errorReason(w), w.Body, reasonOwnerAmbiguous)
	}
	if len(st.callLog()) != 0 {
		t.Errorf("an ambiguous request reached the store: %v", st.callLog())
	}
	if row := erasureRows(h); len(row) != 1 || row[0].Outcome != "denied" || !strings.Contains(string(row[0].Data), reasonOwnerAmbiguous) {
		t.Errorf("person.erasure rows = %+v, want one denied row naming %s", row, reasonOwnerAmbiguous)
	}
}

// A bare name the directory does not know is taken as typed, and the row says it was not known.
func TestMiscCovErasePersonOfANameTheDirectoryDoesNotKnowIsMarkedInTheRow(t *testing.T) {
	h, srv, st, _ := miscCovPersonRig(t)
	w := do(t, srv, http.MethodPost, "/api/v1/people/ghost/erasure", adminToken, erasureBody("run_tasks"))
	if w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	if st.blanked["ghost"] != 1 {
		t.Errorf("tasks blanked for %v, want ghost, as typed", st.blanked)
	}
	row, data := miscCovErasureRow(t, h)
	if row.Outcome != "success" || row.Target != "ghost" || data["owner_known"] != false {
		t.Errorf("row = %+v data %v, want a success for ghost with owner_known=false", row, data)
	}
}

// A scope that fails on a lock refusal is the caller's retry, not a server fault: 503 with Retry-After,
// the row names the failed scope, and nothing after it ran.
func TestMiscCovErasePersonAnswersALockRefusalWithARetry(t *testing.T) {
	h, srv, st, sec := miscCovPersonRig(t)
	st.blankErr = fmt.Errorf("blank tasks: %w", db.ErrLockBusy)
	if err := sec.For("bob").Put(t.Context(), "anthropic-api-key", []byte("seeded-credential-value-0000")); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", adminToken, erasureBody("run_tasks", "credentials"))
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonLockUnavailable || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("erasure = %d %s retry-after %q (%s), want 503 %s with Retry-After 5", w.Code, errorReason(w), w.Header().Get("Retry-After"), w.Body, reasonLockUnavailable)
	}
	if len(namesOf(t, sec, "bob")) == 0 {
		t.Error("credentials were erased after the scope before them failed")
	}
	row, data := miscCovErasureRow(t, h)
	oc, _ := data["outcome"].(map[string]any)
	if row.Outcome != "failure" || oc["run_tasks"] != "failed" || oc["credentials"] != "not_run" {
		t.Errorf("row = %+v outcome %v, want a failure with run_tasks failed and credentials not_run", row, oc)
	}
}

// The credentials scope refuses to start when the sign-in end cannot be recorded, and erases nothing.
func TestMiscCovErasePersonCredentialsRefusedWhenTheSignInEndCannotBeRecorded(t *testing.T) {
	_, srv, st, sec := miscCovPersonRig(t)
	st.beginErr = errors.New("sign-in end store down")
	if err := sec.For("bob").Put(t.Context(), "anthropic-api-key", []byte("seeded-credential-value-0000")); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", adminToken, erasureBody("credentials"))
	if w.Code != http.StatusInternalServerError || errorReason(w) != reasonErasureIncomplete {
		t.Fatalf("erasure = %d %s (%s), want 500 %s", w.Code, errorReason(w), w.Body, reasonErasureIncomplete)
	}
	var body erasureIncompleteBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(body.Remaining, []string{"credentials"}) || len(body.Done) != 0 {
		t.Errorf("done %v remaining %v, want credentials left", body.Done, body.Remaining)
	}
	if got := namesOf(t, sec, "bob"); len(got) == 0 {
		t.Errorf("credentials erased although the end could not be recorded: %v", got)
	}
	if !slices.Contains(st.callLog(), "begin-end") {
		t.Errorf("the sign-in end was never begun: %v", st.callLog())
	}
}

// Every scope the server has a backing for is registered: a scope with none is refused, and one with
// one runs its own step and not the "not available" refusal.
func TestMiscCovErasureOrchestratorRegistersABackedScopeOnly(t *testing.T) {
	pool, closedErr := miscCovClosedPool(t)
	srv := newHarness(t).srv
	run := func(sc erasure.Scope) error {
		_, err := srv.erasureOrchestrator(nil).Orchestrate(t.Context(), "bob", []erasure.Scope{sc})
		return err
	}
	for _, sc := range []erasure.Scope{erasure.MaskCopies, erasure.AuditPersonalFields, erasure.Credentials} {
		if err := run(sc); !errors.Is(err, erasure.ErrNotAvailable) {
			t.Errorf("%s with no backing = %v, want ErrNotAvailable", sc, err)
		}
	}

	srv.cfg.MaskManifests = maskmanifest.New(pool, nil, secretmask.NewRegistry())
	srv.cfg.SubjectKeys = subjectkey.New(pool, subjectkey.Resolver{})
	for _, sc := range []erasure.Scope{erasure.MaskCopies, erasure.AuditPersonalFields} {
		err := run(sc)
		var inc *erasure.IncompleteError
		if !errors.As(err, &inc) || errors.Is(err, erasure.ErrNotAvailable) || !errors.Is(err, closedErr) {
			t.Errorf("%s with a backing = %v, want its step to run and fail on the closed pool", sc, err)
		}
	}
}

func TestMiscCovEraseMaskCopiesStopsWhenTheFenceFails(t *testing.T) {
	pool, closedErr := miscCovClosedPool(t)
	srv := newHarness(t).srv
	srv.cfg.MaskManifests = maskmanifest.New(pool, nil, secretmask.NewRegistry())
	srv.cfg.ADORunPATs = adorunpat.New(pool, nil)
	detail, err := srv.eraseMaskCopies(t.Context(), "bob")
	if !errors.Is(err, closedErr) {
		t.Fatalf("err = %v, want the fence's error", err)
	}
	m, _ := detail.(map[string]any)
	if m["runs_fenced"] != 0 {
		t.Errorf("detail = %v, want runs_fenced 0", detail)
	}
	if _, ran := m["run_tokens"]; ran {
		t.Errorf("run tokens were deleted after the fence failed: %v", detail)
	}
	if _, err := srv.eraseMaskCopies(t.Context(), ""); !errors.Is(err, maskmanifest.ErrNoOwner) {
		t.Errorf("a blank person = %v, want ErrNoOwner", err)
	}
}

func TestMiscCovEraseRunOutputsOf(t *testing.T) {
	ids := []uuid.UUID{uuid.MustParse("00000000-0000-4000-8000-0000000000d1"), uuid.MustParse("00000000-0000-4000-8000-0000000000d2")}

	t.Run("a store that cannot list a person's runs is not available", func(t *testing.T) {
		_, err := newHarness(t).srv.eraseRunOutputsOf(t.Context(), "bob")
		if !errors.Is(err, erasure.ErrNotAvailable) {
			t.Errorf("err = %v, want ErrNotAvailable", err)
		}
	})
	t.Run("every run's output is erased in one call", func(t *testing.T) {
		_, srv, st, _ := miscCovPersonRig(t)
		st.runs["bob"] = ids
		got, err := srv.eraseRunOutputsOf(t.Context(), "bob")
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := got.(map[string]any); m["runs"] != 2 {
			t.Errorf("detail = %v, want runs 2", got)
		}
		if !slices.Equal(st.callLog(), []string{"runs:bob", "erase-outputs:2"}) {
			t.Errorf("store calls = %v, want one list then one erase of both runs", st.callLog())
		}
		for _, id := range ids {
			if !st.erased[id] {
				t.Errorf("run %s output was not erased", id)
			}
		}
	})
	t.Run("a person with no runs asks the store to erase nothing", func(t *testing.T) {
		_, srv, st, _ := miscCovPersonRig(t)
		got, err := srv.eraseRunOutputsOf(t.Context(), "nobody")
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := got.(map[string]any); m["runs"] != 0 {
			t.Errorf("detail = %v, want runs 0", got)
		}
		if !slices.Equal(st.callLog(), []string{"runs:nobody"}) {
			t.Errorf("store calls = %v, want the list alone", st.callLog())
		}
	})
	t.Run("a list that fails is the step's error, with nothing erased", func(t *testing.T) {
		_, srv, st, _ := miscCovPersonRig(t)
		st.runsErr = errors.New("list refused")
		if _, err := srv.eraseRunOutputsOf(t.Context(), "bob"); !errors.Is(err, st.runsErr) {
			t.Errorf("err = %v, want the list's error", err)
		}
		for _, c := range st.callLog() {
			if strings.HasPrefix(c, "erase-outputs:") {
				t.Errorf("store calls = %v, want no erase after a failed list", st.callLog())
			}
		}
		if len(st.erased) != 0 {
			t.Errorf("runs erased after a failed list: %v", st.erased)
		}
	})
	t.Run("an erase that fails is the step's error", func(t *testing.T) {
		_, srv, st, _ := miscCovPersonRig(t)
		st.eraseErr = errors.New("erase refused")
		if _, err := srv.eraseRunOutputsOf(t.Context(), "bob"); !errors.Is(err, st.eraseErr) {
			t.Errorf("err = %v, want the erase's error", err)
		}
	})
}

// miscCovRecordings is a recording store with nothing in it; deleter adds the delete that erasure needs.
type miscCovRecordings struct{ recording.Store }

type miscCovRecordingDeleter struct {
	miscCovRecordings
	counts map[string]int
	fail   map[string]error
	asked  []string
}

func (d *miscCovRecordingDeleter) DeleteRun(_ context.Context, runID string) (int, error) {
	d.asked = append(d.asked, runID)
	return d.counts[runID], d.fail[runID]
}

func TestMiscCovEraseRecordingsOf(t *testing.T) {
	a, b := uuid.MustParse("00000000-0000-4000-8000-0000000000e1"), uuid.MustParse("00000000-0000-4000-8000-0000000000e2")

	t.Run("no recording store has nothing to delete", func(t *testing.T) {
		_, srv, st, _ := miscCovPersonRig(t)
		got, err := srv.eraseRecordingsOf(t.Context(), "bob")
		if m, _ := got.(map[string]any); err != nil || m["recordings"] != 0 {
			t.Fatalf("got %v, %v; want recordings 0", got, err)
		}
		if len(st.callLog()) != 0 {
			t.Errorf("the store was asked: %v", st.callLog())
		}
	})
	t.Run("a store that cannot delete is not available, never reported erased", func(t *testing.T) {
		_, srv, _, _ := miscCovPersonRig(t)
		srv.cfg.RecordingStore = miscCovRecordings{}
		if _, err := srv.eraseRecordingsOf(t.Context(), "bob"); !errors.Is(err, erasure.ErrNotAvailable) {
			t.Errorf("err = %v, want ErrNotAvailable", err)
		}
	})
	t.Run("a server whose store cannot list runs is not available", func(t *testing.T) {
		srv := newHarness(t).srv
		srv.cfg.RecordingStore = &miscCovRecordingDeleter{}
		if _, err := srv.eraseRecordingsOf(t.Context(), "bob"); !errors.Is(err, erasure.ErrNotAvailable) {
			t.Errorf("err = %v, want ErrNotAvailable", err)
		}
	})
	t.Run("every run's recordings are deleted and counted", func(t *testing.T) {
		_, srv, st, _ := miscCovPersonRig(t)
		st.runs["bob"] = []uuid.UUID{a, b}
		del := &miscCovRecordingDeleter{counts: map[string]int{a.String(): 2, b.String(): 3}}
		srv.cfg.RecordingStore = del
		got, err := srv.eraseRecordingsOf(t.Context(), "bob")
		if m, _ := got.(map[string]any); err != nil || m["recordings"] != 5 {
			t.Fatalf("got %v, %v; want recordings 5", got, err)
		}
		if !slices.Equal(del.asked, []string{a.String(), b.String()}) {
			t.Errorf("deleted runs %v, want both, in order", del.asked)
		}
	})
	t.Run("a failed list or delete stops the scope", func(t *testing.T) {
		_, srv, st, _ := miscCovPersonRig(t)
		st.runs["bob"] = []uuid.UUID{a, b}
		boom := errors.New("delete refused")
		del := &miscCovRecordingDeleter{fail: map[string]error{a.String(): boom}}
		srv.cfg.RecordingStore = del
		if _, err := srv.eraseRecordingsOf(t.Context(), "bob"); !errors.Is(err, boom) {
			t.Errorf("err = %v, want the delete's error", err)
		}
		if !slices.Equal(del.asked, []string{a.String()}) {
			t.Errorf("deleted runs %v, want the second run left for the retry", del.asked)
		}
		st.runsErr = errors.New("list refused")
		del.asked = nil
		if _, err := srv.eraseRecordingsOf(t.Context(), "bob"); !errors.Is(err, st.runsErr) || len(del.asked) != 0 {
			t.Errorf("err = %v, deleted %v; want the list's error and no delete", err, del.asked)
		}
	})
}

func TestMiscCovEraseRunTasksNeedsAStoreThatListsRuns(t *testing.T) {
	_, err := newHarness(t).srv.eraseRunTasks(t.Context(), "bob")
	if !errors.Is(err, erasure.ErrNotAvailable) {
		t.Fatalf("err = %v, want ErrNotAvailable", err)
	}
}

// The audit_personal_fields scope resolves the person to their principal and aliases before it destroys
// a key, and destroys no key and touches no governance change when a resolve fails.
func TestMiscCovEraseAuditSealKeys(t *testing.T) {
	pool, closedErr := miscCovClosedPool(t)
	setup := func(t *testing.T) (*Server, *miscCovPersonStore) {
		_, srv, st, _ := miscCovPersonRig(t)
		srv.cfg.SubjectKeys = subjectkey.New(pool, subjectkey.Resolver{})
		return srv, st
	}

	t.Run("a failed principal lookup stops before the aliases", func(t *testing.T) {
		srv, st := setup(t)
		st.principErr = errors.New("directory down")
		if _, err := srv.eraseAuditSealKeys(t.Context(), "bob@corp.example"); !errors.Is(err, st.principErr) {
			t.Fatalf("err = %v, want the lookup's error", err)
		}
		if !slices.Equal(st.callLog(), []string{"principal:bob@corp.example"}) {
			t.Errorf("store calls = %v, want the lookup alone", st.callLog())
		}
	})
	t.Run("a failed alias lookup stops before any key is destroyed", func(t *testing.T) {
		srv, st := setup(t)
		st.principals["bob@corp.example"] = "bob"
		st.aliasErr = errors.New("aliases down")
		_, err := srv.eraseAuditSealKeys(t.Context(), "bob@corp.example")
		if !errors.Is(err, st.aliasErr) || errors.Is(err, closedErr) {
			t.Fatalf("err = %v, want the alias lookup's error and no destroy attempt", err)
		}
		if !slices.Equal(st.callLog(), []string{"principal:bob@corp.example", "aliases:bob"}) {
			t.Errorf("store calls = %v, want the principal then the aliases of bob", st.callLog())
		}
	})
	t.Run("a key that cannot be destroyed leaves the governance changes alone", func(t *testing.T) {
		srv, st := setup(t)
		st.principals["bob@corp.example"] = "bob"
		st.aliases["bob"] = []string{"bob@corp.example", "entra:tenant:object"}
		_, err := srv.eraseAuditSealKeys(t.Context(), "bob@corp.example")
		if !errors.Is(err, closedErr) {
			t.Fatalf("err = %v, want the destroy's error", err)
		}
		for _, c := range st.callLog() {
			if strings.HasPrefix(c, "governance:") {
				t.Errorf("governance changes were touched after the key destroy failed: %v", st.callLog())
			}
		}
	})
	t.Run("with a store that cannot resolve people the destroy is still attempted and its error returned", func(t *testing.T) {
		srv := newHarness(t).srv
		srv.cfg.SubjectKeys = subjectkey.New(pool, subjectkey.Resolver{})
		if _, err := srv.eraseAuditSealKeys(t.Context(), "bob"); !errors.Is(err, closedErr) {
			t.Errorf("err = %v, want the destroy's error", err)
		}
	})
}

func TestMiscCovErasureSelfTargetAndScopeHelpers(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		actor                        types.ActorType
		principal, email, owner, raw string
		want                         bool
	}{
		{"the admin token is no person", types.ActorSystem, "sec-1", "", "sec-1", "sec-1", false},
		{"the caller's own principal", types.ActorHuman, "sec-1", "", "sec-1", "x", true},
		{"the name as typed is the caller's principal", types.ActorHuman, "sec-1", "", "other", "sec-1", true},
		{"the caller's email, folded", types.ActorHuman, "sec-1", "Sec@Corp.example", "other", "sec@corp.example", true},
		{"someone else", types.ActorHuman, "sec-1", "sec@corp.example", "bob", "bob", false},
		{"a caller with no principal or email", types.ActorHuman, "", "", "", "", false},
	} {
		if got := erasureSelfTarget(tc.actor, tc.principal, tc.email, tc.owner, tc.raw); got != tc.want {
			t.Errorf("%s: = %v, want %v", tc.name, got, tc.want)
		}
	}
	got := sortedScopes([]erasure.Scope{erasure.Credentials, erasure.RunTasks, erasure.Credentials, erasure.MaskCopies})
	if want := []erasure.Scope{erasure.MaskCopies, erasure.RunTasks, erasure.Credentials}; !slices.Equal(got, want) {
		t.Errorf("sortedScopes = %v, want %v (deduplicated, in run order)", got, want)
	}
}
