// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (l *maskLab) recordingReplica() replica {
	l.t.Helper()
	rp := l.replica()
	rp.srv.cfg.Runner = &outputRunner{fakeRunner: &fakeRunner{execOutputUncaptured: true}}
	rp.srv.cfg.RecordingStore = recording.NewPGStore(l.pool)
	return rp
}

func (l *maskLab) recordingRunEnded(run types.AgentRun) types.AgentRun {
	l.t.Helper()
	if _, err := l.pool.Exec(l.t.Context(), `UPDATE agent_runs SET state='COMPLETED', ended_at=now(), updated_at=now() WHERE id=$1`, run.ID); err != nil {
		l.t.Fatal(err)
	}
	r, err := store.NewPG(l.pool).GetRun(l.t.Context(), run.ID)
	if err != nil {
		l.t.Fatal(err)
	}
	return r
}

func TestRecordingOutputPG_CommittedUploadRestartAndSingleConnection(t *testing.T) {
	lab := newMaskLab(t)
	cfg := lab.pool.Config().Copy()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.Close)
	l := *lab
	l.pool = one
	a := l.recordingReplica()
	run := l.run()
	a.dispatch(t, run, "split-pg-secret-value")
	cast := partsHeader + `[0,"o","before split-pg-"]` + "\n" + `[1,"o","secret-value after\n"]` + "\n"
	if w := l.upload(a, run, cast); w.Code != http.StatusNoContent {
		t.Fatalf("live upload %d %s", w.Code, w.Body)
	}
	if l.count(`SELECT count(*) FROM run_output_recording_recovery WHERE run_id=$1 AND requested_generation>completed_generation`, run.ID) != 1 {
		t.Fatal("acknowledged upload has no durable work")
	}
	if l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1`, run.ID) != 0 {
		t.Fatal("live recording was mislabeled as final stdout")
	}
	run = l.recordingRunEnded(run)
	// No original process participates: the elected sweep finds durable work.
	b := l.recordingReplica()
	if err := b.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, final := l.storedOutput(run.ID)
	if string(raw) != "before <secret-hidden> after\n" || !final {
		t.Fatalf("unmasked/incomplete column %q final=%v", raw, final)
	}
	if _, err := l.pool.Exec(t.Context(), `DELETE FROM run_mask_manifest WHERE run_id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	c := l.recordingReplica()
	c.srv.cfg.RecordingStore = nil
	if code, got, body := l.readOutput(c, run.ID); code != http.StatusOK || got.Source != recordingOutputSource || got.Output != string(raw) || !got.Complete || !got.Incomplete || got.MaskScope != "run" {
		t.Fatalf("fresh/off backend read %d %s", code, body)
	}
}

type failRecordingQueue struct{ store.PG }

func (failRecordingQueue) QueueRecordingRunOutput(context.Context, uuid.UUID, time.Duration, time.Duration, bool) error {
	return errors.New("output work commit unavailable")
}

func TestRecordingOutputPG_UploadReceiptRequiresDurableWorkAndReadRepairsGap(t *testing.T) {
	l := newMaskLab(t)
	a := l.recordingReplica()
	run := l.run()
	a.dispatch(t, run, "receipt-secret-value")
	run = l.recordingRunEnded(run)
	a.srv.cfg.Store = failRecordingQueue{PG: store.NewPG(l.pool)}
	if w := l.upload(a, run, partsHeader+`[0,"o","kept receipt-secret-value\n"]`+"\n"); w.Code != http.StatusInternalServerError {
		t.Fatalf("work failure was acknowledged: %d %s", w.Code, w.Body)
	}
	if l.count(`SELECT count(*) FROM run_output_recording_recovery WHERE run_id=$1`, run.ID) != 0 {
		t.Fatal("work unexpectedly committed")
	}
	rc, err := a.srv.cfg.RecordingStore.OpenCast(t.Context(), run.ID.String())
	if err != nil {
		t.Fatalf("source must already be committed: %v", err)
	}
	_ = rc.Close()
	b := l.recordingReplica()
	if code, got, body := l.readOutput(b, run.ID); code != http.StatusOK || got.Output != "kept <secret-hidden>\n" {
		t.Fatalf("authorized read repair %d %s", code, body)
	}
	// A failed source save never claims it left durable recovery work.
	other := l.run()
	b.dispatch(t, other, "other-secret-value")
	b.srv.cfg.RecordingStore = &fakeRecordingStore{saveErr: errors.New("source failed")}
	if w := l.upload(b, other, partsHeader); w.Code != http.StatusInternalServerError {
		t.Fatalf("source failure %d %s", w.Code, w.Body)
	}
	if l.count(`SELECT count(*) FROM run_output_recording_recovery WHERE run_id=$1`, other.ID) != 0 {
		t.Fatal("failed upload queued recovery")
	}
}

type heldRecordingCommit struct {
	store.PG
	ready, release chan struct{}
	once           sync.Once
}

func (s *heldRecordingCommit) SaveRecordingRunOutput(ctx context.Context, claim store.RecordingOutputClaim, row store.RunOutput, retention time.Duration) (bool, error) {
	s.once.Do(func() { close(s.ready) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return s.PG.SaveRecordingRunOutput(ctx, claim, row, retention)
}

func awaitRecordingSignal(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("recording recovery did not reach the test barrier")
	}
}

func TestRecordingOutputPG_ErasureAndRetentionFenceAlreadyDecodedBytes(t *testing.T) {
	for _, mode := range []string{"recordings", "recordings disabled", "general output", "mask fence", "retention"} {
		t.Run(mode, func(t *testing.T) {
			l := newMaskLab(t)
			a, b := l.recordingReplica(), l.recordingReplica()
			run := l.run()
			a.dispatch(t, run, "fence-secret-value")
			run = l.recordingRunEnded(run)
			saveOutputCast(t, a.srv.cfg.RecordingStore, run.ID, partsHeader+`[0,"o","fence-secret-value kept\n"]`+"\n")
			gate := &heldRecordingCommit{PG: store.NewPG(l.pool), ready: make(chan struct{}), release: make(chan struct{})}
			a.srv.cfg.Store = gate
			a.srv.cfg.RunOutputRetention = 24 * time.Hour
			done := make(chan struct{})
			go func() { defer close(done); a.srv.FinishRunOutput(t.Context(), run.ID) }()
			awaitRecordingSignal(t, gate.ready)
			switch mode {
			case "recordings", "recordings disabled":
				if mode == "recordings disabled" {
					b.srv.cfg.RecordingStore = nil
					b.srv.cfg.RunOutputPersistOff = true
					b.srv.cfg.ExecOutputTailOff = true
				}
				if _, err := b.srv.eraseRecordingsOf(t.Context(), maskOwner); err != nil {
					t.Fatal(err)
				}
			case "general output":
				if err := b.srv.EraseRunOutputs(t.Context(), []uuid.UUID{run.ID}); err != nil {
					t.Fatal(err)
				}
			case "mask fence":
				if _, err := b.srv.cfg.MaskManifests.FenceSubject(t.Context(), maskOwner); err != nil {
					t.Fatal(err)
				}
			case "retention":
				if _, err := l.pool.Exec(t.Context(), `UPDATE agent_runs SET ended_at=now()-interval '2 days' WHERE id=$1`, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			close(gate.release)
			awaitRecordingSignal(t, done)
			if mode == "mask fence" {
				out, gap, incomplete, scope := l.storedRow(run.ID)
				if out != "" || !gap || !incomplete || scope != nil {
					t.Fatalf("mask fence row %q gap=%v incomplete=%v scope=%v", out, gap, incomplete, scope)
				}
			} else if l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1`, run.ID) != 0 {
				t.Fatal("decoded bytes persisted after fence/expiry")
			}
			if strings.HasPrefix(mode, "recordings") && l.count(`SELECT count(*) FROM run_output_recording_recovery WHERE run_id=$1 AND erased_at IS NOT NULL`, run.ID) != 1 {
				t.Fatal("source fence missing")
			}
		})
	}
}

func TestRecordingOutputPG_UncoveredNeverReadsSource(t *testing.T) {
	l := newMaskLab(t)
	a := l.recordingReplica()
	run := l.recordingRunEnded(l.run())
	spy := &outputRecordingStore{Store: a.srv.cfg.RecordingStore, open: func(context.Context, string) (io.ReadCloser, error) {
		t.Error("uncovered recovery touched source")
		return nil, recording.ErrNotFound
	}}
	a.srv.cfg.RecordingStore = spy
	a.srv.FinishRunOutput(t.Context(), run.ID)
	out, gap, incomplete, scope := l.storedRow(run.ID)
	if out != "" || !gap || !incomplete || scope != nil || spy.opens.Load() != 0 {
		t.Fatalf("uncovered row %q gap=%v incomplete=%v scope=%v", out, gap, incomplete, scope)
	}
}

func TestRecordingOutputPG_BackendChangesDoNotWidenErasureClaims(t *testing.T) {
	for _, backend := range []string{"off", "reconfigured", "unsupported"} {
		t.Run(backend, func(t *testing.T) {
			l := newMaskLab(t)
			a := l.recordingReplica()
			fs, err := recording.NewFSStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a.srv.cfg.RecordingStore = fs
			run := l.run()
			a.dispatch(t, run)
			run = l.recordingRunEnded(run)
			saveOutputCast(t, fs, run.ID, partsHeader+`[0,"o","old backend marker"]`+"\n")
			a.srv.FinishRunOutput(t.Context(), run.ID)
			pg := store.NewPG(l.pool)
			independent := map[uuid.UUID]string{}
			for _, source := range []string{"stdout", paneSnapshotSource} {
				id := l.run().ID
				independent[id] = source
				if err := pg.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: id, Source: source, Output: []byte(source)}); err != nil {
					t.Fatal(err)
				}
			}
			b := l.recordingReplica()
			switch backend {
			case "off":
				b.srv.cfg.RecordingStore = nil
				b.srv.cfg.RunOutputPersistOff = true
				b.srv.cfg.ExecOutputTailOff = true
			case "unsupported":
				b.srv.cfg.RecordingStore = &fakeRecordingStore{}
			}
			detail, err := b.srv.eraseRecordingsOf(t.Context(), maskOwner)
			if backend == "unsupported" {
				if !errors.Is(err, erasure.ErrNotAvailable) {
					t.Fatalf("unsupported backend claimed erasure: %v", err)
				}
			} else if err != nil || detail.(map[string]any)["recordings"] != 0 {
				t.Fatalf("claimed old backend erasure: %v %v", detail, err)
			}
			if row, found, err := pg.GetRunOutput(t.Context(), run.ID); err != nil || found || !row.RecordingErased {
				t.Fatalf("derived copy still present: %+v %v %v", row, found, err)
			}
			for id, source := range independent {
				if row, found, err := pg.GetRunOutput(t.Context(), id); err != nil || !found || row.Source != source || string(row.Output) != source {
					t.Fatalf("independent output changed: %+v %v %v", row, found, err)
				}
			}
			rc, err := fs.OpenCast(t.Context(), run.ID.String())
			if err != nil {
				t.Fatalf("disconnected backend was claimed/deleted: %v", err)
			}
			_ = rc.Close()
		})
	}
}

func TestRecordingOutputPG_FreshnessLostBetweenPartsDropsWholeResult(t *testing.T) {
	l := newMaskLab(t)
	a := l.recordingReplica()
	run := l.run()
	a.dispatch(t, run, "part-boundary-secret-value")
	run = l.recordingRunEnded(run)
	source := a.srv.cfg.RecordingStore
	// The next event forces a full masked batch before part 2 loses freshness.
	saveOutputCast(t, source, run.ID, partsHeader+`[0,"o","`+strings.Repeat("x", maskPipeMax)+`"]`+"\n"+
		`[1,"o","part-boundary-"]`+"\n")
	if err := source.SaveCastNamed(t.Context(), run.ID.String(), recording.PartSuffix(2), strings.NewReader(partsHeader+`[1,"o","secret-value"]`+"\n")); err != nil {
		t.Fatal(err)
	}
	maskPool, err := pgxpool.NewWithConfig(t.Context(), l.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(maskPool.Close)
	b := l.replicaMasking(maskPool)
	b.srv.cfg.Runner = a.srv.cfg.Runner
	opened, release := make(chan struct{}), make(chan struct{})
	b.srv.cfg.RecordingStore = &outputRecordingStore{Store: source, open: func(ctx context.Context, key string) (io.ReadCloser, error) {
		rc, err := source.OpenCast(ctx, key)
		if key == recording.CastKey(run.ID.String(), recording.PartSuffix(2)) && err == nil {
			close(opened)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return rc, err
	}}
	done := make(chan struct{})
	go func() { defer close(done); b.srv.FinishRunOutput(t.Context(), run.ID) }()
	awaitRecordingSignal(t, opened)
	maskPool.Close()
	close(release)
	awaitRecordingSignal(t, done)
	out, gap, incomplete, scope := l.storedRow(run.ID)
	if out != "" || !gap || !incomplete || scope != nil {
		t.Fatalf("lost freshness kept %q gap=%v incomplete=%v scope=%v", out, gap, incomplete, scope)
	}
}
