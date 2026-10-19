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
