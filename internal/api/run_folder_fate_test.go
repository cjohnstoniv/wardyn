// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run view says what became of an ended run's folder changes: retained with the last report's count
// and names, given up with the record's, complete, or nothing while the run is live. The sandbox copy is
// deleted only once its ref was cleared, and a count is null only when no pass ever counted it.
func TestComputerFolderRunView_FateContract(t *testing.T) {
	ended := time.Now().UTC().Truncate(time.Second)
	giveUpAt := `"give_up_at":"` + ended.Add(pendingFloor).Format(time.RFC3339Nano) + `"`
	count := 3
	for name, tc := range map[string]struct {
		state  types.RunState
		ref    string
		status types.RunLocalFolderStatus
		want   []string
		absent []string
	}{
		"live": {state: types.RunRunning, ref: "sbx", absent: []string{`"fate"`, `"sandbox_copy"`, `"last_report"`}},
		"retained, counted": {state: types.RunStopped, ref: "sbx",
			status: types.RunLocalFolderStatus{Resume: &types.FolderResume{Stalled: 1, Unsynced: 53, Counted: true, Paths: []string{"a.txt", "b.txt"}}},
			want:   []string{`"fate":"retained"`, `"sandbox_copy":"kept"`, `"unsynced":53`, `"paths":["a.txt","b.txt"]`, `"paths_more":51`, giveUpAt}},
		"retained, uncounted": {state: types.RunStopped, ref: "sbx",
			status: types.RunLocalFolderStatus{Resume: &types.FolderResume{Stalled: 1}},
			want:   []string{`"fate":"retained"`, `"unsynced":null`}},
		"given up, stop not confirmed": {state: types.RunStopped, ref: "sbx",
			status: types.RunLocalFolderStatus{Back: &types.FolderPassRecord{GivenUpAt: &ended, GivenUpCount: &count, GivenUpPaths: []string{"a.txt"}, GivenUpPathsMore: 2, Refused: []types.FolderRefusal{}}},
			want:   []string{`"fate":"given_up"`, `"sandbox_copy":"kept"`, `"unsynced":3`, `"paths_more":2`},
			absent: []string{`"give_up_at"`}},
		"given up, uncounted": {state: types.RunStopped,
			status: types.RunLocalFolderStatus{Back: &types.FolderPassRecord{GivenUpAt: &ended, Refused: []types.FolderRefusal{}}},
			want:   []string{`"fate":"given_up"`, `"sandbox_copy":"deleted"`, `"unsynced":null`}},
		"complete, deleted": {state: types.RunStopped,
			status: types.RunLocalFolderStatus{Back: &types.FolderPassRecord{Files: 2, Refused: []types.FolderRefusal{}}},
			want:   []string{`"fate":"complete"`, `"sandbox_copy":"deleted"`},
			absent: []string{`"last_report"`}},
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
			if !s.holdForLocalFolder(context.Background(), run, "sbx") {
				t.Fatal(cs.FailureHint())
			}
			s.cfg.EndedRunGrace = 0
			cs.mu.Lock()
			cs.state, cs.run.SandboxRef, cs.run.EndedAt = tc.state, tc.ref, &ended
			cs.mu.Unlock()
			cs.smu.Lock()
			seed := cs.status.Seed
			cs.status = tc.status
			cs.status.Seed = seed
			cs.smu.Unlock()
			b, err := json.Marshal(s.computerFolderRunView(context.Background(), run.ID))
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(b), w) {
					t.Errorf("view %s; want %s", b, w)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(string(b), a) {
					t.Errorf("view %s; want no %s", b, a)
				}
			}
		})
	}
}
