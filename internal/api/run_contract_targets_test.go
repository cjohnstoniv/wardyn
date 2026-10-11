// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"math/rand"
	"net/http"
	"path"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// workspaceTargetsRefusalPairwise is the original pairwise check, kept as the
// oracle workspaceTargetsRefusal must agree with on every input.
func workspaceTargetsRefusalPairwise(req createRunRequest) *runRefusal {
	type placed struct{ id, target string }
	var seen []placed
	drive := req.Drive != nil && req.Drive.Enabled
	for i, sel := range req.Workspaces {
		t := sel.Target
		if t == "" {
			continue
		}
		if drive && pathWithin(t, runner.DriveTarget) && path.IsAbs(t) && path.Clean(t) == t {
			return overlapRefusal(t, sel.WorkspaceID, runDriveName, runner.DriveTarget)
		}
		if err := workspaceTargetShape(t); err != nil {
			return runError(http.StatusBadRequest, reasonWorkspaceTargetInvalid, fmt.Sprintf("workspaces[%d].target: %v", i, err))
		}
		for _, p := range seen {
			if p.id != sel.WorkspaceID && (pathWithin(t, p.target) || pathWithin(p.target, t)) {
				return overlapRefusal(t, sel.WorkspaceID, p.id, p.target)
			}
		}
		seen = append(seen, placed{sel.WorkspaceID, t})
	}
	return nil
}

func sameRefusal(a, b *runRefusal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.status == b.status && a.body == b.body
}

func targetSels(pairs ...string) []client.WorkspaceSelection {
	var out []client.WorkspaceSelection
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, client.WorkspaceSelection{WorkspaceID: pairs[i], Target: pairs[i+1]})
	}
	return out
}

func TestWorkspaceTargetsRefusalCases(t *testing.T) {
	const h = "/home/agent/"
	for _, tc := range []struct {
		name   string
		drive  bool
		sels   []client.WorkspaceSelection
		reason string
		msg    string
	}{
		{"none", false, nil, "", ""},
		{"empty targets are skipped", false, targetSels("A", "", "B", ""), "", ""},
		{"equal across workspaces", false, targetSels("A", h+"w", "B", h+"w"), reasonWorkspaceTargetOverlap, h + "w is also where A mounts. Give one of them another path."},
		{"nested inside an earlier one", false, targetSels("A", h+"w", "B", h+"w/x"), reasonWorkspaceTargetOverlap, h + "w/x is inside " + h + "w, where A mounts. Two mounts can't nest."},
		{"an earlier one nested inside", false, targetSels("A", h+"w/x", "B", h+"w"), reasonWorkspaceTargetOverlap, h + "w/x is inside " + h + "w, where B mounts. Two mounts can't nest."},
		{"sibling prefix is not nesting", false, targetSels("A", h+"w", "B", h+"w2"), "", ""},
		{"same workspace may repeat and nest", false, targetSels("A", h+"w", "A", h+"w", "A", h+"w/x"), "", ""},
		{"earliest overlapping mount is named", false, targetSels("A", h+"w/x", "A", h+"w", "B", h+"w/x/y"), reasonWorkspaceTargetOverlap, h + "w/x/y is inside " + h + "w/x, where A mounts. Two mounts can't nest."},
		{"same-id mount is skipped for a later one", false, targetSels("A", h+"w", "B", h+"w/x"), reasonWorkspaceTargetOverlap, h + "w/x is inside " + h + "w, where A mounts. Two mounts can't nest."},
		{"overlap before a later invalid target", false, targetSels("A", h+"w", "B", h+"w", "C", "rel"), reasonWorkspaceTargetOverlap, h + "w is also where A mounts. Give one of them another path."},
		{"invalid target before a later overlap", false, targetSels("A", h+"w", "C", "rel", "B", h+"w"), reasonWorkspaceTargetInvalid, ""},
		{"drive equal", true, targetSels("A", h+"drive"), reasonWorkspaceTargetOverlap, h + "drive is also where your drive mounts. Give one of them another path."},
		{"drive reserved without the drive", false, targetSels("A", h+"drive"), reasonWorkspaceTargetInvalid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := createRunRequest{Workspaces: tc.sels}
			if tc.drive {
				req.Drive = &client.DriveSelection{Enabled: true}
			}
			got := workspaceTargetsRefusal(req)
			if !sameRefusal(got, workspaceTargetsRefusalPairwise(req)) {
				t.Fatalf("refusal %+v differs from the pairwise check %+v", got, workspaceTargetsRefusalPairwise(req))
			}
			if tc.reason == "" {
				if got != nil {
					t.Fatalf("refused: %+v", got.body)
				}
				return
			}
			if got == nil || got.body.Reason != tc.reason || (tc.msg != "" && got.body.Error != tc.msg) {
				t.Fatalf("refusal = %+v, want %s %q", got, tc.reason, tc.msg)
			}
		})
	}
}

// TestWorkspaceTargetsRefusalAgreesWithPairwise runs random selection lists, with
// shapes that are valid, invalid, nested, prefix-alike and drive-reserved,
// through both checks and requires the same refusal, message and order.
func TestWorkspaceTargetsRefusalAgreesWithPairwise(t *testing.T) {
	targets := []string{"", "rel", "/home/agent", "/home/agent/../x", "/home/agent/a/", "/home/agent/drive", "/home/agent/drive/x",
		"/home/agent/a", "/home/agent/a/b", "/home/agent/a/b/c", "/home/agent/ab", "/home/agent/b", "/home/agent/b/c", "/home/agent/a/bc"}
	ids := []string{"A", "B", "C"}
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 30000; n++ {
		req := createRunRequest{}
		for k := rng.Intn(7); k > 0; k-- {
			req.Workspaces = append(req.Workspaces, client.WorkspaceSelection{WorkspaceID: ids[rng.Intn(len(ids))], Target: targets[rng.Intn(len(targets))]})
		}
		if rng.Intn(4) == 0 {
			req.Drive = &client.DriveSelection{Enabled: true}
		}
		if got, want := workspaceTargetsRefusal(req), workspaceTargetsRefusalPairwise(req); !sameRefusal(got, want) {
			t.Fatalf("selections %+v drive=%v: refusal %+v, pairwise check %+v", req.Workspaces, req.Drive != nil, got, want)
		}
	}
}

// TestWorkspaceTargetsRefusalLargeListIsFast pins the cost of a body-sized list
// (about 19,000 selections fit under maxJSONBody). The pairwise check needs
// seconds for it and this one a few milliseconds, so the bound leaves two
// orders of magnitude either way.
func TestWorkspaceTargetsRefusalLargeListIsFast(t *testing.T) {
	const n = 19000
	req := createRunRequest{}
	for i := 0; i < n; i++ {
		req.Workspaces = append(req.Workspaces, client.WorkspaceSelection{WorkspaceID: fmt.Sprint(i), Target: fmt.Sprintf("/home/agent/%d", i)})
	}
	req.Workspaces = append(req.Workspaces, client.WorkspaceSelection{WorkspaceID: "last", Target: "/home/agent/7/inner"})
	start := time.Now()
	got := workspaceTargetsRefusal(req)
	elapsed := time.Since(start)
	if got == nil || got.body.Reason != reasonWorkspaceTargetOverlap {
		t.Fatalf("refusal = %+v, want %s", got, reasonWorkspaceTargetOverlap)
	}
	if elapsed > time.Second {
		t.Fatalf("%d selections took %v", n, elapsed)
	}
}
