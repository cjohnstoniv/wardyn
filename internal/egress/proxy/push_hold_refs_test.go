// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPushHoldSwappedRefsAskAgain: an approved two-ref push, replayed with
// its commits swapped between the refs, is a different push. Through the
// broker it is held again — the approval service now answers denied — and
// never reaches the forge on the first push's approval.
func TestPushHoldSwappedRefsAskAgain(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalApproved)
	refA, refB := BranchNSPrefix(p.runID)+"review", BranchNSPrefix(p.runID)+"release"
	f := forgeOn(t, httptest.NewServer)
	bare := filepath.Join(f.root, "octocat", "hello-world.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.root, "init", "-q", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", work)
	writeFiles(t, work, workflowPush)
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "first")
	runGit(t, work, "branch", "review")
	writeFiles(t, work, map[string]string{".github/workflows/ci.yml": "on: push\njobs: changed\n"})
	runGit(t, work, "commit", "-qam", "second")
	runGit(t, work, "push", "-q", f.srv.URL+"/octocat/hello-world.git", "review:"+refA, "HEAD:"+refB)
	body := f.pushBody()
	res, err := gitpack.Inspect(body)
	if err != nil || len(res.Commands) != 2 || res.Commands[0].New == res.Commands[1].New {
		t.Fatalf("recording invalid: %+v %v", res.Commands, err)
	}
	if first := postPush(t, p, string(body)); first.Code != http.StatusOK {
		t.Fatalf("first push = %d %s", first.Code, first.Body.String())
	}
	var swapped bytes.Buffer
	for i, c := range res.Commands {
		line := fmt.Sprintf("%s %s %s", c.New, res.Commands[1-i].New, c.Ref)
		if i == 0 {
			line += "\x00report-status"
		}
		line += "\n"
		fmt.Fprintf(&swapped, "%04x%s", len(line)+4, line)
	}
	swapped.WriteString("0000")
	swapped.Write(body[bytes.Index(body, []byte("PACK")):])
	if _, err := gitpack.Inspect(swapped.Bytes()); err != nil {
		t.Fatal(err)
	}
	cp.set(types.ApprovalDenied)
	up.gitPath, up.gitBody = "", nil
	second := postPush(t, p, swapped.String())
	// The content rules still read the forge before the hold (gitHits counts
	// that read); what must not reach it is the push itself.
	forwarded := strings.HasSuffix(up.gitPath, "/git-receive-pack") || bytes.Equal(up.gitBody, swapped.Bytes())
	if raises, _ := cp.counts(); second.Code != http.StatusForbidden || raises != 2 || forwarded {
		t.Fatalf("swapped refs reused the approval: status=%d raises=%d forwarded=%v (last forge request %q), want 403, a second raise and no push forwarded",
			second.Code, raises, forwarded, up.gitPath)
	}
}

// TestPushHoldBindsEachRefToItsCommit: the approval covers each ref set to
// its own commit, deletions included. The same refs and the same commits
// paired differently raise a fresh approval rather than reuse the one given.
func TestPushHoldBindsEachRefToItsCommit(t *testing.T) {
	a, b, zero := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("0", 40)
	for _, tc := range []struct {
		name        string
		first, then [3]string // what refs A, B and C are set to ("" leaves C out)
	}{
		{"swapped", [3]string{a, b, ""}, [3]string{b, a, ""}},
		{"regrouped", [3]string{a, b, b}, [3]string{a, a, b}},
		{"deleted instead", [3]string{a, b, zero}, [3]string{a, zero, b}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{runID: uuid.New()}
			cp := newPushCP(t, types.ApprovalApproved)
			cp.wire(t, p)
			refs := [3]string{BranchNSPrefix(p.runID) + "a", BranchNSPrefix(p.runID) + "b", BranchNSPrefix(p.runID) + "c"}
			target := appPushTarget("owner/repo", uuid.New())
			rules := &pushRuleSet{hold: time.Second}
			try := func(news [3]string) bool {
				var cmds []gitpack.Command
				for i, n := range news {
					if n != "" {
						cmds = append(cmds, gitpack.Command{Ref: refs[i], New: n})
					}
				}
				rv := pushReview{paths: []string{".github/workflows/ci.yml"}, cmds: cmds}
				return p.holdPush(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), rules,
					rv, target, slog.String("repo", target.repo), func(string) {})
			}
			if !try(tc.first) {
				t.Fatal("initial approved push was refused")
			}
			if !try(tc.first) {
				t.Fatal("the same push again was refused")
			}
			cp.set(types.ApprovalDenied)
			forwarded := try(tc.then)
			if raises, _ := cp.counts(); forwarded || raises != 2 {
				t.Fatalf("forwarded=%v raises=%d, want a refusal after a fresh approval", forwarded, raises)
			}
		})
	}
}
