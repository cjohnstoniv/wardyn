// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNewSessionRecorder_MasksAndPersists exercises the PIECE 3 recording
// pipeline end-to-end (without a live attach): PTY output teed through the
// recorder is (a) secret-masked, (b) serialized as asciicast v2, (c) persisted
// under a per-run+session key, and (d) emits a session.recording audit event.
func TestNewSessionRecorder_MasksAndPersists(t *testing.T) {
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	runID := uuid.New()
	const secret = "super-secret-token-value-1234"
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte(secret))

	audit := &recRecorder{}
	srv := New(Config{
		RecordingStore: store,
		MaskRegistry:   reg,
		Audit:          audit,
		AdminToken:     adminToken,
	})

	tee, finish := srv.newSessionRecorder(runID, "session-xyz", runner.AttachOptions{Cols: 100, Rows: 30})
	if tee == nil {
		t.Fatal("tee writer is nil with a RecordingStore configured")
	}

	// Simulate PTY output that contains a secret verbatim.
	_, _ = tee.Write([]byte("starting up\r\n"))
	_, _ = tee.Write([]byte("token is " + secret + "\r\n"))
	_, _ = tee.Write([]byte("done\r\n"))

	finish(context.Background(), types.ActorHuman, "alice@example.com")

	// The cast is keyed per run+session, NOT under the bare runID.
	key := recording.CastKey(runID.String(), "session-xyz")
	rc, err := store.OpenCast(context.Background(), key)
	if err != nil {
		t.Fatalf("OpenCast(%q): %v", key, err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	cast := string(body)

	// (a) secret must be masked out of the recording (invariant 1).
	if strings.Contains(cast, secret) {
		t.Errorf("recording leaked the verbatim secret:\n%s", cast)
	}
	// The placeholder is JSON-encoded in the cast (< and > become </>).
	// Assert on the decoded output payloads, which is what the player renders.
	if !strings.Contains(decodeCastOutput(t, cast), "<secret-hidden>") {
		t.Errorf("expected the mask placeholder in the decoded cast output:\n%s", cast)
	}
	// (b) asciicast v2 header present on line 1.
	if !strings.HasPrefix(cast, `{"version":2`) {
		t.Errorf("cast missing asciicast v2 header:\n%s", cast)
	}
	// non-secret output is retained.
	if !strings.Contains(cast, "starting up") || !strings.Contains(cast, "done") {
		t.Errorf("non-secret output missing from cast:\n%s", cast)
	}

	// (d) a session.recording audit event was emitted, success, keyed.
	var found bool
	for _, ev := range audit.events {
		if ev.Action == "session.recording" {
			found = true
			if ev.Outcome != "success" {
				t.Errorf("session.recording outcome = %q, want success", ev.Outcome)
			}
			if ev.Target != key {
				t.Errorf("session.recording target = %q, want %q", ev.Target, key)
			}
			if ev.RunID == nil || *ev.RunID != runID {
				t.Errorf("session.recording run id = %v, want %s", ev.RunID, runID)
			}
		}
	}
	if !found {
		t.Error("no session.recording audit event emitted")
	}
}

// TestNewSessionRecorder_NoStoreIsNoop: with no RecordingStore configured, the
// recorder is a no-op (nil tee, no panic, no audit) so attach works unchanged in
// headless/no-store mode.
func TestNewSessionRecorder_NoStoreIsNoop(t *testing.T) {
	audit := &recRecorder{}
	srv := New(Config{Audit: audit, AdminToken: adminToken})
	tee, finish := srv.newSessionRecorder(uuid.New(), "s", runner.AttachOptions{})
	if tee != nil {
		t.Error("tee should be nil when no RecordingStore is configured")
	}
	finish(context.Background(), types.ActorHuman, "bob") // must not panic
	for _, ev := range audit.events {
		if ev.Action == "session.recording" {
			t.Error("no session.recording event should be emitted without a store")
		}
	}
}

// TestNewSessionRecorder_EmptySessionNotPersisted: an attach that produced no
// output yields no cast artifact (and no audit event).
func TestNewSessionRecorder_EmptySessionNotPersisted(t *testing.T) {
	store, _ := recording.NewFSStore(t.TempDir())
	audit := &recRecorder{}
	srv := New(Config{RecordingStore: store, Audit: audit, AdminToken: adminToken})
	runID := uuid.New()

	_, finish := srv.newSessionRecorder(runID, "empty", runner.AttachOptions{})
	finish(context.Background(), types.ActorHuman, "carol")

	if _, err := store.OpenCast(context.Background(), recording.CastKey(runID.String(), "empty")); err == nil {
		t.Error("an output-less session should not persist a cast")
	}
	for _, ev := range audit.events {
		if ev.Action == "session.recording" {
			t.Error("no session.recording event for an empty session")
		}
	}
}

// TestNewSessionRecorder_MasksSecretSplitAcrossWrites is the FIX #12 check: a
// secret whose verbatim bytes are split across two consecutive Write calls to the
// recording tee must still be masked in the persisted cast — the boundary-straddle
// case the old per-chunk fresh-Masker (no retained tail) leaked verbatim.
func TestNewSessionRecorder_MasksSecretSplitAcrossWrites(t *testing.T) {
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	runID := uuid.New()
	const secret = "super-secret-token-value-1234"
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte(secret))

	audit := &recRecorder{}
	srv := New(Config{RecordingStore: store, MaskRegistry: reg, Audit: audit, AdminToken: adminToken})

	tee, finish := srv.newSessionRecorder(runID, "split", runner.AttachOptions{Cols: 80, Rows: 24})
	if tee == nil {
		t.Fatal("tee writer is nil with a RecordingStore configured")
	}

	half := len(secret) / 2
	_, _ = tee.Write([]byte("prefix " + secret[:half])) // first half of the secret
	_, _ = tee.Write([]byte(secret[half:]))             // second half completes it across the boundary

	finish(context.Background(), types.ActorHuman, "alice@example.com")

	key := recording.CastKey(runID.String(), "split")
	rc, err := store.OpenCast(context.Background(), key)
	if err != nil {
		t.Fatalf("OpenCast(%q): %v", key, err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	cast := string(body)

	if strings.Contains(cast, secret) {
		t.Errorf("boundary-split secret leaked verbatim into persisted cast:\n%s", cast)
	}
	// The tail-resident placeholder is only present if finish flushed the tail;
	// this proves both the split reassembly and the flush (not-dropped) path.
	if !strings.Contains(decodeCastOutput(t, cast), "<secret-hidden>") {
		t.Errorf("expected the mask placeholder in the decoded cast output:\n%s", cast)
	}
	if !strings.Contains(decodeCastOutput(t, cast), "prefix ") {
		t.Errorf("non-secret prefix missing from cast (over-masked or dropped):\n%s", cast)
	}
}

// TestNewSessionRecorder_FlushesRetainedTail exercises the FIX #12 flush path:
// when a session ends while its final write left a dangling in-progress secret
// prefix withheld in the masker tail, finish must FLUSH that tail so the bytes
// are not silently dropped from (truncated out of) the persisted cast.
func TestNewSessionRecorder_FlushesRetainedTail(t *testing.T) {
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	runID := uuid.New()
	const secret = "super-secret-token-value-1234"
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte(secret))

	srv := New(Config{RecordingStore: store, MaskRegistry: reg, Audit: &recRecorder{}, AdminToken: adminToken})
	tee, finish := srv.newSessionRecorder(runID, "tail", runner.AttachOptions{Cols: 80, Rows: 24})

	// The write ends with the first half of the secret: a strict prefix, so it is
	// withheld in the tail (it might complete into the full secret next write).
	half := len(secret) / 2
	trailing := secret[:half]
	_, _ = tee.Write([]byte("log: " + trailing))

	finish(context.Background(), types.ActorHuman, "erin@example.com")

	rc, err := store.OpenCast(context.Background(), recording.CastKey(runID.String(), "tail"))
	if err != nil {
		t.Fatalf("OpenCast: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	out := decodeCastOutput(t, string(body))
	// The withheld prefix must survive to the cast (flush did not drop it). It is a
	// strict prefix, not the whole secret, so emitting it verbatim is not a leak.
	if !strings.Contains(out, "log: "+trailing) {
		t.Errorf("finish dropped the withheld tail; cast output = %q", out)
	}
}

// TestNewSessionRecorder_ConcurrentWriteAndFinishRaceFree is the FIX #13 check
// (run under `go test -race`): the recording tee (fed by the attach Read pump) is
// written concurrently with a detach that flushes + reads + persists the same
// buffer. Pre-fix, finish read bytes.Buffer.Bytes()/Len() (and the masker tail)
// while the pump goroutine was still writing them — a data race the -race detector
// flags. The single mutex in liveMaskWriter must serialise the two.
func TestNewSessionRecorder_ConcurrentWriteAndFinishRaceFree(t *testing.T) {
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte("super-secret-token-value-1234")) // non-empty registry: exercises the tail path

	audit := &recRecorder{}
	srv := New(Config{RecordingStore: store, MaskRegistry: reg, Audit: audit, AdminToken: adminToken})

	tee, finish := srv.newSessionRecorder(runID, "race", runner.AttachOptions{Cols: 80, Rows: 24})

	// Pump goroutine hammers the recording buffer, mirroring the attach Read pump
	// that keeps writing until sess.Close (which, pre-fix, ran AFTER finish).
	started := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = tee.Write([]byte("interactive terminal output chunk\r\n"))
			}
		}
	}()

	<-started
	// Detach + persist concurrently with the still-running pump.
	finish(context.Background(), types.ActorHuman, "dave@example.com")
	close(stop)
	<-done

	// The persisted cast must be well-formed asciicast v2 (no torn/partial JSON),
	// proving the concurrent read produced a consistent snapshot.
	rc, err := store.OpenCast(context.Background(), recording.CastKey(runID.String(), "race"))
	if err != nil {
		t.Fatalf("OpenCast: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if !strings.HasPrefix(string(body), `{"version":2`) {
		t.Errorf("cast missing/torn asciicast v2 header:\n%s", string(body))
	}
	for i, ln := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if i == 0 || ln == "" {
			continue
		}
		var ev []json.RawMessage
		if err := json.Unmarshal([]byte(ln), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("torn cast event on line %d: %q (err=%v)", i, ln, err)
		}
	}
}

// decodeCastOutput concatenates the decoded "o" event payloads from an
// asciicast v2 document (skipping the header line), so a test can assert on the
// rendered terminal output rather than the JSON-escaped wire form.
func decodeCastOutput(t *testing.T, cast string) string {
	t.Helper()
	var out strings.Builder
	for i, ln := range strings.Split(strings.TrimRight(cast, "\n"), "\n") {
		if i == 0 || ln == "" {
			continue // header / blank
		}
		var ev []json.RawMessage
		if err := json.Unmarshal([]byte(ln), &ev); err != nil || len(ev) != 3 {
			continue
		}
		var data string
		_ = json.Unmarshal(ev[2], &data)
		out.WriteString(data)
	}
	return out.String()
}
