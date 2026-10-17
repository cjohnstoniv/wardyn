// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var govCovAssignID = uuid.MustParse("eeeeeeee-0000-0000-0000-000000000001")

// govCovAsRefusal unwraps the refusal an apply decided, failing the test when err is anything else.
func govCovAsRefusal(t *testing.T, err error) *profileWriteError {
	t.Helper()
	var ref *profileWriteError
	if !errors.As(err, &ref) {
		t.Fatalf("error = %v, want a refusal the apply decided", err)
	}
	return ref
}

// govCovExecFailingOn answers every Exec with a one-row tag except the nth (1-based), which fails.
func govCovExecFailingOn(n int, err error) func(string) (pgconn.CommandTag, error) {
	calls := 0
	return func(string) (pgconn.CommandTag, error) {
		calls++
		if calls == n {
			return pgconn.CommandTag{}, err
		}
		return pgconn.NewCommandTag("SELECT 1"), nil
	}
}

func govCovExecTag(tag string) func(string) (pgconn.CommandTag, error) {
	return func(sql string) (pgconn.CommandTag, error) {
		if strings.HasPrefix(strings.TrimSpace(sql), "DELETE") {
			return pgconn.NewCommandTag(tag), nil
		}
		return pgconn.NewCommandTag("SELECT 1"), nil
	}
}

func TestGovCovApplyProfileChange(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	r := govCovHumanReq(http.MethodPost, "/x", "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin)
	boom := errors.New("pg: down")

	t.Run("a target key that is not a profile id is an error before any statement", func(t *testing.T) {
		q := &govCovQuerier{}
		_, err := applyProfileChange(s, r, q, types.GovernanceChange{ID: govCovChangeID, TargetKey: "nope", Op: "update"})
		if err == nil || !strings.Contains(err.Error(), `target key "nope" is not a profile id`) || !strings.Contains(err.Error(), govCovChangeID.String()) {
			t.Errorf("error = %v", err)
		}
		if len(q.seen) != 0 {
			t.Errorf("statements = %v", q.seen)
		}
	})

	t.Run("a payload that no longer validates is a 400 and touches nothing", func(t *testing.T) {
		q := &govCovQuerier{}
		_, err := applyProfileChange(s, r, q, types.GovernanceChange{TargetKey: govCovPID.String(), Op: "update", Payload: json.RawMessage(`{"name":"  "}`)})
		ref := govCovAsRefusal(t, err)
		if ref.status != http.StatusBadRequest || ref.reason != reasonGovernanceProfileRequestInvalid || ref.msg != "name is required" || len(q.seen) != 0 {
			t.Errorf("refusal = %+v after %v", ref, q.seen)
		}
	})

	t.Run("a write whose profile list cannot be read writes nothing", func(t *testing.T) {
		q := &govCovQuerier{queryErr: boom}
		_, err := applyProfileChange(s, r, q, types.GovernanceChange{TargetKey: govCovPID.String(), Op: "update", Payload: json.RawMessage(govCovProfileBody("p", "pypi.org"))})
		if !errors.Is(err, boom) || len(q.writes()) != 0 || len(q.seen) != 2 {
			t.Errorf("error = %v, statements %v; want the injected error after the lock and the list", err, q.seen)
		}
	})

	t.Run("a delete stops at a failed graph lock", func(t *testing.T) {
		q := &govCovQuerier{execFn: govCovExecFailingOn(1, boom)}
		_, err := applyProfileChange(s, r, q, types.GovernanceChange{TargetKey: govCovPID.String(), Op: "delete"})
		if !errors.Is(err, boom) || len(q.seen) != 1 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("a delete whose profile list cannot be read deletes nothing", func(t *testing.T) {
		q := &govCovQuerier{queryErr: boom}
		_, err := applyProfileChange(s, r, q, types.GovernanceChange{TargetKey: govCovPID.String(), Op: "delete"})
		if !errors.Is(err, boom) || len(q.writes()) != 0 {
			t.Errorf("error = %v, writes %v", err, q.writes())
		}
	})
}

func govCovAssignChange(t *testing.T, s *Server, op string, req governanceAssignmentRequest) types.GovernanceChange {
	t.Helper()
	ch := types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindAssignment, Op: op, TargetKey: assignmentKey(req.SubjectType, req.Subject), ProposedBy: "sub-alice"}
	if op == "delete" {
		ch.Payload, _ = json.Marshal(map[string]any{"id": govCovAssignID})
		ch.BaseHash = assignmentState{chain: []types.GovernanceProfile{}}.hash()
	} else {
		ch.Payload, _ = json.Marshal(req)
		ch.BaseHash = assignmentState{chain: []types.GovernanceProfile{}}.hash()
	}
	return ch
}

func TestGovCovApplyAssignmentChange(t *testing.T) {
	boom := errors.New("pg: down")
	userReq := governanceAssignmentRequest{SubjectType: types.CapabilitySubjectUser, Subject: "u@corp.example", ProfileID: govCovPID, Priority: 3}
	r := govCovHumanReq(http.MethodPost, "/x", "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin)

	t.Run("a malformed key is an error before any statement", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		_, err := applyAssignmentChange(s, r, q, types.GovernanceChange{ID: govCovChangeID, TargetKey: "nocolon"})
		if err == nil || !strings.Contains(err.Error(), "is not an assignment key") || len(q.seen) != 0 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	for _, c := range []struct {
		name      string
		failOn    int
		wantSeen  int
		wantErrIn string
	}{
		{"the graph lock fails first and nothing follows", 1, 1, "lock the governance profile graph"},
		{"the key lock fails second and nothing follows", 2, 2, "lock the governance assignment key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := govCovServer(t, &govCovStore{})
			q := &govCovQuerier{execFn: govCovExecFailingOn(c.failOn, boom)}
			_, err := applyAssignmentChange(s, r, q, govCovAssignChange(t, s, "upsert", userReq))
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), c.wantErrIn) || len(q.seen) != c.wantSeen {
				t.Errorf("error = %v after %d statements, want %q after %d", err, len(q.seen), c.wantErrIn, c.wantSeen)
			}
		})
	}

	t.Run("a delete payload that does not decode is an error after the two locks", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		ch := govCovAssignChange(t, s, "delete", userReq)
		ch.Payload = json.RawMessage(`{"id":`)
		_, err := applyAssignmentChange(s, r, q, ch)
		if err == nil || !strings.Contains(err.Error(), "payload") || len(q.seen) != 2 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("a delete of a changed target is stale and deletes nothing", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		ch := govCovAssignChange(t, s, "delete", userReq)
		ch.BaseHash = `"something-else"`
		if _, err := applyAssignmentChange(s, r, q, ch); !errors.Is(err, store.ErrGovernanceChangeStale) || len(q.writes()) != 0 {
			t.Errorf("error = %v, writes %v", err, q.writes())
		}
	})

	t.Run("a delete reads the assignment and its profile chain under the row lock", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{execFn: govCovExecTag("DELETE 1")}
		got, err := applyAssignmentChange(s, r, q, govCovAssignChange(t, s, "delete", userReq))
		if err != nil {
			t.Fatal(err)
		}
		if got.action != "governance.assignment.delete" || got.target != govCovAssignID.String() || got.data != nil {
			t.Errorf("applied = %+v", got)
		}
		if w := q.writes(); len(w) != 1 || !strings.Contains(w[0], "DELETE FROM governance_assignments") {
			t.Errorf("writes = %v, want the one assignment delete", w)
		}
		if !strings.HasSuffix(strings.TrimSpace(q.seen[2]), "FOR UPDATE") {
			t.Errorf("the assignment was read without the row lock: %s", q.seen[2])
		}
	})

	t.Run("a delete that matches no row is the direct delete's 404", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{execFn: govCovExecTag("DELETE 0")}
		_, err := applyAssignmentChange(s, r, q, govCovAssignChange(t, s, "delete", userReq))
		if ref := govCovAsRefusal(t, err); ref.status != http.StatusNotFound || ref.reason != reasonGovernanceAssignmentNotFound {
			t.Errorf("refusal = %+v", ref)
		}
	})

	t.Run("a failing delete statement is an error, not a refusal", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{execFn: func(sql string) (pgconn.CommandTag, error) {
			if strings.HasPrefix(strings.TrimSpace(sql), "DELETE") {
				return pgconn.CommandTag{}, boom
			}
			return pgconn.NewCommandTag("SELECT 1"), nil
		}}
		_, err := applyAssignmentChange(s, r, q, govCovAssignChange(t, s, "delete", userReq))
		var ref *profileWriteError
		if !errors.Is(err, boom) || errors.As(err, &ref) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a failing assignment read is returned and nothing is written", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{rowErr: func(string) error { return boom }}
		_, err := applyAssignmentChange(s, r, q, govCovAssignChange(t, s, "delete", userReq))
		if !errors.Is(err, boom) || len(q.writes()) != 0 {
			t.Errorf("error = %v, writes %v", err, q.writes())
		}
	})
}

func TestGovCovApplyAssignmentUpsertRefusals(t *testing.T) {
	boom := errors.New("pg: down")
	userReq := governanceAssignmentRequest{SubjectType: types.CapabilitySubjectUser, Subject: "u@corp.example", ProfileID: govCovPID, Priority: 3}
	r := govCovHumanReq(http.MethodPost, "/x", "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin)
	fkViolation := &pgconn.PgError{Code: "23503"}

	for _, c := range []struct {
		name       string
		mutate     func(*types.GovernanceChange)
		st         *govCovStore
		rowErr     func(string) error
		check      func(t *testing.T, err error)
		wantWrites int
	}{
		{name: "a payload with an unknown field", mutate: func(ch *types.GovernanceChange) { ch.Payload = json.RawMessage(`{"subject_type":"user","extra":1}`) },
			check: func(t *testing.T, err error) {
				if ref := govCovAsRefusal(t, err); ref.status != http.StatusBadRequest || ref.reason != reasonGovernanceAssignmentInvalid || !strings.Contains(ref.msg, "invalid assignment") {
					t.Errorf("refusal = %+v", ref)
				}
			}},
		{name: "an assignment that no longer validates", mutate: func(ch *types.GovernanceChange) {
			ch.Payload = json.RawMessage(`{"subject_type":"bogus","subject":"x","profile_id":"` + govCovPID.String() + `"}`)
		}, check: func(t *testing.T, err error) {
			if ref := govCovAsRefusal(t, err); ref.status != http.StatusBadRequest || ref.reason != reasonGovernanceAssignmentInvalid || !strings.Contains(ref.msg, "subject_type") {
				t.Errorf("refusal = %+v", ref)
			}
		}},
		{name: "a user type deleted since the proposal", mutate: func(ch *types.GovernanceChange) {
			ch.Payload = json.RawMessage(`{"subject_type":"user_type","subject":"ghost","profile_id":"` + govCovPID.String() + `"}`)
			ch.TargetKey = "user_type:ghost"
		}, st: &govCovStore{getTypeErr: store.ErrNotFound}, check: func(t *testing.T, err error) {
			if ref := govCovAsRefusal(t, err); ref.status != http.StatusBadRequest || ref.reason != reasonAccessUnknownUserType || !strings.Contains(ref.msg, `"ghost"`) {
				t.Errorf("refusal = %+v", ref)
			}
		}},
		{name: "a user type lookup that fails", mutate: func(ch *types.GovernanceChange) {
			ch.Payload = json.RawMessage(`{"subject_type":"user_type","subject":"ghost","profile_id":"` + govCovPID.String() + `"}`)
			ch.TargetKey = "user_type:ghost"
		}, rowErr: func(sql string) error {
			if strings.Contains(sql, "FROM user_types") {
				return boom
			}
			return pgx.ErrNoRows
		}, check: func(t *testing.T, err error) {
			if !errors.Is(err, boom) {
				t.Errorf("error = %v, want the lookup failure", err)
			}
		}},
		{name: "a target that changed since the proposal is stale", mutate: func(ch *types.GovernanceChange) { ch.BaseHash = `"old"` },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, store.ErrGovernanceChangeStale) {
					t.Errorf("error = %v, want stale", err)
				}
			}},
		{name: "a profile deleted since the proposal is a 404", rowErr: func(sql string) error {
			if strings.Contains(sql, "INSERT INTO governance_assignments") {
				return fkViolation
			}
			return nil
		}, check: func(t *testing.T, err error) {
			if ref := govCovAsRefusal(t, err); ref.status != http.StatusNotFound || ref.reason != reasonGovernanceProfileNotFoundByID {
				t.Errorf("refusal = %+v", ref)
			}
		}, wantWrites: 1},
		{name: "a failing write is an error", rowErr: func(sql string) error {
			if strings.Contains(sql, "INSERT INTO governance_assignments") {
				return boom
			}
			return nil
		}, check: func(t *testing.T, err error) {
			var ref *profileWriteError
			if !errors.Is(err, boom) || errors.As(err, &ref) {
				t.Errorf("error = %v", err)
			}
		}, wantWrites: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := c.st
			if st == nil {
				st = &govCovStore{}
			}
			s, _ := govCovServer(t, st)
			ch := govCovAssignChange(t, s, "upsert", userReq)
			if c.mutate != nil {
				c.mutate(&ch)
			}
			q := &govCovQuerier{}
			if c.rowErr != nil {
				fn := c.rowErr
				q.rowErr = func(sql string) error {
					if err := fn(sql); err != nil {
						return err
					}
					return pgx.ErrNoRows
				}
			}
			_, err := applyAssignmentChange(s, r, q, ch)
			c.check(t, err)
			if len(q.writes()) != c.wantWrites {
				t.Errorf("writes = %v, want %d", q.writes(), c.wantWrites)
			}
		})
	}
}

func TestGovCovCheckUserTypeSubject(t *testing.T) {
	boom := errors.New("pg: down")
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	for _, c := range []struct {
		name        string
		subjectType types.CapabilitySubjectType
		subject     string
		err         error
		wantLookup  bool
		wantErr     func(error) bool
	}{
		{"a user subject is never looked up", types.CapabilitySubjectUser, "ghost", store.ErrNotFound, false, func(e error) bool { return e == nil }},
		{"the standard type always exists", types.CapabilitySubjectUserType, types.UserTypeStandard, store.ErrNotFound, false, func(e error) bool { return e == nil }},
		{"a known custom type", types.CapabilitySubjectUserType, "contractor", nil, true, func(e error) bool { return e == nil }},
		{"an unknown custom type is a 400", types.CapabilitySubjectUserType, "ghost", store.ErrNotFound, true, func(e error) bool {
			var ref *profileWriteError
			return errors.As(e, &ref) && ref.status == http.StatusBadRequest && ref.reason == reasonAccessUnknownUserType && ref.msg == accessUnknownUserType("ghost")
		}},
		{"a failing lookup is returned as is", types.CapabilitySubjectUserType, "ghost", boom, true, func(e error) bool { return errors.Is(e, boom) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			looked := 0
			q := &govCovQuerier{rowErr: func(string) error {
				looked++
				if errors.Is(c.err, store.ErrNotFound) {
					return pgx.ErrNoRows
				}
				return c.err
			}}
			s, _ := govCovServer(t, &govCovStore{})
			err := s.checkUserTypeSubject(ctx, q, c.subjectType, c.subject)
			if !c.wantErr(err) {
				t.Errorf("error = %v", err)
			}
			if (looked > 0) != c.wantLookup {
				t.Errorf("lookups = %d, want lookup %v", looked, c.wantLookup)
			}
		})
	}
}

func TestGovCovHoldAssignment(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)
	saved := types.GovernanceChange{ID: govCovChangeID}
	body := `{"subject_type":"user","subject":"u@corp.example","profile_id":"` + govCovPID.String() + `","priority":3}`
	fk := &pgconn.PgError{Code: "23503"}

	t.Run("an upsert is held as a proposal on the natural key", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPost, "/api/v1/governance/assignments", super, body)
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindAssignment || ch.Op != "upsert" || ch.TargetKey != "user:u@corp.example" {
			t.Errorf("stored change = %+v", ch)
		}
		var payload governanceAssignmentRequest
		if err := json.Unmarshal(ch.Payload, &payload); err != nil || payload.ProfileID != govCovPID || payload.Priority != 3 || payload.Subject != "u@corp.example" {
			t.Errorf("payload = %s (%v)", ch.Payload, err)
		}
		d := govCovDiff(t, ch)
		if d.Before != nil || d.After == nil || !reflect.DeepEqual(d.Changed, []string{"priority", "profile_id", "subject", "subject_type"}) {
			t.Errorf("diff = %s", ch.Diff)
		}
	})

	t.Run("the dry run reads the assignment, then its profile, then tries the write", func(t *testing.T) {
		q := &govCovQuerier{rowErr: func(sql string) error {
			if strings.Contains(sql, "INSERT") {
				return fk
			}
			return pgx.ErrNoRows
		}}
		st := &govCovStore{proposeSaved: saved, dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPost, "/api/v1/governance/assignments", super, body)
		if w.Code != http.StatusNotFound || errorReason(w) != reasonGovernanceProfileNotFoundByID {
			t.Errorf("an unknown profile = %d %q, want the direct write's 404", w.Code, errorReason(w))
		}
		if len(q.seen) != 3 || !strings.Contains(q.seen[0], "governance_assignments") || !strings.Contains(q.seen[1], "governance_profiles") || !strings.Contains(q.seen[2], "INSERT") {
			t.Errorf("statements = %v", q.seen)
		}
		if len(st.proposed) != 0 {
			t.Errorf("a refused upsert was proposed")
		}
	})

	t.Run("a failing dry run is a 500", func(t *testing.T) {
		st := &govCovStore{dryRunFn: func(func(store.Querier) error) error { return errors.New("pg: down") }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPost, "/api/v1/governance/assignments", super, body)
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 {
			t.Errorf("= %d, proposals %d", w.Code, len(st.proposed))
		}
	})

	deletePath := "/api/v1/governance/assignments/" + govCovAssignID.String()
	t.Run("a delete of an unknown assignment is the direct delete's 404", func(t *testing.T) {
		q := &govCovQuerier{}
		st := &govCovStore{dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, deletePath, super, "")
		if w.Code != http.StatusNotFound || errorReason(w) != reasonGovernanceAssignmentNotFound || len(q.seen) != 1 || len(st.proposed) != 0 {
			t.Errorf("= %d %q after %v, proposals %d", w.Code, errorReason(w), q.seen, len(st.proposed))
		}
	})

	t.Run("a delete whose read fails is a 500 that does not name the assignment", func(t *testing.T) {
		st := &govCovStore{dryRunFn: func(func(store.Querier) error) error { return errors.New("pg: down") }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, deletePath, super, "")
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 {
			t.Errorf("= %d, proposals %d", w.Code, len(st.proposed))
		}
		if strings.Contains(w.Body.String(), govCovAssignID.String()) || strings.Contains(w.Body.String(), "pg: down") {
			t.Errorf("the 500 names the assignment or the driver error: %s", w.Body.String())
		}
	})

	t.Run("a dry run that returns no assignment and no error is a 500, never a proposal", func(t *testing.T) {
		st := &govCovStore{}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, deletePath, super, "")
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 {
			t.Errorf("= %d, proposals %d; a delete with nothing read must never be stored", w.Code, len(st.proposed))
		}
	})
}
