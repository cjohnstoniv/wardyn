// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/google/uuid"
)

// each checks a replayed batch row by row, failing the batch at the first error.
func each(fn func(Record) error) func(context.Context, []Record) error {
	return func(_ context.Context, rows []Record) error {
		for _, r := range rows {
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	}
}

func testSpool(t *testing.T) (*Spool, string, uuid.UUID) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spool")
	id := uuid.New()
	s, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir, id
}
func decision(id uuid.UUID) egress.DecisionLog {
	return egress.DecisionLog{Request: egress.Request{RunID: id, Host: "example.test", Time: time.Now().UTC()}, Decision: egress.Allow, RuleSource: "policy:allowed"}
}

func TestReplayCrashRetainsUnacknowledgedBatch(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	for range 3 {
		if err := s.Append(ctx, decision(id)); err != nil {
			t.Fatal(err)
		}
	}
	received := map[string]int{}
	lostACK := errors.New("crashed before durable ACK")
	if err := s.Replay(ctx, each(func(r Record) error {
		received[r.Hash]++
		if r.Seq == 2 {
			return lostACK
		}
		return nil
	})); !errors.Is(err, lostACK) {
		t.Fatal(err)
	}
	pos, err := s.Get(ctx)
	if err != nil || pos.Seq != 0 {
		t.Fatalf("cursor advanced without ACK: %+v %v", pos, err)
	}
	reopened, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var seqs []int64
	if err := reopened.Replay(ctx, each(func(r Record) error { received[r.Hash]++; seqs = append(seqs, r.Seq); return nil })); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[2] != 3 || len(received) != 3 {
		t.Fatalf("replay changed stable ordered identities: %v %v", seqs, received)
	}
	if depth, b, _, err := reopened.Stats(ctx); err != nil || depth != 0 || b != 0 {
		t.Fatalf("retained acknowledged payload: %d %d %v", depth, b, err)
	}
	if err := reopened.Replay(ctx, func(context.Context, []Record) error { t.Fatal("replayed acknowledged row"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Append(ctx, decision(id)); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Replay(ctx, each(func(r Record) error {
		if r.Seq != 4 {
			t.Fatal(r.Seq)
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentProcessHandlesPreserveEveryRow(t *testing.T) {
	ctx := context.Background()
	a, dir, id := testSpool(t)
	b, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for _, s := range []*Spool{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				if err := s.Append(ctx, decision(id)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	var seq int64
	if err := a.Replay(ctx, each(func(r Record) error {
		seq++
		if r.Seq != seq {
			t.Fatalf("sequence %d after %d", r.Seq, seq-1)
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if seq != 60 {
		t.Fatal(seq)
	}
}

func TestByteBoundOverflowFirstStableAndCounted(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	log := decision(id)
	log.Request.Path = strings.Repeat("x", MaxRecordBytes-1024)
	count := 0
	for {
		err := s.Append(ctx, log)
		if errors.Is(err, ErrFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count < 30 || count > 40 {
		t.Fatalf("unexpected byte limit count %d", count)
	}
	if err := s.Append(ctx, decision(id)); !errors.Is(err, ErrFull) {
		t.Fatalf("overflow did not latch: %v", err)
	}
	first, err := s.Overflow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Count != 2 || first.DropEnd != 2 || first.Decision.RuleSource != "builtin:spool-full" {
		t.Fatalf("summary %+v", first)
	}
	reopened, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Overflow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Hash != first.Hash {
		t.Fatal("overflow identity changed on restart")
	}
	sent := 0
	if err := reopened.Replay(ctx, each(func(r Record) error {
		if sent == 0 && r.Hash != first.Hash {
			t.Fatal("overflow was not first")
		}
		sent++
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if sent != count+1 {
		t.Fatal(sent, count)
	}
	if err := reopened.Append(ctx, decision(id)); err != nil {
		t.Fatalf("ACK did not reopen admission: %v", err)
	}
}

func TestRowBoundIsIndependentOfBytes(t *testing.T) {
	ctx := context.Background()
	s, _, id := testSpool(t)
	// Materialize the real row ceiling without 100,000 fsync calls; each fixture
	// row has the same stable wire identity and manifest accounting as Append.
	if err := s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		for n := int64(1); n <= MaxRecords; n++ {
			r := Record{SpoolID: m.SpoolID, Seq: n, Decision: decision(id)}
			r.Hash = r.digest()
			b, err := r.Marshal()
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(s.root.Name(), recordName(n)), b, 0600); err != nil {
				return err
			}
			m.Bytes += int64(len(b))
		}
		m.Last = MaxRecords
		return s.writeManifest(m)
	}); err != nil {
		t.Fatal(err)
	}
	depth, b, _, err := s.Stats(ctx)
	if err != nil || depth != MaxRecords || b >= MaxBytes {
		t.Fatalf("fixture %d %d %v", depth, b, err)
	}
	if err := s.Append(ctx, decision(id)); !errors.Is(err, ErrFull) {
		t.Fatalf("row ceiling bypass: %v", err)
	}
	if _, err := s.root.Stat(recordName(MaxRecords + 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overflow row created: %v", err)
	}
}

func TestRecoveryReclaimsOnlyDurablyAcknowledgedRows(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	if err := s.Append(ctx, decision(id)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.After(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.locked(ctx, func() error {
		m, e := s.readManifest()
		if e != nil {
			return e
		}
		m.Ack = rows[0].Position()
		m.Bytes = 0
		return s.writeManifest(m)
	}); err != nil {
		t.Fatal(err)
	}
	// Simulates process death after the cursor fsync and before file deletion.
	reopened, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.root.Stat(recordName(1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acknowledged file leaked: %v", err)
	}
	if err := reopened.Append(ctx, decision(id)); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Replay(ctx, each(func(r Record) error {
		if r.Seq != 2 {
			t.Fatal(r.Seq)
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolRefusesCorruptionAndUnsafeState(t *testing.T) {
	for _, kind := range []string{"hash", "manifest", "cross-run", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, id := testSpool(t)
			ctx := context.Background()
			if err := s.Append(ctx, decision(id)); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "hash":
				if err := os.WriteFile(filepath.Join(dir, recordName(1)), []byte(`{"seq":1}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := s.Replay(ctx, func(context.Context, []Record) error { t.Fatal("corrupt row sent"); return nil }); err == nil {
					t.Fatal("corrupt row accepted")
				}
			case "manifest":
				if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
					t.Fatal(err)
				}
				if got, err := Open(dir, id); err == nil {
					got.Close()
					t.Fatal("reset missing metadata")
				}
			case "cross-run":
				if got, err := Open(dir, uuid.New()); err == nil {
					got.Close()
					t.Fatal("cross-run accepted")
				}
			case "symlink":
				if err := os.Remove(filepath.Join(dir, "lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("state.json", filepath.Join(dir, "lock")); err != nil {
					t.Fatal(err)
				}
				if got, err := Open(dir, id); err == nil {
					got.Close()
					t.Fatal("symlink lock accepted")
				}
			}
		})
	}
}
