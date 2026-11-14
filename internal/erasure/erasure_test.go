// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package erasure

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func recordingSteps(ran *[]Scope, fail map[Scope]error) map[Scope]Step {
	steps := map[Scope]Step{}
	for _, s := range Scopes() {
		steps[s] = func(context.Context, string) (any, error) {
			if err := fail[s]; err != nil {
				return nil, err
			}
			*ran = append(*ran, s)
			return string(s), nil
		}
	}
	return steps
}

func TestOrchestrateRunsInTheFixedOrderWhateverWasAsked(t *testing.T) {
	var ran []Scope
	o := &Orchestrator{Steps: recordingSteps(&ran, nil)}
	rep, err := o.Orchestrate(t.Context(), "alice", []Scope{Credentials, MaskCopies, RunTasks, MaskCopies})
	if err != nil {
		t.Fatal(err)
	}
	want := []Scope{MaskCopies, RunTasks, Credentials}
	if !slices.Equal(ran, want) || !slices.Equal(rep.Done, want) {
		t.Fatalf("ran %v, done %v, want %v", ran, rep.Done, want)
	}
	if rep.Details[RunTasks] != "run_tasks" {
		t.Errorf("details = %v, want each step's own", rep.Details)
	}
}

func TestOrchestrateStopsAtTheFirstFailureAndNamesWhatIsLeft(t *testing.T) {
	var ran []Scope
	boom := errors.New("store down")
	fail := map[Scope]error{RunTasks: boom}
	o := &Orchestrator{Steps: recordingSteps(&ran, fail)}
	rep, err := o.Orchestrate(t.Context(), "alice", Scopes())
	var inc *IncompleteError
	if !errors.As(err, &inc) {
		t.Fatalf("err = %v, want *IncompleteError", err)
	}
	if want := []Scope{RunTasks, Components, RunSetups, RunnerPools, MemberDrives, AuditPersonalFields, Credentials}; !slices.Equal(inc.Remaining, want) {
		t.Errorf("remaining = %v, want %v", inc.Remaining, want)
	}
	if want := []Scope{MaskCopies, RunOutputs, Recordings}; !slices.Equal(inc.Done, want) || !slices.Equal(rep.Done, want) {
		t.Errorf("done = %v / %v, want %v", inc.Done, rep.Done, want)
	}
	if !errors.Is(err, boom) {
		t.Error("the cause is not reachable with errors.Is")
	}

	// A retry finishes the rest, and runs the finished steps again (idempotent).
	delete(fail, RunTasks)
	ran = nil
	rep, err = o.Orchestrate(t.Context(), "alice", Scopes())
	if err != nil || len(rep.Done) != len(Scopes()) {
		t.Fatalf("retry = %v, done %v", err, rep.Done)
	}
}

func TestOrchestrateRefusesAScopeWithNoStepInsteadOfReportingItErased(t *testing.T) {
	o := &Orchestrator{Steps: map[Scope]Step{}}
	_, err := o.Orchestrate(t.Context(), "alice", []Scope{Recordings})
	if !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("err = %v, want ErrNotAvailable", err)
	}
}

func TestOrchestrateRefusesBadInputBeforeRunningAnything(t *testing.T) {
	var ran []Scope
	o := &Orchestrator{Steps: recordingSteps(&ran, nil)}
	for name, tc := range map[string]struct {
		person string
		scopes []Scope
	}{
		"no person":     {"", []Scope{Credentials}},
		"empty scopes":  {"alice", nil},
		"unknown scope": {"alice", []Scope{Credentials, "everything"}},
	} {
		if _, err := o.Orchestrate(t.Context(), tc.person, tc.scopes); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if len(ran) != 0 {
		t.Errorf("ran %v, want nothing", ran)
	}
}

func TestParseScopes(t *testing.T) {
	got, err := ParseScopes([]string{"credentials", "recordings"})
	if err != nil || !slices.Equal(got, []Scope{Credentials, Recordings}) {
		t.Fatalf("ParseScopes = %v, %v", got, err)
	}
	for _, bad := range [][]string{nil, {}, {"credentials", "nope"}, {""}} {
		if _, err := ParseScopes(bad); !errors.Is(err, ErrScopeUnknown) {
			t.Errorf("ParseScopes(%q) = %v, want ErrScopeUnknown", bad, err)
		}
	}
}

func TestComponentsScopeRunsBeforeTheKeys(t *testing.T) {
	var ran []Scope
	o := &Orchestrator{Steps: recordingSteps(&ran, nil)}
	scopes, err := ParseScopes([]string{"credentials", "components", "audit_personal_fields"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Orchestrate(t.Context(), "alice", scopes); err != nil {
		t.Fatal(err)
	}
	if want := []Scope{Components, AuditPersonalFields, Credentials}; !slices.Equal(ran, want) {
		t.Fatalf("ran %v, want %v", ran, want)
	}
}

// A step that fails after doing part of its work hands that part back on the error, so the caller's
// failure row can still say what the step did.
func TestOrchestrateKeepsWhatAFailedStepReported(t *testing.T) {
	part := map[string]any{"tokens_revocation_failed": true}
	o := &Orchestrator{Steps: map[Scope]Step{
		RunTasks:    func(context.Context, string) (any, error) { return 1, nil },
		Credentials: func(context.Context, string) (any, error) { return part, errors.New("the key store is down") },
	}}
	rep, err := o.Orchestrate(context.Background(), "p", []Scope{Credentials, RunTasks})
	var inc *IncompleteError
	if !errors.As(err, &inc) {
		t.Fatalf("err = %v, want *IncompleteError", err)
	}
	if got, _ := inc.Detail.(map[string]any); got["tokens_revocation_failed"] != true {
		t.Errorf("Detail = %v, want the failed step's data", inc.Detail)
	}
	if _, done := rep.Details[Credentials]; done || len(rep.Done) != 1 {
		t.Errorf("report = %+v, want the failed scope neither done nor detailed", rep)
	}
}
