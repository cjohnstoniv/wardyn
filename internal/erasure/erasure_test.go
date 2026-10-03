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
	if want := []Scope{RunTasks, AuditPersonalFields, Credentials}; !slices.Equal(inc.Remaining, want) {
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
