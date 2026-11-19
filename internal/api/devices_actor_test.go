// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDevices_IngestKeepsEveryStoredActorType: a laptop's service row is stored as claimed and its cursor moves
// past it and the human row after it; before, the 400 on the service row stopped the forwarder for good.
func TestDevices_IngestKeepsEveryStoredActorType(t *testing.T) {
	srv, st, _ := newDeviceTestServer(t, false)
	id, tok := enrolTestDevice(t, srv, "alices-laptop")
	path := "/api/v1/devices/" + id.String() + "/audit"

	rows := chainRows(1, 2, "")
	rows[0].ActorType, rows[0].Actor = types.ActorService, "service:ci-bot"
	if got := ackedSeq(t, do(t, srv, http.MethodPost, path, tok, string(mustJSON(rows)))); got != 2 {
		t.Fatalf("acked_seq = %d, want 2: the service row then the human row", got)
	}
	st.mu.Lock()
	stored := append([]types.FederatedAuditEvent(nil), st.ingested[id]...)
	st.mu.Unlock()
	if len(stored) != 2 || stored[0].ActorType != types.ActorService || stored[1].ActorType != types.ActorHuman {
		t.Fatalf("stored rows = %+v, want the service row then the human row, each keeping its claimed actor_type", stored)
	}

	// Every other stored kind is accepted too, and a value no row can hold is still a 400.
	next := int64(3)
	prev := stored[1].RowHash
	for _, a := range []types.ActorType{types.ActorHuman, types.ActorAgent, types.ActorSystem, types.ActorService} {
		r := chainRows(next, 1, prev)
		r[0].ActorType = a
		if got := ackedSeq(t, do(t, srv, http.MethodPost, path, tok, string(mustJSON(r)))); got != next {
			t.Fatalf("actor_type %q: acked_seq = %d, want %d", a, got, next)
		}
		next, prev = next+1, r[0].RowHash
	}
	for _, bad := range []string{"device", "Service", "service:ci-bot", ""} {
		r := string(mustJSON(chainRows(next, 1, prev)))
		r = strings.Replace(r, `"actor_type":"human"`, `"actor_type":"`+bad+`"`, 1)
		if w := do(t, srv, http.MethodPost, path, tok, r); w.Code != http.StatusBadRequest {
			t.Fatalf("actor_type %q: status = %d, want 400: %s", bad, w.Code, w.Body.String())
		}
	}
}
