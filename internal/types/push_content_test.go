// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

func TestPushContentScopeValidate(t *testing.T) {
	good := func() PushContentScope {
		return PushContentScope{
			Repo:        "github.com/octocat/hello-world",
			Branch:      "refs/heads/wardyn/run/x",
			ActsAs:      "github_token:3f1c",
			Paths:       []string{".github/workflows/ci.yml"},
			PathsTotal:  1,
			Commits:     []string{strings.Repeat("a", 40)},
			PathsDigest: strings.Repeat("0", 64),
		}
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("a well-formed scope is refused: %v", err)
	}
	many := good()
	many.PathsTotal = 25
	for i := range PushContentMaxPaths {
		many.Paths = append(many.Paths, strings.Repeat("z", i+1))
	}
	many.Paths = many.Paths[1:]
	if err := many.Validate(); err != nil {
		t.Errorf("ten of twenty-five is refused: %v", err)
	}

	for name, mut := range map[string]func(*PushContentScope){
		"no repo":              func(s *PushContentScope) { s.Repo = "" },
		"no acts_as":           func(s *PushContentScope) { s.ActsAs = "" },
		"branch not a ref":     func(s *PushContentScope) { s.Branch = "main" },
		"no paths":             func(s *PushContentScope) { s.Paths, s.PathsTotal = nil, 0 },
		"total below the list": func(s *PushContentScope) { s.Paths = append(s.Paths, "z"); s.PathsTotal = 1 },
		"unsorted paths":       func(s *PushContentScope) { s.Paths = []string{"b", "a"}; s.PathsTotal = 2 },
		"eleven paths": func(s *PushContentScope) {
			s.Paths = strings.Split("a b c d e f g h i j k", " ")
			s.PathsTotal = 11
		},
		"no commits":       func(s *PushContentScope) { s.Commits = nil },
		"short commit":     func(s *PushContentScope) { s.Commits = []string{"abc"} },
		"upper-case hex":   func(s *PushContentScope) { s.Commits = []string{strings.Repeat("A", 40)} },
		"digest not hex":   func(s *PushContentScope) { s.PathsDigest = strings.Repeat("g", 64) },
		"digest too short": func(s *PushContentScope) { s.PathsDigest = "00" },
	} {
		s := good()
		mut(&s)
		if s.Validate() == nil {
			t.Errorf("%s: accepted %+v", name, s)
		}
	}
}
