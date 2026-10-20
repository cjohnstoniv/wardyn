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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// FUG-L759: a payload written in several statements reads back byte for byte, whatever its size
// against the chunk, and a smaller save replaces a larger one whole.
func TestPGStore_ChunkedWriteRoundTrips(t *testing.T) {
	pool := pgPool(t)
	s := recording.NewPGStore(pool)
	ctx := context.Background()
	const chunk = 1000
	recording.SetCastWriteChunk(t, chunk)

	payload := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7 + i/chunk) // differs across chunk boundaries, so a lost or reordered chunk shows
		}
		return b
	}
	runID := "chunked-" + uuid.NewString()
	for _, n := range []int{3*chunk + 17, 0, 1, chunk - 1, chunk, chunk + 1, 2 * chunk, 4*chunk + 1, 5} {
		want := payload(n)
		if err := s.SaveCast(ctx, runID, bytes.NewReader(want)); err != nil {
			t.Fatalf("save %d bytes: %v", n, err)
		}
		rc, err := s.OpenCast(ctx, runID)
		if err != nil {
			t.Fatalf("open after %d bytes: %v", n, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("saved %d bytes, read back %d (equal=%v)", n, len(got), bytes.Equal(got, want))
		}
	}
}

// StatAndTail answers from the size and tail stored with the payload when they are current, and from the
// payload when they are not (a row never backfilled, or saved by a binary that does not write them).
func TestPGStore_StatAndTailUsesStoredStats(t *testing.T) {
	pool := pgPool(t)
	s := recording.NewPGStore(pool)
	ctx := context.Background()

	check := func(runID string, body []byte, tailBytes int64) {
		t.Helper()
		size, tail, err := s.StatAndTail(ctx, runID, tailBytes)
		if err != nil {
			t.Fatal(err)
		}
		want := body[max(len(body)-int(tailBytes), 0):]
		if size != int64(len(body)) || !bytes.Equal(tail, want) {
			t.Errorf("%d bytes, tail %d: got (%d, %d bytes), want (%d, %d bytes)", len(body), tailBytes, size, len(tail), len(body), len(want))
		}
	}
	for _, n := range []int{0, 1, 100, 8192, 8193, 20000} {
		body := make([]byte, n)
		for i := range body {
			body[i] = byte(i * 13)
		}
		runID := "stats-" + uuid.NewString()
		if err := s.SaveCast(ctx, runID, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		for _, tailBytes := range []int64{1, 6, 8192, 8193, 100000} {
			check(runID, body, tailBytes)
		}
	}

	runID := "stats-" + uuid.NewString()
	if err := s.SaveCast(ctx, runID, strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE recordings SET size_bytes = 99, tail = 'stored' WHERE cast_key = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if size, tail, err := s.StatAndTail(ctx, runID, 4); err != nil || size != 99 || string(tail) != "ored" {
		t.Fatalf("current stats: StatAndTail = (%d, %q, %v), want the stored (99, %q)", size, tail, err, "ored")
	}
	// A save by a binary from before the columns moves updated_at and leaves the stats behind.
	if _, err := pool.Exec(ctx, `UPDATE recordings SET payload = '0123456789abcdef', updated_at = now() WHERE cast_key = $1`, runID); err != nil {
		t.Fatal(err)
	}
	check(runID, []byte("0123456789abcdef"), 4)
	// The migration's backfill makes the stored stats current again.
	mig, err := os.ReadFile("../db/migrations/0206_recordings_size_tail.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE recordings SET stat_updated_at = NULL WHERE cast_key = $1`, runID); err != nil {
		t.Fatal(err)
	}
	check(runID, []byte("0123456789abcdef"), 4)
	if _, err := pool.Exec(ctx, string(mig)); err != nil {
		t.Fatal(err)
	}
	var size int64
	if err := pool.QueryRow(ctx, `SELECT size_bytes FROM recordings WHERE cast_key = $1`, runID).Scan(&size); err != nil || size != 16 {
		t.Fatalf("backfilled size_bytes = %d, %v; want 16", size, err)
	}
	check(runID, []byte("0123456789abcdef"), 4)
}

// An opened cast holds a read slot until it is closed: with castReadSlots held a further open is refused,
// a close frees exactly its own slot (a second close frees nothing), and a missing cast takes none.
func TestPGStore_OpenCastHoldsAReadSlotUntilClosed(t *testing.T) {
	s := recording.NewPGStore(pgPool(t))
	ctx := context.Background()
	runID := uuid.NewString()
	if err := s.SaveCast(ctx, runID, strings.NewReader("cast")); err != nil {
		t.Fatal(err)
	}
	if held := recording.CastReadSlotsHeld(); held != 0 {
		t.Fatalf("%d read slots held before the test opened anything", held)
	}

	var open []io.ReadCloser
	for range recording.CastReadSlots {
		rc, err := s.OpenCast(ctx, runID)
		if err != nil {
			t.Fatalf("open within the slots: %v", err)
		}
		open = append(open, rc)
	}
	if _, err := s.OpenCast(ctx, runID); !errors.Is(err, recording.ErrReadBusy) {
		t.Fatalf("open with every slot held = %v, want ErrReadBusy", err)
	}
	open[0].Close()
	open[0].Close()
	if held := recording.CastReadSlotsHeld(); held != recording.CastReadSlots-1 {
		t.Fatalf("after closing one cast twice %d slots are held, want %d", held, recording.CastReadSlots-1)
	}
	rc, err := s.OpenCast(ctx, runID)
	if err != nil {
		t.Fatalf("open after a close: %v", err)
	}
	open[0] = rc
	for _, rc := range open {
		rc.Close()
	}
	if _, err := s.OpenCast(ctx, uuid.NewString()); !errors.Is(err, recording.ErrNotFound) {
		t.Fatalf("open of a missing cast = %v, want ErrNotFound", err)
	}
	if held := recording.CastReadSlotsHeld(); held != 0 {
		t.Fatalf("%d read slots still held after every close and a failed open", held)
	}
}

// The replay handler answers a full set of read slots 503 with Retry-After, and serves again once one is free.
func TestPGStore_ReplayIs503WithRetryAfterWhileReadSlotsAreFull(t *testing.T) {
	s := recording.NewPGStore(pgPool(t))
	ctx := context.Background()
	runID := uuid.NewString()
	if err := s.SaveCast(ctx, runID, strings.NewReader("cast")); err != nil {
		t.Fatal(err)
	}
	var open []io.ReadCloser
	for range recording.CastReadSlots {
		rc, err := s.OpenCast(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, rc)
	}
	srv := httptest.NewServer(newTestRouter(s))
	defer srv.Close()
	get := func() *http.Response {
		resp, err := http.Get(srv.URL + "/api/v1/runs/" + runID + "/recording/" + runID)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := get(); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("replay with every slot held: %d, Retry-After %q; want 503 with one", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	for _, rc := range open {
		rc.Close()
	}
	if resp := get(); resp.StatusCode != http.StatusOK {
		t.Fatalf("replay after the slots were freed: %d, want 200", resp.StatusCode)
	}
	if held := recording.CastReadSlotsHeld(); held != 0 {
		t.Fatalf("%d read slots held after the replay finished", held)
	}
}

// A joined replay holds one slot at a time, so it finishes with the other slot taken, and frees both.
func TestPGStore_JoinedReplayNeedsOneReadSlotAtATime(t *testing.T) {
	s := recording.NewPGStore(pgPool(t))
	ctx := context.Background()
	runID := "run-pg-" + uuid.NewString()
	saveParts(t, s, runID, partHeader+partEv1, partHeader+partEv2, partHeader+partEv3)
	other := uuid.NewString()
	if err := s.SaveCast(ctx, other, strings.NewReader("cast")); err != nil {
		t.Fatal(err)
	}
	var held []io.ReadCloser
	for range recording.CastReadSlots - 1 {
		rc, err := s.OpenCast(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, rc)
	}
	rc, err := recording.OpenJoined(ctx, s, runID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != partHeader+partEv1+partEv2+partEv3 {
		t.Fatalf("joined replay = %q, %v", got, err)
	}
	for _, rc := range held {
		rc.Close()
	}
	if n := recording.CastReadSlotsHeld(); n != 0 {
		t.Fatalf("%d read slots held after the replay", n)
	}
}

// DeleteRun is what a person's erasure reaches recordings through: the run's
// casts go on every replica, another run's stay, and a repeat is harmless.
func TestPGStoreDeleteRun(t *testing.T) {
	deleteRunContract(t, recording.NewPGStore(pgPool(t)))
}
