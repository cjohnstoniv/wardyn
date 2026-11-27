// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identitytest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// failingRevocations is a denylist whose every call errors, for the fail-closed case.
type failingRevocations struct{}

var errRevocationsDown = errors.New("identitytest: revocation store unavailable")

func (failingRevocations) IsRevoked(context.Context, string, uuid.UUID) (bool, error) {
	return false, errRevocationsDown
}
func (failingRevocations) RevokeRun(context.Context, uuid.UUID) error { return errRevocationsDown }
func (failingRevocations) RevokeJTI(context.Context, string, uuid.UUID) error {
	return errRevocationsDown
}

func workloadMinter(t *testing.T, p identity.Provider) identity.WorkloadRunMinter {
	t.Helper()
	m, ok := p.(identity.WorkloadRunMinter)
	if !ok {
		t.Fatal("the provider cannot mint for a run a workload launched (identity.WorkloadRunMinter)")
	}
	return m
}

// workloadRunConformance is RunConformance's workload-run case, kept apart so neither function grows
// past the complexity limit.
func workloadRunConformance(t *testing.T, p identity.Provider, aud string) {
	t.Helper()
	ctx := context.Background()
	m := workloadMinter(t, p)
	w := identity.Workload{Owner: "service:ci-bot", Name: "nightly"}
	runID, other := uuid.New(), uuid.New()
	ri, err := m.MintWorkloadRunIdentity(ctx, runID, w, "service:ci-bot", "sponsor@example.com", aud, false)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasSuffix(ri.SPIFFEID, "/workload/service_3Aci-bot/nightly/run/"+runID.String()) {
		t.Fatalf("spiffe id %q is not spiffe://<td>/workload/<owner>/<name>/run/<run-id>", ri.SPIFFEID)
	}
	ro, err := m.MintWorkloadRunIdentity(ctx, other, w, "service:ci-bot", "sponsor@example.com", aud, false)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if cl, err := p.Verify(ctx, ri.Token, aud); err != nil || cl.RunID != runID || cl.SPIFFEID != ri.SPIFFEID {
		t.Fatalf("verify = (%+v, %v), want the run and its SPIFFE id", cl, err)
	}
	if err := p.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if _, err := p.Verify(ctx, ri.Token, aud); err == nil {
		t.Fatal("verify after RevokeRun must fail closed")
	}
	if cl, err := p.Verify(ctx, ro.Token, aud); err != nil || cl.RunID != other {
		t.Fatalf("the workload's other run = (%+v, %v): a revoke is per run", cl, err)
	}
	workloadShapeRefusals(t, m, aud)
}

// workloadShapeRefusals: a zero workload is the ad-hoc form, and an owner past the SPIFFE ID length
// cap or a name outside the workload name rule is refused.
func workloadShapeRefusals(t *testing.T, m identity.WorkloadRunMinter, aud string) {
	t.Helper()
	ctx := context.Background()
	runID := uuid.New()
	ri, err := m.MintWorkloadRunIdentity(ctx, runID, identity.Workload{}, "a", "a", aud, false)
	if err != nil || !strings.HasSuffix(ri.SPIFFEID, "/agent-run/"+runID.String()) {
		t.Fatalf("zero workload = (%q, %v), want the /agent-run/ form", ri.SPIFFEID, err)
	}
	long := identity.Workload{Owner: strings.Repeat("o", 2048), Name: "nightly"}
	if _, err := m.MintWorkloadRunIdentity(ctx, uuid.New(), long, "a", "a", aud, false); err == nil {
		t.Fatal("an owner past the 2048-byte SPIFFE ID cap must be refused")
	}
	bad := identity.Workload{Owner: "alice", Name: "Not_A_Name"}
	if _, err := m.MintWorkloadRunIdentity(ctx, uuid.New(), bad, "a", "a", aud, false); err == nil {
		t.Fatal("a workload name outside the name rule must be refused")
	}
}

// workloadClaimConformance pins the workload claim: Verify returns the minted workload, derived from the
// signed run path, the same for every run of one workload and zero for an ad-hoc run.
func workloadClaimConformance(t *testing.T, p identity.Provider) {
	ctx := context.Background()
	m := workloadMinter(t, p)
	w := identity.Workload{Owner: "alice@example.com", Name: "nightly"}
	var ids []string
	for i := 0; i < 2; i++ {
		ri, err := m.MintWorkloadRunIdentity(ctx, uuid.New(), w, "alice@example.com", "", conformanceAud, false)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		cl, err := p.Verify(ctx, ri.Token, conformanceAud)
		if err != nil || cl.Workload != w {
			t.Fatalf("verify = (%+v, %v), want workload %q", cl, err, w.String())
		}
		sid, err := spiffeid.FromString(cl.SPIFFEID)
		if err != nil {
			t.Fatalf("claims spiffe id: %v", err)
		}
		if got, err := identity.WorkloadFromSPIFFEID(cl.SPIFFEID, sid.TrustDomain().Name()); err != nil || got != cl.Workload {
			t.Fatalf("WorkloadFromSPIFFEID = (%+v, %v), want the claim %+v", got, err, cl.Workload)
		}
		ids = append(ids, cl.SPIFFEID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("two runs of one workload share SPIFFE id %q: the ID must stay per run", ids[0])
	}
	ri := mint(t, p, uuid.New(), "a", "a", false)
	if cl, err := p.Verify(ctx, ri.Token, conformanceAud); err != nil || cl.Workload != (identity.Workload{}) {
		t.Fatalf("ad-hoc verify = (%+v, %v), want a zero workload", cl, err)
	}
}
