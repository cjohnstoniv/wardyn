// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordResultsMap decodes the workspace's opaque record_results blob. A
// missing/malformed blob is an empty map (fail-open to "nothing recorded").
func recordResultsMap(ws types.Workspace) map[string]RecordTaskResult {
	out := map[string]RecordTaskResult{}
	if len(ws.RecordResults) > 0 {
		_ = json.Unmarshal(ws.RecordResults, &out)
	}
	return out
}

// inFlightRecordResults is raw reduced to the entries still `recording`, nil when
// there are none. A sources edit clears the reviewed results but must leave a live
// session's marker: the capture that settles it is written under that entry.
func inFlightRecordResults(raw json.RawMessage) json.RawMessage {
	var all map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &all) != nil {
		return nil
	}
	keep := map[string]json.RawMessage{}
	for k, v := range all {
		var e struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(v, &e) == nil && e.Status == recordStatusRecording {
			keep[k] = v
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return mustJSON(keep)
}

// putRecordResult upserts one task's entry via the store's ATOMIC per-key
// jsonb merge (never a whole-map read-modify-write, so writers of different
// tasks can't lose each other). onlyIfStatus, when non-empty, makes the write
// a compare-and-set on the entry's CURRENT status — the guard that stops a
// late streaming upload from reverting a completed capture, and makes capture
// itself idempotent across the watcher/kill/boot/read-repair triggers.
func (s *Server) putRecordResult(ctx context.Context, wsID uuid.UUID, taskKey string, res RecordTaskResult, onlyIfStatus string) (types.Workspace, bool, error) {
	return s.cfg.Store.SetWorkspaceRecordResult(ctx, wsID, taskKey, mustJSON(res), onlyIfStatus)
}
