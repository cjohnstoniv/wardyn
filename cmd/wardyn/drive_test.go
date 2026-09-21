// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// reclaimOK is the 200 body the daemon answers with.
func reclaimOK(driveID uuid.UUID, object, outcome string) map[string]any {
	return map[string]any{
		"drive": "Corp NAS", "drive_id": driveID.String(), "subject_type": "user",
		"subject": "sub-bob", "backend": "docker_volume", "object": object, "outcome": outcome,
	}
}

// TestDriveReclaim_PostsTheSubjectAndPrintsWhatWent.
func TestDriveReclaim_PostsTheSubjectAndPrintsWhatWent(t *testing.T) {
	driveID := uuid.New()
	object := "wardyn-drive-corp-nas-d-9f3a1c"
	cs := newCmdServer(t, http.StatusOK, reclaimOK(driveID, object, "deleted"))

	if err := execCmd(t, "--url", cs.URL, "drive", "reclaim", driveID.String(),
		"--subject", "sub-bob", "--yes"); err != nil {
		t.Fatalf("drive reclaim: %v", err)
	}
	got := cs.last()
	if got.method != http.MethodPost || got.path != "/api/v1/drives/"+driveID.String()+"/reclaim" {
		t.Fatalf("request = %s %s, want POST /api/v1/drives/<id>/reclaim", got.method, got.path)
	}
	var sent map[string]string
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("decode sent body %q: %v", got.body, err)
	}
	if sent["subject"] != "sub-bob" || sent["subject_type"] != "user" {
		t.Errorf("body = %v, want the (subject_type, subject) pair the allocation is keyed on", sent)
	}
}

// TestDriveReclaim_RefusesWithoutYes is the CLI's own rail. A verb that
// destroys a person's data must not run off a mistyped drive id, and the flag
// is required rather than prompted so the automated offboarding path keeps the
// same guard a keyboard does.
func TestDriveReclaim_RefusesWithoutYes(t *testing.T) {
	driveID := uuid.New()
	cs := newCmdServer(t, http.StatusOK, reclaimOK(driveID, "wardyn-drive-corp-nas-d-9f3a1c", "deleted"))

	err := execCmd(t, "--url", cs.URL, "drive", "reclaim", driveID.String(), "--subject", "sub-bob")
	if err == nil {
		t.Fatal("drive reclaim ran without --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error %q does not name the flag the operator has to add", err)
	}
	if len(cs.reqs) != 0 {
		t.Errorf("the unconfirmed command still reached the daemon: %v", cs.reqs)
	}
}

// TestDriveReclaim_RefusesWithoutASubject: a reclaim names ONE person's
// storage, and a missing subject must not become a request the server has to
// interpret.
func TestDriveReclaim_RefusesWithoutASubject(t *testing.T) {
	driveID := uuid.New()
	cs := newCmdServer(t, http.StatusOK, reclaimOK(driveID, "x", "deleted"))

	if err := execCmd(t, "--url", cs.URL, "drive", "reclaim", driveID.String(), "--yes"); err == nil {
		t.Fatal("drive reclaim ran with no --subject")
	}
	if len(cs.reqs) != 0 {
		t.Errorf("the command reached the daemon with no subject: %v", cs.reqs)
	}
}

// TestDriveReclaim_SurfacesTheConflictVerbatim: the 409 the daemon answers
// while a run still holds the object is the operator's whole remedy ("wait for
// the run"), so it reaches them as the server's own message — not as a
// fallback, and not as a generic failure.
func TestDriveReclaim_SurfacesTheConflictVerbatim(t *testing.T) {
	driveID := uuid.New()
	cs := newCmdServer(t, http.StatusConflict,
		map[string]string{"error": "reclaim refused: the drive's storage is still held by a running sandbox"})

	err := execCmd(t, "--url", cs.URL, "drive", "reclaim", driveID.String(), "--subject", "sub-bob", "--yes")
	if err == nil {
		t.Fatal("a 409 was reported as success")
	}
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("err = %v (%T), want *sdk.APIError with status 409", err, err)
	}
	if !strings.Contains(err.Error(), "still held by a running sandbox") {
		t.Errorf("the operator does not see the server's reason: %v", err)
	}
}

// TestDriveReclaim_RefusesANonUUIDDriveID, the same refusal `attach` makes for
// the same reason: a mistyped id must not be posted at a path that could match
// something else.
func TestDriveReclaim_RefusesANonUUIDDriveID(t *testing.T) {
	cs := newCmdServer(t, http.StatusOK, nil)
	if err := execCmd(t, "--url", cs.URL, "drive", "reclaim", "corp-nas", "--subject", "sub-bob", "--yes"); err == nil {
		t.Fatal("drive reclaim accepted a non-uuid drive id")
	}
	if len(cs.reqs) != 0 {
		t.Errorf("a non-uuid id was still sent: %v", cs.reqs)
	}
}
