// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRunKeepsItsCorrelationID creates runs through the real door: the run keeps the trimmed id
// and returns it on create and on read, its stored run.create row carries no copy (the sinks stamp it),
// and a bad id is a 422 that creates nothing.
func TestPG_CreateRunKeepsItsCorrelationID(t *testing.T) {
	srv, _ := pgHarness(t)
	srv.cfg.AgentImages = composeAgentImages
	const rest = `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"title":"t","correlation_id":"  build-42/a:b  ",`+rest+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	var created types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.CorrelationID != "build-42/a:b" {
		t.Errorf("create answered correlation_id %q, want build-42/a:b", created.CorrelationID)
	}
	var read types.AgentRun
	if r := do(t, srv, http.MethodGet, "/api/v1/runs/"+created.ID.String(), adminToken, ""); r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &read) != nil || read.CorrelationID != "build-42/a:b" {
		t.Errorf("read = %d %s; want correlation_id build-42/a:b", r.Code, r.Body.String())
	}
	rec, ok := srv.cfg.Audit.(*recRecorder)
	if !ok {
		t.Fatalf("the harness records into %T, want *recRecorder", srv.cfg.Audit)
	}
	ev := waitForRecAudit(t, rec, created.ID, "run.create", "success")
	if strings.Contains(string(ev.Data), "correlation_id") {
		t.Errorf("the recorded run.create data carries the id (%s); it is stamped on the way to the sinks only", ev.Data)
	}

	none := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"title":"t",`+rest+`}`)
	if none.Code != http.StatusCreated || strings.Contains(none.Body.String(), "correlation_id") {
		t.Errorf("a run with none = %d %s; want 201 and no correlation_id", none.Code, none.Body.String())
	}

	bad := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"title":"t","correlation_id":"two words",`+rest+`}`)
	if bad.Code != http.StatusUnprocessableEntity || !strings.Contains(bad.Body.String(), `"reason":"correlation_id_invalid"`) || !strings.Contains(bad.Body.String(), correlationIDInvalidMsg) {
		t.Errorf("a bad id = %d %s; want 422 correlation_id_invalid", bad.Code, bad.Body.String())
	}
}
