// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

// The owner hit this VERBATIM on the compose stack: a directory that plainly
// exists on the host reported "not found on this host", because sealed wardynd
// sees only what WARDYN_WORKSPACES_ROOT mounts in — and the message never said
// so. The detail is the wizard's failure headline, so it carries the fix.
func TestLocalDirScanFailureDetail(t *testing.T) {
	const p = "/home/cjohn/containerized-agent-envs"
	cases := []struct {
		name          string
		notDir        bool
		root          string
		containerized bool
		want          []string
		wantAbsent    []string
	}{
		{
			name:       "host mode plain miss stays a plain typo message",
			want:       []string{"local directory not found on this host: " + p},
			wantAbsent: []string{"WARDYN_WORKSPACES_ROOT"},
		},
		{
			name:          "sealed daemon with NO root says nothing is visible and names the fix",
			containerized: true,
			want:          []string{"inside a container", "no WARDYN_WORKSPACES_ROOT set", "make setup", "not your whole home"},
		},
		{
			name: "path outside the configured root names the root",
			root: "/home/cjohn/projects", containerized: true,
			want: []string{"under its configured workspaces root (/home/cjohn/projects)", "move the project under it"},
		},
		{
			// Under the root but still absent = a real miss (typo / not created)
			// — blaming the mount would send the operator to the wrong fix.
			name: "path UNDER the configured root falls back to the plain miss",
			root: "/home/cjohn", containerized: true,
			want:       []string{"local directory not found on this host: " + p},
			wantAbsent: []string{"workspaces root"},
		},
		{
			name:   "not-a-directory stays its own message",
			notDir: true, root: "/home/cjohn/projects", containerized: true,
			want:       []string{"onboarded source is not a directory: " + p},
			wantAbsent: []string{"not found"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := localDirScanFailureDetail(p, c.notDir, c.root, c.containerized)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("detail = %q\nwant it to contain %q", got, w)
				}
			}
			for _, a := range c.wantAbsent {
				if strings.Contains(got, a) {
					t.Errorf("detail = %q\nmust NOT contain %q", got, a)
				}
			}
		})
	}
}

// A sibling-prefix path must not count as inside the root — /home/x-evil is
// not within /home/x.
func TestPathWithinRoot(t *testing.T) {
	cases := []struct {
		root, path string
		want       bool
	}{
		{"/home/cjohn/projects", "/home/cjohn/projects/app", true},
		{"/home/cjohn/projects", "/home/cjohn/projects", true},
		{"/home/cjohn/projects", "/home/cjohn/projects-evil", false},
		{"/home/cjohn/projects", "/home/cjohn", false},
		{"/home/cjohn/projects/", "/home/cjohn/projects/app/", true},
	}
	for _, c := range cases {
		if got := pathWithinRoot(c.root, c.path); got != c.want {
			t.Errorf("pathWithinRoot(%q, %q) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}
