// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// poolReviveStore is reviveStore plus the two reads the owner's pool re-check makes. The embedded pool
// and remote-runner stores are nil: any other method of those seams being called is a test failure
// worth a panic.
type poolReviveStore struct {
	*reviveStore
	store.RunnerPoolStore
	store.RemoteRunnerStore
	rows    []store.RunnerPoolCatalogueRow
	runners map[string]types.RemoteRunner
}

func (s *poolReviveStore) ListRunnerPoolCatalogue(context.Context, string, bool) ([]store.RunnerPoolCatalogueRow, error) {
	return s.rows, nil
}

func (s *poolReviveStore) GetRemoteRunner(_ context.Context, id string) (store.RemoteRunnerRow, error) {
	rr, ok := s.runners[id]
	if !ok {
		return store.RemoteRunnerRow{}, store.ErrNotFound
	}
	return store.RemoteRunnerRow{Runner: rr}, nil
}

// reviveInPool puts the fixture's run in a pool the owner's catalogue holds (nil: the pool is gone from
// it), on remote runner executor when given.
func (f *reviveFixture) reviveInPool(row *store.RunnerPoolCatalogueRow, executor string, runners map[string]types.RemoteRunner) {
	ps := &poolReviveStore{reviveStore: f.rs, runners: runners}
	pool := uuid.New()
	if row != nil {
		row.Pool.ID = pool
		ps.rows = []store.RunnerPoolCatalogueRow{*row}
	}
	f.srv.cfg.Store = ps
	f.st.run.UserType = types.UserTypeStandard
	f.st.run.RunnerPoolID, f.st.run.RunnerPoolExecutor = &pool, executor
}

// reviveAsOwnerInGroups is the revive by the run's owner, signed in with a group snapshot.
func (f *reviveFixture) reviveAsOwnerInGroups(t *testing.T, groups ...string) (int, string) {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: f.run.CreatedBy, Email: ownerEmail, Role: oidc.RoleUser,
		UserType: types.UserTypeStandard, Groups: groups, Expiry: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", signedSessionCookie(payload), "")
	return w.Code, w.Body.String()
}

// TestRevive_RechecksThePoolUsePolicyForTheOwner: a person a pool's use policy no longer names cannot
// revive a run on it, as they could not launch there. The owner's own session is judged by their groups;
// anyone else's revive by what the run records of the owner, so a group row counts as possibly matching
// and a user row only for the owner's sub.
func TestRevive_RechecksThePoolUsePolicyForTheOwner(t *testing.T) {
	group := func(g string) types.RunnerPoolSubject {
		return types.RunnerPoolSubject{SubjectType: types.CapabilitySubjectGroup, Subject: g}
	}
	user := func(u string) types.RunnerPoolSubject {
		return types.RunnerPoolSubject{SubjectType: types.CapabilitySubjectUser, Subject: u}
	}
	for _, tc := range []struct {
		name    string
		policy  []types.RunnerPoolSubject // nil: no use policy
		gone    bool                      // the pool is no longer in the owner's catalogue
		asOwner bool
		groups  []string
		refused bool
	}{
		{name: "owner still in a named group", policy: []types.RunnerPoolSubject{group("sre")}, asOwner: true, groups: []string{"sre"}},
		{name: "owner left the named group", policy: []types.RunnerPoolSubject{group("sre")}, asOwner: true, groups: []string{"design"}, refused: true},
		{name: "no use policy", asOwner: true, groups: []string{"design"}},
		{name: "pool gone from the catalogue", gone: true, asOwner: true, groups: []string{"sre"}, refused: true},
		{name: "admin, a group row could match the owner", policy: []types.RunnerPoolSubject{group("sre")}},
		{name: "admin, a user row for another person", policy: []types.RunnerPoolSubject{user("someone.else")}, refused: true},
		{name: "admin, no use policy"},
		{name: "admin, pool gone from the catalogue", gone: true, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			var row *store.RunnerPoolCatalogueRow
			if !tc.gone {
				row = &store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Name: "SRE", Scope: types.RunnerPoolOrganization}}
				if tc.policy != nil {
					row.Policy = &types.RunnerPoolUsePolicy{Subjects: tc.policy}
				}
			}
			f.reviveInPool(row, "", nil)
			var code int
			var body string
			if tc.asOwner {
				code, body = f.reviveAsOwnerInGroups(t, tc.groups...)
			} else {
				code, body = f.reviveAs(t, false)
			}
			if !tc.refused {
				if code != http.StatusOK {
					t.Fatalf("revive = %d %s, want 200", code, body)
				}
				return
			}
			if code != runnerpool.ReasonNotFound.Status() || !strings.Contains(body, runnerpool.NotFoundMsg()) {
				t.Fatalf("revive = %d %s; want %d with %q", code, body, runnerpool.ReasonNotFound.Status(), runnerpool.NotFoundMsg())
			}
			f.assertReviveRefused(t, string(runnerpool.ReasonNotFound))
		})
	}
}

// TestRevive_RechecksTheRemoteRunnerPeoplePolicyForTheOwner: a run on a remote-hosted runner in the
// owner's own pool is revived only while the runner's people policy still admits the owner.
func TestRevive_RechecksTheRemoteRunnerPeoplePolicyForTheOwner(t *testing.T) {
	const rid = "laptop-farm"
	for _, tc := range []struct {
		name    string
		runner  *types.RemoteRunner // nil: the runner is gone
		asOwner bool
		groups  []string
		refused bool
	}{
		{name: "everyone", runner: &types.RemoteRunner{ID: rid, PeoplePools: types.PeoplePoolsEveryone}, asOwner: true, groups: []string{"design"}},
		{name: "policy switched off", runner: &types.RemoteRunner{ID: rid, PeoplePools: types.PeoplePoolsOff}, asOwner: true, groups: []string{"design"}, refused: true},
		{name: "listed group, owner still in it", runner: &types.RemoteRunner{ID: rid, PeoplePools: types.PeoplePoolsListed,
			PeopleSubjects: []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectGroup, Subject: "sre"}}}, asOwner: true, groups: []string{"sre"}},
		{name: "listed group, owner left it", runner: &types.RemoteRunner{ID: rid, PeoplePools: types.PeoplePoolsListed,
			PeopleSubjects: []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectGroup, Subject: "sre"}}}, asOwner: true, groups: []string{"design"}, refused: true},
		{name: "runner gone", asOwner: true, groups: []string{"design"}, refused: true},
		{name: "admin, policy switched off", runner: &types.RemoteRunner{ID: rid, PeoplePools: types.PeoplePoolsOff}, refused: true},
		{name: "admin, listed group could match the owner", runner: &types.RemoteRunner{ID: rid, PeoplePools: types.PeoplePoolsListed,
			PeopleSubjects: []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectGroup, Subject: "sre"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			runners := map[string]types.RemoteRunner{}
			if tc.runner != nil {
				runners[rid] = *tc.runner
			}
			f.reviveInPool(&store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Name: "Mine", Scope: types.RunnerPoolUser, Owner: f.run.CreatedBy}}, rid, runners)
			var code int
			var body string
			if tc.asOwner {
				code, body = f.reviveAsOwnerInGroups(t, tc.groups...)
			} else {
				code, body = f.reviveAs(t, false)
			}
			if !tc.refused {
				if code != http.StatusOK {
					t.Fatalf("revive = %d %s, want 200", code, body)
				}
				return
			}
			if code != runnerpool.ReasonMemberMismatch.Status() || !strings.Contains(body, runnerpool.RemoteNotAvailableMsg(rid)) {
				t.Fatalf("revive = %d %s; want %d with %q", code, body, runnerpool.ReasonMemberMismatch.Status(), runnerpool.RemoteNotAvailableMsg(rid))
			}
			f.assertReviveRefused(t, string(runnerpool.ReasonMemberMismatch))
		})
	}
}
