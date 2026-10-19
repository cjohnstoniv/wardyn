// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "encoding/json"

// recordObservationEgressKeys are the record-result observation keys that name where a recording
// reached and what it ran: hosts, ip:port destinations, paths written, executables, and the anomaly
// sentences that quote them. Gone at the member tier, as the egress: requirement keys are.
var recordObservationEgressKeys = []string{"domains", "connects", "file_writes", "exec_argv0s", "anomalies"}

// redactRecordResultsForRead projects the workspace's record_results (task key -> RecordTaskResult)
// to its reader's tier, the way redactProfileForRead projects the scanned profile: below the full
// tier no stored secret name (secret_names_minted), and at the member tier none of the observed
// egress. It deletes keys from the decoded objects so a field it does not know survives, and a blob
// that will not decode is withheld entirely.
func redactRecordResultsForRead(raw json.RawMessage, tier workspaceReadTier) json.RawMessage {
	if tier == workspaceReadFull || len(raw) == 0 {
		return raw
	}
	var tasks map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return nil
	}
	for _, task := range tasks {
		delete(task, "secret_names_minted")
		if tier != workspaceReadMember || len(task["observations"]) == 0 {
			continue
		}
		var obs map[string]json.RawMessage
		if err := json.Unmarshal(task["observations"], &obs); err != nil {
			delete(task, "observations")
			continue
		}
		for _, k := range recordObservationEgressKeys {
			delete(obs, k)
		}
		b, err := json.Marshal(obs)
		if err != nil {
			return nil
		}
		task["observations"] = b
	}
	out, err := json.Marshal(tasks)
	if err != nil {
		return nil
	}
	return out
}
