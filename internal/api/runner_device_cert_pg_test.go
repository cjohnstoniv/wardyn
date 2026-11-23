// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A runner's device certificate chains on Postgres: the stored chains round-trip behind the session fence, and
// an event that stores them leaves the trust class alone. Guarded by WARDYN_TEST_PG (throwawayPGPool).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// claimedPGRunner is a claimed runner with a real key's fingerprint, and an inventory session open on it.
func claimedPGRunner(t *testing.T, st store.PG, owner string) (types.Runner, string) {
	t.Helper()
	ctx := context.Background()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	each := types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 32768, DiskMiB: 102400}
	limits := &types.RunnerLimits{RunsAtOnce: 1, Total: each, PerRun: each}
	rn, err := st.CreateRunner(ctx, types.Runner{ID: uuid.New(), Owner: owner, Name: "studio", PublicKey: pub, KeyFingerprint: runnerwire.Fingerprint(pub), Version: "0.11.0", Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimRunner(ctx, rn.ID, owner, rn.KeyFingerprint, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	session := uuid.NewString()
	if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
		t.Fatal(err)
	}
	return rn, session
}

func TestRecordRunnerDeviceCertFrom_PG(t *testing.T) {
	st := store.NewPG(throwawayPGPool(t))
	ctx := context.Background()
	rn, session := claimedPGRunner(t, st, "device-cert-owner")
	now := time.Now().UTC().Truncate(time.Microsecond)
	chains := [][][]byte{{{1, 2, 3}, {4, 5}}, {{6}}}
	if changed, err := st.RecordRunnerDeviceCertFrom(ctx, rn.ID, session, chains, now); err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	got, err := st.GetRunner(ctx, rn.ID)
	if err != nil || got.DeviceCert == nil || len(got.DeviceCert.Chains) != 2 || !slices.Equal(got.DeviceCert.Chains[0][1], []byte{4, 5}) ||
		got.DeviceCertReportedAt == nil || !got.DeviceCertReportedAt.Equal(now) {
		t.Fatalf("stored %+v at %v, %v; want both chains and the report time", got.DeviceCert, got.DeviceCertReportedAt, err)
	}
	later := now.Add(time.Hour)
	if changed, err := st.RecordRunnerDeviceCertFrom(ctx, rn.ID, session, chains, later); err != nil || changed {
		t.Fatalf("the same chains again: changed=%v err=%v, want no change", changed, err)
	}
	if got, _ := st.GetRunner(ctx, rn.ID); got.DeviceCertReportedAt == nil || !got.DeviceCertReportedAt.Equal(later) {
		t.Fatalf("report time %v after an unchanged report, want it moved to %v", got.DeviceCertReportedAt, later)
	}
	fresh := uuid.NewString()
	if err := st.BeginRunnerInventory(ctx, rn.ID, fresh); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.RecordRunnerDeviceCertFrom(ctx, rn.ID, session, nil, later.Add(time.Hour)); err != nil || changed {
		t.Fatalf("a superseded session's report: changed=%v err=%v, want nothing written", changed, err)
	}
	if got, _ := st.GetRunner(ctx, rn.ID); got.DeviceCert == nil || len(got.DeviceCert.Chains) != 2 {
		t.Fatalf("a superseded session changed the stored chains: %+v", got.DeviceCert)
	}
	if changed, err := st.RecordRunnerDeviceCertFrom(ctx, rn.ID, fresh, nil, later.Add(time.Hour)); err != nil || !changed {
		t.Fatalf("the fresh session clearing them: changed=%v err=%v", changed, err)
	}
	if got, _ := st.GetRunner(ctx, rn.ID); got.DeviceCert == nil || len(got.DeviceCert.Chains) != 0 {
		t.Fatalf("after clearing: %+v, want an empty list", got.DeviceCert)
	}
	other, err := st.CreateRunner(ctx, types.Runner{ID: uuid.New(), Owner: "device-cert-owner", Name: "unclaimed", PublicKey: make([]byte, 32), KeyFingerprint: "fp-" + uuid.NewString(), Limits: rn.Limits})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordRunnerDeviceCertFrom(ctx, other.ID, session, chains, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unclaimed runner: %v, want ErrNotFound", err)
	}
}

// Nothing the runner sends writes the trust class: a device_cert event on a personal runner stores the
// chains and leaves trust_class, trust_set_by and trust_set_at as the claim left them.
func TestDeviceCertFromRunner_NeverWritesTrustClass_PG(t *testing.T) {
	st := store.NewPG(throwawayPGPool(t))
	ctx := context.Background()
	rn, session := claimedPGRunner(t, st, "device-cert-trust-owner")
	before, err := st.GetRunner(ctx, rn.ID)
	if err != nil || before.TrustClass != types.RunnerTrustPersonal {
		t.Fatalf("runner before: %+v, %v; want a personal runner", before.TrustClass, err)
	}
	d := newDeviceProofEnv(t)
	d.fp = rn.KeyFingerprint
	d.store = &deviceFake{}
	srv := &Server{cfg: Config{Store: st, Audit: d.audit, RunnerOrgURL: deviceOrgURL, Now: func() time.Time { return d.now }}}
	proof := d.proof("device", d.fp, d.org, d.now)
	ev := runnerwire.Event{Kind: runnerwire.EventDeviceCert, Data: mustJSON(runnerwire.DeviceCertReport{Proofs: []runnerwire.DeviceCertProof{proof}})}
	if err := srv.DeviceCertFromRunner(ctx, rn.ID, session, ev); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetRunner(ctx, rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DeviceCert == nil || len(after.DeviceCert.Chains) != 1 {
		t.Fatalf("the event stored %+v, want one chain", after.DeviceCert)
	}
	if after.TrustClass != before.TrustClass || after.TrustSetBy != before.TrustSetBy || !equalTimePtr(after.TrustSetAt, before.TrustSetAt) || after.Posture != before.Posture {
		t.Fatalf("trust %q/%q/%v posture %+v after, was %q/%q/%v %+v", after.TrustClass, after.TrustSetBy, after.TrustSetAt, after.Posture,
			before.TrustClass, before.TrustSetBy, before.TrustSetAt, before.Posture)
	}
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
