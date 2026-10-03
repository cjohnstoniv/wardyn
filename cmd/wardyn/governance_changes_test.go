// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// fakePendingGovernance answers the governance family the way a deployment
// that requires a second approver does: every profile and assignment write is
// a 202 pending_change (docs/design/0.8/0.8.6-gov4.md, D2), and the /changes
// routes list, approve and reject one stored change.
type fakePendingGovernance struct {
	existing []sdk.GovernanceProfile
	change   sdk.GovernanceChange
	writes   []string
	calls    []string
	reason   string
}

func (f *fakePendingGovernance) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		pend := func(kind string) {
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"pending_change": map[string]any{
				"id": uuid.New(), "target_kind": kind, "op": "create", "target_key": "k",
				"state": "pending", "proposed_by": "alice",
				"proposed_at": time.Now().UTC().Format(time.RFC3339),
				"expires_at":  time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339),
				"diff":        map[string]any{"changed": []string{"name"}},
			}})
		}
		decided := func(state string) {
			ch := f.change
			ch.State = state
			_ = json.NewEncoder(w).Encode(ch)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance":
			_ = json.NewEncoder(w).Encode(sdk.GovernanceDocument{Profiles: f.existing})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/profiles":
			pend("governance_profile")
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/assignments":
			pend("governance_assignment")
		case r.Method == http.MethodDelete:
			pend("governance_profile")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance/changes":
			_ = json.NewEncoder(w).Encode([]sdk.GovernanceChange{f.change})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/approve"):
			decided("applied")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reject"):
			b, _ := io.ReadAll(r.Body)
			f.reason = string(b)
			decided("rejected")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// runGovernance runs the governance command with args against url and returns
// stdout, stderr and the error.
func runGovernance(t *testing.T, url string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := rootCmd()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	root.SetArgs(append([]string{"governance"}, append(args, "--url", url, "--token", "tok")...))
	root.SetOut(out)
	root.SetErr(errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func writeGovernanceFile(t *testing.T, doc sdk.GovernanceDocument) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "governance.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestGovernanceSet_PendingExitsZeroAndListsPendingAndDeferred: pending is the
// expected outcome under four-eyes, so it is not a failure. stdout stays a
// document `set` can strict-decode; the lists go to stderr.
func TestGovernanceSet_PendingExitsZeroAndListsPendingAndDeferred(t *testing.T) {
	f := &fakePendingGovernance{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	fileID := uuid.New()
	path := writeGovernanceFile(t, sdk.GovernanceDocument{
		Profiles: []sdk.GovernanceProfile{{ID: fileID, Name: "contractor"}},
		Assignments: []sdk.GovernanceAssignment{
			{SubjectType: sdk.CapabilitySubjectUser, Subject: "bob", ProfileID: fileID}},
	})
	stdout, stderr, err := runGovernance(t, srv.URL, "set", path)
	if err != nil {
		t.Fatalf("set exited non-zero on a pending change: %v", err)
	}
	for _, want := range []string{"pending approval: 1 change(s)", "deferred: 1 assignment(s)", `user "bob" -> profile "contractor"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	var doc sdk.GovernanceDocument
	if err := decodeOneJSONStrict(bytes.NewReader([]byte(stdout)), &doc); err != nil {
		t.Errorf("stdout is not a strict-decodable document: %v\n%s", err, stdout)
	}
	if got := strings.Join(f.writes, ","); got != "POST /api/v1/governance/profiles" {
		t.Errorf("writes = %q, want only the profile create (the dependent assignment is deferred)", got)
	}
}

func TestGovernanceSet_PrunePendingSkipsPrune(t *testing.T) {
	f := &fakePendingGovernance{existing: []sdk.GovernanceProfile{{ID: uuid.New(), Name: "stale"}}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	path := writeGovernanceFile(t, sdk.GovernanceDocument{Profiles: []sdk.GovernanceProfile{{Name: "new"}}})
	_, stderr, err := runGovernance(t, srv.URL, "set", path, "--prune")
	if err != nil {
		t.Fatalf("set --prune exited non-zero: %v", err)
	}
	if !strings.Contains(stderr, "prune skipped") {
		t.Errorf("stderr does not say prune was skipped:\n%s", stderr)
	}
	for _, w := range f.writes {
		if strings.HasPrefix(w, "DELETE ") {
			t.Errorf("a prune delete was issued after a pending write: %s", w)
		}
	}
}

func TestGovernanceChanges_ListApproveReject(t *testing.T) {
	f := &fakePendingGovernance{change: sdk.GovernanceChange{
		ID: uuid.New(), TargetKind: "governance_profile", Op: "update", TargetKey: "k",
		State: "pending", ProposedBy: "alice",
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	id := f.change.ID.String()

	out, _, err := runGovernance(t, srv.URL, "changes", "list", "--state", "pending")
	if err != nil {
		t.Fatalf("changes list: %v", err)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, "alice") {
		t.Errorf("list output missing the change:\n%s", out)
	}
	if got := f.calls[len(f.calls)-1]; got != "GET /api/v1/governance/changes?state=pending" {
		t.Errorf("list request = %q", got)
	}
	jsonOut, _, err := runGovernance(t, srv.URL, "changes", "list", "--json")
	if err != nil {
		t.Fatalf("changes list --json: %v", err)
	}
	var listed []sdk.GovernanceChange
	if err := json.Unmarshal([]byte(jsonOut), &listed); err != nil || len(listed) != 1 || listed[0].ID != f.change.ID {
		t.Errorf("list --json = %q (%v)", jsonOut, err)
	}

	out, _, err = runGovernance(t, srv.URL, "changes", "approve", id)
	if err != nil || !strings.Contains(out, id+" -> applied") {
		t.Errorf("approve = %q, %v", out, err)
	}
	out, _, err = runGovernance(t, srv.URL, "changes", "reject", id, "--reason", "not now")
	if err != nil || !strings.Contains(out, id+" -> rejected") || !strings.Contains(f.reason, `"reason":"not now"`) {
		t.Errorf("reject = %q, %v (body %q)", out, err, f.reason)
	}
	if _, _, err := runGovernance(t, srv.URL, "changes", "approve", "not-a-uuid"); err == nil {
		t.Error("approve accepted a malformed id")
	}
}

// TestGovernanceSet_PendingBaseListsDeferredChildProfile: a child profile whose base is held for
// approval is reported as deferred, and only the base is written.
func TestGovernanceSet_PendingBaseListsDeferredChildProfile(t *testing.T) {
	f := &fakePendingGovernance{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	baseID := uuid.New()
	path := writeGovernanceFile(t, sdk.GovernanceDocument{Profiles: []sdk.GovernanceProfile{
		{Name: "child", BaseProfileID: &baseID},
		{ID: baseID, Name: "base"},
	}})
	_, stderr, err := runGovernance(t, srv.URL, "set", path)
	if err != nil {
		t.Fatalf("set exited non-zero on a pending change: %v", err)
	}
	if !strings.Contains(stderr, `profile "child" -> base "base"`) {
		t.Errorf("stderr missing the deferred child:\n%s", stderr)
	}
	if got := strings.Join(f.writes, ","); got != "POST /api/v1/governance/profiles" {
		t.Errorf("writes = %q, want only the base create", got)
	}
}
