// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// carriedPush is a pack holding one commit (and its empty tree) with a command
// for it plus a command for each extra "ref": "oid" update the pack lacks.
func carriedPush(carriedRef string, extra ...[2]string) string {
	const objCommit, objTree byte = 1, 2
	commit := fmt.Appendf(nil, "tree %s\nauthor T <t@example.com> 1767225600 +0000\n"+
		"committer T <t@example.com> 1767225600 +0000\n\nx\n", gitObjectID("tree", nil))
	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = append(pack, packObjHeader(objCommit, int64(len(commit)))...)
	pack = append(pack, storedZlib(commit)...)
	pack = append(pack, packObjHeader(objTree, 0)...)
	pack = append(pack, storedZlib(nil)...)
	sum := sha1.Sum(pack)
	cmds := pkt(zeroOID + " " + gitObjectID("commit", commit) + " " + carriedRef + firstCaps)
	for _, e := range extra {
		cmds += pkt(zeroOID + " " + e[1] + " " + e[0])
	}
	return cmds + "0000" + string(pack) + string(sum[:])
}

// A push whose ref points at a commit the request does not carry stays refused
// (415, the same rule source, nothing forwarded), but the text names each such
// ref and no longer sends the person to a complete clone. A push carrying no
// object at all asks the forge first (TestPushRulesPassAPushThatAddsNoNewCommits);
// this forge holds neither commit, so it is refused as one that adds none.
func TestPushRulesNameRefsWhoseTargetTheRequestLacks(t *testing.T) {
	anyBranch := types.RunPolicySpec{
		GitPushAnyBranch: true,
		PushRules:        &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
	}
	for _, tc := range []struct {
		name     string
		body     string
		wording  string
		want     []string
		notWant  []string
		askForge bool
	}{
		{"a branch created at a commit the forge has", pkt(zeroOID+" "+someOID+" refs/heads/second"+firstCaps) + "0000" + emptyPack(),
			"This push adds no new commits; the remote already has them.", []string{"refs/heads/second"}, nil, true},
		{"no pack at all", pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000",
			"This push adds no new commits; the remote already has them.", []string{"refs/heads/main"}, nil, true},
		{"one ref carried, one not", carriedPush("refs/heads/carried", [2]string{"refs/heads/absent", someOID}),
			"does not carry the commit these refs point to", []string{"refs/heads/absent"}, []string{"refs/heads/carried"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, sink := newGitBrokerProxyWithSpec(t,
				map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), anyBranch)

			rec := postPush(t, p, tc.body)
			got := rec.Body.String()

			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415 (body %q)", rec.Code, got)
			}
			if !strings.Contains(got, tc.wording) {
				t.Errorf("refusal = %q, want %q", got, tc.wording)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("refusal = %q, want it to name %s", got, w)
				}
			}
			for _, w := range append(tc.notWant, "unshallow", "complete clone") {
				if strings.Contains(got, w) {
					t.Errorf("refusal = %q, must not contain %q", got, w)
				}
			}
			if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBlind+`"`) {
				t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBlind)
			}
			// Asking the forge is one API read with the run's own credential; a
			// push it cannot clear is still never forwarded.
			switch {
			case !tc.askForge && (up.mintCalls != 0 || up.gitHits != 0):
				t.Errorf("a refused push minted %d credentials and reached the forge %d times, want 0 and 0",
					up.mintCalls, up.gitHits)
			case tc.askForge && (up.gitHits != 1 || up.gitHost != githubAPIHost):
				t.Errorf("the forge saw %d request(s), the last to %s %s, want one API read", up.gitHits, up.gitHost, up.gitPath)
			}
		})
	}
}

// A pack with no objects.
func emptyPack() string {
	pack := binary.BigEndian.AppendUint32([]byte("PACK"), 2)
	pack = binary.BigEndian.AppendUint32(pack, 0)
	sum := sha1.Sum(pack)
	return string(pack) + string(sum[:])
}

// TestPushRulesPassAPushThatAddsNoNewCommits: a push that sets a ref to a
// commit the remote already has (a branch created at an existing commit, a
// rollback) sends no object, so the content rules have nothing to read. The
// broker asks the forge, with the run's own credential, whether each commit is
// in the history of the default branch or of a branch the push updates; when
// every one is, the push goes through as carrying no new content. When the
// forge cannot say so, the push is refused with that reason.
func TestPushRulesPassAPushThatAddsNoNewCommits(t *testing.T) {
	const deny = ".github/workflows/**"
	const headline = "This push adds no new commits; the remote already has them."
	push := func(r *forgeRun, spec string) (string, error) {
		cmd := exec.Command("git", "push", "origin", spec)
		cmd.Dir, cmd.Env = r.work, gitEnv()
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	refused := func(t *testing.T, r *forgeRun, out string, err error, why string) {
		t.Helper()
		if err == nil {
			t.Fatalf("git push succeeded, want a refusal\n%s", out)
		}
		for _, w := range []string{headline, r.ref, why} {
			if !strings.Contains(r.lastRefusal(), w) {
				t.Errorf("refusal %q does not say %q", r.lastRefusal(), w)
			}
		}
		if r.landed(t) {
			t.Error("the refused push moved the forge's run branch")
		}
	}

	t.Run("an empty-pack branch create at the default branch's tip", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, nil, deny)
		out, err := r.push()
		r.wantAllowed(t, out, err)
		if n, auth := r.forge.reads(); n == 0 || auth == "" {
			t.Errorf("the forge was read %d time(s) with credential %q, want the run's own", n, auth)
		}
		if !strings.Contains(r.decisions.String(), `"rule_source":"`+ruleSourceGitForgeRead+`"`) {
			t.Errorf("no %s decision for the lookup:\n%s", ruleSourceGitForgeRead, r.decisions)
		}
	})
	t.Run("a rollback of the run branch to a commit in its history", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, nil, deny)
		r.editMain(t, "forward")
		out, err := r.push()
		r.wantAllowed(t, out, err)
		runGit(t, r.work, "reset", "-q", "--hard", "HEAD~1")
		out, err = push(r, "+HEAD:"+r.ref)
		r.wantAllowed(t, out, err)
	})
	t.Run("a commit the forge holds only on another branch", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, nil, deny)
		side := r.asHuman(t, "main", "side", map[string]string{".github/workflows/x.yml": "on: push\n"})
		runGit(t, r.work, "fetch", "-q", "origin", "side")
		runGit(t, r.work, "checkout", "-q", side)
		out, err := r.push()
		refused(t, r, out, err, whyNotHeld)
	})
	t.Run("the forge cannot be read", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, nil, deny)
		r.forge.apiHook = func(w http.ResponseWriter, _ *http.Request) bool {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return true
		}
		out, err := r.push()
		refused(t, r, out, err, whyHeldUnread)
		if !strings.Contains(r.lastRefusal(), "answered 500") {
			t.Errorf("refusal %q does not give the forge's answer", r.lastRefusal())
		}
	})
	t.Run("a forge other than GitHub", func(t *testing.T) {
		r := newForgeRunVia(t, "gitlab.com", ordinaryRepo, nil, deny)
		out, err := r.push()
		refused(t, r, out, err, whyHeldNotGitHub)
		if n, _ := r.forge.reads(); n != 0 {
			t.Errorf("a push to a forge that is not GitHub read the GitHub API %d time(s)", n)
		}
	})
	t.Run("the branch rules still apply", func(t *testing.T) {
		r := newForgeRun(t, ordinaryRepo, nil, deny)
		out, err := push(r, "HEAD:refs/heads/elsewhere")
		if err == nil {
			t.Fatalf("a push outside the run's branch namespace succeeded\n%s", out)
		}
		if exec.Command("git", "-C", r.bare, "rev-parse", "-q", "--verify", "refs/heads/elsewhere").Run() == nil {
			t.Error("the forge holds refs/heads/elsewhere")
		}
		if n, _ := r.forge.reads(); n != 0 {
			t.Errorf("a push the branch rules refuse read the forge %d time(s)", n)
		}
	})
}
