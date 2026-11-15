// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPushRulesCompareAPushThatCarriesNoCommits: a push that moves a branch to
// a commit the forge already holds carries no tree, so what that commit holds
// at a denied path is read from the forge and compared with the branch's tip.
func TestPushRulesCompareAPushThatCarriesNoCommits(t *testing.T) {
	for _, tc := range []struct {
		name string
		// push moves the run's branch from its tip b, over the older commit a.
		push    func(t *testing.T, r *forgeRun, a, b string) (string, error)
		allowed bool
	}{
		{"a force-push back to an older commit", func(t *testing.T, r *forgeRun, a, _ string) (string, error) {
			runGit(t, r.work, "checkout", "-q", "--detach", a)
			return r.pushRefs("+" + a + ":" + r.ref)
		}, false},
		{"a move onto a merge with the older commit that a second branch already holds", func(t *testing.T, r *forgeRun, a, b string) (string, error) {
			side := strings.TrimSuffix(r.ref, "work") + "side"
			r.mergeOlder(t, b, a)
			merge := gitOut(t, r.work, "rev-parse", "HEAD")
			r.editMain(t, "side")
			if out, err := r.pushRefs("HEAD:" + side); err != nil {
				t.Fatalf("creating the second branch: %v\n%s\n%s", err, out, r.lastRefusal())
			}
			runGit(t, r.work, "checkout", "-q", "--detach", merge)
			return r.pushRefs("+"+merge+":"+side, merge+":"+r.ref)
		}, false},
		{"a force-push back past a commit that changed no denied path", func(t *testing.T, r *forgeRun, _, b string) (string, error) {
			branch := strings.TrimPrefix(r.ref, "refs/heads/")
			r.asHuman(t, "origin/"+branch, branch, map[string]string{"README.md": "hello\n"})
			runGit(t, r.work, "fetch", "-q", "origin", branch)
			runGit(t, r.work, "checkout", "-q", "--detach", b)
			return r.pushRefs("+" + b + ":" + r.ref)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newForgeRun(t, ordinaryRepo, nil, ".github/workflows/**")
			a := gitOut(t, r.work, "rev-parse", "HEAD")
			branch := strings.TrimPrefix(r.ref, "refs/heads/")
			b := r.asHuman(t, "main", branch, map[string]string{".github/workflows/ci.yml": "on: reviewed\n"})
			runGit(t, r.work, "fetch", "-q", "origin", branch)
			out, err := tc.push(t, r, a, b)
			if tc.allowed {
				r.wantAllowed(t, out, err)
				return
			}
			r.wantTipRefusal(t, out, err)
			if body := r.lastRefusal(); !strings.Contains(body, tipRemedy(whyTipChanged)) || strings.Contains(body, "remove these paths") {
				t.Errorf("refusal %q does not say %q alone", body, tipRemedy(whyTipChanged))
			}
		})
	}
}

// TestPushRulesRefuseADeletedBranchAndARefNamedTwice: a branch created again
// has no tip to compare with, so deleting one is refused, and so is a request
// that deletes and creates the same ref at once, which git never sends.
func TestPushRulesRefuseADeletedBranchAndARefNamedTwice(t *testing.T) {
	r := newForgeRunVia(t, githubHost, ordinaryRepo, nil, ".github/workflows/**")
	a := gitOut(t, r.work, "rev-parse", "HEAD")
	branch := strings.TrimPrefix(r.ref, "refs/heads/")
	b := r.asHuman(t, "main", branch, map[string]string{".github/workflows/ci.yml": "on: reviewed\n"})
	runGit(t, r.work, "fetch", "-q", "origin", branch)
	runGit(t, r.work, "checkout", "-q", "--detach", a)
	r.editMain(t, "older")
	c := gitOut(t, r.work, "rev-parse", "HEAD")
	zero := strings.Repeat("0", 40)

	status, body := r.postCommands(t, c+"\n^"+a, [][3]string{{b, zero, r.ref}, {zero, c, r.ref}})
	if status != http.StatusUnsupportedMediaType || !strings.Contains(body, "more than once") {
		t.Errorf("a ref named twice: status %d %q, want 415 naming it", status, body)
	}
	if out, err := r.pushRefs(":" + r.ref); err == nil || !strings.Contains(r.lastRefusal(), whyBranchDeleted) ||
		!strings.Contains(r.lastRefusal(), tipRemedy(whyBranchDeleted)) {
		t.Errorf("deleting the branch: err %v, refusal %q\n%s", err, r.lastRefusal(), out)
	}
	if got := gitOut(t, r.bare, "rev-parse", r.ref); got != b {
		t.Errorf("the forge's branch is at %s, want it left at %s", got, b)
	}
}

// TestADOPushTipComparisonNamesAndFailsClosed: the comparison with the tip runs
// beside the first one, so a held push names every path; a walk past its
// bounds and a directory the tip does not hold both hold the push.
func TestADOPushTipComparisonNamesAndFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// tip, when set, replaces the commit the run's branch stands at.
		tip      func(t *testing.T, r *adoPushRun)
		push     func(t *testing.T, r *adoPushRun, a, b string) []byte
		wants    []string
		unwanted string
		setup    func(t *testing.T, r *adoPushRun)
	}{
		{"a push that also changes a review path names the path the tip comparison found", nil, func(t *testing.T, r *adoPushRun, a, b string) []byte {
			runGit(t, r.work, "checkout", "-q", "--detach", b)
			runGit(t, r.work, "checkout", "-q", a, "--", ".github")
			writeFiles(t, r.work, map[string]string{".github/workflows/other.yml": "on: [push]\n"})
			runGit(t, r.work, "add", "-A")
			merge := gitOut(t, r.work, "commit-tree", gitOut(t, r.work, "write-tree"), "-p", b, "-p", a, "-m", "merge")
			return r.gitPush(t, merge+":"+r.ref)
		}, []string{adoTouches, ".github/workflows/ci.yml", ".github/workflows/other.yml", whyTipChanged, tipRemedy(whyTipChanged)}, "remove these paths", nil},
		{"a walk past its bounds holds the whole tree", nil, func(t *testing.T, r *adoPushRun, _, b string) []byte {
			return r.gitPush(t, adoCommitOn(t, r, b, "", "lib/c.txt")+":"+r.ref)
		}, []string{adoWorkedOut, "Paths: /", whyTipUnread}, adoTouches, func(t *testing.T, _ *adoPushRun) {
			prev := packTips
			t.Cleanup(func() { packTips = prev })
			packTips = func(gitpack.Result, func(gitpack.Change) (bool, error)) ([][]gitpack.Change, error) {
				return nil, gitpack.ErrTooLarge
			}
		}},
		{"a merge bringing back a directory the tip removed is held", func(t *testing.T, r *adoPushRun) {
			runGit(t, r.work, "rm", "-rq", ".github")
			r.record(t, map[string]string{"lib/b.txt": "b\n"})
		}, func(t *testing.T, r *adoPushRun, a, b string) []byte {
			return r.gitPush(t, adoMergeWith(t, r, b, a)+":"+r.ref)
		}, []string{adoTouches, ".github/", whyTipChanged}, adoWorkedOut, nil},
		{"a move that carries no commits, to a default-branch commit that changed a review path, is held", nil, func(t *testing.T, r *adoPushRun, _, b string) []byte {
			runGit(t, r.work, "checkout", "-q", "--detach", b)
			writeFiles(t, r.work, map[string]string{".github/workflows/ci.yml": "on: main\n"})
			commitAll(t, r.work)
			runGit(t, r.work, "push", "-q", r.bare, "HEAD:refs/heads/main")
			r.snapshot(t)
			return r.gitPush(t, "HEAD:"+r.ref)
		}, []string{adoTouches, ".github/workflows/ci.yml", whyTipChanged}, adoWorkedOut, nil},
		{"a branch whose tip the forge does not show hides nothing a later branch brings back", nil, func(t *testing.T, r *adoPushRun, a, _ string) []byte {
			other := BranchNSPrefix(r.p.runID) + "other"
			runGit(t, r.work, "checkout", "-q", "--detach", a)
			writeFiles(t, r.work, map[string]string{".github/workflows/other.yml": "on: reviewed\n"})
			commitAll(t, r.work)
			tip := gitOut(t, r.work, "rev-parse", "HEAD")
			runGit(t, r.work, "push", "-q", r.bare, tip+":"+other)
			r.snapshot(t)
			stale, merge := adoCommitOn(t, r, a, "", "lib/c.txt"), adoMergeWith(t, r, tip, a)
			return adoHandPush(t, r, [][3]string{{a, stale, r.ref}, {tip, merge, other}}, stale+"\n"+merge+"\n^"+a+"\n^"+tip)
		}, []string{adoWorkedOut, ".github/, .github/workflows/other.yml", whyNoTip, tipRemedy(whyNoTip)}, adoTouches, nil},
		{"a branch the push deletes after one the forge does not show leaves the push unresolved", nil, func(t *testing.T, r *adoPushRun, a, b string) []byte {
			other := BranchNSPrefix(r.p.runID) + "other"
			runGit(t, r.work, "push", "-q", r.bare, b+":"+other)
			r.snapshot(t)
			stale := adoCommitOn(t, r, a, "", "lib/c.txt")
			return adoHandPush(t, r, [][3]string{{a, stale, r.ref}, {b, strings.Repeat("0", 40), other}}, stale+"\n^"+a)
		}, []string{adoWorkedOut, whyNoTip}, adoTouches, nil},
		{"deleting the branch is held", nil, func(t *testing.T, r *adoPushRun, _, _ string) []byte {
			return r.gitPush(t, ":"+r.ref)
		}, []string{adoTouches, "Paths: /", whyBranchDeleted, tipRemedy(whyBranchDeleted)}, adoWorkedOut, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newADOPushRun(t, adoReview(), true)
			a := gitOut(t, r.work, "rev-parse", "HEAD")
			if tc.tip != nil {
				tc.tip(t, r)
			} else {
				r.record(t, map[string]string{".github/workflows/ci.yml": "on: reviewed\n"})
			}
			b := gitOut(t, r.work, "rev-parse", "HEAD")
			r.snapshot(t)
			body := tc.push(t, r, a, b)
			if tc.setup != nil {
				tc.setup(t, r)
			}
			r.wantHeld(t, postEntraPush(t, r.p, body), tc.wants, tc.unwanted)
		})
	}
}

// TestPushHoldNamesWhatTheTipComparisonFound: the approval an admin decides
// names every path, counted and digested, when a push both changes a review
// path and brings another back from an older commit, and approving it
// forwards the push.
func TestPushHoldNamesWhatTheTipComparisonFound(t *testing.T) {
	r := newADOPushRun(t, adoReview(), false)
	cp := newPushCP(t, types.ApprovalApproved)
	cp.wire(t, r.p)
	a := gitOut(t, r.work, "rev-parse", "HEAD")
	r.record(t, map[string]string{".github/workflows/ci.yml": "on: reviewed\n"})
	b := gitOut(t, r.work, "rev-parse", "HEAD")
	r.snapshot(t)
	runGit(t, r.work, "checkout", "-q", "--detach", b)
	runGit(t, r.work, "checkout", "-q", a, "--", ".github")
	writeFiles(t, r.work, map[string]string{".github/workflows/other.yml": "on: [push]\n"})
	runGit(t, r.work, "add", "-A")
	merge := gitOut(t, r.work, "commit-tree", gitOut(t, r.work, "write-tree"), "-p", b, "-p", a, "-m", "merge")
	r.wantPassed(t, postEntraPush(t, r.p, r.gitPush(t, merge+":"+r.ref)))
	_, raises := cp.raised()
	want := []string{".github/workflows/ci.yml", ".github/workflows/other.yml"}
	if len(raises) != 1 || !slices.Equal(raises[0].Paths, want) || raises[0].PathsTotal != 2 ||
		raises[0].PathsDigest != types.PushPathsDigest(want) {
		t.Fatalf("raised %+v, want one approval naming %v", raises, want)
	}
}

// TestADOPushHoldNamesWhatThePackLeavesOutBesideACarriedMatch: a push that
// changes one review path and moves a directory the forge already holds onto
// another is held for both, on a branch it creates too.
func TestADOPushHoldNamesWhatThePackLeavesOutBesideACarriedMatch(t *testing.T) {
	r := newADOPushRun(t, reviewSpec(1, []string{".github/workflows/**", "certs/**"}), true)
	runGit(t, r.work, "mv", "lib", "certs")
	body := r.record(t, map[string]string{".github/workflows/other.yml": "on: [push]\n"})
	r.wantHeld(t, postEntraPush(t, r.p, body), []string{adoTouches, ".github/workflows/other.yml, certs/", whyChanged}, adoWorkedOut)
}
