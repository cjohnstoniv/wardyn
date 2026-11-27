// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// gateGroupsStore is the sign-in gate's store that also keeps the record of each person's last sign-in.
type gateGroupsStore struct {
	gapCovGateStore
	putErr     error
	puts       int
	gotPrinc   string
	gotPortal  string
	gotEmail   string
	gotGroups  []string
	gotPartial bool
	gotIssued  time.Time
}

func (g *gateGroupsStore) PutPrincipalGroups(_ context.Context, principal, portal, email string, groups []string, truncated bool, issuedAt time.Time) error {
	g.puts++
	g.gotPrinc, g.gotPortal, g.gotEmail, g.gotGroups, g.gotPartial, g.gotIssued = principal, portal, email, groups, truncated, issuedAt
	return g.putErr
}

func gateGroupsFacts() oidc.LoginFacts {
	return oidc.LoginFacts{Sub: "alice", Issuer: "https://issuer.test", Email: "Alice@Example.Test",
		Groups: []string{"eng", "wardyn-admins"}, GroupsTruncated: true}
}

// An issued sign-in leaves its email and group snapshot, truncation bit included, on the record of a
// sign-in to Wardyn itself (portal ""), with no issue time, so the database stamps its own.
func TestIdentityGateIssueRecordsTheSignInGroups(t *testing.T) {
	st := &gateGroupsStore{gapCovGateStore: gapCovGateStore{epoch: 7}}
	epoch, err := NewIdentityGate(st).Issue(t.Context(), gateGroupsFacts())
	if err != nil || epoch != 7 {
		t.Fatalf("Issue = %d, %v; want 7, nil", epoch, err)
	}
	if st.puts != 1 || st.gotPrinc != "alice" || st.gotPortal != "" || st.gotEmail != "Alice@Example.Test" ||
		!slices.Equal(st.gotGroups, []string{"eng", "wardyn-admins"}) || !st.gotPartial {
		t.Fatalf("recorded %d time(s): %q %q %v %v; want alice's sign-in once, as it came", st.puts, st.gotPrinc, st.gotEmail, st.gotGroups, st.gotPartial)
	}
	if !st.gotIssued.IsZero() {
		t.Fatalf("issued at %v; want no time, for the database's clock", st.gotIssued)
	}
}

// A record that cannot be written refuses the sign-in: no epoch, and the store's error, which the
// callback answers as a sign-in it could not check.
func TestIdentityGateIssueRefusedWhenTheRecordFails(t *testing.T) {
	boom := errors.New("record down")
	st := &gateGroupsStore{gapCovGateStore: gapCovGateStore{epoch: 7}, putErr: boom}
	epoch, err := NewIdentityGate(st).Issue(t.Context(), gateGroupsFacts())
	if epoch != 0 || !errors.Is(err, boom) {
		t.Fatalf("Issue = %d, %v; want 0 and the record's error", epoch, err)
	}
}

// A sign-in the identity rows refuse writes no record.
func TestIdentityGateRefusedSignInWritesNoRecord(t *testing.T) {
	for _, issueErr := range []error{store.ErrIdentityDeactivated, store.ErrConflict, errors.New("identity rows down")} {
		st := &gateGroupsStore{gapCovGateStore: gapCovGateStore{issueErr: issueErr}}
		if _, err := NewIdentityGate(st).Issue(t.Context(), gateGroupsFacts()); err == nil {
			t.Fatalf("%v: Issue succeeded", issueErr)
		}
		if st.puts != 0 {
			t.Errorf("%v: the refused sign-in wrote %d record(s)", issueErr, st.puts)
		}
	}
}

// gateRolesStore is a store that keeps the roles claim of a sign-in on its own as well (PutPrincipalSignIn).
type gateRolesStore struct {
	gateGroupsStore
	signIns   int
	gotRoles  []string
	signInErr error
}

func (g *gateRolesStore) PutPrincipalSignIn(_ context.Context, principal, email string, groups, roles []string, truncated bool) error {
	g.signIns++
	g.gotPrinc, g.gotEmail, g.gotGroups, g.gotRoles, g.gotPartial = principal, email, groups, roles, truncated
	return g.signInErr
}

// A store that keeps the roles claim records the sign-in through it, roles included, and not a second time
// through the plain group record.
func TestIdentityGateIssueRecordsTheRolesClaim(t *testing.T) {
	st := &gateRolesStore{gateGroupsStore: gateGroupsStore{gapCovGateStore: gapCovGateStore{epoch: 7}}}
	facts := gateGroupsFacts()
	facts.Roles = []string{"wardyn-admins"}
	if _, err := NewIdentityGate(st).Issue(t.Context(), facts); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if st.signIns != 1 || st.puts != 0 || st.gotPrinc != "alice" || !slices.Equal(st.gotRoles, []string{"wardyn-admins"}) ||
		!slices.Equal(st.gotGroups, []string{"eng", "wardyn-admins"}) || !st.gotPartial {
		t.Fatalf("sign-in writes %d, group writes %d, roles %v, groups %v; want one sign-in write carrying the roles", st.signIns, st.puts, st.gotRoles, st.gotGroups)
	}
}

// A sign-in record that cannot be written through the roles-keeping store refuses the sign-in too.
func TestIdentityGateIssueRefusedWhenTheSignInRecordFails(t *testing.T) {
	boom := errors.New("sign-in record down")
	st := &gateRolesStore{gateGroupsStore: gateGroupsStore{gapCovGateStore: gapCovGateStore{epoch: 7}}, signInErr: boom}
	if epoch, err := NewIdentityGate(st).Issue(t.Context(), gateGroupsFacts()); epoch != 0 || !errors.Is(err, boom) {
		t.Fatalf("Issue = %d, %v; want 0 and the record's error", epoch, err)
	}
}
