// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestRecorderArgv(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	agent := []string{"claude", "code", "--task", "fix bug"}

	// Delivery mount set => -out-dir flag present with the mount target.
	withOut := recorderArgv("/var/log/wardyn", RecordingMountTarget, "", runID, agent)
	foundOut := false
	for i, a := range withOut {
		if a == "-out-dir" && i+1 < len(withOut) && withOut[i+1] == RecordingMountTarget {
			foundOut = true
		}
	}
	if !foundOut {
		t.Errorf("want -out-dir %s in argv, got %v", RecordingMountTarget, withOut)
	}
	// Enabled => wrapped, agent argv after the "--" separator, run id present,
	// and the brokered upload route passed via -upload-url (default delivery).
	uploadURL := "http://wardyn-proxy:3128/wardyn/v1/recordings/22222222-2222-2222-2222-222222222222"
	got := recorderArgv("/var/log/wardyn", "", uploadURL, runID, agent)
	if got[0] != "wardyn-rec" {
		t.Fatalf("want wardyn-rec first, got %v", got)
	}
	foundUp := false
	for i, a := range got {
		if a == "-upload-url" && i+1 < len(got) && got[i+1] == uploadURL {
			foundUp = true
		}
	}
	if !foundUp {
		t.Errorf("want -upload-url %s in argv, got %v", uploadURL, got)
	}
	sep := -1
	for i, a := range got {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep == -1 {
		t.Fatalf("missing -- separator: %v", got)
	}
	if !slices.Equal(got[sep+1:], agent) {
		t.Errorf("agent argv after -- = %v, want %v", got[sep+1:], agent)
	}
	if !slices.Contains(got[:sep], runID.String()) {
		t.Errorf("run id must appear before --, got %v", got[:sep])
	}
}

// TestRecorderArgv_NeverUnmaskedOutDirWithUpload asserts the HIGH-finding
// invariant directly at the argv-construction boundary: wardyn-rec must NEVER be
// handed BOTH a shared-mount -out-dir (which writes an UNMASKED cast the API
// serves) AND an -upload-url (the masked control-plane path). When the masked
// upload path is configured it must win; the unmasked shared-mount delivery is
// suppressed so no unmasked cast ever lands in a viewer-exposed path.
func TestRecorderArgv_NeverUnmaskedOutDirWithUpload(t *testing.T) {
	runID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	agent := []string{"claude", "code"}
	uploadURL := "http://wardyn-proxy:3128/wardyn/v1/recordings/" + runID.String()

	// Both delivery paths offered: the masked upload must win, the unmasked
	// shared-mount -out-dir must be dropped.
	got := recorderArgv("/var/log/wardyn", RecordingMountTarget, uploadURL, runID, agent)
	if slices.Contains(got, "-out-dir") {
		t.Errorf("masked upload configured: argv must NOT carry an unmasked -out-dir, got %v", got)
	}
	if !slices.Contains(got, "-upload-url") {
		t.Errorf("masked upload configured: argv must keep -upload-url, got %v", got)
	}

	// Shared-mount-only (no upload path, e.g. nil run id): the reduced-isolation
	// fallback is preserved so single-host delivery still works.
	gotFallback := recorderArgv("/var/log/wardyn", RecordingMountTarget, "", runID, agent)
	if !slices.Contains(gotFallback, "-out-dir") {
		t.Errorf("no upload path: shared-mount fallback -out-dir must be present, got %v", gotFallback)
	}
}
