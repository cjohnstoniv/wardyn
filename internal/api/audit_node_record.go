// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// auditRunnerActionRefuse is the runner's refusal receipt (store.recordRunnerActionRefusalTx).
const auditRunnerActionRefuse = "runner.action.refuse"

// withoutNodeRecordSource is evs as a reader below the security tier is served them, holding back
// what the containment read holds back from the same reader: a node record's source_id, and the
// runner's own reason for refusing a stop_proxy, which can quote a Node's name (that reader gets
// nodeRecordReasonWithheld). The stored row, and what the security tier and the sinks read, keep
// both. A row whose data does not decode is served without data rather than unread.
func withoutNodeRecordSource(evs []types.AuditEvent) []types.AuditEvent {
	for i, ev := range evs {
		if (ev.Action != auditRunNodeRecord && ev.Action != auditRunnerActionRefuse) || len(ev.Data) == 0 {
			continue
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			evs[i].Data = nil
			continue
		}
		switch {
		case ev.Action == auditRunNodeRecord && data["source_id"] != nil:
			delete(data, "source_id")
		case ev.Action == auditRunnerActionRefuse && string(data["action"]) == `"`+string(types.RunnerActionStopProxy)+`"` && data["reason"] != nil:
			data["reason"] = mustJSON(nodeRecordReasonWithheld)
		default:
			continue
		}
		b, err := json.Marshal(data)
		if err != nil {
			b = nil
		}
		evs[i].Data = b
	}
	return evs
}
