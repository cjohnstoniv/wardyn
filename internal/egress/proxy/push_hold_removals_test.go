// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// removalCases take .github/workflows/ci.yml off a branch whose tip holds it.
var removalCases = []struct {
	name   string
	remove func(t *testing.T, work string)
}{
	{"deleting it", func(t *testing.T, work string) { runGit(t, work, "rm", "-q", ".github/workflows/ci.yml") }},
	{"renaming it out", func(t *testing.T, work string) { runGit(t, work, "mv", ".github/workflows/ci.yml", "ci.yml") }},
	{"replacing its parent with a file", func(t *testing.T, work string) {
		runGit(t, work, "rm", "-rq", ".github")
		writeFiles(t, work, map[string]string{".github": "not a directory\n"})
	}},
}

// TestPushHoldRemovalsOffAddsNoConfigByte: off, the proxy config a run is dispatched with carries no
// new key, so an older proxy reads it as it did.
func TestPushHoldRemovalsOffAddsNoConfigByte(t *testing.T) {
	off, err := json.Marshal(Config{RunID: uuid.Nil})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(off, []byte("push_hold_removals")) {
		t.Errorf("off config = %s, want no push_hold_removals key", off)
	}
	on, _ := json.Marshal(Config{PushHoldRemovals: true})
	var back Config
	if err := json.Unmarshal(on, &back); err != nil || !back.PushHoldRemovals {
		t.Errorf("on config %s did not round-trip: %+v, %v", on, back.PushHoldRemovals, err)
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// removalRun is an unattended run against a local forge whose branch tip holds tipFiles on top of
// ordinaryRepo, with the working tree checked out at that tip.
func removalRun(t *testing.T, lane string, on bool, tipFiles map[string]string, review ...string) (*forgeRun, string) {
	t.Helper()
	r := newForgeRunSpec(t, lane, reviewSpec(1, review), ordinaryRepo, nil)
	r.p.pushHolds.unattended, r.p.pushHoldRemovals = true, on
	branch := strings.TrimPrefix(r.ref, "refs/heads/")
	tip := r.asHuman(t, "main", branch, tipFiles)
	runGit(t, r.work, "fetch", "-q", "origin", branch)
	runGit(t, r.work, "checkout", "-q", "--detach", tip)
	return r, tip
}

func TestPushHoldRemovals_GitHub(t *testing.T) {
	const ci = ".github/workflows/ci.yml"
	workflows := []string{".github/workflows/**"}
	readme := map[string]string{"README.md": "hello\n"}
	for _, lane := range []struct{ name, host string }{{"App", ""}, {"git_pat", githubHost}} {
		for _, on := range []bool{false, true} {
			t.Run(lane.name+"/"+onOff(on), func(t *testing.T) {
				for _, tc := range removalCases {
					t.Run(tc.name, func(t *testing.T) {
						r, _ := removalRun(t, lane.host, on, readme, workflows...)
						tc.remove(t, r.work)
						r.commit(t, "remove")
						out, err := r.push()
						if !on {
							r.wantAllowed(t, out, err)
							return
						}
						wantRejectedRef(t, r, out, err, ci, ruleSourceGitPushHeldUnattended)
						if !strings.Contains(out, whyTipChanged) {
							t.Errorf("git output does not say %q:\n%s", whyTipChanged, out)
						}
					})
				}
				t.Run("a force-push back past a commit that added one", func(t *testing.T) {
					r := newForgeRunSpec(t, lane.host, reviewSpec(1, workflows), ordinaryRepo, nil)
					r.p.pushHolds.unattended, r.p.pushHoldRemovals = true, on
					branch := strings.TrimPrefix(r.ref, "refs/heads/")
					a := gitOut(t, r.work, "rev-parse", "HEAD")
					tip := r.asHuman(t, "main", branch, map[string]string{".github/workflows/new.yml": "on: push\n"})
					runGit(t, r.work, "fetch", "-q", "origin", branch)
					runGit(t, r.work, "checkout", "-q", "--detach", a)
					out, err := r.pushRefs("+" + a + ":" + r.ref)
					if !on {
						r.wantAllowed(t, out, err)
						return
					}
					if tip == a {
						t.Fatal("the tip is the commit pushed back to")
					}
					wantRejectedRef(t, r, out, err, ".github/workflows/new.yml", ruleSourceGitPushHeldUnattended)
				})
				t.Run("a restore of the older directory", func(t *testing.T) {
					r := newForgeRunSpec(t, lane.host, reviewSpec(1, workflows), ordinaryRepo, nil)
					r.p.pushHolds.unattended, r.p.pushHoldRemovals = true, on
					branch := strings.TrimPrefix(r.ref, "refs/heads/")
					a := gitOut(t, r.work, "rev-parse", "HEAD")
					tip := r.asHuman(t, "main", branch, map[string]string{".github/workflows/new.yml": "on: push\n"})
					runGit(t, r.work, "fetch", "-q", "origin", branch)
					runGit(t, r.work, "checkout", "-q", "--detach", tip)
					runGit(t, r.work, "rm", "-rq", ".github")
					runGit(t, r.work, "checkout", "-q", a, "--", ".github")
					writeFiles(t, r.work, map[string]string{"README.md": "edited\n"})
					r.commit(t, "restore")
					out, err := r.push()
					if !on {
						r.wantAllowed(t, out, err)
						return
					}
					wantRejectedRef(t, r, out, err, ".github/workflows/new.yml", ruleSourceGitPushHeldUnattended)
				})
				if !on {
					return
				}
				t.Run("a file replaced by a directory", func(t *testing.T) {
					r, _ := removalRun(t, lane.host, on, readme, "**/*.yml")
					runGit(t, r.work, "rm", "-q", ci)
					writeFiles(t, r.work, map[string]string{ci + "/readme": "a directory now\n"})
					r.commit(t, "replace")
					out, err := r.push()
					wantRejectedRef(t, r, out, err, ci, ruleSourceGitPushHeldUnattended)
				})
				for _, tc := range []struct {
					name  string
					tip   map[string]string
					apply func(t *testing.T, r *forgeRun)
				}{
					{"an edit elsewhere", readme, func(t *testing.T, r *forgeRun) { r.editMain(t, "x") }},
					{"a removal outside the pattern", readme, func(t *testing.T, r *forgeRun) {
						runGit(t, r.work, "rm", "-q", "Makefile")
						r.commit(t, "remove Makefile")
					}},
					{"an edit beside a tip's own change to the file", map[string]string{ci: "on: reviewed\n"}, func(t *testing.T, r *forgeRun) { r.editMain(t, "x") }},
				} {
					t.Run("passes: "+tc.name, func(t *testing.T) {
						r, _ := removalRun(t, lane.host, on, tc.tip, workflows...)
						tc.apply(t, r)
						out, err := r.push()
						r.wantAllowed(t, out, err)
					})
				}
			})
		}
	}
}

// TestPushHoldRemovals_DenyPathsUnchanged: a deny pattern still judges only what a push introduces.
func TestPushHoldRemovals_DenyPathsUnchanged(t *testing.T) {
	r := newForgeRun(t, ordinaryRepo, nil, ".github/workflows/**")
	r.p.pushHolds.unattended, r.p.pushHoldRemovals = true, true
	branch := strings.TrimPrefix(r.ref, "refs/heads/")
	tip := r.asHuman(t, "main", branch, map[string]string{"README.md": "hello\n"})
	runGit(t, r.work, "fetch", "-q", "origin", branch)
	runGit(t, r.work, "checkout", "-q", "--detach", tip)
	removalCases[0].remove(t, r.work)
	r.commit(t, "remove")
	out, err := r.push()
	r.wantAllowed(t, out, err)
}

// removalADORun is a run on Azure DevOps whose branch the forge holds, with its REST view
// snapshotted, ready for the change a case makes.
func removalADORun(t *testing.T, spec types.RunPolicySpec, unattended, on bool) *adoPushRun {
	t.Helper()
	r := newADOPushRun(t, spec, unattended)
	r.p.pushHoldRemovals = on
	r.record(t, map[string]string{"lib/b.txt": "b\n"})
	r.snapshot(t)
	return r
}

func TestPushHoldRemovals_AzureDevOps(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, tc := range removalCases {
			t.Run(onOff(on)+"/"+tc.name, func(t *testing.T) {
				r := removalADORun(t, adoReview(), true, on)
				tc.remove(t, r.work)
				commitAll(t, r.work)
				rec := postEntraPush(t, r.p, r.gitPush(t, "HEAD:"+r.ref))
				if !on {
					r.wantPassed(t, rec)
					r.finish(t)
					return
				}
				r.wantHeld(t, rec, []string{adoTouches, ".github/workflows/ci.yml", whyTipChanged}, adoWorkedOut)
			})
		}
		if on {
			t.Run("a file outside the pattern deleted passes", func(t *testing.T) {
				r := removalADORun(t, adoReview(), true, on)
				runGit(t, r.work, "rm", "-q", "lib/a.txt")
				commitAll(t, r.work)
				r.wantPassed(t, postEntraPush(t, r.p, r.gitPush(t, "HEAD:"+r.ref)))
				r.finish(t)
			})
		}
	}
}

// TestPushHoldRemovals_AzureDevOpsComparisonFailsClosed: a listing the removal walk cannot read holds the push.
func TestPushHoldRemovals_AzureDevOpsComparisonFailsClosed(t *testing.T) {
	for _, on := range []bool{false, true} {
		r := removalADORun(t, adoReview(), true, on)
		tip := gitOut(t, r.work, "rev-parse", "HEAD")
		tree := "/trees/" + gitOut(t, r.work, "rev-parse", tip+":.github")
		fail := func(w http.ResponseWriter, req *http.Request) bool {
			if !strings.HasSuffix(req.URL.Path, tree) {
				return false
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return true
		}
		r.rest.hook.Store(&fail)
		runGit(t, r.work, "rm", "-rq", ".github")
		commitAll(t, r.work)
		rec := postEntraPush(t, r.p, r.gitPush(t, "HEAD:"+r.ref))
		if !on {
			r.wantPassed(t, rec)
			r.finish(t)
			continue
		}
		r.wantHeld(t, rec, []string{adoWorkedOut, whyTipUnread}, adoTouches)
	}
}

// TestPushHoldRemovals_HeldForAnAdmin: attended, a removal is held like a change, and approving it
// forwards the push; off, nothing is raised.
func TestPushHoldRemovals_HeldForAnAdmin(t *testing.T) {
	for _, on := range []bool{false, true} {
		r := newADOPushRun(t, adoReview(), false)
		cp := newPushCP(t, types.ApprovalApproved)
		cp.wire(t, r.p)
		r.p.pushHoldRemovals = on
		r.record(t, map[string]string{"lib/b.txt": "b\n"})
		r.snapshot(t)
		removalCases[0].remove(t, r.work)
		commitAll(t, r.work)
		r.wantPassed(t, postEntraPush(t, r.p, r.gitPush(t, "HEAD:"+r.ref)))
		_, raises := cp.raised()
		if !on {
			if len(raises) != 0 {
				t.Errorf("off: raised %+v, want none", raises)
			}
			continue
		}
		want := []string{".github/workflows/ci.yml"}
		if len(raises) != 1 || !slices.Equal(raises[0].Paths, want) || raises[0].PathsTotal != 1 ||
			raises[0].PathsDigest != types.PushPathsDigest(want) {
			t.Fatalf("raised %+v, want one approval naming %v", raises, want)
		}
	}
}

// TestPushHoldRemovals_RemoteWithNoReader: on a git_pat remote the broker cannot read, a removal
// is not forwarded unseen. An Azure DevOps Services remote in a "_git/_full/" spelling, and an
// Azure DevOps Server remote, are held or refused as unreadable, with a remedy that is not "try
// again"; off, and for a host the broker never reads, the push is judged as before.
func TestPushHoldRemovals_RemoteWithNoReader(t *testing.T) {
	for _, tc := range []struct {
		name, host, path string
		held             bool
		fix              string
	}{
		{"Azure DevOps Services, the _git _full spelling", "dev.azure.com", "/acme/proj/_git/_full/app.git", true, "standard remote"},
		{"Azure DevOps Server", "tfs.corp.example", "/tfs/DefaultCollection/proj/_git/app.git", true, "Azure DevOps grant"},
		{"Azure DevOps Server, a _git segment with a trailing dot", "tfs.corp.example", "/tfs/DefaultCollection/proj/_git./app.git", true, "Azure DevOps grant"},
		{"a host with no reader and no _git path", "git.corp.example", "/acme/app.git", false, ""},
	} {
		for _, on := range []bool{false, true} {
			t.Run(tc.name+"/"+onOff(on), func(t *testing.T) {
				r, _ := removalRun(t, tc.host, on, map[string]string{"README.md": "hello\n"}, ".github/workflows/**")
				at := filepath.Join(r.forge.root, filepath.FromSlash(strings.TrimPrefix(tc.path, "/")))
				if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(r.bare, at); err != nil {
					t.Fatal(err)
				}
				runGit(t, r.work, "remote", "set-url", "origin", r.brokerURL+"/wardyn/git/"+tc.host+tc.path)
				removalCases[0].remove(t, r.work)
				r.commit(t, "remove")
				out, err := r.push()
				if !on || !tc.held {
					r.wantAllowed(t, out, err)
					return
				}
				wantRejectedRef(t, r, out, err, "WARDYN_PUSH_HOLD_REMOVALS", ruleSourceGitPushHeldUnattended)
				for _, want := range []string{tc.fix, "will not help"} {
					if !strings.Contains(out, want) {
						t.Errorf("git output does not say %q:\n%s", want, out)
					}
				}
				if strings.Contains(out, "try the push again") {
					t.Errorf("git output tells the person to try again:\n%s", out)
				}
			})
		}
	}
}

// TestTipRemedy_ReadCapSaysSplit: a comparison that needs more reads than one push may make is not
// cured by a retry.
func TestTipRemedy_ReadCapSaysSplit(t *testing.T) {
	why := whyTipUnread + fmt.Sprintf("the comparison needs more than %d reads of the forge", maxForgeReads)
	if got := tipRemedy(why); !strings.Contains(got, "split the push") || strings.Contains(got, "try the push again") {
		t.Errorf("tipRemedy(%q) = %q, want a remedy that splits the push", why, got)
	}
	if got := tipRemedy(whyTipUnread + "a read failed"); !strings.Contains(got, "try the push again") {
		t.Errorf("tipRemedy of another unread cause = %q, want the retry remedy kept", got)
	}
	if got := tipRemedy(whyUnread + fmt.Sprintf("the comparison needs more than %d reads of the forge", maxForgeReads)); got != "" {
		t.Errorf("tipRemedy of the first comparison's read cap = %q, want none: splitting does not cut those reads", got)
	}
}
