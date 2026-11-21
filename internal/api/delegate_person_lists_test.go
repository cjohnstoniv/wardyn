// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errPersonListWrite is what the fake querier answers every statement but the binding read with: a write that
// gets it got past the ruling.
var errPersonListWrite = errors.New("the write ran past the ruling")

// personListQuerier stands in for the write's transaction. The ruling's binding read finds a binding when bound
// and none otherwise; any other statement counts as the write and fails with errPersonListWrite.
type personListQuerier struct {
	bound          bool
	bindings, past int
}

type personListScan func(dest ...any) error

func (f personListScan) Scan(dest ...any) error { return f(dest...) }

func (q *personListQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	q.past++
	return pgconn.CommandTag{}, errPersonListWrite
}

func (q *personListQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.past++
	return nil, errPersonListWrite
}

func (q *personListQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "FROM portal_issuers WHERE issuer_key = $1 FOR KEY SHARE") {
		q.bindings++
		return personListScan(func(dest ...any) error {
			if !q.bound {
				return pgx.ErrNoRows
			}
			*dest[0].(*int) = 1
			return nil
		})
	}
	q.past++
	return personListScan(func(...any) error { return errPersonListWrite })
}

// personListRow is one row of the table of admin-written person lists: the plan's row number, what the list
// is, whether it must refuse a portal person (refuse) or may name one whose issuer is bound (allow), and the
// list's real store write naming subject.
type personListRow struct {
	n      int
	list   string
	refuse bool
	write  func(ctx context.Context, q *personListQuerier, subject string) error
}

// personListRows are the rows whose write takes a querier. Rows 4 (drive grant), 5 (UI-app callers) and 9
// (template group admin) are written through the store's pool and rows 12-14 are refused at their own doors;
// the PG test covers them.
var personListRows = []personListRow{
	{1, "capability grant, user subject", false, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.UpsertCapabilityGrantQ(ctx, q, types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser, Subject: s})
		return err
	}},
	{2, "governance membership, user subject", false, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.UpsertGovernanceMembershipQ(ctx, q, types.GovernanceMembership{SubjectType: types.CapabilitySubjectUser, Subject: s, UnitID: uuid.New()})
		return err
	}},
	{3, "restriction members", false, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.PutRestrictionQ(ctx, q, types.Restriction{Members: []types.RestrictionMember{{SubjectType: types.CapabilitySubjectUser, Subject: s}}}, "t")
		return err
	}},
	{6, "runner pool use-policy subjects", false, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.PutRunnerPoolUsePolicyQ(ctx, q, uuid.New(), nil, []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectUser, Subject: s}}, "t")
		return err
	}},
	{7, "remote runner people subjects", true, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.SetRemoteRunnerPeopleQ(ctx, q, "runner", nil, types.PeoplePoolsListed, []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectUser, Subject: s}}, "t")
		return err
	}},
	{8, "unit admin grant", true, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.GrantUnitAdminQ(ctx, q, types.GovernanceUnitAdmin{UnitID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: s})
		return err
	}},
	{10, "role mapping value", true, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.UpsertRoleMappingQ(ctx, q, types.RoleMapping{Value: s, Role: "admin"})
		return err
	}},
	{11, "service identity sponsor", true, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := store.CreateServiceIdentityQ(ctx, q, types.ServiceIdentity{Name: "svc", Sponsor: s})
		return err
	}},
	{16, "key-domain assignment, user subject", true, func(ctx context.Context, q *personListQuerier, s string) error {
		_, err := keydomain.NewService(nil, nil).SetQ(ctx, q, keydomain.Assignment{SubjectType: "user", Subject: s, Domain: keydomain.Default})
		return err
	}},
}

// A person list's ruling is the store write's, and its answer is the API's. Each row runs its real write on a
// fake transaction: a refuse row stops a portal person before any statement, bound or not; an allow row reads
// the issuer's binding and stops at an unbound one, and a bound one reaches the write; a subject that is not a
// portal person reaches the write on every row. The refusal, wrapped as a caller wraps it, answers 422 with
// the list's reason as a direct write and as a held decision, which writes one failure row. The prefix-only
// half of the ruling folds case first, as the user-grant path does.
func TestAdminPersonLists_DelegatePersonRuling(t *testing.T) {
	key := types.PortalIssuerKey("https://idp.example.com")
	person := types.PortalPrincipal(key, "00u1abc")
	for _, subject := range []string{person, "DELEGATE:PERSON:" + key + ":ABC", "  " + person} {
		if err := types.RefuseDelegatePersonSubject("user", subject); err == nil {
			t.Errorf("the prefix-only ruling let %q through a refuse row", subject)
		}
	}
	for _, c := range []struct{ typ, subject string }{{"group", person}, {"user", "alice@corp.example"}, {"user", "delegate:abc"}, {"user", "sub-123"}} {
		if err := types.RefuseDelegatePersonSubject(c.typ, c.subject); err != nil {
			t.Errorf("RefuseDelegatePersonSubject(%q, %q) = %v, want nil", c.typ, c.subject, err)
		}
	}

	ctx := context.Background()
	h := newHarness(t)
	for _, row := range personListRows {
		t.Run(fmt.Sprintf("row %d %s", row.n, row.list), func(t *testing.T) {
			run := func(bound bool, subject string) (*personListQuerier, error) {
				q := &personListQuerier{bound: bound}
				return q, row.write(ctx, q, subject)
			}
			// A subject that is not a portal person is not ruled.
			if q, err := run(false, "alice@corp.example"); !errors.Is(err, errPersonListWrite) || q.bindings != 0 || q.past == 0 {
				t.Errorf("a workforce subject = %v (binding reads %d, statements %d), want the write reached with no binding read", err, q.bindings, q.past)
			}
			var refusal error
			wantReason := reasonDelegatePersonUnknownPortal
			if row.refuse {
				wantReason = reasonDelegatePersonNotAllowed
				for _, bound := range []bool{false, true} {
					q, err := run(bound, person)
					if !errors.Is(err, store.ErrDelegatePersonNotAllowed) || q.bindings != 0 || q.past != 0 {
						t.Errorf("bound=%v: %v (binding reads %d, statements %d), want %v before any statement", bound, err, q.bindings, q.past, store.ErrDelegatePersonNotAllowed)
					}
					refusal = err
				}
			} else {
				q, err := run(false, person)
				if !errors.Is(err, store.ErrDelegatePersonUnknownPortal) || q.bindings != 1 || q.past != 0 {
					t.Errorf("unbound: %v (binding reads %d, statements %d), want %v after one binding read", err, q.bindings, q.past, store.ErrDelegatePersonUnknownPortal)
				}
				refusal = err
				if q, err := run(true, person); !errors.Is(err, errPersonListWrite) || q.bindings != 1 || q.past == 0 {
					t.Errorf("bound: %v (binding reads %d, statements %d), want the write reached after one binding read", err, q.bindings, q.past)
				}
			}
			if refusal == nil {
				t.Fatal("the row refused nothing")
			}
			// The write's own refusal, wrapped as a caller wraps it, reaches the answer as 422.
			r := httptest.NewRequest(http.MethodPost, "/api/v1/x", nil)
			w := httptest.NewRecorder()
			writeServerError(w, r, "write person list", fmt.Errorf("write row %d: %w", row.n, refusal))
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != wantReason {
				t.Errorf("write = %d %s, want 422 %s", w.Code, w.Body.String(), wantReason)
			}
			// The decision of a held change that carries it: 422 and one failure row.
			head := types.GovernanceChange{TargetKind: "person_list", TargetKey: fmt.Sprintf("row-%d", row.n)}
			w = httptest.NewRecorder()
			h.srv.answerFailedDecision(w, r, head, true, false, types.ActorHuman, "approver", fmt.Errorf("apply: %w", refusal))
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != wantReason {
				t.Errorf("held decision = %d %s, want 422 %s", w.Code, w.Body.String(), wantReason)
			}
		})
	}
	if got := h.auditOutcomes("governance.change.approve"); len(got) != len(personListRows) {
		t.Errorf("approve rows = %v, want one failure per row", got)
	} else {
		for _, o := range got {
			if o != "failure" {
				t.Errorf("approve outcome = %q, want failure", o)
			}
		}
	}
}

// A person list that names a portal person whose issuer no portal is registered for is refused 422
// delegate_person_unknown_portal, and the refusal is not a server fault: nothing is logged as a 500.
func TestAdminPersonLists_UnknownPortalKeyRefused(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/v1/x", nil)
	w := httptest.NewRecorder()
	writeServerError(w, r, "write person list", fmt.Errorf("write: %w", store.ErrDelegatePersonUnknownPortal))
	if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonDelegatePersonUnknownPortal {
		t.Fatalf("= %d %s, want 422 %s", w.Code, w.Body.String(), reasonDelegatePersonUnknownPortal)
	}
	// The two rulings are different answers: a role a portal person never holds, and an unregistered issuer.
	w = httptest.NewRecorder()
	writeServerError(w, r, "write person list", store.ErrDelegatePersonNotAllowed)
	if reasonOf(t, w) == reasonDelegatePersonUnknownPortal || w.Code != http.StatusUnprocessableEntity {
		t.Errorf("not-allowed = %d %s", w.Code, w.Body.String())
	}
	// Anything else is still a server fault.
	w = httptest.NewRecorder()
	writeServerError(w, r, "write person list", fmt.Errorf("db down"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("a store failure = %d, want 500", w.Code)
	}
}
