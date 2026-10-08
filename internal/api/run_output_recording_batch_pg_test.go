// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func TestRecordingOutputPG_ManyEventsMaskAcrossBatchAndPartBoundary(t *testing.T) {
	l := newMaskLab(t)
	a := l.recordingReplica()
	run := l.run()
	a.dispatch(t, run, "part-split-secret", "event-split-secret")
	run = l.recordingRunEnded(run)
	const prefix = "part-split-"
	// Exactly one batch, ending in a secret prefix: the next part must flush
	// this batch and join the withheld prefix to its first decoded payload.
	first := partsHeader + strings.Repeat(`[0,"o","`+strings.Repeat("x", 512)+`"]`+"\n", 1023) +
		`[0,"o","` + strings.Repeat("x", 512-len(prefix)) + prefix + `"]` + "\n"
	saveOutputCast(t, a.srv.cfg.RecordingStore, run.ID, first)
	second := partsHeader + `[1,"o","secret "]` + "\n" + `[2,"o","event-split-"]` + "\n" +
		`[3,"o","secret FINAL-MANY-EVENTS\n"]` + "\n"
	if err := a.srv.cfg.RecordingStore.SaveCastNamed(t.Context(), run.ID.String(), recording.PartSuffix(2), strings.NewReader(second)); err != nil {
		t.Fatal(err)
	}
	b := l.recordingReplica()
	b.srv.cfg.RunOutputTailBytes = 128
	start := time.Now()
	b.srv.FinishRunOutput(t.Context(), run.ID)
	if elapsed := time.Since(start); elapsed >= b.srv.outputRecoverWait() {
		t.Fatalf("1,027 events exceeded the recovery budget: %s", elapsed)
	}
	var raw []byte
	var truncated, incomplete, gap bool
	var source, scope string
	if err := l.pool.QueryRow(t.Context(), `SELECT output, truncated, incomplete, capture_gap, source, mask_scope
		FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, run.ID).
		Scan(&raw, &truncated, &incomplete, &gap, &source, &scope); err != nil {
		t.Fatal(err)
	}
	want := "<secret-hidden> <secret-hidden> FINAL-MANY-EVENTS\n"
	want = strings.Repeat("x", 128-len(want)) + want
	if string(raw) != want || !truncated || !incomplete || gap || source != recordingOutputSource || scope != "run" {
		t.Fatalf("stored BYTEA %q truncated=%v incomplete=%v gap=%v source=%s scope=%s", raw, truncated, incomplete, gap, source, scope)
	}
	if l.count(`SELECT count(*) FROM run_output_recording_recovery
		WHERE run_id=$1 AND requested_generation=completed_generation AND claim_token IS NULL`, run.ID) != 1 {
		t.Fatal("successful recovery left a retry obligation")
	}
}

func TestRecordingOutputPG_BatchesRefreshNewGlobalBeforeMasking(t *testing.T) {
	l := newMaskLab(t)
	a := l.recordingReplica()
	run := l.run()
	a.dispatch(t, run)
	run = l.recordingRunEnded(run)
	source := a.srv.cfg.RecordingStore
	saveOutputCast(t, source, run.ID, partsHeader+`[0,"o","`+strings.Repeat("x", maskPipeMax)+`"]`+"\n"+
		`[1,"o"," fresh-global-"]`+"\n")
	if err := source.SaveCastNamed(t.Context(), run.ID.String(), recording.PartSuffix(2),
		strings.NewReader(partsHeader+`[2,"o","secret FINAL-NEW-GLOBAL\n"]`+"\n")); err != nil {
		t.Fatal(err)
	}
	b := l.recordingReplica()
	b.srv.cfg.RunOutputTailBytes = 128
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); b.srv.FinishRunOutput(ctx, run.ID) }()
	awaitRecordingSignal(t, opened)
	generation, err := a.reg.GlobalGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.reg.AddGlobalUntil(generation, maskOwner, "recording-batch-refresh", time.Now(), time.Now().Add(time.Hour), []byte("fresh-global-secret")); err != nil {
		t.Fatal(err)
	}
	close(release)
	awaitRecordingSignal(t, done)
	out, gap, incomplete, scope := l.storedRow(run.ID)
	want := " <secret-hidden> FINAL-NEW-GLOBAL\n"
	want = strings.Repeat("x", 128-len(want)) + want
	if out != want || gap || !incomplete || scope == nil || *scope != "run" {
		t.Fatalf("batch used stale registry: %q gap=%v incomplete=%v scope=%v", out, gap, incomplete, scope)
	}
}
