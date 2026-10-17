// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

const inst = "0123456789abcdef0123456789abcdef"

func hashOf(seq int64) string {
	h := sha256.Sum256(fmt.Appendf(nil, "body-%d", seq))
	return hex.EncodeToString(h[:])
}

func produce(t *testing.T, s *Spool, id uuid.UUID, gen uint64, seq int64, admitted bool) (Disposition, bool) {
	t.Helper()
	d, fresh, err := s.Produce(context.Background(), Produced{Gen: gen, Instance: inst, Seq: seq, Hash: hashOf(seq), Admitted: admitted, Decision: decision(id)})
	if err != nil {
		t.Fatalf("produce %d: %v", seq, err)
	}
	return d, fresh
}

func ackAll(t *testing.T, s *Spool) {
	t.Helper()
	if err := s.Replay(context.Background(), func(context.Context, []Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func liability(t *testing.T, s *Spool) Liability {
	t.Helper()
	l, err := s.Liability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestRemoteProduceCrashPointsDedupe: a crash before the commit leaves nothing
// (the resend appends once); after the commit and before the ack the resend is
// re-acked with its stored disposition; after the ack an older seq is a plain
// re-ack. An org ack and reclaim in between never erase the dedupe.
func TestRemoteProduceCrashPointsDedupe(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	acc, err := s.Accept(ctx, inst)
	if err != nil || acc.Gen != 1 || acc.Seq != 0 || acc.Grant.Through != GrantRecords {
		t.Fatalf("accept = %+v, %v", acc, err)
	}
	if d, fresh := produce(t, s, id, 1, 1, true); d != Appended || !fresh {
		t.Fatalf("seq 1 = %s fresh=%v", d, fresh)
	}
	// Crash after commit, before ack: org ack + reclaim, runnerd restart, resend.
	ackAll(t, s)
	s.Close()
	s, err = Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	acc, err = s.Accept(ctx, inst)
	if err != nil || acc.Gen != 2 || acc.Seq != 1 {
		t.Fatalf("reaccept = %+v, %v", acc, err)
	}
	if d, fresh := produce(t, s, id, 2, 1, true); d != Appended || fresh {
		t.Fatalf("resend of committed seq = %s fresh=%v", d, fresh)
	}
	if d, fresh := produce(t, s, id, 2, 0, true); d != "" || fresh {
		t.Fatalf("older seq = %s fresh=%v", d, fresh)
	}
	// A different body under the committed seq closes the session.
	if _, _, err := s.Produce(ctx, Produced{Gen: 2, Instance: inst, Seq: 1, Hash: hashOf(9), Admitted: true, Decision: decision(id)}); !errors.Is(err, ErrProducer) {
		t.Fatalf("hash mismatch: %v", err)
	}
	// A gap closes the session; the next seq is contiguous.
	if _, _, err := s.Produce(ctx, Produced{Gen: 2, Instance: inst, Seq: 3, Hash: hashOf(3), Decision: decision(id)}); !errors.Is(err, ErrProducer) {
		t.Fatalf("gap: %v", err)
	}
	produce(t, s, id, 2, 2, false)
	depth, _, _, _ := s.Stats(ctx)
	if l := liability(t, s); l.Producer.Seq != 2 || depth != 1 {
		t.Fatalf("producer seq %d depth %d: duplicate or loss", l.Producer.Seq, depth)
	}
	// A superseded generation is fenced; a foreign instance is refused.
	if _, _, err := s.Produce(ctx, Produced{Gen: 1, Instance: inst, Seq: 3, Hash: hashOf(3), Decision: decision(id)}); !errors.Is(err, ErrStaleGen) {
		t.Fatalf("stale gen: %v", err)
	}
	if _, err := s.Accept(ctx, "ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrProducer) {
		t.Fatalf("foreign instance: %v", err)
	}
}

// TestRemoteRefusedAndLostDispositions (P5, R3-M2): an unreserved record that
// meets unreported Drops is refused in the Drops write; its resend returns
// refused without recounting, after a restart too, and the next seq appends
// contiguously. An admitted record that does not fit is lost, never Drops.
func TestRemoteRefusedAndLostDispositions(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	if _, err := s.Accept(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := s.ProducerRefused(ctx, 1, inst, 2); err != nil { // two refused admissions
		t.Fatal(err)
	}
	if err := s.ProducerRefused(ctx, 1, inst, 2); err != nil { // resent total: no recount
		t.Fatal(err)
	}
	if d, _ := produce(t, s, id, 1, 1, false); d != Refused {
		t.Fatalf("unreserved with unreported drops = %s", d)
	}
	_, _, drops, _ := s.Stats(ctx)
	if drops != 3 {
		t.Fatalf("drops = %d, want 3", drops)
	}
	s.Close()
	s, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Accept(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if d, fresh := produce(t, s, id, 2, 1, false); d != Refused || fresh {
		t.Fatalf("resend after restart = %s fresh=%v", d, fresh)
	}
	if err := s.ProducerRefused(ctx, 2, inst, 2); err != nil {
		t.Fatal(err)
	}
	if d, _ := produce(t, s, id, 2, 2, true); d != Appended {
		t.Fatalf("next admitted seq = %s", d)
	}
	if _, _, drops, _ = s.Stats(ctx); drops != 3 {
		t.Fatalf("drops recounted: %d", drops)
	}
	// An admitted record that cannot fit: lost into pending_loss.
	big := decision(id)
	big.Request.Path = string(make([]byte, MaxRecordBytes))
	d, _, err := s.Produce(ctx, Produced{Gen: 2, Instance: inst, Seq: 3, Hash: hashOf(3), Admitted: true, Decision: big})
	if err != nil || d != Lost {
		t.Fatalf("oversized admitted = %s, %v", d, err)
	}
	l := liability(t, s)
	if l.PendingLoss == nil || l.PendingLoss.Count != 1 {
		t.Fatalf("pending loss %+v", l.PendingLoss)
	}
	if _, _, drops, _ = s.Stats(ctx); drops != 3 {
		t.Fatalf("lost counted as drops: %d", drops)
	}
}

// TestRemoteRestartKnowsLiabilityBeforeRegrant: an outstanding grant survives a
// restart and is never re-issued lower.
func TestRemoteRestartKnowsLiabilityBeforeRegrant(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	acc, _ := s.Accept(ctx, inst)
	produce(t, s, id, 1, 1, true)
	s.Close()
	s, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	l := liability(t, s)
	if l.Grant == nil || l.Grant.Through-l.Producer.Seq != acc.Grant.Through-1 {
		t.Fatalf("liability after restart = %+v", l.Grant)
	}
	re, err := s.Accept(ctx, inst)
	if err != nil || re.Grant.Through < acc.Grant.Through {
		t.Fatalf("regrant lowered the ceiling: %+v (was %+v), %v", re.Grant, acc.Grant, err)
	}
}

// TestRemoteLossSettlement: a clean drain leaves no loss row; an unemitted
// reservation after a drain is the exact loss; no drained reply is the upper
// bound; a settled producer is never accepted again.
func TestRemoteLossSettlement(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		drained func(gen uint64) *Drained
		count   uint64
		basis   LossBasis
	}{
		{"clean drain", func(g uint64) *Drained { return &Drained{Gen: g, FinalSeq: 2, Consumed: 2} }, 0, ""},
		{"exact", func(g uint64) *Drained { return &Drained{Gen: g, FinalSeq: 2, Consumed: 3} }, 1, LossExact},
		{"crash", func(uint64) *Drained { return nil }, GrantRecords - 2, LossUpperBound},
		{"stale drained gen", func(g uint64) *Drained { return &Drained{Gen: g - 1, FinalSeq: 2, Consumed: 2} }, GrantRecords - 2, LossUpperBound},
		{"uncommitted tail", func(g uint64) *Drained { return &Drained{Gen: g, FinalSeq: 3, Consumed: 3} }, GrantRecords - 2, LossUpperBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, id := testSpool(t)
			s.Accept(ctx, inst)
			acc, _ := s.Accept(ctx, inst)
			produce(t, s, id, acc.Gen, 1, true)
			produce(t, s, id, acc.Gen, 2, true)
			loss, err := s.SettleLoss(ctx, tc.drained(acc.Gen))
			if err != nil || loss.Count != tc.count || (tc.count > 0 && loss.Basis != tc.basis) {
				t.Fatalf("loss = %+v, %v; want %d %s", loss, err, tc.count, tc.basis)
			}
			l := liability(t, s)
			if l.Grant != nil || (tc.count == 0) != (l.PendingLoss == nil) {
				t.Fatalf("after settle: %+v", l)
			}
			if _, err := s.Accept(ctx, inst); !errors.Is(err, ErrProducer) {
				t.Fatalf("settled producer reaccepted: %v", err)
			}
		})
	}
}

// TestRemoteSealWaitsForSettledAndAckedLoss (R6, P3): with the writer lock
// lost (a runnerd crash), an outstanding grant still defers Seal; after the
// write-off, the pending loss and then its unacked row defer it; Seal succeeds
// only once the org acked the loss row. A crash between loss detection and
// emission keeps the obligation; unreported Drops never refuse the loss row.
func TestRemoteSealWaitsForSettledAndAckedLoss(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	s.Accept(ctx, inst)
	produce(t, s, id, 1, 1, true)
	if err := s.ProducerRefused(ctx, 1, inst, 1); err != nil { // unreported Drops
		t.Fatal(err)
	}
	ackAll(t, s)
	seal := func() bool {
		t.Helper()
		r, err := s.Receipt(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := s.Seal(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if seal() {
		t.Fatal("sealed with an outstanding write-ahead grant")
	}
	if _, err := s.SettleLoss(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// Crash between loss detection and emission.
	s.Close()
	s, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if l := liability(t, s); l.PendingLoss == nil || l.PendingLoss.Basis != LossUpperBound {
		t.Fatalf("pending loss lost in a crash: %+v", l.PendingLoss)
	}
	if seal() {
		t.Fatal("sealed with a pending loss")
	}
	if ok, err := s.EmitLoss(ctx); err != nil || !ok {
		t.Fatalf("emit = %v, %v (unreported drops must not refuse it)", ok, err)
	}
	if seal() {
		t.Fatal("sealed before the org acked the loss row")
	}
	var lossRow *Record
	if err := s.Replay(ctx, func(_ context.Context, rows []Record) error {
		for i := range rows {
			if rows[i].Decision.RuleSource == "builtin:spool-lost" {
				lossRow = &rows[i]
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if lossRow == nil || lossRow.Seq <= 0 || !lossRow.ValidHash() {
		t.Fatalf("loss row = %+v", lossRow)
	}
	if !seal() {
		t.Fatal("settled, acked spool did not seal")
	}
}

// TestRemoteLossRowFitsTheReserve: a grant never reaches the last slot or the
// last 4 KiB, so the loss row lands even when the producer used every credit;
// while an earlier unacked row still fills the reserve the emission waits.
func TestRemoteLossRowFitsTheReserve(t *testing.T) {
	ctx := context.Background()
	s, _, id := testSpool(t)
	// Fill the spool to one grant short of capacity with acked-later rows.
	if err := s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		m.Last, m.Ack.Seq, m.Bytes = int64(MaxRecords-2), 0, MaxBytes-LossReserveBytes-grantRecordEnvelope
		return s.writeManifest(m)
	}); err != nil {
		t.Fatal(err)
	}
	acc, err := s.Accept(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Grant.Through != 1 || acc.Grant.Bytes != 0 {
		t.Fatalf("grant into the reserve: %+v", acc.Grant)
	}
	if _, err := s.SettleLoss(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.EmitLoss(ctx); err != nil || !ok {
		t.Fatalf("loss row did not fit the reserve: %v %v", ok, err)
	}
	// A later loss while unacked rows still fill every slot waits.
	if err := s.locked(ctx, func() error {
		m, _ := s.readManifest()
		m.PendingLoss = &PendingLoss{Count: 1, Basis: LossExact}
		m.Last = m.Ack.Seq + MaxRecords
		return s.writeManifest(m)
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.EmitLoss(ctx); err != nil || ok {
		t.Fatalf("emitted over an occupied reserve: %v %v", ok, err)
	}
	_ = id
}

// TestRemoteCorruptSealedManifestIsRefused (L2): a sealed manifest carrying a
// grant without a producer is refused as invalid, never a nil dereference.
func TestRemoteCorruptSealedManifestIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, _ := testSpool(t)
	if err := s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		m.Sealed, m.Grant = true, &Grant{Through: 1}
		return s.writeManifest(m)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Liability(ctx); err == nil {
		t.Fatal("corrupt sealed manifest accepted")
	}
}
