// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedRecordedSession puts one COMPLETED capture in the workspace's
// record_results under key, labelled label, with observations worth losing.
func seedRecordedSession(t *testing.T, ws *types.Workspace, key, label string) {
	t.Helper()
	blob, err := json.Marshal(map[string]RecordTaskResult{
		key: {
			RunID: uuid.New(), Label: label, Mode: recordModeInteractive,
			Status: "recorded", StartedAt: time.Now().UTC().Add(-time.Hour),
			Observations: &recordmode.Observations{},
		},
	})
	if err != nil {
		t.Fatalf("marshal record_results: %v", err)
	}
	ws.RecordResults = blob
}

// TestRecordWorkspace_DifferentLabelUnderTheSameSlugIsRefused pins this:
// recordSessionKey slugs "build & test" and "Build/Test" to the SAME key
// (build-test), and the launch write is an unconditional per-key upsert — so
// naming a second session with different punctuation silently destroyed the
// first session's completed capture (Observations, Clean/Caught,
// SecretNamesMinted) with no 409 and no audit.
//
// Two invariants, and the second is why this cannot be "409 on any re-record":
//   - a DIFFERENT label under an existing key is refused, and the stored entry
//     is untouched;
//   - the SAME label still overwrites — re-recording a session is the normal
//     operator loop, and the guard must not brick it.
func TestRecordWorkspace_DifferentLabelUnderTheSameSlugIsRefused(t *testing.T) {
	wsID := uuid.New()
	ws := types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
	}
	seedRecordedSession(t, &ws, "build-test", "build & test")
	fake := &recordStore{importStateFake: importStateFake{ws: ws}}
	srv := newTestSrv(t, fake)
	url := "/api/v1/workspaces/" + wsID.String() + "/record"

	// A different label that slugs onto the SAME key must be refused BEFORE the
	// launch, i.e. ahead of the no-runner 503 every other arm of this fixture
	// answers.
	w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"Build/Test"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("re-record under a different label: code = %d, want 409 — "+
			"\"Build/Test\" slugs onto \"build & test\"'s key and would replace its completed capture; body=%s",
			w.Code, w.Body.String())
	}
	if fake.saved != nil {
		t.Errorf("the refused request still wrote record_results: %s", fake.saved)
	}

	// NEGATIVE CONTROL, in the same test so the guard cannot be "409 always":
	// the same label re-records, reaching the no-runner 503 like every other
	// well-formed request against this fixture.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"build & test"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("same-label re-record: code = %d, want 503 (allowed through, no runner) — "+
			"re-recording a session is the normal loop; body=%s", w.Code, w.Body.String())
	}

	// The derived verify: key is namespaced and NEVER collides with the
	// recording it replays, so a confined request for the same session name
	// must pass the guard too.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"Build/Test","confined":true}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("confined verify of the same session: code = %d, want 503 (allowed through, no runner) — "+
			"the derived verify: key must never trip the relabel guard; body=%s", w.Code, w.Body.String())
	}
}
