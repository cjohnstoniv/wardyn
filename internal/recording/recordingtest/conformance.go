// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recordingtest provides a reusable conformance suite for any
// recording.Store implementation. A blessed default and any future alternate
// (e.g. an object-storage backend) are held to the identical contract.
package recordingtest

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// RunConformance exercises the recording.Store contract. newStore must return a
// FRESH, empty store on each call (e.g. fs over a t.TempDir()).
func RunConformance(t *testing.T, newStore func(t *testing.T) recording.Store) {
	ctx := context.Background()

	t.Run("save_open_roundtrip", func(t *testing.T) {
		s := newStore(t)
		want := []byte("asciicast-v2\x00binary\xffbytes")
		if err := s.SaveCast(ctx, "run-1", bytes.NewReader(want)); err != nil {
			t.Fatalf("SaveCast: %v", err)
		}
		rc, err := s.OpenCast(ctx, "run-1")
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
		if _, err := s.OpenCast(ctx, "nope"); err != recording.ErrNotFound {
			t.Fatalf("OpenCast(missing) = %v, want ErrNotFound", err)
		}
	})

	t.Run("save_replaces", func(t *testing.T) {
		s := newStore(t)
		_ = s.SaveCast(ctx, "r", bytes.NewReader([]byte("first")))
		_ = s.SaveCast(ctx, "r", bytes.NewReader([]byte("second")))
		rc, err := s.OpenCast(ctx, "r")
		if err != nil {
			t.Fatalf("OpenCast: %v", err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != "second" {
			t.Fatalf("save did not replace: %q", got)
		}
	})

	t.Run("named_isolation", func(t *testing.T) {
		// A bare-runID batch cast and a "<runID>~<suffix>" attach cast must not
		// clobber each other.
		s := newStore(t)
		if err := s.SaveCast(ctx, "run-2", bytes.NewReader([]byte("batch"))); err != nil {
			t.Fatalf("SaveCast: %v", err)
		}
		if err := s.SaveCastNamed(ctx, "run-2", "sess-a", bytes.NewReader([]byte("attach"))); err != nil {
			t.Fatalf("SaveCastNamed: %v", err)
		}
		batch, _ := s.OpenCast(ctx, "run-2")
		b, _ := io.ReadAll(batch)
		_ = batch.Close()
		attach, _ := s.OpenCast(ctx, recording.CastKey("run-2", "sess-a"))
		a, _ := io.ReadAll(attach)
		_ = attach.Close()
		if string(b) != "batch" || string(a) != "attach" {
			t.Fatalf("named isolation broken: batch=%q attach=%q", b, a)
		}
	})
}
