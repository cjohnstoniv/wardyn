// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// withdrawnStore is the capability rows (capStore), the organisation's components (fakeComponentStore)
// and the sign-in records the live doors' component re-check reads: records by principal for a sign-in
// to Wardyn itself, viaPortal by principal and portal id for a portal's token exchange.
type withdrawnStore struct {
	*capStore
	fakeComponentStore
	records     map[string]store.PrincipalGroups
	viaPortal   map[[2]string]store.PrincipalGroups
	recordErr   error
	recordReads int
}

func (s *withdrawnStore) GetPrincipalGroups(_ context.Context, principal, portal string) (store.PrincipalGroups, error) {
	s.recordReads++
	if s.recordErr != nil {
		return store.PrincipalGroups{}, s.recordErr
	}
	rec, ok := s.records[principal]
	if portal != "" {
		rec, ok = s.viaPortal[[2]string{principal, portal}]
	}
	if !ok {
		return store.PrincipalGroups{}, store.ErrNotFound
	}
	return rec, nil
}

func (s *withdrawnStore) ListRoleMappings(context.Context) ([]types.RoleMapping, error) {
	return nil, nil
}

func (s *withdrawnStore) ListUserTypes(context.Context) ([]types.UserType, error) {
	return s.userTypes, nil
}

// withdrawnFixture is a server over a withdrawnStore and a run of capSub's that carries one
// organisation component, restricted ("Available to") as its create leaves it, so only an allow
// naming it admits anyone.
func withdrawnFixture(t *testing.T) (*Server, *withdrawnStore, types.AgentRun, []types.RunComponent) {
	t.Helper()
	st := &withdrawnStore{capStore: &capStore{}, records: map[string]store.PrincipalGroups{}, viaPortal: map[[2]string]store.PrincipalGroups{}}
	id := st.seedComponent("")
	st.capStore.restricted = map[string]map[string]bool{capComponent: {id.String(): true}}
	return capServer(st), st, types.AgentRun{ID: uuid.New(), CreatedBy: capSub},
		[]types.RunComponent{{ComponentID: &id, Name: "org-tool"}}
}

func componentRow(st types.CapabilitySubjectType, subject string, c types.RunComponent, effect types.CapabilityEffect) types.CapabilityGrant {
	return grant(st, subject, capComponent, c.ComponentID.String(), effect)
}

func wantWithdrawn(t *testing.T, srv *Server, run types.AgentRun, comps []types.RunComponent, step, want string) {
	t.Helper()
	got, err := srv.componentWithdrawn(t.Context(), run, comps)
	if err != nil || got != want {
		t.Fatalf("%s: componentWithdrawn = %q, %v; want %q", step, got, err, want)
	}
}

// A component the owner holds through a group stands while the record of their last sign-in names
// the group; once the grant is gone, the run is no longer given credentials. A run that carries no
// component reads no record.
func TestComponentWithdrawnSeesAGrantHeldThroughAGroup(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	wantWithdrawn(t, srv, run, nil, "no components", "")
	if st.recordReads != 0 {
		t.Fatalf("%d sign-in record reads for a run with no components, want none", st.recordReads)
	}
	st.grants = []types.CapabilityGrant{componentRow(types.CapabilitySubjectGroup, "eng", comps[0], types.CapabilityAllow)}
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}}
	wantWithdrawn(t, srv, run, comps, "held through the owner's group", "")
	st.grants = nil
	wantWithdrawn(t, srv, run, comps, "grant withdrawn", reasonOwnerCapabilityComponent)
}

// With no record the owner's groups and email are unknown, not empty: they are judged by sub and user
// type alone, as an admin's revive of their run is. A grant held through a group is not seen, a grant
// to the person still stands, and a deny of the component that a group or the email could carry
// refuses until a sign-in records them.
func TestComponentWithdrawnWithoutARecordJudgesTheOwnerBySubAlone(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	st.grants = []types.CapabilityGrant{componentRow(types.CapabilitySubjectGroup, "eng", comps[0], types.CapabilityAllow)}
	wantWithdrawn(t, srv, run, comps, "group grant, no record", reasonOwnerCapabilityComponent)
	st.grants = append(st.grants, componentRow(types.CapabilitySubjectUser, capSub, comps[0], types.CapabilityAllow))
	wantWithdrawn(t, srv, run, comps, "a grant to the person, no record", "")
	deny := componentRow(types.CapabilitySubjectGroup, "contractors", comps[0], types.CapabilityDeny)
	st.grants = append(st.grants, deny)
	wantWithdrawn(t, srv, run, comps, "a group deny, no record", reasonOwnerCapabilityComponent)
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}}
	wantWithdrawn(t, srv, run, comps, "a group deny the record rules out", "")

	st.grants[2] = componentRow(types.CapabilitySubjectUser, capEmail, comps[0], types.CapabilityDeny)
	wantWithdrawn(t, srv, run, comps, "a deny to the recorded email", reasonOwnerCapabilityComponent)
	delete(st.records, capSub)
	wantWithdrawn(t, srv, run, comps, "a deny to the email, no record", reasonOwnerCapabilityComponent)
}

// An owner the current role mappings make an admin, from the record of their last sign-in, keeps the run
// while admin runs stand outside governance; not when admin runs are governed, not for a portal's launch,
// and not once the mappings stop making them one. A partial group list still makes an admin of one a
// mapping matches in what it holds, never of one whose admin role is the default role.
func TestComponentWithdrawnSparesAnUngovernedAdminOwner(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	srv.cfg.OIDC = newAccessAuth(t, map[string]string{"wardyn-admins": oidc.RoleAdmin}, oidc.RoleUser, nil, nil)
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"wardyn-admins"}}
	wantWithdrawn(t, srv, run, comps, "admin owner", "")

	portal := run
	via := uuid.New()
	portal.CreatedVia = &via
	st.viaPortal[[2]string{capSub, via.String()}] = store.PrincipalGroups{Email: capEmail, Groups: []string{"wardyn-admins"}}
	wantWithdrawn(t, srv, portal, comps, "a portal's launch", reasonOwnerCapabilityComponent)

	srv.cfg.GovernAdminRuns = true
	wantWithdrawn(t, srv, run, comps, "admin runs governed", reasonOwnerCapabilityComponent)
	srv.cfg.GovernAdminRuns = false

	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"wardyn-admins"}, Truncated: true}
	wantWithdrawn(t, srv, run, comps, "partial groups, an admin mapping matched", "")

	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}}
	wantWithdrawn(t, srv, run, comps, "no longer an admin", reasonOwnerCapabilityComponent)

	// Admin by default, and a group the list may lack maps lower.
	srv.cfg.OIDC = newAccessAuth(t, map[string]string{"contractors": oidc.RoleUser}, oidc.RoleAdmin, nil, nil)
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}}
	wantWithdrawn(t, srv, run, comps, "admin by default, complete groups", "")
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}, Truncated: true}
	wantWithdrawn(t, srv, run, comps, "admin by default, partial groups", reasonOwnerCapabilityComponent)
	// The default role never spares an owner with no record: nothing says what they hold.
	delete(st.records, capSub)
	wantWithdrawn(t, srv, run, comps, "admin by default, no record", reasonOwnerCapabilityComponent)
}

// The owner's groups are read from the record of the door that launched the run: a portal's token
// exchange for a run it launched, their sign-in to Wardyn itself for any other. Neither door's record
// stands in for the other's.
func TestComponentWithdrawnReadsTheRecordOfTheLaunchingDoor(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	st.grants = []types.CapabilityGrant{componentRow(types.CapabilitySubjectGroup, "eng", comps[0], types.CapabilityAllow)}
	via := uuid.New()
	portal := run
	portal.CreatedVia = &via
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}}
	wantWithdrawn(t, srv, run, comps, "a sign-in's run, eng signed in", "")
	wantWithdrawn(t, srv, portal, comps, "a portal's run, no exchange recorded", reasonOwnerCapabilityComponent)
	st.viaPortal[[2]string{capSub, via.String()}] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}}
	wantWithdrawn(t, srv, portal, comps, "a portal's run, eng exchanged", "")
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"ops"}}
	wantWithdrawn(t, srv, run, comps, "a sign-in's run, eng only through the portal", reasonOwnerCapabilityComponent)
	wantWithdrawn(t, srv, portal, comps, "a portal's run, eng still exchanged", "")
}

// A service identity resolves with its complete, empty group snapshot and a device as itself: neither
// reads a sign-in record, and a group deny cannot reach a service.
func TestComponentWithdrawnServiceAndDeviceOwners(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	svc := run
	svc.CreatedBy = types.ServicePrincipalPrefix + "ci-bot"
	st.grants = []types.CapabilityGrant{
		componentRow(types.CapabilitySubjectUser, svc.CreatedBy, comps[0], types.CapabilityAllow),
		componentRow(types.CapabilitySubjectGroup, "contractors", comps[0], types.CapabilityDeny),
	}
	wantWithdrawn(t, srv, svc, comps, "service with a grant", "")
	st.grants = st.grants[1:]
	wantWithdrawn(t, srv, svc, comps, "service, grant withdrawn", reasonOwnerCapabilityComponent)

	dev := run
	dev.CreatedBy = "device:" + uuid.NewString()
	st.grants = []types.CapabilityGrant{componentRow(types.CapabilitySubjectAll, "", comps[0], types.CapabilityAllow)}
	wantWithdrawn(t, srv, dev, comps, "device, granted to everyone", "")
	st.grants = nil
	wantWithdrawn(t, srv, dev, comps, "device, grant withdrawn", reasonOwnerCapabilityComponent)
	if st.recordReads != 0 {
		t.Errorf("%d sign-in record reads for a service and a device, want none", st.recordReads)
	}
}

// A run that is not operator-owned and names no owner is no admin token's: its components stand only
// on grants to everyone.
func TestComponentWithdrawnRunWithNoOwnerIsNotTheOperator(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	run.CreatedBy = ""
	wantWithdrawn(t, srv, run, comps, "no owner, no grant", reasonOwnerCapabilityComponent)
	st.grants = []types.CapabilityGrant{componentRow(types.CapabilitySubjectAll, "", comps[0], types.CapabilityAllow)}
	wantWithdrawn(t, srv, run, comps, "no owner, granted to everyone", "")
}

// A deleted organisation component is gone for every run, an operator-owned one included; an
// operator-owned run is otherwise not asked.
func TestComponentWithdrawnDeletedComponentIsGoneForEveryRun(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	op := run
	op.OperatorOwned = true
	wantWithdrawn(t, srv, op, comps, "operator-owned, no grant", "")
	if st.recordReads != 0 {
		t.Errorf("%d sign-in record reads for an operator-owned run, want none", st.recordReads)
	}
	if _, err := st.DeleteComponent(t.Context(), *comps[0].ComponentID, ""); err != nil {
		t.Fatal(err)
	}
	wantWithdrawn(t, srv, op, comps, "operator-owned, component deleted", reasonOwnerComponentGone)
	wantWithdrawn(t, srv, run, comps, "component deleted", reasonOwnerComponentGone)
}

// A component the owner defined asks the custom_component feature, as a revive does.
func TestComponentWithdrawnOwnComponentAsksTheFeature(t *testing.T) {
	srv, st, run, _ := withdrawnFixture(t)
	own := []types.RunComponent{{SelfDefined: true}}
	wantWithdrawn(t, srv, run, own, "feature on by default", "")
	st.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capFeature, featureCustomComponent, types.CapabilityDeny)}
	wantWithdrawn(t, srv, run, own, "feature denied", reasonOwnerCapabilityComponent)
}

// A record or a row the store cannot read is an error for the door to answer as unread, never an
// answer; a user type deleted since launch withholds, as it refuses at every other door, whether or not
// the owner's groups are on record.
func TestComponentWithdrawnUnreadIsAnErrorAndAGoneTypeWithholds(t *testing.T) {
	srv, st, run, comps := withdrawnFixture(t)
	st.grants = []types.CapabilityGrant{componentRow(types.CapabilitySubjectUser, capSub, comps[0], types.CapabilityAllow)}
	boom := errors.New("store down")
	st.recordErr = boom
	if got, err := srv.componentWithdrawn(t.Context(), run, comps); !errors.Is(err, boom) || got != "" {
		t.Fatalf("record unread: %q, %v; want the error and no answer", got, err)
	}
	st.recordErr = nil
	st.capStore.err = boom
	if got, err := srv.componentWithdrawn(t.Context(), run, comps); !errors.Is(err, boom) || got != "" {
		t.Fatalf("rows unread, no record: %q, %v; want the error and no answer", got, err)
	}
	st.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{}}
	if got, err := srv.componentWithdrawn(t.Context(), run, comps); !errors.Is(err, boom) || got != "" {
		t.Fatalf("rows unread, on record: %q, %v; want the error and no answer", got, err)
	}
	st.capStore.err = nil
	gone := run
	gone.UserType = "contractor"
	wantWithdrawn(t, srv, gone, comps, "user type deleted, on record", reasonOwnerCapabilityComponent)
	delete(st.records, capSub)
	wantWithdrawn(t, srv, gone, comps, "user type deleted, no record", reasonOwnerCapabilityComponent)
}

// A store that keeps no sign-in record asks nothing: a build with no record of who holds what.
func TestComponentWithdrawnStoreWithoutARecordAsksNothing(t *testing.T) {
	st := &struct {
		*capStore
		fakeComponentStore
	}{capStore: &capStore{}}
	srv := capServer(st)
	wantWithdrawn(t, srv, types.AgentRun{ID: uuid.New(), CreatedBy: capSub}, []types.RunComponent{{ComponentID: new(uuid.UUID)}},
		"no record store", "")
}
