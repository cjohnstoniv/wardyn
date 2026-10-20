// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// openSealFixture is a run that allows all egress and lists one host of its own.
func openSealFixture() types.RunPolicySpec {
	return types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"pypi.org"}}
}

func hardBoundary(sp types.BoundarySpec) sealBoundaryStore {
	return sealBoundaryStore{b: types.Boundary{Mode: types.BoundaryHardLimits, Spec: sp}}
}

// TestBoundarySealKeepsAnExplicitAllowAllFalse: the boundary's allow_all_egress is read on its own,
// whether or not it names an allowlist. false alone turns open egress off and keeps the run's own
// allowlist; with neither named the boundary only denies; an explicit empty allowlist allows nothing;
// an unreadable boundary allows nothing.
func TestBoundarySealKeepsAnExplicitAllowAllFalse(t *testing.T) {
	off := false
	denied := []string{"evil.example"}

	falseOnly := openSealFixture()
	prov := sealUnder(hardBoundary(types.BoundarySpec{AllowAllEgress: &off}), &falseOnly)
	if falseOnly.AllowAllEgress || !slices.Equal(falseOnly.AllowedDomains, []string{"pypi.org"}) {
		t.Errorf("allow_all_egress false alone sealed %+v, want allow-all off and pypi.org kept", falseOnly)
	}
	if prov["allow_all_egress"].Layer != policyref.LayerBoundary {
		t.Errorf("provenance allow_all_egress = %+v, want the boundary", prov["allow_all_egress"])
	}

	denyOnly := openSealFixture()
	sealUnder(hardBoundary(types.BoundarySpec{DeniedDomains: &denied}), &denyOnly)
	if !denyOnly.AllowAllEgress || !slices.Contains(denyOnly.DeniedDomains, "evil.example") {
		t.Errorf("no allow_all_egress and no allowlist sealed %+v, want it open with evil.example denied", denyOnly)
	}

	empty := openSealFixture()
	sealUnder(hardBoundary(types.BoundarySpec{AllowedDomains: &[]string{}}), &empty)
	if empty.AllowAllEgress || empty.AllowedDomains == nil || len(empty.AllowedDomains) != 0 {
		t.Errorf("an explicit empty allowlist sealed %+v, want no egress", empty)
	}

	unread := openSealFixture()
	sealUnder(sealBoundaryStore{err: errors.New("down")}, &unread)
	if unread.AllowAllEgress || len(unread.AllowedDomains) != 0 {
		t.Errorf("an unreadable boundary sealed %+v, want no egress", unread)
	}
}

// TestBoundaryReviveClosesAnOpenFrozenRun: a run frozen while it allowed all egress is revived under
// a boundary that names only allow_all_egress false. The revive reads the frozen config and the
// ceiling only adds denies, so the seal is what closes it: the revived proxy's policy is not open and
// keeps the run's own allowlist.
func TestBoundaryReviveClosesAnOpenFrozenRun(t *testing.T) {
	f := newReviveFixture(t)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(f.rs.cfg, &fields); err != nil {
		t.Fatal(err)
	}
	fields["policy"], _ = json.Marshal(types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"api.anthropic.com"}})
	f.rs.cfg, _ = json.Marshal(fields)
	off := false
	f.srv.cfg.Store = boundaryReviveStore{reviveStore: f.rs, b: types.Boundary{Mode: types.BoundaryHardLimits,
		Spec: types.BoundarySpec{AllowAllEgress: &off}}}
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	cfg := f.newConfig(t)
	if cfg.Policy.AllowAllEgress || !slices.Equal(cfg.Policy.AllowedDomains, []string{"api.anthropic.com"}) {
		t.Errorf("revived policy = %+v, want allow-all off and api.anthropic.com kept", cfg.Policy)
	}
}

// boundaryReviveStore is the revive fixture's store holding a Wardyn boundary.
type boundaryReviveStore struct {
	*reviveStore
	b types.Boundary
}

func (s boundaryReviveStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s boundaryReviveStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}
