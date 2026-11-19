// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// poolUnitSubject is a use-policy subject naming a unit.
func poolUnitSubject(id uuid.UUID) types.RunnerPoolSubject {
	return types.RunnerPoolSubject{SubjectType: types.CapabilitySubjectType("unit"), Subject: id.String()}
}

var errPoolProbe = errors.New("pool found")

// unitPoolStore serves one organization pool's catalogue row beside the governance fake. Reading the
// organization's defaults fails with errPoolProbe when probe is set: the queue's room check reaches
// that read only for a pool the owner may use, so the error says "permitted".
type unitPoolStore struct {
	*capStore
	store.RunnerPoolStore
	rows  []store.RunnerPoolCatalogueRow
	probe bool
}

func (s *unitPoolStore) ListRunnerPoolCatalogue(context.Context, string, bool) ([]store.RunnerPoolCatalogueRow, error) {
	return slices.Clone(s.rows), nil
}

func (s *unitPoolStore) GetRunnerPoolOrgDefaults(context.Context) (types.RunnerPoolDefaults, error) {
	if s.probe {
		return types.RunnerPoolDefaults{}, errPoolProbe
	}
	return types.RunnerPoolDefaults{}, nil
}

func (s *unitPoolStore) GetPrincipalPref(context.Context, string, string) (json.RawMessage, error) {
	return nil, store.ErrNotFound
}

// unitPools is a unit tree (platform > team > desk, and an unrelated other) and a pool whose use
// policy a case sets.
type unitPools struct {
	srv                         *Server
	st                          *unitPoolStore
	platform, team, desk, other types.GovernanceUnit
	pool                        uuid.UUID
}

const unitPoolMember = "sub-gov-bob"

func newUnitPools(t *testing.T) *unitPools {
	t.Helper()
	f := &unitPools{pool: uuid.New()}
	f.platform = types.GovernanceUnit{ID: uuid.New(), Name: "platform", Ceiling: govProfileSpec()}
	f.team = types.GovernanceUnit{ID: uuid.New(), Name: "team", ParentID: &f.platform.ID, Overlay: &types.CeilingOverlay{}}
	f.desk = types.GovernanceUnit{ID: uuid.New(), Name: "desk", ParentID: &f.team.ID, Overlay: &types.CeilingOverlay{}}
	f.other = types.GovernanceUnit{ID: uuid.New(), Name: "other", Ceiling: govProfileSpec()}
	f.st = &unitPoolStore{capStore: &capStore{govTier: types.CapabilitySubjectUser,
		govGraph: []types.GovernanceUnit{f.platform, f.team, f.desk, f.other}}}
	f.srv = &Server{cfg: Config{DefaultPolicy: govDeployment(), Store: f.st}}
	return f
}

// memberOf puts the member in u now (nil: in no unit) and sets the pool's use policy.
func (f *unitPools) memberOf(u *types.GovernanceUnit, policy ...types.RunnerPoolSubject) {
	f.st.govProfile = u
	f.st.rows = []store.RunnerPoolCatalogueRow{{
		Pool:   types.RunnerPool{ID: f.pool, Name: "Shared", Scope: types.RunnerPoolOrganization, State: types.RunnerPoolActive},
		Policy: &types.RunnerPoolUsePolicy{PoolID: f.pool, Subjects: policy},
	}}
}

// admits is, for every place a pool is judged for a person, whether the pool is open to the member
// whose run is frozen in unit (nil: none).
func (f *unitPools) admits(t *testing.T, frozen *uuid.UUID) map[string]bool {
	t.Helper()
	ctx := govMemberCtx([]string{"eng"}, false)
	out := map[string]bool{}

	_, _, _, refusal := f.srv.resolveRunPool(ctx, f.st, f.pool.String())
	if refusal != nil && refusal.err != nil {
		t.Fatalf("create: %v", refusal.err)
	}
	out["create"] = refusal == nil

	rows, refusal := f.srv.permittedPools(ctx, f.st, true)
	if refusal != nil {
		t.Fatalf("catalogue: %v", refusal.err)
	}
	_, out["catalogue"] = catalogueRow(rows, f.pool)

	_, out["drives"] = f.srv.catalogueDrivePool(ctx, f.pool.String())

	f.st.probe = true
	_, err := f.srv.queueRunnerFree(ctx, types.RunnerPool{ID: f.pool}, queuedLaunch{UnitID: frozen})
	f.st.probe = false
	if err != nil && !errors.Is(err, errPoolProbe) {
		t.Fatalf("queue: %v", err)
	}
	out["queue"] = errors.Is(err, errPoolProbe)

	run := types.AgentRun{ID: uuid.New(), CreatedBy: unitPoolMember, UserType: types.UserTypeStandard, RunnerPoolID: &f.pool, UnitID: frozen}
	ref, err := f.srv.ownerPoolRefusal(ctx, run)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	out["revive"] = ref == nil
	return out
}

func (f *unitPools) expect(t *testing.T, got map[string]bool, want bool) {
	t.Helper()
	for _, site := range []string{"create", "catalogue", "drives", "queue", "revive"} {
		if got[site] != want {
			t.Errorf("%s admits = %v, want %v", site, got[site], want)
		}
	}
}

// TestPoolCatalogueAndAdmissionAgreeUnit: a use policy naming a unit gives one answer at the run's
// create, the catalogue, the drives catalogue, the queue's room check and a revive. The unit is the
// run's own or one above it; a unit below it, a sibling, an unknown one and a person in no unit all
// refuse.
func TestPoolCatalogueAndAdmissionAgreeUnit(t *testing.T) {
	f := newUnitPools(t)
	for _, tc := range []struct {
		name   string
		member func() *types.GovernanceUnit // nil result: in no unit
		names  func() uuid.UUID
		want   bool
	}{
		{"the run's own unit", func() *types.GovernanceUnit { return &f.team }, func() uuid.UUID { return f.team.ID }, true},
		{"the unit above", func() *types.GovernanceUnit { return &f.team }, func() uuid.UUID { return f.platform.ID }, true},
		{"two units above", func() *types.GovernanceUnit { return &f.desk }, func() uuid.UUID { return f.platform.ID }, true},
		{"a unit below", func() *types.GovernanceUnit { return &f.team }, func() uuid.UUID { return f.desk.ID }, false},
		{"an unrelated unit", func() *types.GovernanceUnit { return &f.team }, func() uuid.UUID { return f.other.ID }, false},
		{"an unknown unit", func() *types.GovernanceUnit { return &f.team }, uuid.New, false},
		{"no unit", func() *types.GovernanceUnit { return nil }, func() uuid.UUID { return f.platform.ID }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.member()
			f.memberOf(u, poolUnitSubject(tc.names()))
			var frozen *uuid.UUID
			if u != nil {
				frozen = &u.ID
			}
			f.expect(t, f.admits(t, frozen), tc.want)
		})
	}
	t.Run("a person's own row still admits beside a unit row", func(t *testing.T) {
		f.memberOf(&f.team, poolUnitSubject(f.other.ID),
			types.RunnerPoolSubject{SubjectType: types.CapabilitySubjectUser, Subject: types.CanonicalUserSubject(unitPoolMember)})
		f.expect(t, f.admits(t, &f.team.ID), true)
	})
}

// A member of another unit is refused: the pool names the team, and the member is in the unrelated unit.
func TestPoolUnitRefusesMemberOfAnotherUnit(t *testing.T) {
	f := newUnitPools(t)
	f.memberOf(&f.other, poolUnitSubject(f.team.ID))
	f.expect(t, f.admits(t, &f.other.ID), false)
}

// A queued start is judged by the unit its run froze, not the one its owner is in now.
func TestPoolUnitQueuedStartUsesFrozenUnit(t *testing.T) {
	f := newUnitPools(t)
	ctx := govMemberCtx([]string{"eng"}, false)
	queued := func(frozen *uuid.UUID) bool {
		f.st.probe = true
		defer func() { f.st.probe = false }()
		_, err := f.srv.queueRunnerFree(ctx, types.RunnerPool{ID: f.pool}, queuedLaunch{UnitID: frozen})
		if err != nil && !errors.Is(err, errPoolProbe) {
			t.Fatalf("queue: %v", err)
		}
		return errors.Is(err, errPoolProbe)
	}
	f.memberOf(&f.other, poolUnitSubject(f.team.ID)) // the owner has since moved to the other unit
	if !queued(&f.team.ID) {
		t.Error("a run frozen in the team was refused because its owner is in another unit now")
	}
	if queued(&f.other.ID) {
		t.Error("a run frozen in another unit was admitted")
	}
	f.memberOf(&f.team, poolUnitSubject(f.team.ID)) // the owner is in the team now; the run froze none
	if queued(nil) {
		t.Error("a run frozen in no unit was admitted because its owner is in the team now")
	}
}

// A revived run is judged by the unit it froze: the admin token reviving it has no unit of its own.
func TestPoolUnitReviveUsesFrozenUnit(t *testing.T) {
	f := newUnitPools(t)
	f.memberOf(nil, poolUnitSubject(f.team.ID))
	revive := func(frozen *uuid.UUID) bool {
		run := types.AgentRun{ID: uuid.New(), CreatedBy: unitPoolMember, UserType: types.UserTypeStandard, RunnerPoolID: &f.pool, UnitID: frozen}
		ref, err := f.srv.ownerPoolRefusal(context.Background(), run)
		if err != nil {
			t.Fatalf("revive: %v", err)
		}
		return ref == nil
	}
	if !revive(&f.desk.ID) {
		t.Error("a run frozen in a unit below the team was refused")
	}
	if !revive(&f.team.ID) {
		t.Error("a run frozen in the team was refused")
	}
	if revive(&f.other.ID) {
		t.Error("a run frozen in another unit was admitted")
	}
	if revive(nil) {
		t.Error("a run frozen in no unit was admitted")
	}
}

// The list reads the subjects the other places do (poolSubjects), so a row it marks usable is one
// the create, the catalogue and the drives catalogue admit.
func TestPoolListSubjectsAgreeUnit(t *testing.T) {
	f := newUnitPools(t)
	for _, tc := range []struct {
		name  string
		names func() uuid.UUID
		want  bool
	}{
		{"the unit above", func() uuid.UUID { return f.platform.ID }, true},
		{"a unit below", func() uuid.UUID { return f.desk.ID }, false},
		{"an unrelated unit", func() uuid.UUID { return f.other.ID }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.memberOf(&f.team, poolUnitSubject(tc.names()))
			ctx := govMemberCtx([]string{"eng"}, false)
			subj, err := f.srv.poolSubjects(ctx, f.st.rows)
			if err != nil {
				t.Fatal(err)
			}
			if got := subj.permits(f.st.rows[0], poolOwner(ctx)); got != tc.want {
				t.Errorf("list marks the pool usable = %v, want %v", got, tc.want)
			}
			rows, refusal := f.srv.permittedPools(ctx, f.st, true)
			if refusal != nil {
				t.Fatal(refusal.err)
			}
			if _, usable := catalogueRow(rows, f.pool); usable != tc.want {
				t.Errorf("catalogue admits = %v, want %v", usable, tc.want)
			}
		})
	}
}
