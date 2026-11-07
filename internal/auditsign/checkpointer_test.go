// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditsign

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fakeHash(seq int64) string {
	s := sha256.Sum256([]byte(fmt.Sprint("row", seq)))
	return hex.EncodeToString(s[:])
}

// fakeRow is one chained audit row: its prev_hash and row_hash as stored.
type fakeRow struct {
	seq        int64
	prev, hash string
	id         uuid.UUID
}

// fakeStore models audit_events (a chain the walk re-checks) and audit_checkpoints (overlap refused).
type fakeStore struct {
	rows      []fakeRow
	cps       []Checkpoint
	insertErr error
	inserts   int
}

func (s *fakeStore) appendRows(n int) {
	for i := 0; i < n; i++ {
		s.appendEvent(uuid.New())
	}
}

func (s *fakeStore) appendEvent(id uuid.UUID) {
	seq, prev := int64(1), ""
	if l := len(s.rows); l > 0 {
		seq, prev = s.rows[l-1].seq+1, s.rows[l-1].hash
	}
	s.rows = append(s.rows, fakeRow{seq: seq, prev: prev, hash: fakeHash(seq), id: id})
}

func (s *fakeStore) EventSeq(_ context.Context, id uuid.UUID, afterSeq int64) (int64, bool, error) {
	for _, r := range s.rows {
		if r.id == id && r.seq > afterSeq {
			return r.seq, true, nil
		}
	}
	return 0, false, nil
}

func (s *fakeStore) ChainHead(context.Context) (int64, string, error) {
	if len(s.rows) == 0 {
		return 0, "", nil
	}
	r := s.rows[len(s.rows)-1]
	return r.seq, r.hash, nil
}

func (s *fakeStore) Range(_ context.Context, afterSeq int64, afterHash string, max int) ([]Row, int64, error) {
	hw, _, _ := s.ChainHead(context.Background())
	prev := afterHash
	var out []Row
	for _, r := range s.rows {
		if r.seq <= afterSeq {
			continue
		}
		if len(out) == max {
			break
		}
		if r.hash == "" {
			return nil, 0, &ChainBreak{Seq: r.seq, Reason: "row has no row_hash"}
		}
		if r.prev != prev {
			return nil, 0, &ChainBreak{Seq: r.seq, Reason: "prev_hash does not match"}
		}
		prev = r.hash
		out = append(out, Row{Seq: r.seq, RowHash: r.hash})
	}
	return out, hw, nil
}

func (s *fakeStore) Insert(_ context.Context, cp Checkpoint) (Checkpoint, error) {
	if s.insertErr != nil {
		return Checkpoint{}, s.insertErr
	}
	for _, c := range s.cps {
		if c.SeqLo <= cp.SeqHi && cp.SeqLo <= c.SeqHi {
			return c, ErrExists
		}
	}
	s.inserts++
	s.cps = append(s.cps, cp)
	return Checkpoint{}, nil
}

func (s *fakeStore) Latest(context.Context) (Checkpoint, bool, error) {
	var best Checkpoint
	found := false
	for _, c := range s.cps {
		if !found || c.SeqHi > best.SeqHi {
			best, found = c, true
		}
	}
	return best, found, nil
}

// appendingRecorder is the audit chain as the signer sees it: each recorded row lands in the store's chain.
type appendingRecorder struct {
	st     *fakeStore
	events []types.AuditEvent
}

func (r *appendingRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.events = append(r.events, ev)
	r.st.appendEvent(ev.ID)
	return nil
}

// spoolingRecorder keeps every row for a later write: none reaches the store's chain now.
type spoolingRecorder struct{ events []types.AuditEvent }

func (r *spoolingRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.events = append(r.events, ev)
	return audit.ErrSpooled
}

// failingRecorder writes nothing.
type failingRecorder struct{}

func (failingRecorder) Record(context.Context, types.AuditEvent) error {
	return errors.New("audit store down")
}

func newTestSigner(t *testing.T, st *fakeStore) (*Signer, *appendingRecorder) {
	t.Helper()
	rec := &appendingRecorder{st: st}
	return &Signer{
		Store:   st,
		Key:     mustLoad(t, mustNewKey(t)),
		Locker:  db.NewLocalLocker(),
		Record:  rec,
		MaxRows: 10000,
		Now:     func() time.Time { return t0 },
		Status:  &Status{},
	}, rec
}

// checkSigned re-derives every stored checkpoint from the fake chain and verifies the export with the
// offline verifier, the way an operator holding the public key would.
func checkSigned(t *testing.T, s *Signer, st *fakeStore) {
	t.Helper()
	b := auditreceipt.Bundle{Format: auditreceipt.CheckpointsFormat}
	prev := ""
	for i, c := range st.cps {
		stm, err := auditreceipt.ParseStatement(c.Statement)
		if err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
		var seqs []int64
		var hashes []string
		for _, r := range st.rows {
			if r.seq >= stm.SeqLo && r.seq <= stm.SeqHi {
				seqs, hashes = append(seqs, r.seq), append(hashes, r.hash)
			}
		}
		if stm.Prev != prev || stm.Rows != int64(len(hashes)) || stm.HeadHash != hashes[len(hashes)-1] ||
			stm.Digest != auditreceipt.Fold(prev, hashes) || stm.SeqDigest != auditreceipt.SeqDigest(seqs, hashes) {
			t.Fatalf("checkpoint %d does not fold its rows: %s", i, c.Statement)
		}
		if c.SeqLo != stm.SeqLo || c.SeqHi != stm.SeqHi || c.Kid != stm.Kid || stm.Kid != s.Key.KeyID() {
			t.Fatalf("checkpoint %d columns differ from its statement", i)
		}
		prev = auditreceipt.StatementHash(c.Statement)
		b.Checkpoints = append(b.Checkpoints, auditreceipt.Checkpoint{Statement: c.Statement, Signature: c.Signature})
	}
	rep := auditreceipt.VerifyBundle(b, auditreceipt.Keys{Format: auditreceipt.KeysFormat, Keys: s.Key.PublicKeys()})
	if !rep.OK() {
		t.Fatalf("offline verify of the export failed: %+v", rep.Failures)
	}
}

func TestCheckpointLoop_SkipsIdleAndOwnRow(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	st.appendRows(5) // history before signing was on: never in a checkpoint
	s, rec := newTestSigner(t, st)

	s.Tick(ctx)
	if st.inserts != 0 || len(rec.events) != 0 {
		t.Fatalf("idle start wrote %d checkpoints, %d rows", st.inserts, len(rec.events))
	}
	st.appendRows(2) // seq 6, 7
	s.Tick(ctx)
	if st.inserts != 1 || st.cps[0].SeqLo != 6 || st.cps[0].SeqHi != 7 {
		t.Fatalf("first checkpoint = %+v, want seq 6-7", st.cps)
	}
	if len(rec.events) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Action != "audit.checkpoint.create" || ev.ActorType != types.ActorSystem || ev.Actor != "wardynd" ||
		ev.Target != "audit_checkpoints" || ev.Outcome != "success" || ev.RunID != nil {
		t.Fatalf("checkpoint row = %+v", ev)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"seq_lo", "seq_hi", "rows", "head_hash", "digest", "seq_digest", "kid", "public_key", "statement", "signature"} {
		if _, ok := data[k]; !ok {
			t.Errorf("checkpoint row data lacks %q", k)
		}
	}
	if data["statement"] != st.cps[0].Statement || data["signature"] != b64(st.cps[0].Signature) || data["public_key"] != b64(s.Key.PublicKey()) {
		t.Error("checkpoint row does not carry the stored statement, signature and public key")
	}

	// The only new row is the checkpoint's own: nothing is signed, so an idle install writes nothing.
	s.Tick(ctx)
	s.Tick(ctx)
	if st.inserts != 1 || len(rec.events) != 1 {
		t.Fatalf("own row alone made %d checkpoints, %d rows", st.inserts, len(rec.events))
	}
	st.appendRows(1)
	s.Tick(ctx)
	if st.inserts != 2 || st.cps[1].SeqLo != 8 || st.cps[1].SeqHi != 9 {
		t.Fatalf("second checkpoint = %+v, want seq 8-9 (own row included)", st.cps)
	}
	checkSigned(t, s, st)
	snap := s.Status.Snapshot()
	if !snap.LastSigned.Equal(t0) || snap.Failures[ReasonChainBreak] != 0 {
		t.Errorf("status = %+v", snap)
	}

	// A new process (or another replica) chains to the stored checkpoint, not to the chain head.
	s2, _ := newTestSigner(t, st)
	s2.Key = s.Key
	s2.Record = rec
	st.appendRows(3)
	s2.Tick(ctx)
	if st.inserts != 3 || st.cps[2].SeqLo != 10 {
		t.Fatalf("restart did not continue from the stored checkpoint: %+v", st.cps)
	}
	checkSigned(t, s, st)
}

func TestCheckpointLoop_CapsTenThousandPerCheckpoint(t *testing.T) {
	st := &fakeStore{}
	s, rec := newTestSigner(t, st)
	s.Tick(context.Background()) // fixes the start at an empty chain
	st.appendRows(25001)
	s.Tick(context.Background())
	if len(st.cps) != 3 {
		t.Fatalf("checkpoints = %d, want 3", len(st.cps))
	}
	// The first two checkpoints' own audit rows (25002, 25003) are new rows by the third: it covers them too.
	for i, want := range [][2]int64{{1, 10000}, {10001, 20000}, {20001, 25003}} {
		if st.cps[i].SeqLo != want[0] || st.cps[i].SeqHi != want[1] {
			t.Errorf("checkpoint %d = %d-%d, want %d-%d", i, st.cps[i].SeqLo, st.cps[i].SeqHi, want[0], want[1])
		}
	}
	if len(rec.events) != 3 {
		t.Errorf("audit rows = %d, want one per checkpoint", len(rec.events))
	}
	checkSigned(t, s, st)
}

func TestCheckpointLoop_LatchesOnBreakWritesNoAuditRow(t *testing.T) {
	ctx := context.Background()
	for name, breakIt := range map[string]func(st *fakeStore){
		"edited row":   func(st *fakeStore) { st.rows[len(st.rows)-2].prev = fakeHash(999) },
		"hashless row": func(st *fakeStore) { st.rows[len(st.rows)-2].hash = "" },
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			st.appendRows(2)
			s, rec := newTestSigner(t, st)
			s.Tick(ctx)
			st.appendRows(3)
			breakIt(st)
			s.Tick(ctx)
			if st.inserts != 0 || len(rec.events) != 0 {
				t.Fatalf("a break wrote %d checkpoints, %d audit rows", st.inserts, len(rec.events))
			}
			if got := s.Status.Snapshot().Failures[ReasonChainBreak]; got != 1 {
				t.Errorf("chain_break = %d, want 1", got)
			}
			// Latched for the process lifetime: a repaired chain is not signed by this process.
			st.rows = st.rows[:2]
			st.appendRows(3)
			s.Tick(ctx)
			if st.inserts != 0 || s.Status.Snapshot().Failures[ReasonChainBreak] != 1 {
				t.Fatalf("a latched signer signed again (%d) or counted twice", st.inserts)
			}
		})
	}
}

func TestCheckpointLoop_ForeignRowLatches(t *testing.T) {
	ctx := context.Background()
	other := mustLoad(t, mustNewKey(t))
	forged := func(st *fakeStore, lo, hi int64) Checkpoint {
		stm := auditreceipt.Statement{Format: auditreceipt.CheckpointFormat, Kid: other.KeyID(), SeqLo: lo, SeqHi: hi,
			Rows: hi - lo + 1, HeadHash: fakeHash(hi), Digest: fakeHash(1), SeqDigest: fakeHash(2), SignedAt: t0.Format(time.RFC3339Nano)}
		text := string(stm.Bytes())
		return Checkpoint{SeqLo: lo, SeqHi: hi, Kid: other.KeyID(), Statement: text, Signature: other.Sign([]byte(text))}
	}

	t.Run("latest checkpoint signed by another key", func(t *testing.T) {
		st := &fakeStore{}
		s, rec := newTestSigner(t, st)
		s.Tick(ctx)
		st.appendRows(4)
		st.cps = append(st.cps, forged(st, 1, 4))
		st.appendRows(1)
		s.Tick(ctx)
		if st.inserts != 0 || len(rec.events) != 0 || s.Status.Snapshot().Failures[ReasonForeignRow] != 1 {
			t.Fatalf("chained to a foreign checkpoint: inserts=%d rows=%d status=%+v", st.inserts, len(rec.events), s.Status.Snapshot())
		}
	})
	t.Run("conflicting row signed by another key", func(t *testing.T) {
		st := &fakeStore{}
		s, rec := newTestSigner(t, st)
		s.Tick(ctx)
		st.appendRows(4)
		// Inside the range the signer is about to write, above Latest's view: Insert answers ErrExists.
		s.Store = conflictOnce{st, forged(st, 2, 3)}
		s.Tick(ctx)
		if st.inserts != 0 || len(rec.events) != 0 || s.Status.Snapshot().Failures[ReasonForeignRow] != 1 {
			t.Fatalf("foreign conflict not latched: inserts=%d rows=%d status=%+v", st.inserts, len(rec.events), s.Status.Snapshot())
		}
	})
	t.Run("conflicting row this key signed is another replica's", func(t *testing.T) {
		st := &fakeStore{}
		s, rec := newTestSigner(t, st)
		s.Tick(ctx)
		st.appendRows(4)
		twin, _ := newTestSigner(t, st)
		twin.Key, twin.start = s.Key, s.start
		twin.Tick(ctx) // the other replica wins this range
		s.Store = conflictOnce{st, st.cps[0]}
		s.Tick(ctx)
		if s.Status.Snapshot().Failures[ReasonForeignRow] != 0 || len(rec.events) != 0 {
			t.Fatalf("a valid conflicting checkpoint latched the signer: %+v", s.Status.Snapshot())
		}
	})
}

// conflictOnce is a store whose Latest misses a checkpoint and whose Insert then hits it: the race with
// another writer between the two reads.
type conflictOnce struct {
	*fakeStore
	hit Checkpoint
}

func (c conflictOnce) Latest(context.Context) (Checkpoint, bool, error) {
	return Checkpoint{}, false, nil
}
func (c conflictOnce) Insert(context.Context, Checkpoint) (Checkpoint, error) {
	return c.hit, ErrExists
}

func TestCheckpointLoop_WriteFailureRetriesNextTick(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	s, _ := newTestSigner(t, st)
	s.Tick(ctx)
	st.appendRows(2)
	st.insertErr = errors.New("permission denied for table audit_checkpoints")
	s.Tick(ctx)
	if got := s.Status.Snapshot().Failures[ReasonWrite]; got != 1 {
		t.Fatalf("write failures = %d, want 1", got)
	}
	st.insertErr = nil
	s.Tick(ctx)
	if st.inserts != 1 {
		t.Fatalf("a write failure latched the signer: inserts=%d", st.inserts)
	}
	checkSigned(t, s, st)
}

func TestCheckpointLoop_OneSignerUnderLockClass(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	s, _ := newTestSigner(t, st)
	s.Tick(ctx)
	st.appendRows(2)
	_, unlock, ok, err := s.Locker.TryLock(ctx, db.NewLockKey(db.AuditCheckpointLockClass))
	if err != nil || !ok {
		t.Fatalf("hold the checkpoint lock: %v %v", ok, err)
	}
	s.Tick(ctx)
	if st.inserts != 0 {
		t.Fatal("signed while another signer held the checkpoint lock")
	}
	unlock()
	s.Tick(ctx)
	if st.inserts != 1 {
		t.Fatalf("inserts after the lock was free = %d, want 1", st.inserts)
	}
}

func TestStatus_KeyUnavailable(t *testing.T) {
	var s Status
	s.KeyUnavailable()
	snap := s.Snapshot()
	if snap.Failures[ReasonKeyUnavailable] != 1 {
		t.Fatalf("key_unavailable = %d", snap.Failures[ReasonKeyUnavailable])
	}
	for _, r := range Reasons {
		if _, ok := snap.Failures[r]; !ok {
			t.Errorf("snapshot lacks a zero series for %q", r)
		}
	}
}

// A checkpoint whose own audit row was spooled has no row in the chain: the next lone real row is signed.
func TestCheckpointLoop_SpooledOwnRowDoesNotHideARealRow(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	s, _ := newTestSigner(t, st)
	rec := &spoolingRecorder{}
	s.Record = rec
	s.Tick(ctx)
	st.appendRows(2)
	s.Tick(ctx)
	if st.inserts != 1 || len(rec.events) != 1 {
		t.Fatalf("first checkpoint: inserts=%d rows=%d", st.inserts, len(rec.events))
	}
	st.appendRows(1) // a real event, alone in the range
	s.Tick(ctx)
	if st.inserts != 2 || st.cps[1].SeqLo != 3 || st.cps[1].SeqHi != 3 {
		t.Fatalf("the lone real row after a spooled own row was not signed: %+v", st.cps)
	}
	checkSigned(t, s, st)
}

// The newest checkpoint is another replica's: this signer's own row is no longer the tail, and a lone later
// real event is signed.
func TestCheckpointLoop_OtherSignerAdvancedSignsLoneRow(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	a, _ := newTestSigner(t, st)
	a.Tick(ctx)
	st.appendRows(2)
	a.Tick(ctx) // C1 over seq 1-2; its own row lands at seq 3
	b, _ := newTestSigner(t, st)
	b.Key, b.Record = a.Key, failingRecorder{}
	b.Tick(ctx) // C2 over seq 3 (b has no own row to skip); its own row is lost
	if st.inserts != 2 || st.cps[1].SeqLo != 3 {
		t.Fatalf("replica b did not advance: %+v", st.cps)
	}
	st.appendRows(1) // seq 4, a real event alone after C2
	a.Tick(ctx)
	if st.inserts != 3 || st.cps[2].SeqLo != 4 || st.cps[2].SeqHi != 4 {
		t.Fatalf("a lone real row after another replica's checkpoint was not signed: %+v", st.cps)
	}
	checkSigned(t, a, st)
}

// retireScenario signs a checkpoint with a key, then rotates with -retire-previous and returns the signer now
// holding the new key, the retired key (a thief's) and the store.
func retireScenario(t *testing.T) (*Signer, *Key, *fakeStore) {
	t.Helper()
	ctx := context.Background()
	st := &fakeStore{}
	raw := mustNewKey(t)
	s, _ := newTestSigner(t, st)
	old := mustLoad(t, raw)
	s.Key = old
	s.Tick(ctx)
	st.appendRows(3)
	s.Tick(ctx) // C1 over seq 1-3 by the old key; its own audit row lands at seq 4
	if st.inserts != 1 {
		t.Fatalf("setup: inserts=%d, want the first checkpoint", st.inserts)
	}
	rotated, err := Rotate(raw, true, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	s.Key = mustLoad(t, rotated)
	return s, old, st
}

// After -retire-previous the old key is treated as stolen. A checkpoint it signs, inserted by someone with
// database write access, is not this install's: the signer does not chain to it. It counts a foreign row, logs
// it, and starts a new genesis from the chain head, and it keeps signing (a legitimate retire must not stop it).
func TestCheckpointLoop_RetiredKeyCheckpointIsNotChained(t *testing.T) {
	ctx := context.Background()
	s, old, st := retireScenario(t)
	st.appendRows(2) // seq 5, 6
	forgedStm := auditreceipt.Statement{Format: auditreceipt.CheckpointFormat, Kid: old.KeyID(), SeqLo: 5, SeqHi: 6, Rows: 2,
		HeadHash: fakeHash(6), Digest: fakeHash(1), SeqDigest: fakeHash(2), SignedAt: t0.Format(time.RFC3339Nano)}
	text := string(forgedStm.Bytes())
	st.cps = append(st.cps, Checkpoint{SeqLo: 5, SeqHi: 6, Kid: old.KeyID(), Statement: text, Signature: old.Sign([]byte(text))})
	st.appendRows(1) // seq 7

	s.Tick(ctx)
	if got := s.Status.Snapshot().Failures[ReasonForeignRow]; got != 1 {
		t.Fatalf("foreign_row = %d after a retired-key checkpoint, want 1", got)
	}
	if s.latched {
		t.Fatalf("the signer latched: a retire must not stop signing")
	}
	s.Tick(ctx) // nothing new above the new genesis point
	st.appendRows(2)
	s.Tick(ctx)
	if st.inserts != 2 {
		t.Fatalf("inserts = %d, want a new checkpoint over the rows after the chain head", st.inserts)
	}
	got := st.cps[len(st.cps)-1]
	stm, err := auditreceipt.ParseStatement(got.Statement)
	if err != nil {
		t.Fatal(err)
	}
	if stm.Kid != s.Key.KeyID() || stm.Prev != "" || stm.SeqLo != 8 {
		t.Fatalf("new checkpoint = %+v, want the new key, an empty prev (a new genesis) and seq_lo 8", stm)
	}
	if stm.Prev == auditreceipt.StatementHash(text) {
		t.Fatalf("the signer chained to the retired key's checkpoint")
	}
	// One alarm for the one checkpoint, however many ticks follow; and the next checkpoint chains normally.
	st.appendRows(2)
	s.Tick(ctx)
	if st.inserts != 3 || s.Status.Snapshot().Failures[ReasonForeignRow] != 1 {
		t.Fatalf("inserts=%d foreign_row=%d, want a third checkpoint and still one alarm", st.inserts, s.Status.Snapshot().Failures[ReasonForeignRow])
	}
	third, _ := auditreceipt.ParseStatement(st.cps[len(st.cps)-1].Statement)
	if third.Prev != auditreceipt.StatementHash(got.Statement) {
		t.Fatalf("the third checkpoint does not chain to the new genesis")
	}
}

// forgedBy is a checkpoint over lo..hi that key signs, as a thief holding it would insert one.
func forgedBy(key *Key, lo, hi int64) Checkpoint {
	stm := auditreceipt.Statement{Format: auditreceipt.CheckpointFormat, Kid: key.KeyID(), SeqLo: lo, SeqHi: hi, Rows: hi - lo + 1,
		HeadHash: fakeHash(hi), Digest: fakeHash(1), SeqDigest: fakeHash(2), SignedAt: t0.Format(time.RFC3339Nano)}
	text := string(stm.Bytes())
	return Checkpoint{SeqLo: lo, SeqHi: hi, Kid: key.KeyID(), Statement: text, Signature: key.Sign([]byte(text))}
}

// restartedAfterRetire is a new process (a new Signer: nothing remembered) holding the rotated key over the
// store the old key's signer left behind.
func restartedAfterRetire(t *testing.T) (*Signer, *Key, *fakeStore) {
	t.Helper()
	ctx := context.Background()
	st := &fakeStore{}
	raw := mustNewKey(t)
	a, _ := newTestSigner(t, st)
	old := mustLoad(t, raw)
	a.Key = old
	a.Tick(ctx)
	st.appendRows(3)
	a.Tick(ctx) // C1 over seq 1-3 by the old key; its own audit row lands at seq 4
	rotated, err := Rotate(raw, true, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	b, _ := newTestSigner(t, st)
	b.Key = mustLoad(t, rotated)
	return b, old, st
}

// A legitimate retire: the old key signed the newest checkpoint before this process took over. That checkpoint
// is already there at the first read, so the signer chains onto it: no foreign row, no new genesis, no gap.
func TestCheckpointLoop_LegitimateRetireChainsWithoutAlarm(t *testing.T) {
	ctx := context.Background()
	b, _, st := restartedAfterRetire(t)
	c1 := st.cps[0]
	st.appendRows(2) // seq 5, 6 (seq 4 is the old signer's own audit row)
	b.Tick(ctx)
	if st.inserts != 2 || b.latched {
		t.Fatalf("inserts=%d latched=%v, want a checkpoint after the retire", st.inserts, b.latched)
	}
	c2, err := auditreceipt.ParseStatement(st.cps[1].Statement)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Kid != b.Key.KeyID() || c2.Prev != auditreceipt.StatementHash(c1.Statement) || c2.SeqLo != c1.SeqHi+1 {
		t.Fatalf("checkpoint after the retire = %+v, want the new key, chained to the old key's, with no gap", c2)
	}
	for i := 0; i < 2; i++ {
		st.appendRows(2)
		b.Tick(ctx)
	}
	if n := b.Status.Snapshot().Failures[ReasonForeignRow]; n != 0 || st.inserts < 4 {
		t.Fatalf("foreign_row=%d inserts=%d, want no alarm and signing to continue", n, st.inserts)
	}
}

// A forgery that appears after the legitimate chain (newer than the first read and than this signer's own
// checkpoint) alarms once, does not latch, and signing starts a new genesis.
func TestCheckpointLoop_ForgeryAfterLegitimateChainStillAlarms(t *testing.T) {
	ctx := context.Background()
	b, old, st := restartedAfterRetire(t)
	st.appendRows(2)
	b.Tick(ctx) // chains onto the old key's checkpoint
	if st.inserts != 2 {
		t.Fatalf("setup: inserts=%d", st.inserts)
	}
	head := st.rows[len(st.rows)-1].seq
	st.appendRows(2)
	st.cps = append(st.cps, forgedBy(old, head+1, head+2))
	st.appendRows(1)
	b.Tick(ctx)
	if n := b.Status.Snapshot().Failures[ReasonForeignRow]; n != 1 || b.latched {
		t.Fatalf("foreign_row=%d latched=%v after a retired-key checkpoint inserted later, want one alarm and no latch", n, b.latched)
	}
	st.appendRows(2)
	b.Tick(ctx)
	last, err := auditreceipt.ParseStatement(st.cps[len(st.cps)-1].Statement)
	if err != nil {
		t.Fatal(err)
	}
	if st.inserts != 3 || last.Prev != "" || last.Kid != b.Key.KeyID() {
		t.Fatalf("inserts=%d last=%+v, want a new genesis by the new key", st.inserts, last)
	}
}
