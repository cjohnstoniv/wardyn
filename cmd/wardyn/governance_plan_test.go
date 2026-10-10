// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runGovernancePlan runs `governance plan` against url and returns what it
// printed to stdout and to stderr.
func runGovernancePlan(t *testing.T, url string, doc sdk.GovernanceDocument, prune bool) (stdout, stderr string, err error) {
	t.Helper()
	b, merr := json.Marshal(doc)
	if merr != nil {
		t.Fatalf("marshal doc: %v", merr)
	}
	path := filepath.Join(t.TempDir(), "governance.json")
	if werr := os.WriteFile(path, b, 0o600); werr != nil {
		t.Fatalf("write doc: %v", werr)
	}
	args := []string{"governance", "plan", path, "--url", url, "--token", "tok"}
	if prune {
		args = append(args, "--prune")
	}
	root := rootCmd()
	var out, errOut strings.Builder
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func TestGovernancePlan_PrintsPlanAndSendsNoWrites(t *testing.T) {
	srv, seen := countingServer(t)
	doc := sdk.GovernanceDocument{
		Profiles:    []sdk.GovernanceProfile{{Name: "fresh"}},
		Assignments: []sdk.GovernanceAssignment{{SubjectType: sdk.CapabilitySubjectUser, Subject: "alice", ProfileID: uuid.New()}},
	}
	stdout, stderr, err := runGovernancePlan(t, srv.URL, doc, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var plan sdk.GovernancePlan
	if err := dec.Decode(&plan); err != nil {
		t.Fatalf("stdout is not a strict GovernancePlan: %v\n%s", err, stdout)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %+v, want a profile create and an assignment create", plan.Steps)
	}
	if !strings.Contains(stderr, "plan: 2 create, 0 update, 0 delete, 0 unchanged\n") {
		t.Fatalf("stderr = %q, want the plan summary line", stderr)
	}
	for _, req := range seen() {
		if req != "GET /api/v1/governance" {
			t.Fatalf("plan sent %q, want only the read", req)
		}
	}
}

func TestGovernancePlan_PruneListsEveryDeleteAndWritesNothing(t *testing.T) {
	fake := newFakeGovernanceServer()
	stale := sdk.GovernanceProfile{ID: uuid.New(), Name: "stale"}
	fake.profiles[stale.ID] = stale
	asgID := uuid.New()
	fake.assignments[asgID] = sdk.GovernanceAssignment{ID: asgID, SubjectType: sdk.CapabilitySubjectUser,
		Subject: "alice", ProfileID: stale.ID}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	stdout, stderr, err := runGovernancePlan(t, srv.URL, sdk.GovernanceDocument{}, true)
	if err != nil {
		t.Fatalf("plan --prune: %v", err)
	}
	for _, want := range []string{
		"plan: 0 create, 0 update, 2 delete, 0 unchanged\n",
		"deletes:\n",
		"  assignment user \"alice\"\n",
		"  profile \"stale\"\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	if !strings.Contains(stdout, `"destructive": true`) {
		t.Fatalf("stdout = %s, want destructive true", stdout)
	}
	if fake.writeCount != 0 || len(fake.profiles) != 1 || len(fake.assignments) != 1 {
		t.Fatalf("plan --prune changed the install: writes %d, profiles %d, assignments %d",
			fake.writeCount, len(fake.profiles), len(fake.assignments))
	}
}
