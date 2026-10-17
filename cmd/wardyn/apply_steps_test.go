// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runCaptured runs args on the root command and returns what it printed to
// stdout and to stderr.
func runCaptured(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := rootCmd()
	var out, errOut strings.Builder
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// writeDocFile marshals doc into a file under t.TempDir and returns its path.
func writeDocFile(t *testing.T, doc any) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	return path
}

// TestGovernanceSet_FailurePrintsStepsOnStderr: the assignment write fails
// after the profile is created, so stderr names both writes and stdout stays
// empty.
func TestGovernanceSet_FailurePrintsStepsOnStderr(t *testing.T) {
	fake := newFakeGovernanceServer()
	inner := fake.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/assignments" {
			writeErrorJSON(w, http.StatusInternalServerError, "boom")
			return
		}
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	path := writeDocFile(t, sdk.GovernanceDocument{
		Profiles:    []sdk.GovernanceProfile{{Name: "fresh"}},
		Assignments: []sdk.GovernanceAssignment{{SubjectType: sdk.CapabilitySubjectUser, Subject: "alice", ProfileID: uuid.New()}},
	})
	stdout, stderr, err := runCaptured(t, "governance", "set", path, "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("want the assignment failure")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing on a failed apply", stdout)
	}
	for _, want := range []string{
		"applied: 1, pending: 0, failed: 1\n",
		"  applied profile create fresh\n",
		"  failed assignment upsert user alice: ",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}

// TestDriveSet_FailurePrintsStepsOnStderr: the grant write fails after the
// drive is created, so stderr names both writes and stdout stays empty.
func TestDriveSet_FailurePrintsStepsOnStderr(t *testing.T) {
	fake := newFakeDriveServer()
	inner := fake.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/drives/grants" {
			writeErrorJSON(w, http.StatusInternalServerError, "boom")
			return
		}
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	path := writeDocFile(t, sdk.DrivesDocument{
		Drives: []sdk.UserDriveListItem{{UserDrive: sdk.UserDrive{Name: "home"}}},
		Grants: []sdk.UserDriveGrant{{SubjectType: sdk.CapabilitySubjectUser, Subject: "alice", DriveID: uuid.New()}},
	})
	stdout, stderr, err := runCaptured(t, "drive", "set", path, "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("want the grant failure")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing on a failed apply", stdout)
	}
	for _, want := range []string{
		"applied: 1, pending: 0, failed: 1\n",
		"  applied drive create home\n",
		"  failed drive_grant upsert user alice: ",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}
