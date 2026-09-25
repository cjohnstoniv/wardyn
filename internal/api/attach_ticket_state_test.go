// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAttachTicket_NonRunningRunIs409AndMintsNothing: a ticket is a live PTY
// credential, so none is minted for a run that cannot be attached — the
// refusal is a 409 at the mint, not a ticket that fails later at the socket.
func TestAttachTicket_NonRunningRunIs409AndMintsNothing(t *testing.T) {
	for _, state := range []types.RunState{types.RunPending, types.RunStarting, types.RunWaiting, types.RunStopped, types.RunCompleted, types.RunFailed, types.RunKilled} {
		t.Run(string(state), func(t *testing.T) {
			ast := newAuthzStore()
			h := newHarness(t)
			srv := New(baseTestConfig(h, ast))
			run := uuid.New()
			ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "alice", State: state, SandboxRef: "sbx-1"}

			w := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.String()+"/attach-ticket", adminToken, "")
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
			}
			if len(ast.tickets) != 0 {
				t.Fatalf("%d tickets minted for a %s run", len(ast.tickets), state)
			}
		})
	}
}
