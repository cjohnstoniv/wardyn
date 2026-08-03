// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for the recording store (migration 0028). These prove
// the HA property S3 exists for: a cast saved through the replica that
// dispatched a run must be readable through replay landing on ANY OTHER
// replica — FSStore fails this by construction (its directory is per-pod).
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset. Every case uses a
// unique run id so it is isolated inside the shared database.
// Run: WARDYN_TEST_PG=postgres://... go test ./internal/recording/...
package recording_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/recording/recordingtest"
)

// pgPool connects + migrates against the live substrate named by
// WARDYN_TEST_PG, skipping cleanly when it is unset. Mirrors the
// connect/migrate/cleanup dance internal/store/store_runs_pg_test.go's
// runsPGPool and internal/secretstore/pg's newPGStore open-code.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed recording test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The blessed pg store must pass the same shared conformance suite FSStore
// does (docs/PLUGGABILITY.md: "a competing component ... held to the same
// conformance contract").
func TestPGStore_Conformance(t *testing.T) {
	pool := pgPool(t)
	recordingtest.RunConformance(t, func(t *testing.T) recording.Store {
		return recording.NewPGStore(pool)
	})
}

// TestPGStore_CrossReplicaVisibility is the HA property S3 closes: a cast
// written through one INDEPENDENTLY-CONSTRUCTED store handle (standing in for
// the replica that dispatched the run) must be readable through a SECOND,
// separate handle (standing in for the replica a replay request happens to
// land on). The two pools share no in-process state whatsoever — only the
// Postgres table — which is exactly what makes this the proxy for
// replica-A-writes / replica-B-reads that FSStore cannot pass (its casts live
// on one pod's local disk).
func TestPGStore_CrossReplicaVisibility(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed recording test")
	}
	ctx := context.Background()

	poolA, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (replica A): %v", err)
	}
	defer poolA.Close()
	if err := db.Migrate(ctx, poolA); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	poolB, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (replica B): %v", err)
	}
	defer poolB.Close()

	storeA := recording.NewPGStore(poolA)
	storeB := recording.NewPGStore(poolB)

	runID := "replica-cross-" + uuid.NewString()
	want := []byte("asciicast written by replica A\x00binary\xfftail")
	if err := storeA.SaveCast(ctx, runID, bytes.NewReader(want)); err != nil {
		t.Fatalf("replica A SaveCast: %v", err)
	}

	rc, err := storeB.OpenCast(ctx, runID)
	if err != nil {
		t.Fatalf("replica B OpenCast: %v (a cast written on one replica must be visible on another)", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("replica B read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cross-replica round-trip mismatch: got %q want %q", got, want)
	}
}

// TestPGStore_SaveCastNamed_Addressing pins the composite-key contract
// SaveCastNamed promises (store.go): exactly "<runID>~<suffix>", the SAME
// format FSStore uses, and that it stays distinct from the bare-runID key.
func TestPGStore_SaveCastNamed_Addressing(t *testing.T) {
	pool := pgPool(t)
	s := recording.NewPGStore(pool)
	ctx := context.Background()
	runID := "addr-" + uuid.NewString()

	if err := s.SaveCastNamed(ctx, runID, "sess-1", strings.NewReader("attach-bytes")); err != nil {
		t.Fatalf("SaveCastNamed: %v", err)
	}

	wantKey := runID + "~sess-1"
	if got := recording.CastKey(runID, "sess-1"); got != wantKey {
		t.Fatalf("CastKey = %q, want %q", got, wantKey)
	}
	rc, err := s.OpenCast(ctx, wantKey)
	if err != nil {
		t.Fatalf("OpenCast(%q): %v", wantKey, err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "attach-bytes" {
		t.Fatalf("attach cast = %q, want %q", got, "attach-bytes")
	}

	// The bare runID must stay a DISTINCT miss: no batch cast was ever saved
	// under it, only the named composite.
	if _, err := s.OpenCast(ctx, runID); err != recording.ErrNotFound {
		t.Fatalf("OpenCast(bare runID) = %v, want ErrNotFound (must not alias the named cast)", err)
	}

	// An invalid suffix (would collide with the '~' composite-key delimiter)
	// is rejected before anything is written, identically to FSStore.
	if err := s.SaveCastNamed(ctx, runID, "evil~suffix", strings.NewReader("x")); err == nil {
		t.Fatal("SaveCastNamed accepted a suffix containing the composite-key delimiter")
	}
}

// TestPGStore_SizeCap proves SaveCast refuses a cast over the size cap with a
// clear error, and that a rejected save leaves no partial row for OpenCast to
// serve. 64 MiB mirrors pgstore.go's maxCastBytes (kept unexported: the exact
// cap is an implementation constant, not part of the Store contract).
func TestPGStore_SizeCap(t *testing.T) {
	pool := pgPool(t)
	s := recording.NewPGStore(pool)
	ctx := context.Background()
	runID := "oversize-" + uuid.NewString()

	const oversizedCastBytes = 64<<20 + 1
	err := s.SaveCast(ctx, runID, bytes.NewReader(make([]byte, oversizedCastBytes)))
	if err == nil {
		t.Fatal("SaveCast accepted a cast over the size cap")
	}

	if _, err := s.OpenCast(ctx, runID); err != recording.ErrNotFound {
		t.Fatalf("OpenCast after a rejected oversized save = %v, want ErrNotFound (no partial row)", err)
	}
}
