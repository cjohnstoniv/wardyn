// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A workspace list the library door cannot read is a server fault that scans nothing, recorded as the workspace
// door records it: one failed source.scan row on the folder, whose only field is the error as its detail.
func TestHandleScanSource_AnUnreadableWorkspaceListWritesAFailedScanRow(t *testing.T) {
	_, project := memberProjectRoot(t)
	id := uuid.New()
	st := &scanSourceStore{sources: map[uuid.UUID]types.Source{id: {ID: id, Kind: types.SourceLocalDir, Locator: project}}, wsListErr: errors.New("store down")}
	h := newHarness(t)
	srv := New(baseTestConfig(h, st))
	if w := do(t, srv, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, ""); w.Code != http.StatusInternalServerError || st.statuses[id] != "" {
		t.Fatalf("an unreadable workspace list = %d %s (status %q), want 500 and nothing written", w.Code, w.Body.String(), st.statuses[id])
	}
	var rows []types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "source.scan" {
			rows = append(rows, ev)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("source.scan rows = %+v, want one", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if rows[0].Outcome != "failure" || rows[0].Target != id.String() || len(data) != 1 || !strings.Contains(fmt.Sprint(data["detail"]), "store down") {
		t.Fatalf("source.scan row = %s %s %s, want a failure on the folder with the error as its only field, detail", rows[0].Outcome, rows[0].Target, rows[0].Data)
	}
}
