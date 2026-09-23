// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// adoNamePairs are Azure DevOps project and repository names carrying a space
// and the other printable characters the naming rules permit (#485), with the
// canonical path the control plane stores for each.
var adoNamePairs = []struct{ project, repo, path string }{
	{"Payments Platform", "Card Auth (v2).Service", "Payments%20Platform/_git/Card%20Auth%20(v2).Service"},
	{"R&D Ops!", "Bob's @Tools~1", "R&D%20Ops!/_git/Bob's%20@Tools~1"},
	{"Café Équipe", "Ünïcode Repo", "Café%20Équipe/_git/Ünïcode%20Repo"},
	{"100% Done", "Half 50%", "100%25%20Done/_git/Half%2050%25"},
}

// A SPACED REPOSITORY CLONES, FETCHES AND PUSHES THROUGH THE BROKER: the real
// agent-run rewrite, the real proxy, and a real git http-backend behind a fake
// that looks the repository up by its literal names, as Azure DevOps does.
func TestADONames_GitBrokerCloneFetchPush(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	for _, pr := range adoNamePairs {
		bare := adofake.NewFixtureRepo(t)
		h.fake.RegisterRepo("acme", pr.project, pr.repo, bare)
		cloneURL := "https://dev.azure.com/acme/" + pr.path
		dir := h.clone(t, cloneURL)
		if out, err := h.git(t, "-C", dir, "fetch", "origin"); err != nil {
			t.Fatalf("fetch %s: %v\n%s", cloneURL, err, out)
		}
		if out, err := h.push(t, dir, h.runBranch()); err != nil {
			t.Fatalf("push %s: %v\n%s", cloneURL, err, out)
		}
		if out, err := exec.Command("git", "-C", bare, "rev-parse", "--verify", "refs/heads/"+h.runBranch()).CombinedOutput(); err != nil {
			t.Fatalf("%s: the pushed branch is not in the repository: %v\n%s", cloneURL, err, out)
		}
		want := "/acme/" + pr.project + "/_git/" + pr.repo + "/git-receive-pack"
		if !slices.ContainsFunc(h.fake.Requests(), func(r adofake.RecordedRequest) bool { return r.Path == want }) {
			t.Errorf("the fake never saw %q — the names did not arrive as Azure DevOps reads them", want)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	h.finish(t)
}

// ONE REPOSITORY IS ONE APPROVAL KEY, whichever door asks and however the
// client spelled it: the git broker's held push and the REST gate's
// repositories route name the same repository the same way, so a sticky deny
// on one spelling is not reopened by another.
func TestADONames_ApprovalRepoKeyIsOneSpelling(t *testing.T) {
	const want = "card auth (v2).service"
	for _, path := range []string{
		"/acme/Payments%20Platform/_apis/git/repositories/Card%20Auth%20(v2).Service/pushes",
		"/acme/Payments%20Platform/_apis/git/repositories/Card%20Auth%20%28v2%29.Service/pushes",
		"/acme/_apis/git/repositories/CARD%20AUTH%20(V2).SERVICE/refs",
	} {
		if got := adoRepoOf(path); got != want {
			t.Errorf("REST door: adoRepoOf(%q) = %q, want %q", path, got, want)
		}
	}
	for _, path := range []string{
		"/wardyn/git/dev.azure.com/acme/Payments%20Platform/_git/Card%20Auth%20(v2).Service/git-receive-pack",
		"/wardyn/git/dev.azure.com/acme/Payments%20Platform/_git/Card%20Auth%20%28v2%29.Service/git-receive-pack",
	} {
		if got := adoGitAsk(mustLocalReq(t, http.MethodPost, path, nil)).repo; got != want {
			t.Errorf("git door: adoGitAsk(%q).repo = %q, want %q", path, got, want)
		}
	}
	// A spelling that hides structure names no repository at all.
	if got := adoRepoOf("/acme/_apis/git/repositories/a%252Fb/pushes"); got != "" {
		t.Errorf("adoRepoOf of a doubly-encoded separator = %q, want none", got)
	}
}

// A HELD PUSH on a spaced repository asks the control plane about the
// repository by its name, and the query carries the space intact.
func TestADONames_HeldPushAsksByName(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead)
	pr := adoNamePairs[0]
	h.fake.RegisterRepo("acme", pr.project, pr.repo, adofake.NewFixtureRepo(t))
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalApproved)})
	dir := h.clone(t, "https://dev.azure.com/acme/"+pr.path)
	if out, err := h.push(t, dir, h.runBranch()); err != nil {
		t.Fatalf("held push, approved: %v\n%s", err, out)
	}
	asks, raised := cp.snapshot()
	if len(raised) != 1 || asks[0].Get("repo") != strings.ToLower(pr.repo) {
		t.Errorf("asks = %v (raised %d), want one approval for repo %q", asks, len(raised), strings.ToLower(pr.repo))
	}
	h.finish(t)
}

// A SPELLING THE REST GATE REFUSES IS REFUSED AT THE GIT DOOR TOO — a trailing
// dot or an edge space the service trims away, an escaped separator, a double
// encoding — rather than keyed there as a second repository whose sticky deny
// the real one would not inherit.
func TestADONames_EdgeSpellingsRefusedAtBothDoors(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead)
	grant := ADOGrant{Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapRead}}
	for _, repo := range []string{"Repo.", "%20Repo", "Repo%20", "a%2Fb", "a%252Fb"} {
		rest := httptest.NewRequest(http.MethodGet, "https://dev.azure.com/acme/proj/_apis/git/repositories/"+repo+"/items", nil)
		if msg, _ := adoCheck(rest, "dev.azure.com", grant, adoRefProtected); msg == "" {
			t.Errorf("REST door forwarded repository %q", repo)
		}
		path := "/wardyn/git/dev.azure.com/acme/proj/_git/" + repo + "/info/refs"
		if got := adoGitAsk(mustLocalReq(t, http.MethodGet, path, nil)).repo; got != "" {
			t.Errorf("git door keyed %q as %q, want no key", repo, got)
		}
		before := len(h.fake.Requests())
		rec := httptest.NewRecorder()
		h.p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, path+"?service=git-upload-pack", nil))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "would read as another") {
			t.Errorf("git door, repository %q: %d %q, want the refusal", repo, rec.Code, rec.Body.String())
		}
		if n := len(h.fake.Requests()) - before; n != 0 {
			t.Errorf("git door, repository %q: Azure DevOps saw %d request(s), want none", repo, n)
		}
	}
	// The spelling both doors accept keys identically at both.
	rest := adoRepoOf("/acme/proj/_apis/git/repositories/Repo%2E1/items")
	git := adoGitAsk(mustLocalReq(t, http.MethodGet, "/wardyn/git/dev.azure.com/acme/proj/_git/Repo.1/info/refs", nil)).repo
	if rest != "repo.1" || git != rest {
		t.Errorf("REST key %q, git key %q, want both repo.1", rest, git)
	}
	h.finish(t)
}
