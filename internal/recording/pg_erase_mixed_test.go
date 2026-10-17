// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// gatedReader is a stream (no Len) that serves its bytes only after gate closes.
type gatedReader struct {
	gate    chan struct{}
	started chan struct{}
	r       io.Reader
	once    bool
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if !g.once {
		g.once = true
		close(g.started)
		<-g.gate
	}
	return g.r.Read(p)
}

// One erase meets a stream writer paused mid-read and an in-memory (Len) writer paused at the run's
// lock, both on the same run. Whichever order the lock hands them out in, the run ends erased: no
// stored part survives, each writer returned nil or ErrErased, and no admission slot is left held.
func TestPGStore_EraseWithAPausedStreamAndAnInMemoryWriter(t *testing.T) {
	pool := pgPool(t)
	s := recording.NewPGStore(pgPool(t))
	run := uuid.NewString()
	ctx := t.Context()

	stream := &gatedReader{gate: make(chan struct{}), started: make(chan struct{}), r: strings.NewReader("stream part")}
	streamErr := make(chan error, 1)
	go func() { streamErr <- s.SaveCastNamed(ctx, run, "part-1", stream) }()
	<-stream.started // it holds the run's stream group and a slot, and is paused reading

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, db.RecordingLockClass, run); err != nil {
		t.Fatal(err)
	}
	lenErr := make(chan error, 1)
	go func() { lenErr <- s.SaveCastNamed(ctx, run, "part-2", bytes.NewReader([]byte("in-memory part"))) }()
	eraseErr := make(chan error, 1)
	go func() { _, err := s.DeleteRun(ctx, run); eraseErr <- err }()
	time.Sleep(150 * time.Millisecond) // both are queued behind the lock
	close(stream.gate)
	time.Sleep(50 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	for name, ch := range map[string]chan error{"stream": streamErr, "in-memory": lenErr} {
		select {
		case err := <-ch:
			if err != nil && !errors.Is(err, recording.ErrErased) {
				t.Errorf("%s writer = %v, want nil or ErrErased", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s writer did not return", name)
		}
	}
	select {
	case err := <-eraseErr:
		if err != nil {
			t.Fatalf("erase = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("erase did not return")
	}
	for _, key := range []string{run, run + "~part-1", run + "~part-2"} {
		assertErased(t, s, key)
	}
	if n := recording.CastSaveSlotsHeld(); n != 0 {
		t.Errorf("slots held after the erase = %d", n)
	}
	if err := s.SaveCast(ctx, run, bytes.NewReader([]byte("late"))); !errors.Is(err, recording.ErrErased) {
		t.Fatalf("save after the erase = %v", err)
	}
}
