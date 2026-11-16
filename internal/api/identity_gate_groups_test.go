// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"testing"

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
}

func (g *gateGroupsStore) PutPrincipalGroups(_ context.Context, principal, portal, email string, groups []string, truncated bool) error {
	g.puts++
	g.gotPrinc, g.gotPortal, g.gotEmail, g.gotGroups, g.gotPartial = principal, portal, email, groups, truncated
	return g.putErr
}

func gateGroupsFacts() oidc.LoginFacts {
	return oidc.LoginFacts{Sub: "alice", Issuer: "https://issuer.test", Email: "Alice@Example.Test",
		Groups: []string{"eng", "wardyn-admins"}, GroupsTruncated: true}
}

// An issued sign-in leaves its email and group snapshot, truncation bit included, on the record of a
// sign-in to Wardyn itself (portal "").
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
