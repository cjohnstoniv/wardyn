// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// targetOverlapRefusalPairwise is the plain pairwise check, kept as the oracle
// targetOverlapRefusal must agree with on every input.
func targetOverlapRefusalPairwise(targets []runTarget) *runRefusal {
	var placed []runTarget
	for _, t := range targets {
		if t.kind == attachmentRepository {
			if i := indexRepoAt(placed, t.target); i >= 0 {
				if placed[i].repo == t.repo && placed[i].ref == t.ref {
					continue
				}
				return overlapRefusal(t.target, t.name, placed[i].name, placed[i].target)
			}
		}
		for _, p := range placed {
			if !sharesCarrier(p.carriers, t.carriers) && (pathWithin(t.target, p.target) || pathWithin(p.target, t.target)) {
				return overlapRefusal(t.target, t.name, p.name, p.target)
			}
		}
		placed = append(placed, t)
	}
	return nil
}

func indexRepoAt(placed []runTarget, target string) int {
	for i, p := range placed {
		if p.kind == attachmentRepository && p.target == target {
			return i
		}
	}
	return -1
}

func sameRefusal(a, b *runRefusal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.status == b.status && a.body == b.body
}

// folders builds folder targets from owner/target pairs; the owner is its name.
func folders(pairs ...string) []runTarget {
	var out []runTarget
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, runTarget{kind: attachmentFolder, owner: pairs[i], name: pairs[i], carriers: []string{pairs[i]}, target: pairs[i+1]})
	}
	return out
}

// shared is a repository the policy carries, shown under owner and carried by each of carriers.
func shared(owner string, carriers []string, repo, target string) runTarget {
	return runTarget{kind: attachmentRepository, owner: owner, name: owner, carriers: carriers, repo: repo, target: target}
}

func repoAt(owner, repo, ref, target string) runTarget {
	return runTarget{kind: attachmentRepository, owner: owner, name: owner, carriers: []string{owner}, repo: repo, ref: ref, target: target}
}

func TestTargetOverlapRefusalCases(t *testing.T) {
	const h = "/home/agent/"
	drive := runTarget{kind: attachmentDrive, owner: attachmentDrive, name: runDriveName, carriers: []string{attachmentDrive}, target: h + "drive"}
	for _, tc := range []struct {
		name    string
		targets []runTarget
		msg     string
	}{
		{"none", nil, ""},
		{"equal across workspaces", folders("A", h+"w", "B", h+"w"), h + "w is also where A mounts. Give one of them another path."},
		{"nested inside an earlier one", folders("A", h+"w", "B", h+"w/x"), h + "w/x is inside " + h + "w, where A mounts. Two mounts can't nest."},
		{"an earlier one nested inside", folders("A", h+"w/x", "B", h+"w"), h + "w/x is inside " + h + "w, where B mounts. Two mounts can't nest."},
		{"sibling prefix is not nesting", folders("A", h+"w", "B", h+"w2"), ""},
		{"same workspace may repeat and nest", folders("A", h+"w", "A", h+"w", "A", h+"w/x"), ""},
		{"earliest overlapping mount is named", folders("A", h+"w/x", "A", h+"w", "B", h+"w/x/y"), h + "w/x/y is inside " + h + "w/x, where A mounts. Two mounts can't nest."},
		{"drive equal", append([]runTarget{drive}, folders("A", h+"drive")...), h + "drive is also where your drive mounts. Give one of them another path."},
		{"drive nested in a workspace", append(folders("A", h+"work"), runTarget{kind: attachmentDrive, owner: attachmentDrive, name: runDriveName, carriers: []string{attachmentDrive}, target: h + "work/drive"}),
			h + "work/drive is inside " + h + "work, where A mounts. Two mounts can't nest."},
		{"two repos one directory in one workspace", []runTarget{repoAt("A", "acme/web", "", h+"work/web"), repoAt("A", "other/web", "", h+"work/web")},
			h + "work/web is also where A mounts. Give one of them another path."},
		{"one repo twice is one clone", []runTarget{repoAt("A", "acme/web", "", h+"work/web"), repoAt("A", "acme/web", "", h+"work/web")}, ""},
		{"one repo at two refs collides", []runTarget{repoAt("A", "acme/web", "", h+"work/web"), repoAt("A", "acme/web", "dev", h+"work/web")},
			h + "work/web is also where A mounts. Give one of them another path."},
		{"a repo inside its own folder", append(folders("A", h+"work"), repoAt("A", "acme/web", "", h+"work/web")), ""},
		// A source two workspaces carry belongs to both: it nests in either one's mount.
		{"shared repo inside a carrier's folder", append(folders("A", h+"work"), shared("B", []string{"B", "A"}, "acme/web", h+"work/web")), ""},
		{"a folder around a shared repo", append([]runTarget{shared("B", []string{"B", "A"}, "acme/web", h+"work/web")}, folders("A", h+"work")...), ""},
		{"shared repo inside a stranger's folder", append(folders("C", h+"work"), shared("B", []string{"B", "A"}, "acme/web", h+"work/web")),
			h + "work/web is inside " + h + "work, where C mounts. Two mounts can't nest."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := targetOverlapRefusal(tc.targets)
			if want := targetOverlapRefusalPairwise(tc.targets); !sameRefusal(got, want) {
				t.Fatalf("refusal %+v differs from the pairwise check %+v", got, want)
			}
			if tc.msg == "" {
				if got != nil {
					t.Fatalf("refused: %+v", got.body)
				}
				return
			}
			if got == nil || got.body.Reason != reasonWorkspaceTargetOverlap || got.body.Error != tc.msg {
				t.Fatalf("refusal = %+v, want %q", got, tc.msg)
			}
		})
	}
}

// TestTargetOverlapRefusalAgreesWithPairwise runs random target lists, with
// shapes that are nested, prefix-alike, repeated and repository clones, through
// both checks and requires the same refusal, message and order.
func TestTargetOverlapRefusalAgreesWithPairwise(t *testing.T) {
	paths := []string{"/home/agent/a", "/home/agent/a/b", "/home/agent/a/b/c", "/home/agent/ab", "/home/agent/b", "/home/agent/b/c",
		"/home/agent/a/bc", "/home/agent/drive", "/home/agent/drive/x"}
	owners := []string{"A", "B", "C", attachmentDrive}
	repos := []string{"acme/web", "other/web"}
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 30000; n++ {
		var targets []runTarget
		for k := rng.Intn(7); k > 0; k-- {
			o := owners[rng.Intn(len(owners))]
			tgt := runTarget{kind: attachmentFolder, owner: o, name: o, carriers: []string{o}, target: paths[rng.Intn(len(paths))]}
			if rng.Intn(3) == 0 {
				tgt.carriers = append(tgt.carriers, owners[rng.Intn(len(owners))])
			}
			if rng.Intn(2) == 0 {
				tgt.kind, tgt.repo, tgt.ref = attachmentRepository, repos[rng.Intn(len(repos))], []string{"", "dev"}[rng.Intn(2)]
			}
			targets = append(targets, tgt)
		}
		if got, want := targetOverlapRefusal(targets), targetOverlapRefusalPairwise(targets); !sameRefusal(got, want) {
			t.Fatalf("targets %+v: refusal %+v, pairwise check %+v", targets, got, want)
		}
	}
}

// TestTargetOverlapRefusalLargeListIsFast pins the cost of a long target list
// (a policy may carry many mounts). The pairwise check needs seconds for it and
// this one a few milliseconds, so the bound leaves two orders of magnitude.
func TestTargetOverlapRefusalLargeListIsFast(t *testing.T) {
	const n = 19000
	var targets []runTarget
	for i := 0; i < n; i++ {
		targets = append(targets, folders(fmt.Sprint(i), fmt.Sprintf("/home/agent/%d", i))...)
	}
	targets = append(targets, folders("last", "/home/agent/7/inner")...)
	start := time.Now()
	got := targetOverlapRefusal(targets)
	elapsed := time.Since(start)
	if got == nil || got.body.Reason != reasonWorkspaceTargetOverlap {
		t.Fatalf("refusal = %+v, want %s", got, reasonWorkspaceTargetOverlap)
	}
	if elapsed > time.Second {
		t.Fatalf("%d targets took %v", n, elapsed)
	}
}

// TestWorkspaceSelectionsShape pins the shape checks of workspaces[]: ids, the
// workspace_id alias, the list's bound, targets (the drive's own path is left
// to the overlap check) and the repos list.
func TestWorkspaceSelectionsShape(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	aID, bID := uuid.MustParse(a), uuid.MustParse(b)
	repos := func(rs ...client.RepoSelection) *[]client.RepoSelection { return &rs }
	sels := func(s ...client.WorkspaceSelection) createRunRequest { return createRunRequest{Workspaces: s} }
	withAlias := func(id *uuid.UUID, req createRunRequest) createRunRequest { req.WorkspaceID = id; return req }
	withDrive := func(req createRunRequest) createRunRequest {
		req.Drive = &client.DriveSelection{Enabled: true}
		return req
	}
	var many []client.WorkspaceSelection
	for i := 0; i <= maxRunWorkspaces; i++ {
		many = append(many, client.WorkspaceSelection{WorkspaceID: uuid.NewString()})
	}
	for _, tc := range []struct {
		name   string
		req    createRunRequest
		reason string
	}{
		{"none", createRunRequest{}, ""},
		{"two workspaces", sels(client.WorkspaceSelection{WorkspaceID: a}, client.WorkspaceSelection{WorkspaceID: b}), ""},
		{"alias equals the first", withAlias(&aID, sels(client.WorkspaceSelection{WorkspaceID: a}, client.WorkspaceSelection{WorkspaceID: b})), ""},
		{"alias names another", withAlias(&bID, sels(client.WorkspaceSelection{WorkspaceID: a}, client.WorkspaceSelection{WorkspaceID: b})), reasonWorkspaceSelectionInvalid},
		{"malformed id", sels(client.WorkspaceSelection{WorkspaceID: "A"}), reasonWorkspaceSelectionInvalid},
		{"listed twice", sels(client.WorkspaceSelection{WorkspaceID: a}, client.WorkspaceSelection{WorkspaceID: a}), reasonWorkspaceSelectionInvalid},
		// r1 H2: another spelling of a valid id would miss the options keyed by it.
		{"upper-case id", sels(client.WorkspaceSelection{WorkspaceID: strings.ToUpper(a), Repos: repos()}), reasonWorkspaceSelectionInvalid},
		{"braced id", sels(client.WorkspaceSelection{WorkspaceID: "{" + a + "}", Repos: repos()}), reasonWorkspaceSelectionInvalid},
		{"urn id", sels(client.WorkspaceSelection{WorkspaceID: "urn:uuid:" + a, Repos: repos()}), reasonWorkspaceSelectionInvalid},
		{"too many", sels(many...), reasonWorkspaceSelectionInvalid},
		{"relative target", sels(client.WorkspaceSelection{WorkspaceID: a, Target: "rel"}), reasonWorkspaceTargetInvalid},
		{"drive path without the drive", sels(client.WorkspaceSelection{WorkspaceID: a, Target: "/home/agent/drive"}), reasonWorkspaceTargetInvalid},
		{"drive path with the drive overlaps it", withDrive(sels(client.WorkspaceSelection{WorkspaceID: a, Target: "/home/agent/drive/x"})), reasonWorkspaceTargetOverlap},
		{"drives root is reserved", sels(client.WorkspaceSelection{WorkspaceID: a, Target: "/home/agent/drives/x"}), reasonWorkspaceTargetInvalid},
		{"shared root is reserved", sels(client.WorkspaceSelection{WorkspaceID: a, Target: "/home/agent/shared"}), reasonWorkspaceTargetInvalid},
		{"empty repos is valid", sels(client.WorkspaceSelection{WorkspaceID: a, Repos: repos()}), ""},
		{"repo listed twice", sels(client.WorkspaceSelection{WorkspaceID: a, Repos: repos(client.RepoSelection{Repo: "x/y"}, client.RepoSelection{Repo: "x/y", Ref: "dev"})}), reasonRunRefInvalid},
		{"empty repo", sels(client.WorkspaceSelection{WorkspaceID: a, Repos: repos(client.RepoSelection{})}), reasonRunRefInvalid},
		{"option-like ref", sels(client.WorkspaceSelection{WorkspaceID: a, Repos: repos(client.RepoSelection{Repo: "x/y", Ref: "--upload-pack=sh"})}), reasonRunRefInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workspaceTargetsRefusal(tc.req)
			switch {
			case tc.reason == "" && got != nil:
				t.Fatalf("refused: %+v", got.body)
			case tc.reason != "" && (got == nil || got.body.Reason != tc.reason):
				t.Fatalf("refusal = %+v, want %s", got, tc.reason)
			}
		})
	}
}

// TestRunRefShape pins validRunRef: nothing git could read as an option or a
// revision expression gets through to clone_one's `git fetch origin "$ref"`.
func TestRunRefShape(t *testing.T) {
	for _, ref := range []string{"-x", "--upload-pack=touch /tmp/p", "a..b", "x@{1}", "a b", "a\tb", "a\x01b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`,
		"dir/", "x.lock", strings.Repeat("a", 256), "é"} {
		if validRunRef(ref) {
			t.Errorf("validRunRef(%q) = true, want refused", ref)
		}
	}
	for _, ref := range []string{"main", "release/1.2", "v1.2.3", "0123456789abcdef0123456789abcdef01234567", "feature/x-y_z", strings.Repeat("a", 255)} {
		if !validRunRef(ref) {
			t.Errorf("validRunRef(%q) = false, want accepted", ref)
		}
	}
}

// TestTargetOverlapSharedChainIsBounded (fix-r4 L-1): the costliest shape the
// shared-carrier lists allow, nested targets carried by 32 workspaces each,
// every set distinct and sharing one carrier, placed deepest first so each
// encloses all placed before it. The 1 MiB request body bounds the chain near
// 1,000 targets; at that depth the check must stay well under a second.
func TestTargetOverlapSharedChainIsBounded(t *testing.T) {
	const depth = 1000
	var targets []runTarget
	for d := depth; d > 0; d-- {
		set := make([]string, 0, 32)
		for i := 0; i < 31; i++ {
			set = append(set, fmt.Sprint("d", d, "-", i))
		}
		set = append(set, "common")
		targets = append(targets, shared(set[0], set, "acme/web", "/home/agent"+strings.Repeat("/c", d)))
	}
	start := time.Now()
	if got := targetOverlapRefusal(targets); got != nil {
		t.Fatalf("refused: %+v", got.body)
	}
	elapsed := time.Since(start)
	t.Logf("a chain of %d shared targets took %v", depth, elapsed)
	if elapsed > raceSlowdown*time.Second {
		t.Fatalf("a chain of %d shared targets took %v", depth, elapsed)
	}
}
