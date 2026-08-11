// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestParseWorkspaceSourceArg(t *testing.T) {
	cases := []struct {
		in      string
		want    sdk.WorkspaceSource
		wantErr bool
	}{
		{
			in:   "dir:/src/api@/work/api",
			want: sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeLocalDir, Path: "/src/api", Target: "/work/api"},
		},
		{
			in:   "dir:/src/api",
			want: sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeLocalDir, Path: "/src/api"},
		},
		{
			in:   "repo:org/lib",
			want: sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeRepo, Source: "org/lib"},
		},
		{
			in:   "repo:org/lib@/work/lib",
			want: sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeRepo, Source: "org/lib", Target: "/work/lib"},
		},
		{
			in:   "ephemeral:@/home/scratch",
			want: sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeEphemeral, Target: "/home/scratch"},
		},
		{
			in:   "ephemeral:",
			want: sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeEphemeral},
		},
		{in: "dir:", wantErr: true},    // no path
		{in: "repo:", wantErr: true},   // no slug
		{in: "bogus:x", wantErr: true}, // unknown type
		{in: "no-colon-here", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseWorkspaceSourceArg(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseWorkspaceSourceArg(%q): want error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseWorkspaceSourceArg(%q): unexpected error: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseWorkspaceSourceArg(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestWorkspaceComposition(t *testing.T) {
	cases := []struct {
		name string
		ws   sdk.Workspace
		want string
	}{
		{
			name: "single local_dir",
			ws:   sdk.Workspace{Sources: []sdk.WorkspaceSource{{Type: sdk.WorkspaceSourceTypeLocalDir, Path: "/src/api"}}},
			want: "local_dir /src/api",
		},
		{
			name: "single repo",
			ws:   sdk.Workspace{Sources: []sdk.WorkspaceSource{{Type: sdk.WorkspaceSourceTypeRepo, Source: "org/lib"}}},
			want: "repo org/lib",
		},
		{
			name: "two dirs one repo",
			ws: sdk.Workspace{Sources: []sdk.WorkspaceSource{
				{Type: sdk.WorkspaceSourceTypeLocalDir, Path: "/a"},
				{Type: sdk.WorkspaceSourceTypeLocalDir, Path: "/b"},
				{Type: sdk.WorkspaceSourceTypeRepo, Source: "org/lib"},
			}},
			want: "2 dirs · 1 repo",
		},
		{
			name: "no sources",
			ws:   sdk.Workspace{},
			want: "no sources",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := workspaceComposition(c.ws); got != c.want {
				t.Errorf("workspaceComposition() = %q, want %q", got, c.want)
			}
		})
	}
}
