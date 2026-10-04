// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// gapCovMaskBackend is a secretmask.Backend that records the retention calls and
// answers each as told.
type gapCovMaskBackend struct {
	purgeErr, sweepErr, freshErr error
	// mergeErr fails a PutGlobal that merges (a credential's first record); addErr one that retires.
	mergeErr, addErr, putRunErr error

	purged      [][]uuid.UUID
	sweepCutoff []time.Time
}

func (b *gapCovMaskBackend) PutRun(uuid.UUID, []byte) error { return b.putRunErr }
func (b *gapCovMaskBackend) PutGlobal(_, _ string, _ []secretmask.GlobalPut, merge bool, _ time.Time) error {
	if merge {
		return b.mergeErr
	}
	return b.addErr
}
func (b *gapCovMaskBackend) EvictGlobal(string, string, time.Time) error { return nil }
func (b *gapCovMaskBackend) SweepGlobals(_ context.Context, cutoff time.Time) (int, error) {
	b.sweepCutoff = append(b.sweepCutoff, cutoff)
	return 0, b.sweepErr
}
func (b *gapCovMaskBackend) PersistedRuns(context.Context) ([]uuid.UUID, error) { return nil, nil }
func (b *gapCovMaskBackend) PurgeRuns(_ context.Context, runs []uuid.UUID) error {
	b.purged = append(b.purged, runs)
	return b.purgeErr
}
func (b *gapCovMaskBackend) EraseOwner(context.Context, string) (int, error) { return 0, nil }
func (b *gapCovMaskBackend) Fresh(context.Context, time.Time) error          { return b.freshErr }

func gapCovMaskServer(b *gapCovMaskBackend, now time.Time) *Server {
	reg := secretmask.NewRegistry()
	reg.SetBackend(b)
	return &Server{cfg: Config{MaskRegistry: reg, Now: func() time.Time { return now }}}
}

func TestGapCovSweepCommittedMasks(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cold := []uuid.UUID{uuid.MustParse("00000000-0000-0000-0000-0000000000c1")}
	purgeErr, sweepErr := errors.New("gapcov: purge failed"), errors.New("gapcov: sweep failed")

	t.Run("a follower sweeps nothing", func(t *testing.T) {
		b := &gapCovMaskBackend{purgeErr: purgeErr, sweepErr: sweepErr}
		if err := gapCovMaskServer(b, now).sweepCommittedMasks(t.Context(), false, cold); err != nil {
			t.Fatalf("follower err = %v, want nil", err)
		}
		if len(b.purged) != 0 || len(b.sweepCutoff) != 0 {
			t.Fatalf("a follower purged %v and swept %v", b.purged, b.sweepCutoff)
		}
	})
	t.Run("the leader purges the cold runs and sweeps past the grace", func(t *testing.T) {
		b := &gapCovMaskBackend{}
		if err := gapCovMaskServer(b, now).sweepCommittedMasks(t.Context(), true, cold); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(b.purged) != 1 || b.purged[0][0] != cold[0] {
			t.Fatalf("purged %v, want the cold run", b.purged)
		}
		if len(b.sweepCutoff) != 1 || !b.sweepCutoff[0].Equal(now.Add(-RunSecretGrace)) {
			t.Fatalf("sweep cutoff %v, want now minus the grace", b.sweepCutoff)
		}
	})
	t.Run("a failed sweep still follows a successful purge and is returned", func(t *testing.T) {
		b := &gapCovMaskBackend{sweepErr: sweepErr}
		err := gapCovMaskServer(b, now).sweepCommittedMasks(t.Context(), true, cold)
		if !errors.Is(err, sweepErr) || len(b.purged) != 1 {
			t.Fatalf("err = %v after %d purge(s), want the sweep error after one", err, len(b.purged))
		}
	})
	t.Run("both failures are reported together", func(t *testing.T) {
		b := &gapCovMaskBackend{purgeErr: purgeErr, sweepErr: sweepErr}
		err := gapCovMaskServer(b, now).sweepCommittedMasks(t.Context(), true, cold)
		if !errors.Is(err, purgeErr) || !errors.Is(err, sweepErr) {
			t.Fatalf("err = %v, want both the purge and the sweep error", err)
		}
		if len(b.sweepCutoff) != 1 {
			t.Fatalf("the sweep ran %d time(s) after a failed purge, want once", len(b.sweepCutoff))
		}
	})
}

// A registry that cannot prove its cache whole replaces the chunk with the
// placeholder, forwards none of it, and marks the capture dropped and uncovered.
func TestGapCovLiveMaskWriterReplacesAChunkItCannotVouchFor(t *testing.T) {
	b := &gapCovMaskBackend{freshErr: errors.New("gapcov: corpus unreadable")}
	reg := secretmask.NewRegistry()
	reg.SetBackend(b)
	var dst bytes.Buffer
	w := &liveMaskWriter{reg: reg, runID: uuid.New(), dst: &dst, tail: []byte("withheld")}

	n, err := w.Write([]byte("an unvetted chunk"))
	if err != nil || n != len("an unvetted chunk") {
		t.Fatalf("Write = %d, %v; want the chunk reported accepted", n, err)
	}
	if !bytes.Equal(dst.Bytes(), secretmask.Placeholder()) {
		t.Fatalf("destination got %q, want only the placeholder", dst.Bytes())
	}
	if w.tail != nil || !w.capture.dropped || !w.capture.uncovered {
		t.Fatalf("tail %q dropped %v uncovered %v, want the tail cleared and the capture marked", w.tail, w.capture.dropped, w.capture.uncovered)
	}
}
