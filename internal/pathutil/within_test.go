// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pathutil

import "testing"

// The rows the copies Within replaced agreed on, and the ones where they did not: a root of "/", a trailing slash,
// a ".." element, a relative path. The last three are the api copy's own test, moved here.
func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		root, p string
		want    bool
	}{
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a", "/ab", false},
		{"/a", "/", false},
		{"/", "/a", true},
		{"/", "/", true},
		{"/a/", "/a/b", true},
		{"/a", "/a/b/", true},
		{"/a", "/a/../b", false},
		{"/a", "/a/./b", true},
		{"/a", "/a/b/..", true},
		{"/a", "/a/..b", true},
		{"/A", "/a/b", false},
		{"a", "a/b", true},
		{".", "a", true},
		{"..", "a", false},
		{"/a", "a", false},
		{"a", "/a/b", false},
		{"", "/a", false},
		{"/home/operator/projects", "/home/operator/projects-evil", false},
		{"/home/operator/projects", "/home/operator", false},
		{"/home/operator/projects/", "/home/operator/projects/app/", true},
	} {
		if got := Within(tc.root, tc.p); got != tc.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tc.root, tc.p, got, tc.want)
		}
	}
}
