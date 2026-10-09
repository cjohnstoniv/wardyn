// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

type delayedCast struct {
	r       io.Reader
	read    chan struct{}
	release chan struct{}
}

func (r *delayedCast) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if errors.Is(err, io.EOF) {
		close(r.read)
		<-r.release
	}
	return n, err
}

func recordingErasureContract(t *testing.T, newStore func() recording.Store) string {
	t.Helper()
	a, b := newStore(), newStore()
	run := uuid.NewString()
	keys := []string{run, run + "~attach", run + "~attach-part-2", run + "~part-2", run + "~part-2048"}
	done := make(chan error, len(keys))
	var releases []func()
	for _, key := range keys {
		if err := a.SaveCast(t.Context(), key, strings.NewReader("committed")); err != nil {
			t.Fatal(err)
		}
		r := &delayedCast{r: strings.NewReader("late bytes"), read: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(r.release) })
		defer release()
		releases = append(releases, release)
		go func() { done <- a.SaveCast(t.Context(), key, r) }()
		select {
		case <-r.read:
		case <-time.After(5 * time.Second):
			t.Fatal("writer did not consume its stream")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if n, err := b.(recording.RunDeleter).DeleteRun(ctx, run); err != nil || n != len(keys) {
		t.Fatalf("erase while streams are paused = %d, %v", n, err)
	}
	for _, release := range releases {
		release()
	}
	for range keys {
		select {
		case err := <-done:
			if !errors.Is(err, recording.ErrErased) {
				t.Errorf("late writer = %v, want ErrErased", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("late writer did not finish")
		}
	}
	fresh := newStore()
	for _, key := range keys {
		assertErased(t, fresh, key)
	}
	if err := fresh.SaveCastNamed(t.Context(), run, "never-seen-part", strings.NewReader("late")); !errors.Is(err, recording.ErrErased) {
		t.Fatalf("new suffix after restart = %v", err)
	}
	if err := fresh.SaveCast(t.Context(), uuid.NewString(), strings.NewReader("new run")); err != nil {
		t.Fatalf("new run after erase: %v", err)
	}
	return run
}

func assertErased(t *testing.T, s recording.Store, key string) {
	t.Helper()
	if rc, err := s.OpenCast(t.Context(), key); !errors.Is(err, recording.ErrErased) {
		if rc != nil {
			_ = rc.Close()
		}
		t.Errorf("OpenCast(%q) = %v, want ErrErased", key, err)
	}
	if size, tail, err := s.StatAndTail(t.Context(), key, 32); !errors.Is(err, recording.ErrErased) || size != 0 || len(tail) != 0 {
		t.Errorf("StatAndTail(%q) = %d, %q, %v", key, size, tail, err)
	}
}

func TestFSStore_ErasureDelayedStreams(t *testing.T) {
	root := t.TempDir()
	run := recordingErasureContract(t, func() recording.Store {
		s, err := recording.NewFSStore(root)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if (strings.HasPrefix(e.Name(), run) && strings.HasSuffix(e.Name(), ".cast")) || strings.HasPrefix(e.Name(), ".tmp-cast-") {
			t.Errorf("late writer left physical recording data: %s", e.Name())
		}
	}
}

func TestPGStore_ErasureDelayedStreamsAcrossReplicas(t *testing.T) {
	run := recordingErasureContract(t, func() recording.Store { return recording.NewPGStore(pgPool(t)) })
	var n int
	if err := pgPool(t).QueryRow(t.Context(), `SELECT count(*) FROM recordings WHERE cast_key = $1 OR starts_with(cast_key, $1 || '~')`, run).Scan(&n); err != nil || n != 0 {
		t.Fatalf("physical rows after late writers = %d, %v", n, err)
	}
}

func concurrentErasureContract(t *testing.T, a, b recording.Store) {
	t.Helper()
	for range 12 {
		run := uuid.NewString()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for n := range 8 {
			wg.Go(func() {
				<-start
				err := a.SaveCastNamed(t.Context(), run, recording.PartSuffix(n+2), strings.NewReader("cast"))
				if err != nil && !errors.Is(err, recording.ErrErased) {
					t.Errorf("racing write: %v", err)
				}
			})
		}
		close(start)
		if _, err := b.(recording.RunDeleter).DeleteRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for n := range 8 {
			assertErased(t, b, recording.CastKey(run, recording.PartSuffix(n+2)))
		}
	}
}

func TestFSStore_ErasureConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	a, _ := recording.NewFSStore(root)
	b, _ := recording.NewFSStore(root)
	concurrentErasureContract(t, a, b)
}

func TestPGStore_ErasureConcurrentWriters(t *testing.T) {
	concurrentErasureContract(t, recording.NewPGStore(pgPool(t)), recording.NewPGStore(pgPool(t)))
}
