// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingRecordingStore wraps a recording.Store, counting StatAndTail calls
// and — when statErr is set — failing every one of them with it instead of
// delegating (a store-outage double, e.g. "pg: connection refused").
// Everything else (SaveCast, SaveCastNamed, OpenCast) passes straight through
// to the embedded Store.
type countingRecordingStore struct {
	recording.Store
	statCalls int
	statErr   error
}

func (c *countingRecordingStore) StatAndTail(ctx context.Context, key string, n int64) (int64, []byte, error) {
	c.statCalls++
	if c.statErr != nil {
		return 0, nil, c.statErr
	}
	return c.Store.StatAndTail(ctx, key, n)
}

// TestProjectRecordingMeta_ListAndGet pins R4-F077: GET /runs?include=recording_meta
// and GET /runs/{id}?include=recording_meta must answer
// has_recording/recording_bytes/recording_duration_sec from
// RecordingStore.StatAndTail alone — no OpenCast, no download of the whole
// cast — for a run that has a recording, and must leave all three at
// zero/false for one that does not.
func TestProjectRecordingMeta_ListAndGet(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	rs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	cfg.RecordingStore = rs
	srv := New(cfg)
	ctx := context.Background()

	withRec, err := ast.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunCompleted})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	withoutRec, err := ast.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunCompleted})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// header + two output events; the LAST event's elapsed time (2.5s) is the
	// duration this run must report.
	cast := "{\"version\":2,\"width\":80,\"height\":24}\n" +
		`[0.1,"o","hello"]` + "\n" +
		`[2.5,"o","world"]` + "\n"
	if err := rs.SaveCast(ctx, withRec.ID.String(), strings.NewReader(cast)); err != nil {
		t.Fatalf("SaveCast: %v", err)
	}

	t.Run("list", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/api/v1/runs?include=recording_meta", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /runs = %d, body=%s", w.Code, w.Body.String())
		}
		var runs []types.AgentRun
		if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
			t.Fatalf("decode: %v", err)
		}
		byID := map[uuid.UUID]types.AgentRun{}
		for _, r := range runs {
			byID[r.ID] = r
		}

		got, ok := byID[withRec.ID]
		if !ok {
			t.Fatalf("run with a recording missing from the list")
		}
		if !got.HasRecording {
			t.Errorf("has_recording = false, want true")
		}
		if got.RecordingBytes != int64(len(cast)) {
			t.Errorf("recording_bytes = %d, want %d", got.RecordingBytes, len(cast))
		}
		if got.RecordingDurationSec != 2.5 {
			t.Errorf("recording_duration_sec = %v, want 2.5", got.RecordingDurationSec)
		}

		got2, ok := byID[withoutRec.ID]
		if !ok {
			t.Fatalf("run with no recording missing from the list")
		}
		if got2.HasRecording || got2.RecordingBytes != 0 || got2.RecordingDurationSec != 0 {
			t.Errorf("run with no recording = %+v, want all three fields zero/false", got2)
		}
	})

	t.Run("get", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/api/v1/runs/"+withRec.ID.String()+"?include=recording_meta", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /runs/{id} = %d, body=%s", w.Code, w.Body.String())
		}
		var got types.AgentRun
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !got.HasRecording || got.RecordingBytes != int64(len(cast)) || got.RecordingDurationSec != 2.5 {
			t.Errorf("GET /runs/{id} = %+v, want has_recording/bytes/duration set", got)
		}

		w2 := do(t, srv, http.MethodGet, "/api/v1/runs/"+withoutRec.ID.String()+"?include=recording_meta", adminToken, "")
		if w2.Code != http.StatusOK {
			t.Fatalf("GET /runs/{id} (no recording) = %d, body=%s", w2.Code, w2.Body.String())
		}
		var got2 types.AgentRun
		if err := json.Unmarshal(w2.Body.Bytes(), &got2); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got2.HasRecording || got2.RecordingBytes != 0 || got2.RecordingDurationSec != 0 {
			t.Errorf("GET /runs/{id} (no recording) = %+v, want all three fields zero/false", got2)
		}
	})
}

// TestProjectRecordingMeta_NilStoreIsNoop: a deployment with no recording
// store configured (a stock Helm install, persistence.enabled=false — see
// F199) must not fail the run list; every run just reports has_recording=false.
func TestProjectRecordingMeta_NilStoreIsNoop(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	srv := New(cfg) // cfg.RecordingStore left nil

	run, err := ast.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunCompleted})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d, body=%s", w.Code, w.Body.String())
	}
	var runs []types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range runs {
		if r.ID == run.ID && r.HasRecording {
			t.Errorf("has_recording = true with no RecordingStore configured")
		}
	}
}

// TestProjectRecordingMeta_DefaultListPathSkipsStatAndTail pins the cost fix:
// GET /runs WITHOUT ?include=recording_meta — the Runs board's shape,
// POLL_MS=3000 at limit=1000 — must issue ZERO StatAndTail calls. A
// sequential StatAndTail per run in that response (~7ms measured per 4 MiB
// cast, PG's substring/octet_length detoasting the whole bytea either way)
// would turn a 3s poll of 1000 runs into up to ~7s of added latency for a
// board that renders none of these three fields.
func TestProjectRecordingMeta_DefaultListPathSkipsStatAndTail(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	counting := &countingRecordingStore{Store: fs}
	cfg.RecordingStore = counting
	srv := New(cfg)

	if _, err := ast.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunCompleted}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if w := do(t, srv, http.MethodGet, "/api/v1/runs", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d, body=%s", w.Code, w.Body.String())
	}
	if counting.statCalls != 0 {
		t.Errorf("GET /runs (no include=) called StatAndTail %d times, want 0", counting.statCalls)
	}

	if w := do(t, srv, http.MethodGet, "/api/v1/runs?include=recording_meta", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("GET /runs?include=recording_meta = %d, body=%s", w.Code, w.Body.String())
	}
	if counting.statCalls == 0 {
		t.Errorf("GET /runs?include=recording_meta called StatAndTail 0 times, want at least 1")
	}
}

// TestProjectRecordingMeta_StoreOutageDoesNot500 pins the best-effort
// contract: a RecordingStore error that is NOT recording.ErrNotFound (a
// store outage — "pg: connection refused" is the shape named in review) must
// still answer 200 with has_recording=false, never fail the whole run list.
func TestProjectRecordingMeta_StoreOutageDoesNot500(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	cfg.RecordingStore = &countingRecordingStore{Store: fs, statErr: errors.New("pg: connection refused")}
	srv := New(cfg)

	run, err := ast.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunCompleted})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs?include=recording_meta", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runs?include=recording_meta with a store outage = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var runs []types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range runs {
		if r.ID == run.ID && r.HasRecording {
			t.Errorf("has_recording = true despite a store outage")
		}
	}
}

// TestProjectRecordingMeta_GarbageTailStillReportsHasRecording pins the
// duration-derivation contract on the OTHER side from ErrNotFound: a stored
// cast that never parses into a valid "o" event (corrupt, truncated, or an
// unrelated file wardyn-rec never wrote) still means the run HAS a recording
// — StatAndTail succeeded — it just can't derive a duration from garbage.
// has_recording=true, recording_bytes>0, recording_duration_sec stays at its
// zero value (not an error, not "no recording").
func TestProjectRecordingMeta_GarbageTailStillReportsHasRecording(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	rs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	cfg.RecordingStore = rs
	srv := New(cfg)
	ctx := context.Background()

	run, err := ast.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunCompleted})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	garbage := "this is not an asciicast at all\nneither is this\n"
	if err := rs.SaveCast(ctx, run.ID.String(), strings.NewReader(garbage)); err != nil {
		t.Fatalf("SaveCast: %v", err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs?include=recording_meta", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runs?include=recording_meta = %d, body=%s", w.Code, w.Body.String())
	}
	var runs []types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range runs {
		if r.ID != run.ID {
			continue
		}
		if !r.HasRecording {
			t.Errorf("has_recording = false for a stored (if unparseable) cast, want true")
		}
		if r.RecordingBytes != int64(len(garbage)) {
			t.Errorf("recording_bytes = %d, want %d", r.RecordingBytes, len(garbage))
		}
		if r.RecordingDurationSec != 0 {
			t.Errorf("recording_duration_sec = %v, want 0 (garbage tail, no derivable event)", r.RecordingDurationSec)
		}
	}
}
