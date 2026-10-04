// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// gapCovGateStore records what the gate asked the store and answers as told.
type gapCovGateStore struct {
	refused    bool
	refusedErr error
	issueErr   error
	epoch      int64

	gotRefused []string
	gotIssue   store.LoginIdentity
	gotNow     time.Time
}

func (g *gapCovGateStore) IdentityRefused(_ context.Context, issuer, tenantID, objectID, principal string) (bool, error) {
	g.gotRefused = []string{issuer, tenantID, objectID, principal}
	return g.refused, g.refusedErr
}

func (g *gapCovGateStore) IssueLoginIdentity(_ context.Context, in store.LoginIdentity, now time.Time) (int64, error) {
	g.gotIssue, g.gotNow = in, now
	return g.epoch, g.issueErr
}

func TestGapCovIdentityGateRefusedAsksTheStoreAboutTheWholeIdentity(t *testing.T) {
	boom := errors.New("gapcov: store down")
	for _, tc := range []struct {
		name    string
		st      *gapCovGateStore
		refused bool
		err     error
	}{
		{"deactivated", &gapCovGateStore{refused: true}, true, nil},
		{"admitted", &gapCovGateStore{}, false, nil},
		{"store error fails the question, not open", &gapCovGateStore{refusedErr: boom}, false, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewIdentityGate(tc.st).Refused(t.Context(), oidc.IdentityRef{
				Issuer: "https://issuer.test", TenantID: "tenant-1", ObjectID: "object-1", Principal: "alice",
			})
			if got != tc.refused || !errors.Is(err, tc.err) {
				t.Fatalf("Refused = %v, %v; want %v, %v", got, err, tc.refused, tc.err)
			}
			want := []string{"https://issuer.test", "tenant-1", "object-1", "alice"}
			for i := range want {
				if tc.st.gotRefused[i] != want[i] {
					t.Fatalf("store asked %v, want %v", tc.st.gotRefused, want)
				}
			}
		})
	}
}

func TestGapCovIdentityGateIssueMapsTheStoresRefusals(t *testing.T) {
	boom := errors.New("gapcov: store down")
	facts := oidc.LoginFacts{Sub: "alice", Issuer: "https://issuer.test", TenantID: "tenant-1", ObjectID: "object-1", Email: "alice@example.test"}
	for _, tc := range []struct {
		name  string
		st    *gapCovGateStore
		epoch int64
		err   error
	}{
		{"issued", &gapCovGateStore{epoch: 7}, 7, nil},
		{"deactivated", &gapCovGateStore{issueErr: store.ErrIdentityDeactivated, epoch: 9}, 0, oidc.ErrIdentityDeactivated},
		{"conflict", &gapCovGateStore{issueErr: store.ErrConflict, epoch: 9}, 0, oidc.ErrIdentityConflict},
		{"other store errors pass through", &gapCovGateStore{issueErr: boom, epoch: 9}, -1, boom}, // -1: no epoch is promised with an error
	} {
		t.Run(tc.name, func(t *testing.T) {
			epoch, err := NewIdentityGate(tc.st).Issue(t.Context(), facts)
			if (tc.epoch >= 0 && epoch != tc.epoch) || !errors.Is(err, tc.err) {
				t.Fatalf("Issue = %d, %v; want %d, %v", epoch, err, tc.epoch, tc.err)
			}
			if want := (store.LoginIdentity{Principal: "alice", Issuer: "https://issuer.test", TenantID: "tenant-1", ObjectID: "object-1", Email: "alice@example.test"}); tc.st.gotIssue != want {
				t.Fatalf("store was asked to issue %+v, want %+v", tc.st.gotIssue, want)
			}
			if tc.st.gotNow.IsZero() || tc.st.gotNow.Location() != time.UTC {
				t.Fatalf("issue time %v, want a UTC timestamp", tc.st.gotNow)
			}
		})
	}
}

// gapCovSharer is a store that holds identity rows: it records the guard it was
// given and runs fn only when asked to.
type gapCovSharer struct {
	store.Store
	guard  store.IdentityGuard
	calls  int
	refuse error
}

func (g *gapCovSharer) WithIdentityShared(_ context.Context, guard store.IdentityGuard, fn func() error) error {
	g.calls++
	g.guard = guard
	if g.refuse != nil {
		return g.refuse
	}
	return fn()
}

func TestGapCovPutOwnedHoldsTheOwnersIdentityRows(t *testing.T) {
	sharer := &gapCovSharer{}
	s := &Server{cfg: Config{Store: sharer}}
	puts := 0
	put := func() error { puts++; return nil }

	ctx := oidc.WithAuthorityEpoch(t.Context(), 5)
	if err := s.putOwned(ctx, "alice", put); err != nil || puts != 1 {
		t.Fatalf("putOwned = %v after %d put(s), want nil after 1", err, puts)
	}
	if want := (store.IdentityGuard{Principal: "alice", Epoch: 5}); sharer.guard != want {
		t.Fatalf("guard = %+v, want %+v (the sign-in being issued for the owner)", sharer.guard, want)
	}

	// A refusal from the store means the write did not run.
	sharer.refuse = store.ErrIdentityDeactivated
	puts = 0
	if err := s.putOwned(t.Context(), "alice", put); !errors.Is(err, store.ErrIdentityDeactivated) || puts != 0 {
		t.Fatalf("putOwned = %v after %d put(s), want the refusal and no write", err, puts)
	}
}

func TestGapCovPutOwnedWritesUnguardedWithoutIdentityRowsOrOwner(t *testing.T) {
	sharer := &gapCovSharer{}
	cases := []struct {
		name  string
		s     *Server
		owner string
	}{
		{"store without identity rows", &Server{cfg: Config{Store: &struct{ store.Store }{}}}, "alice"},
		{"no owner", &Server{cfg: Config{Store: sharer}}, ""},
	}
	for _, c := range cases {
		puts := 0
		boom := errors.New("gapcov: put failed")
		err := c.s.putOwned(t.Context(), c.owner, func() error { puts++; return boom })
		if !errors.Is(err, boom) || puts != 1 {
			t.Errorf("%s: putOwned = %v after %d put(s), want the put's error after 1", c.name, err, puts)
		}
	}
	if sharer.calls != 0 {
		t.Errorf("an ownerless write took the identity rows %d time(s)", sharer.calls)
	}
}
