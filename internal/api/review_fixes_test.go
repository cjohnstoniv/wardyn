// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLiveMaskWriter_ReSnapshotsMidSession proves the attach-recording fix: the
// masker re-snapshots the registry on every write, so a credential REGISTERED
// MID-SESSION (after the attach started) is masked in the recording — not only
// secrets known at attach start. No Postgres required.
func TestLiveMaskWriter_ReSnapshotsMidSession(t *testing.T) {
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	secret := []byte("ghs_midsession_token_abcdef0123456789")

	var buf bytes.Buffer
	w := &liveMaskWriter{reg: reg, runID: runID, dst: &buf}

	// Mid-session mint: register AFTER the writer was constructed.
	reg.Add(runID, secret)

	if _, err := w.Write(append([]byte("$ echo "), secret...)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if bytes.Contains(buf.Bytes(), secret) {
		t.Errorf("mid-session-registered secret leaked into the recording: %q", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("<secret-hidden>")) {
		t.Errorf("expected <secret-hidden> after mid-session registration, got %q", buf.String())
	}
}

// TestLiveMaskWriter_NilRegistryPassThrough confirms a nil registry is a safe
// pass-through (recording still works when masking is not configured).
func TestLiveMaskWriter_NilRegistryPassThrough(t *testing.T) {
	var buf bytes.Buffer
	w := &liveMaskWriter{reg: nil, runID: uuid.New(), dst: &buf}
	if _, err := w.Write([]byte("plain output")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if buf.String() != "plain output" {
		t.Errorf("nil-registry pass-through = %q, want %q", buf.String(), "plain output")
	}
}

// TestKillRun_TerminalGuard proves the kill terminal-guard fix: a kill on an
// already-terminal (COMPLETED) run is rejected with 409 and does NOT clobber the
// COMPLETED state to KILLED (audit-integrity). WARDYN_TEST_PG-gated.
func TestKillRun_TerminalGuard(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed kill-guard test")
	}
	srv, pool := pgHarness(t)
	ctx := context.Background()

	// Create a run (no runner wired in pgHarness, so it stays PENDING), then
	// force it terminal as the completion watcher would.
	cw := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","repo":"acme/widgets"}`)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, body=%s", cw.Code, cw.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(cw.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := store.NewPG(pool).UpdateRunStateIf(ctx, run.ID, run.State, types.RunCompleted); err != nil {
		t.Fatalf("force completed: %v", err)
	}

	// Kill must be refused (409) and must NOT overwrite COMPLETED.
	kw := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", adminToken, "")
	if kw.Code != http.StatusConflict {
		t.Fatalf("kill on terminal run: code = %d, want 409; body=%s", kw.Code, kw.Body.String())
	}
	got, err := store.NewPG(pool).GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != types.RunCompleted {
		t.Errorf("state after kill = %q, want COMPLETED (terminal guard must not clobber)", got.State)
	}
}
