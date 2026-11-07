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
		seq, prev := int64(1), ""
		if l := len(s.rows); l > 0 {
			seq, prev = s.rows[l-1].seq+1, s.rows[l-1].hash
		}
		s.rows = append(s.rows, fakeRow{seq: seq, prev: prev, hash: fakeHash(seq)})
	}
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
	r.st.appendRows(1)
	return nil
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
	for i, want := range [][2]int64{{1, 10000}, {10001, 20000}, {20001, 25001}} {
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

func (c conflictOnce) Latest(context.Context) (Checkpoint, bool, error) { return Checkpoint{}, false, nil }
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
