// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const partsHeader = `{"version":2,"width":80,"height":24,"timestamp":1700000000}` + "\n"

// TestUploadRecordingPart_StoresAndJoins pins RL-12's control-plane half: part
// n >= 2 of a tail-uploaded cast lands under its own key, masked and audited
// like part 1, and the run's replay joins the two into one document.
func TestUploadRecordingPart_StoresAndJoins(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte("part-two-secret-value"))
	rs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := newRecordingHarness(t, rs, reg)
	tok := h.mintRunToken(t, runID)

	part1 := partsHeader + `[0.5,"o","one"]` + "\n"
	part2 := partsHeader + `[1.5,"o","two part-two-secret-value"]` + "\n"
	if w := do(t, h.srv, http.MethodPut, "/api/v1/internal/recordings/"+runID.String(), tok, part1); w.Code != http.StatusNoContent {
		t.Fatalf("part 1: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h.srv, http.MethodPut, "/api/v1/internal/recordings/"+runID.String()+"/parts/2", tok, part2); w.Code != http.StatusNoContent {
		t.Fatalf("part 2: %d %s", w.Code, w.Body.String())
	}

	rc, err := recording.OpenJoined(context.Background(), rs, runID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	want := partsHeader + `[0.5,"o","one"]` + "\n" + `[1.5,"o","two <secret-hidden>"]` + "\n"
	if string(got) != want {
		t.Fatalf("joined cast =\n%q\nwant\n%q", got, want)
	}

	var partAudit map[string]any
	for _, ev := range h.audit.events {
		if ev.Action == "recording.upload" && ev.Outcome == "success" && len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &partAudit)
		}
	}
	if partAudit["part"] != float64(2) {
		t.Errorf("part 2's recording.upload audit data = %v, want part=2", partAudit)
	}
}

// TestUploadRecordingPart_RejectsBadPartsAndForeignRuns: part 1 has exactly one
// address (the bare route), a part number is canonical decimal, and a run token
// can deliver only its own run's parts.
func TestUploadRecordingPart_RejectsBadPartsAndForeignRuns(t *testing.T) {
	store := &fakeRecordingStore{}
	h := newRecordingHarness(t, store, nil)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	for _, part := range []string{"0", "1", "02", "+3", "-2", "x", "99999999999999999999"} {
		w := do(t, h.srv, http.MethodPut, "/api/v1/internal/recordings/"+runID.String()+"/parts/"+part, tok, partsHeader)
		if w.Code != http.StatusNotFound {
			t.Errorf("part %q: status %d, want 404", part, w.Code)
		}
	}
	if store.saved != nil {
		t.Fatalf("a rejected part reached the store: %q", store.saved)
	}
	w := do(t, h.srv, http.MethodPut, "/api/v1/internal/recordings/"+uuid.NewString()+"/parts/2", tok, partsHeader)
	if w.Code != http.StatusForbidden {
		t.Fatalf("another run's part: status %d, want 403", w.Code)
	}
}

// TestProjectRecordingMeta_CountsEveryPart: the Recordings list reports the
// whole tail-uploaded cast — its size across parts and the duration the last
// part reaches — not part 1 alone.
func TestProjectRecordingMeta_CountsEveryPart(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	rs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RecordingStore = rs
	srv := New(cfg)
	ctx := context.Background()
	run, err := ast.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunCompleted})
	if err != nil {
		t.Fatal(err)
	}
	p1 := partsHeader + `[0.5,"o","one"]` + "\n"
	p2 := partsHeader + `[90000.5,"o","a day later"]` + "\n"
	if err := rs.SaveCast(ctx, run.ID.String(), strings.NewReader(p1)); err != nil {
		t.Fatal(err)
	}
	if err := rs.SaveCastNamed(ctx, run.ID.String(), recording.PartSuffix(2), strings.NewReader(p2)); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"?include=recording_meta", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET run: %d %s", w.Code, w.Body.String())
	}
	var got types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RecordingBytes != int64(len(p1)+len(p2)) || got.RecordingDurationSec != 90000.5 {
		t.Fatalf("meta = %d bytes / %v s, want %d / 90000.5", got.RecordingBytes, got.RecordingDurationSec, len(p1)+len(p2))
	}
}
