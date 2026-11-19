// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
)

// A workload's run is minted under the layered SPIFFE ID, which its identity.mint row names; the ad-hoc
// mint is the zero workload's and keeps the path it always had.
func TestMintWorkloadRunIdentityNamesTheWorkload(t *testing.T) {
	ctx := context.Background()
	rec := &recordingRecorder{}
	p := newProvider(t, identitytest.NewMemRevocationStore(), rec)
	runID, adHoc := uuid.New(), uuid.New()
	ri, err := p.MintWorkloadRunIdentity(ctx, runID, identity.Workload{Owner: "alice@example.com", Name: "web"}, "alice@example.com", "", testAudience, false)
	if err != nil {
		t.Fatalf("MintWorkloadRunIdentity: %v", err)
	}
	if want := "spiffe://" + DefaultTrustDomain + "/workload/alice_40example_2Ecom/web/run/" + runID.String(); ri.SPIFFEID != want {
		t.Fatalf("SPIFFEID = %q, want %q", ri.SPIFFEID, want)
	}
	if cl, err := p.Verify(ctx, ri.Token, testAudience); err != nil || cl.RunID != runID || cl.SPIFFEID != ri.SPIFFEID {
		t.Fatalf("Verify = (%+v, %v), want the run and its SPIFFE id", cl, err)
	}
	var minted []string
	for _, ev := range rec.snapshot() {
		if ev.Action == "identity.mint" {
			minted = append(minted, ev.Actor)
		}
	}
	if len(minted) != 1 || minted[0] != ri.SPIFFEID {
		t.Fatalf("identity.mint actors = %v, want the layered id alone", minted)
	}
	plain, err := p.MintRunIdentity(ctx, adHoc, "alice@example.com", "", testAudience, false)
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	if want := "spiffe://" + DefaultTrustDomain + "/agent-run/" + adHoc.String(); plain.SPIFFEID != want {
		t.Fatalf("an ad-hoc SPIFFEID = %q, want %q", plain.SPIFFEID, want)
	}
}

// An ID longer than the SPIFFE ID spec's 2048 bytes is refused, never issued; one of exactly 2048 is
// issued. With the default trust domain and the name "web", 657 escaped bytes and one kept byte make
// the ID 2048 bytes long.
func TestMintWorkloadRunIdentityRefusesAnIDPast2048Bytes(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t, identitytest.NewMemRevocationStore(), &recordingRecorder{})
	at := identity.Workload{Owner: strings.Repeat(":", 657) + "a", Name: "web"}
	ri, err := p.MintWorkloadRunIdentity(ctx, uuid.New(), at, "alice", "", testAudience, false)
	if err != nil || len(ri.SPIFFEID) != 2048 {
		t.Fatalf("a 2048-byte id: %d bytes, %v; want it issued", len(ri.SPIFFEID), err)
	}
	past := identity.Workload{Owner: strings.Repeat(":", 657) + "ab", Name: "web"}
	if ri, err := p.MintWorkloadRunIdentity(ctx, uuid.New(), past, "alice", "", testAudience, false); err == nil {
		t.Fatalf("a 2049-byte id was issued: %q", ri.SPIFFEID)
	}
}

// The run is read from either form, in this trust domain only, and from nothing else.
func TestRunIDFromActorReadsBothForms(t *testing.T) {
	p := newProvider(t, identitytest.NewMemRevocationStore(), &recordingRecorder{})
	run := uuid.New()
	for _, actor := range []string{
		"spiffe://wardyn.local/agent-run/" + run.String(),
		"spiffe://wardyn.local/workload/alice/web/run/" + run.String(),
	} {
		if got, err := p.runIDFromActor(actor); err != nil || got != run {
			t.Errorf("runIDFromActor(%q) = %s, %v; want %s", actor, got, err, run)
		}
	}
	for _, actor := range []string{
		"spiffe://other.example/workload/alice/web/run/" + run.String(),
		"spiffe://wardyn.local/workload/alice/web/" + run.String(),
		"spiffe://wardyn.local/workload/alice.b/web/run/" + run.String(),
		"spiffe://wardyn.local/agent-run/" + run.String() + "/web",
	} {
		if got, err := p.runIDFromActor(actor); err == nil {
			t.Errorf("runIDFromActor(%q) = %s, want a refusal", actor, got)
		}
	}
}
