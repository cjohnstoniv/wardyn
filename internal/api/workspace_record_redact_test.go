// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A recording's results reach each reader at their tier: the owner sees everything, a security admin
// loses the stored secret names, and a member also loses the observed egress, keeping the status
// and the counts the checklist renders.
func TestWorkspaceReadProjectsRecordResultsFieldByField(t *testing.T) {
	raw := json.RawMessage(`{"build":{"status":"recorded","caught":1,"secret_names_minted":["prod-artifactory-token"],
		"observations":{"domains":[{"host":"artifacts.corp.internal"}],"connects":["10.0.0.5:443"],
		"file_writes":["/root/.npmrc"],"exec_argv0s":["npm"],"anomalies":["egress.deny to artifacts.corp.internal"],
		"minted_grant_ids":["7f0c1f5e-0000-4000-8000-000000000001"]}}}`)
	read := func(tier workspaceReadTier) (task, obs map[string]json.RawMessage) {
		out := redactWorkspaceForRead(types.Workspace{RecordResults: raw}, tier)
		var tasks map[string]map[string]json.RawMessage
		if err := json.Unmarshal(out.RecordResults, &tasks); err != nil {
			t.Fatalf("tier %d: record_results = %s: %v", tier, out.RecordResults, err)
		}
		task = tasks["build"]
		if err := json.Unmarshal(task["observations"], &obs); err != nil {
			t.Fatalf("tier %d: observations = %s: %v", tier, task["observations"], err)
		}
		return task, obs
	}
	egress := []string{"domains", "connects", "file_writes", "exec_argv0s", "anomalies"}

	task, obs := read(workspaceReadFull)
	if task["secret_names_minted"] == nil || len(obs) != 6 {
		t.Errorf("the owner's read lost fields: task=%v obs=%v", task, obs)
	}
	task, obs = read(workspaceReadSecurity)
	if task["secret_names_minted"] != nil {
		t.Errorf("a security admin reads secret_names_minted = %s, want it withheld", task["secret_names_minted"])
	}
	for _, k := range egress {
		if obs[k] == nil {
			t.Errorf("a security admin lost observations.%s, which decides this workspace's egress", k)
		}
	}
	task, obs = read(workspaceReadMember)
	if task["secret_names_minted"] != nil {
		t.Errorf("a member reads secret_names_minted = %s, want it withheld", task["secret_names_minted"])
	}
	for _, k := range egress {
		if obs[k] != nil {
			t.Errorf("a member reads observations.%s = %s, want it withheld", k, obs[k])
		}
	}
	if string(task["status"]) != `"recorded"` || string(task["caught"]) != "1" || obs["minted_grant_ids"] == nil {
		t.Errorf("a member lost the status or counts: task=%v obs=%v", task, obs)
	}
	if out := redactWorkspaceForRead(types.Workspace{RecordResults: json.RawMessage(`[1]`)}, workspaceReadMember); out.RecordResults != nil {
		t.Errorf("an undecodable record_results reached a member: %s", out.RecordResults)
	}
}
