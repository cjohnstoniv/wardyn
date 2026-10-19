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
// what the containment read holds back from the same reader: a node record's source_id and the
// runner's own reason for refusing a stop_proxy name infrastructure, security operators only (that
// reader gets nodeRecordReasonWithheld for the reason). The stored row, and what the security tier
// and the sinks read, keep both. A row whose data does not decode is served without data rather than unread.
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
		case ev.Action == auditRunnerActionRefuse && rawJSONString(data["action"]) == string(types.RunnerActionStopProxy) && data["reason"] != nil:
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

// rawJSONString is v decoded as a JSON string, or "" when it is not one, so a match on the decoded value
// holds whatever escaping or spacing the row's writer used.
func rawJSONString(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}
