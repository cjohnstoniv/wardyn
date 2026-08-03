// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recordingtest provides a reusable conformance suite for any
// recording.Store implementation. A blessed default and any future alternate
// (e.g. an object-storage backend) are held to the identical contract. Keys
// are process-unique (uuid-suffixed) so the suite is safe to run against a
// SHARED backing store too — e.g. a pg-backed Store pointed at a table a
// concurrent test run also uses — the same convention secretstoretest uses.
package recordingtest

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// RunConformance exercises the recording.Store contract. newStore must return
// a usable store on each call (it need not be empty; the suite uses
// process-unique keys — e.g. fs over a t.TempDir(), or a pg store over a
// shared table).
func RunConformance(t *testing.T, newStore func(t *testing.T) recording.Store) {
	ctx := context.Background()
	uniq := func(p string) string { return "conformance-" + p + "-" + uuid.NewString() }

	t.Run("save_open_roundtrip", func(t *testing.T) {
		s := newStore(t)
		runID := uniq("run")
		want := []byte("asciicast-v2\x00binary\xffbytes")
		if err := s.SaveCast(ctx, runID, bytes.NewReader(want)); err != nil {
			t.Fatalf("SaveCast: %v", err)
		}
		rc, err := s.OpenCast(ctx, runID)
		if err != nil {
			t.Fatalf("OpenCast: %v", err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, want)
		}
	})

	t.Run("open_missing_returns_ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.OpenCast(ctx, uniq("nope")); err != recording.ErrNotFound {
			t.Fatalf("OpenCast(missing) = %v, want ErrNotFound", err)
		}
	})

	t.Run("save_replaces", func(t *testing.T) {
		s := newStore(t)
		runID := uniq("r")
		_ = s.SaveCast(ctx, runID, bytes.NewReader([]byte("first")))
		_ = s.SaveCast(ctx, runID, bytes.NewReader([]byte("second")))
		rc, err := s.OpenCast(ctx, runID)
		if err != nil {
			t.Fatalf("OpenCast: %v", err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != "second" {
			t.Fatalf("save did not replace: %q", got)
		}
	})

	t.Run("rejects_invalid_keys", func(t *testing.T) {
		// Key validation is part of the CONTRACT, not an fs implementation
		// detail: a key one backend stores and another rejects means flipping
		// WARDYN_RECORDING_STORE silently changes which recordings exist. The
		// dot-dot/separator cases are meaningless to a TEXT primary key, but
		// they must be refused with the same "recording: invalid run id" error
		// rather than quietly stored.
		s := newStore(t)
		for _, key := range []string{"", "../etc/passwd", "../../etc/shadow", "run/../../secret", "run\x00bad", "/abs/path"} {
			if err := s.SaveCast(ctx, key, bytes.NewReader([]byte("x"))); err == nil {
				t.Errorf("SaveCast(%q) should have been rejected", key)
			}
			if _, err := s.OpenCast(ctx, key); err == nil {
				t.Errorf("OpenCast(%q) should have been rejected", key)
			}
		}
	})

	t.Run("named_isolation", func(t *testing.T) {
		// A bare-runID batch cast and a "<runID>~<suffix>" attach cast must not
		// clobber each other.
		s := newStore(t)
		runID := uniq("run")
		if err := s.SaveCast(ctx, runID, bytes.NewReader([]byte("batch"))); err != nil {
			t.Fatalf("SaveCast: %v", err)
		}
		if err := s.SaveCastNamed(ctx, runID, "sess-a", bytes.NewReader([]byte("attach"))); err != nil {
			t.Fatalf("SaveCastNamed: %v", err)
		}
		batch, _ := s.OpenCast(ctx, runID)
		b, _ := io.ReadAll(batch)
		_ = batch.Close()
		attach, _ := s.OpenCast(ctx, recording.CastKey(runID, "sess-a"))
		a, _ := io.ReadAll(attach)
		_ = attach.Close()
		if string(b) != "batch" || string(a) != "attach" {
			t.Fatalf("named isolation broken: batch=%q attach=%q", b, a)
		}
	})
}
