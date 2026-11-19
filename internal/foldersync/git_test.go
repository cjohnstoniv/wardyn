// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"slices"
	"testing"
)

func TestPullCommitsFetch_ChecksObjectsAndSkipsTagsAndSubmodules(t *testing.T) {
	want := []string{"-c", "transfer.fsckObjects=true", "fetch", "-q", "--no-tags", "--no-recurse-submodules", "/tmp/b.bundle", "+HEAD:refs/heads/wardyn/sync-12345678"}
	if got := pullCommitsFetch("/tmp/b.bundle", "wardyn/sync-12345678"); !slices.Equal(got, want) {
		t.Errorf("fetch argv = %q, want %q", got, want)
	}
}

func TestReviewBranch_HoldsTheWholeRunID(t *testing.T) {
	a, b := "a1b2c3d4-5566-4788-99aa-bbccddeeff00", "a1b2c3d4-0000-4000-8000-000000000000"
	if got := reviewBranch(a); got != "wardyn/sync-"+a {
		t.Errorf("reviewBranch(%s) = %q, want wardyn/sync- and the whole id", a, got)
	}
	if reviewBranch(a) == reviewBranch(b) {
		t.Errorf("two runs whose ids start alike share the branch %s", reviewBranch(a))
	}
}
