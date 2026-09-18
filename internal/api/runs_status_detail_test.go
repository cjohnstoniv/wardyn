// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// getRunDetail reads one run back off the wire the way the console's run page
// does, decoding into the wire shape rather than types.AgentRun so a field that
// is supposed to be ABSENT is distinguishable from one that is empty.
func getRunDetail(t *testing.T, srv *Server, id uuid.UUID) map[string]any {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s = %d, body=%s", id, w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode run: %v (%s)", err, w.Body.String())
	}
	return got
}

// TestGetRun_ServesStatusDetailWhileStarting: the whole point of finding 6 —
// the kubelet's reason reaches the person while the run is still starting, with
// the raw substrate string for the UI's parser AND the bare reason token for
// everything that is not a person (metrics, the live specs, later automation).
func TestGetRun_ServesStatusDetailWhileStarting(t *testing.T) {
	ast := newAuthzStore()
	srv := New(baseTestConfig(newHarness(t), ast))
	run, err := ast.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedBy: "admin-token", State: types.RunStarting,
		StatusDetail: "agent: ContainerCreating",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got := getRunDetail(t, srv, run.ID)
	if got["status_detail"] != "agent: ContainerCreating" {
		t.Errorf("status_detail = %v, want the substrate's own words", got["status_detail"])
	}
	if got["status_reason"] != "ContainerCreating" {
		t.Errorf("status_reason = %v, want the bare reason token derived at read", got["status_reason"])
	}
}

// TestGetRun_BlanksStatusDetailOnNonStartingRun: status_detail is never CLEARED
// by a write — the last reason stays on the row for a SQL postmortem — so the
// read is what makes it honest. A RUNNING or COMPLETED run is not waiting on
// anything, and a console that re-renders "Downloading the image" over a
// finished run is worse than saying nothing.
func TestGetRun_BlanksStatusDetailOnNonStartingRun(t *testing.T) {
	for _, state := range []types.RunState{types.RunRunning, types.RunCompleted, types.RunStopped, types.RunKilled} {
		t.Run(string(state), func(t *testing.T) {
			ast := newAuthzStore()
			srv := New(baseTestConfig(newHarness(t), ast))
			run, err := ast.CreateRun(context.Background(), types.AgentRun{
				ID: uuid.New(), CreatedBy: "admin-token", State: state,
				StatusDetail: "agent: ContainerCreating",
			})
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			got := getRunDetail(t, srv, run.ID)
			if _, present := got["status_detail"]; present {
				t.Errorf("status_detail = %v on a %s run, want it blanked at read", got["status_detail"], state)
			}
			if _, present := got["status_reason"]; present {
				t.Errorf("status_reason = %v on a %s run, want it blanked at read", got["status_reason"], state)
			}
		})
	}
}

// TestGetRun_TerminalStartupReasonSurvivesFailed is Codex #11: the run that
// went STARTING -> FAILED between two of the browser's 3-4 second polls.
// waitContainerRunning errors the INSTANT the kubelet says ImagePullBackOff, so
// a reader who never happened to catch a STARTING poll would otherwise be shown
// a reason-less FAILED badge for the one failure mode whose reason is the fix.
// A NON-terminal reason on a FAILED run is still blanked: the run did not fail
// BECAUSE it was creating a container, and presenting a stale wait as a cause is
// the misdiagnosis this whole lane exists to end.
func TestGetRun_TerminalStartupReasonSurvivesFailed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		detail     string
		hint       string
		wantDetail string
		wantReason string
	}{
		{
			name:       "the reason that is terminal survives with the registry's words",
			detail:     "agent: ImagePullBackOff: rpc error: pull access denied",
			hint:       "the sandbox could not be created: agent container stuck waiting (ImagePullBackOff): rpc error: pull access denied",
			wantDetail: "agent: ImagePullBackOff: rpc error: pull access denied",
			wantReason: "ImagePullBackOff",
		},
		{
			name:       "a bad reference is terminal too",
			detail:     "agent: InvalidImageName: couldn't parse image name",
			wantDetail: "agent: InvalidImageName: couldn't parse image name",
			wantReason: "InvalidImageName",
		},
		{
			// S2: the terminal status write is the ONE a 500ms deadline is
			// allowed to drop, and dispatch marks the run FAILED the instant
			// waitContainerRunning errors — so the row can hold nothing at all
			// while failure_hint holds the same sentence. Blanking here would
			// hand every UI surface an empty string and a reason it cannot
			// render: 0.7.5's reason-less FAILED badge, reached a new way.
			name:       "the hint alone still puts the registry's words on the wire",
			hint:       "the sandbox could not be created: k8s: agent pod's main container never started: agent container stuck waiting (ImagePullBackOff): rpc error: pull access denied",
			wantDetail: "agent: ImagePullBackOff: rpc error: pull access denied",
			wantReason: "ImagePullBackOff",
		},
		{
			// The same race one tick earlier: ContainerCreating landed, the
			// ImagePullBackOff that followed it did not. A STALE non-terminal
			// detail must not out-rank the hint that names the real ending.
			name:       "a stale non-terminal detail loses to a stuck hint",
			detail:     "agent: ContainerCreating",
			hint:       "the sandbox could not be created: agent container stuck waiting (CreateContainerConfigError): secret \"wardyn-run\" not found",
			wantDetail: "agent: CreateContainerConfigError: secret \"wardyn-run\" not found",
			wantReason: "CreateContainerConfigError",
		},
		{
			name:   "an ordinary wait is not a cause of death",
			detail: "agent: ContainerCreating",
			hint:   "the sandbox could not be created: context deadline exceeded",
		},
		{
			name:   "nothing was ever read",
			detail: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast := newAuthzStore()
			srv := New(baseTestConfig(newHarness(t), ast))
			run, err := ast.CreateRun(context.Background(), types.AgentRun{
				ID: uuid.New(), CreatedBy: "admin-token", State: types.RunFailed,
				StatusDetail: tc.detail, FailureHint: tc.hint,
			})
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			got := getRunDetail(t, srv, run.ID)
			if d, _ := got["status_detail"].(string); d != tc.wantDetail {
				t.Errorf("status_detail = %q, want %q", d, tc.wantDetail)
			}
			if r, _ := got["status_reason"].(string); r != tc.wantReason {
				t.Errorf("status_reason = %q, want %q", r, tc.wantReason)
			}
		})
	}
}

// TestGetRun_StartingAndFailedBetweenPollsPresentTheSameSentence is the
// assertion live case E2 rests on: the reader who caught the STARTING poll and
// the reader who did not must be shown the same thing. If these two ever
// diverge, the live case is timing-dependent and will flake on somebody else's
// cluster instead of failing here.
func TestGetRun_StartingAndFailedBetweenPollsPresentTheSameSentence(t *testing.T) {
	const detail = "agent: ImagePullBackOff: rpc error: pull access denied"
	ast := newAuthzStore()
	srv := New(baseTestConfig(newHarness(t), ast))
	starting, err := ast.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedBy: "admin-token", State: types.RunStarting, StatusDetail: detail,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	failed, err := ast.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedBy: "admin-token", State: types.RunFailed, StatusDetail: detail,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	a, b := getRunDetail(t, srv, starting.ID), getRunDetail(t, srv, failed.ID)
	if a["status_detail"] != b["status_detail"] || a["status_reason"] != b["status_reason"] {
		t.Fatalf("the run caught in STARTING presents %v/%v; the one that had already FAILED presents %v/%v",
			a["status_detail"], a["status_reason"], b["status_detail"], b["status_reason"])
	}
}

// TestListRuns_BlanksStatusDetailOnNonStartingRun pins the SAME projection on
// the board's route. The Runs board polls this one every three seconds and
// renders the sentence under the badge; a projection applied on the detail read
// alone would have the board narrating a finished run's last wait forever.
func TestListRuns_BlanksStatusDetailOnNonStartingRun(t *testing.T) {
	ast := newAuthzStore()
	srv := New(baseTestConfig(newHarness(t), ast))
	starting, err := ast.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedBy: "admin-token", State: types.RunStarting,
		StatusDetail: "agent: ContainerCreating",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	done, err := ast.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedBy: "admin-token", State: types.RunCompleted,
		StatusDetail: "agent: ContainerCreating",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d, body=%s", w.Code, w.Body.String())
	}
	var runs []types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[uuid.UUID]types.AgentRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	if got := byID[starting.ID]; got.StatusDetail != "agent: ContainerCreating" || got.StatusReason != "ContainerCreating" {
		t.Errorf("starting run on the board = %q/%q, want the reason", got.StatusDetail, got.StatusReason)
	}
	if got := byID[done.ID]; got.StatusDetail != "" || got.StatusReason != "" {
		t.Errorf("completed run on the board = %q/%q, want both blank", got.StatusDetail, got.StatusReason)
	}
}
