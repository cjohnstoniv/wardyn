// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

const adoAPIPrefix = "/acme/proj/_apis/git/repositories/app"

// adoREST answers the Git REST reads the broker makes from a snapshot of the
// repository taken BEFORE the recorded push, because that is all Azure DevOps
// holds while a receive-pack is being inspected: a commit the push carries is
// one it answers 404 for. It also takes the forwarded pack.
type adoREST struct {
	dir      atomic.Pointer[string]
	hook     atomic.Pointer[func(http.ResponseWriter, *http.Request) bool]
	reads    atomic.Int32
	packs    atomic.Int32
	strayed  atomic.Pointer[string]
	badAuth  atomic.Pointer[string]
	wantAuth atomic.Pointer[string]
}

func (a *adoREST) git(args ...string) (string, bool) {
	out, err := exec.Command("git", append([]string{"-C", *a.dir.Load()}, args...)...).Output()
	return strings.TrimSpace(string(out)), err == nil
}

func (a *adoREST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		a.packs.Add(1)
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		return
	}
	a.reads.Add(1)
	rest, ok := strings.CutPrefix(r.URL.Path, adoAPIPrefix)
	if !ok || r.URL.Query().Get("api-version") != "7.1" {
		p := r.URL.String()
		a.strayed.Store(&p)
		http.NotFound(w, r)
		return
	}
	if want := a.wantAuth.Load(); want != nil && r.Header.Get("Authorization") != *want {
		got := r.Header.Get("Authorization")
		a.badAuth.Store(&got)
	}
	if h := a.hook.Load(); h != nil && (*h)(w, r) {
		return
	}
	var body any
	switch {
	case rest == "":
		head, _ := a.git("symbolic-ref", "HEAD")
		body = map[string]string{"defaultBranch": head}
	case rest == "/refs":
		out, _ := a.git("for-each-ref", "--format=%(refname) %(objectname)", "refs/"+r.URL.Query().Get("filter"))
		refs := []map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if name, id, ok := strings.Cut(line, " "); ok {
				refs = append(refs, map[string]string{"name": name, "objectId": id})
			}
		}
		body = map[string]any{"value": refs}
	case rest == "/diffs/commits":
		q := r.URL.Query()
		target := q.Get("targetVersion")
		if q.Get("targetVersionType") == "branch" {
			target = "refs/heads/" + target
		}
		if q.Get("baseVersionType") != "commit" || q.Get("$top") != "1" {
			http.Error(w, "bad diff request", http.StatusBadRequest)
			return
		}
		_, baseOK := a.git("cat-file", "-e", q.Get("baseVersion")+"^{commit}")
		_, targetOK := a.git("rev-parse", "--verify", "-q", target+"^{commit}")
		if !baseOK || !targetOK {
			http.Error(w, `{"message":"TF401175"}`, http.StatusNotFound)
			return
		}
		common, _ := a.git("merge-base", q.Get("baseVersion"), target)
		body = map[string]string{"commonCommit": common}
	case strings.HasPrefix(rest, "/commits/"):
		tree, ok := a.git("rev-parse", strings.TrimPrefix(rest, "/commits/")+"^{tree}")
		if !ok {
			http.NotFound(w, r)
			return
		}
		body = map[string]string{"treeId": tree}
	case strings.HasPrefix(rest, "/trees/"):
		id := strings.TrimPrefix(rest, "/trees/")
		out, ok := a.git("ls-tree", id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		entries := []map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			meta, name, _ := strings.Cut(line, "\t")
			if f := strings.Fields(meta); len(f) == 3 {
				// Azure DevOps spells a directory's mode 40000 (Trees - Get's sample), not ls-tree's 040000.
				entries = append(entries, map[string]string{"relativePath": name, "mode": strings.TrimPrefix(f[0], "0"), "gitObjectType": f[1], "objectId": f[2]})
			}
		}
		body = map[string]any{"objectId": id, "treeEntries": entries}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// adoPushRun is one governed unattended run on the Entra lane and one repository.
type adoPushRun struct {
	p    *Proxy
	sink *bytes.Buffer
	logs *bytes.Buffer
	rest *adoREST
	f    *gitForge
	work string
	bare string
	ref  string
	auth string
}

var adoSeed = map[string]string{".github/workflows/ci.yml": "on: push\n", ".github/workflows/other.yml": "on: push\n", "lib/a.txt": "a\n", "README.md": "hello\n"}

func newADOPushRun(t *testing.T, spec types.RunPolicySpec, unattended bool) *adoPushRun {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	rest := &adoREST{}
	srv := httptest.NewTLSServer(rest)
	t.Cleanup(srv.Close)
	p, sink, _ := newEntraProxy(t, spec, upstreamAddr(srv), adoscope.CapCodeRead, adoscope.CapCodeWrite)
	p.pushHolds.unattended = unattended
	auth := p.inject.byHost["dev.azure.com"].header.value
	rest.wantAuth.Store(&auth)

	logs := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := forgeOn(t, httptest.NewServer)
	bare := filepath.Join(f.root, "octocat", "hello-world.git")
	runGit(t, f.root, "init", "-q", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", work)
	writeFiles(t, work, adoSeed)
	commitAll(t, work)
	runGit(t, work, "push", "-q", bare, "main")
	r := &adoPushRun{p: p, sink: sink, logs: logs, rest: rest, f: f, work: work, bare: bare, ref: BranchNSPrefix(p.runID) + "work", auth: auth}
	r.snapshot(t)
	return r
}

// snapshot makes what the repository holds now all that the REST fake knows.
func (r *adoPushRun) snapshot(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "held.git")
	runGit(t, r.f.root, "clone", "-q", "--bare", r.bare, dir)
	r.rest.dir.Store(&dir)
}

// record commits files on the run branch and returns the bytes git pushes.
func (r *adoPushRun) record(t *testing.T, files map[string]string, pushArgs ...string) []byte {
	t.Helper()
	writeFiles(t, r.work, files)
	commitAll(t, r.work)
	r.pushHead(t, pushArgs...)
	return r.f.pushBody()
}

// pushHead pushes with no deltas, the readable pack the broker's no-thin
// advertisement gets from a real client (TestADOPushThroughTheBroker).
func (r *adoPushRun) pushHead(t *testing.T, pushArgs ...string) {
	t.Helper()
	args := append([]string{"-c", "pack.window=0", "push", "-q"}, pushArgs...)
	runGit(t, r.work, append(args, r.f.srv.URL+"/octocat/hello-world.git", "HEAD:"+r.ref)...)
}

func (r *adoPushRun) finish(t *testing.T) string {
	t.Helper()
	_ = r.p.sink.close(context.Background())
	secret := strings.TrimPrefix(r.auth, "Bearer ")
	for name, s := range map[string]string{"decision log": r.sink.String(), "slog": r.logs.String()} {
		if strings.Contains(s, secret) {
			t.Errorf("%s carries the credential:\n%s", name, s)
		}
	}
	if s := r.rest.strayed.Load(); s != nil {
		t.Errorf("the broker read outside the push's repository, or at another version: %s", *s)
	}
	if s := r.rest.badAuth.Load(); s != nil {
		t.Errorf("a read carried Authorization %q, not the lane's credential", *s)
	}
	return r.sink.String()
}

func (r *adoPushRun) wantPassed(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK || r.rest.packs.Load() != 1 {
		t.Fatalf("status %d, %d pack(s) forwarded, want the push forwarded:\n%s", rec.Code, r.rest.packs.Load(), rec.Body)
	}
}

// wantHeld asserts an unattended refusal with its stable rule source, nothing
// forwarded, and the sentences an honest refusal needs.
func (r *adoPushRun) wantHeld(t *testing.T, rec *httptest.ResponseRecorder, wants []string, unwanted string) {
	t.Helper()
	// A refusal the agent can act on is answered in git's own terms.
	if rec.Code != http.StatusOK || r.rest.packs.Load() != 0 {
		t.Fatalf("status %d, %d pack(s) forwarded, want a rejected ref and none:\n%s", rec.Code, r.rest.packs.Load(), rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ng refs/heads/") {
		t.Errorf("refusal rejects no ref:\n%s", body)
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("refusal does not say %q:\n%s", w, body)
		}
	}
	if strings.Contains(body, unwanted) {
		t.Errorf("refusal says %q:\n%s", unwanted, body)
	}
	if strings.Contains(body, strings.TrimPrefix(r.auth, "Bearer ")) {
		t.Errorf("refusal carries the credential:\n%s", body)
	}
	if log := r.finish(t); !strings.Contains(log, ruleSourceGitPushHeldUnattended) {
		t.Errorf("no %s row:\n%s", ruleSourceGitPushHeldUnattended, log)
	}
}

const (
	adoWorkedOut = "could not be worked out on this lane"
	adoTouches   = "this push touches paths"
)

func adoReview() types.RunPolicySpec { return reviewSpec(1, []string{".github/workflows/**"}) }

// TestADOPushReviewPathHoldsOnlyWhatItMatches: a branch-creating push that
// touches no review path passes, because the REST API shows what it does not
// carry is unchanged. One that does touch one stays held, whatever the pack
// leaves out.
func TestADOPushReviewPathHoldsOnlyWhatItMatches(t *testing.T) {
	t.Run("a new branch touching no review path passes", func(t *testing.T) {
		r := newADOPushRun(t, adoReview(), true)
		body := r.record(t, map[string]string{"lib/b.txt": "b\n"})
		r.wantPassed(t, postEntraPush(t, r.p, body))
		if r.rest.reads.Load() == 0 {
			t.Error("the broker read nothing, so nothing vouched for this push")
		}
		rows := r.finish(t)
		for _, want := range []string{ruleSourceGitForgeRead, `"dev.azure.com"`} {
			if !strings.Contains(rows, want) {
				t.Errorf("decision log lacks %s:\n%s", want, rows)
			}
		}
	})
	t.Run("a file at a review path stays held", func(t *testing.T) {
		r := newADOPushRun(t, adoReview(), true)
		body := r.record(t, map[string]string{".github/workflows/ci.yml": "on: changed\n"})
		r.wantHeld(t, postEntraPush(t, r.p, body), []string{adoTouches, ".github/workflows/ci.yml",
			"remove these paths from the push, or run the task interactively so an admin can approve it"}, adoWorkedOut)
	})
	t.Run("a directory moved onto a review path, which the pack does not carry, stays held", func(t *testing.T) {
		r := newADOPushRun(t, adoReview(), true)
		runGit(t, r.work, "mv", "lib", ".github/workflows/lib")
		body := r.record(t, nil)
		r.wantHeld(t, postEntraPush(t, r.p, body), []string{adoTouches, ".github/workflows/lib", whyChanged}, adoWorkedOut)
	})
	// Pins the shipped behaviour, the same on GitHub: a deletion is no rule match, and
	// the forge's own branch policy protects the file. #3928 tracks holding it, and a
	// fix for that changes this row knowingly.
	t.Run("deleting a file at a review path beside a sibling is forwarded", func(t *testing.T) {
		r := newADOPushRun(t, adoReview(), true)
		runGit(t, r.work, "rm", "-q", ".github/workflows/ci.yml")
		r.wantPassed(t, postEntraPush(t, r.p, r.record(t, nil)))
		r.finish(t)
	})
	t.Run("a forward move of the run's branch passes", func(t *testing.T) {
		r := newADOPushRun(t, adoReview(), true)
		r.record(t, map[string]string{"lib/b.txt": "b\n"})
		r.snapshot(t)
		body := r.record(t, map[string]string{"lib/c.txt": "c\n"})
		r.wantPassed(t, postEntraPush(t, r.p, body))
		r.finish(t)
	})
	t.Run("a deny rule is cleared the same way, and still refuses a match", func(t *testing.T) {
		r := newADOPushRun(t, types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}}, true)
		r.wantPassed(t, postEntraPush(t, r.p, r.record(t, map[string]string{"lib/b.txt": "b\n"})))
		r = newADOPushRun(t, types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}}, true)
		if rec := postEntraPush(t, r.p, r.record(t, map[string]string{".github/workflows/x.yml": "x\n"})); rec.Code != http.StatusForbidden || r.rest.packs.Load() != 0 {
			t.Errorf("status %d, %d pack(s), want refused", rec.Code, r.rest.packs.Load())
		}
	})
}

// TestADOPushThroughTheBroker is the reported push end to end: a real git client
// on the real rewrite, creating a branch that changes nothing a review pattern
// could match, on an unattended run, lands. The credential stays in the proxy.
func TestADOPushThroughTheBroker(t *testing.T) {
	newRun := func(t *testing.T) (*adoGitHarness, *adoREST) {
		t.Helper()
		rest := &adoREST{}
		h := newADOGitHarnessFront(t, func(p *Proxy, _ string) {
			p.policy = CompilePolicy(adoReview())
			p.pushHolds.unattended = true
		}, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/_apis/") {
					rest.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r)
			})
		}, adoscope.CapCodeRead, adoscope.CapCodeWrite)
		rest.dir.Store(&h.bare)
		auth := "Bearer " + h.bearer
		rest.wantAuth.Store(&auth)
		return h, rest
	}
	t.Run("a push the REST API clears lands", func(t *testing.T) {
		h, rest := newRun(t)
		dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
		if out, err := h.push(t, dir, h.runBranch()); err != nil {
			t.Fatalf("push: %v\n%s", err, out)
		}
		if rest.reads.Load() == 0 {
			t.Error("the broker read nothing, so nothing vouched for this push")
		}
		if s := rest.strayed.Load(); s != nil {
			t.Errorf("read outside the push's repository: %s", *s)
		}
		if s := rest.badAuth.Load(); s != nil {
			t.Errorf("a read carried Authorization %q", *s)
		}
		if log := h.finish(t); !strings.Contains(log, ruleSourceGitForgeRead) {
			t.Errorf("no %s row:\n%s", ruleSourceGitForgeRead, log)
		}
	})
	t.Run("git prints what to do about a real match on an unattended run", func(t *testing.T) {
		h, _ := newRun(t)
		dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
		if out, err := h.git(t, "-C", dir, "checkout", "-B", h.runBranch()); err != nil {
			t.Fatalf("checkout: %v\n%s", err, out)
		}
		writeFiles(t, dir, map[string]string{".github/workflows/x.yml": "x\n"})
		if out, err := h.git(t, "-C", dir, "add", "-A"); err != nil {
			t.Fatalf("add: %v\n%s", err, out)
		}
		if out, err := h.git(t, "-C", dir, "commit", "-qm", "workflow"); err != nil {
			t.Fatalf("commit: %v\n%s", err, out)
		}
		out, err := h.git(t, "-C", dir, "push", "origin", h.runBranch())
		for _, want := range []string{"remote rejected", adoTouches, "run the task interactively so an admin can approve it"} {
			mustBeGitRefusal(t, out, err, want)
		}
		if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
			t.Errorf("Azure DevOps saw %d pack uploads, want none", n)
		}
	})
	t.Run("git prints why a push it could not clear is refused", func(t *testing.T) {
		h, rest := newRun(t)
		fail := func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusInternalServerError, `{}`)
		}
		rest.hook.Store(&fail)
		dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
		out, err := h.push(t, dir, h.runBranch())
		for _, want := range []string{"remote rejected", adoWorkedOut, whyUnread, "this run is unattended"} {
			mustBeGitRefusal(t, out, err, want)
		}
		if strings.Contains(out, adoTouches) {
			t.Errorf("git printed %q:\n%s", adoTouches, out)
		}
		if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
			t.Errorf("Azure DevOps saw %d pack uploads, want none", n)
		}
		if log := h.finish(t); !strings.Contains(log, ruleSourceGitPushHeldUnattended) {
			t.Errorf("no %s row:\n%s", ruleSourceGitPushHeldUnattended, log)
		}
	})
}

func writeRawJSON(w http.ResponseWriter, status int, body string) bool {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	return true
}

// TestADOPushReviewPathFailsClosed: every way the REST API can fall short of a
// whole answer leaves the push held, and the refusal says the paths could not
// be worked out rather than that the push touches them.
func TestADOPushReviewPathFailsClosed(t *testing.T) {
	onTrees := func(do func(http.ResponseWriter, *http.Request) bool) func(http.ResponseWriter, *http.Request) bool {
		return func(w http.ResponseWriter, r *http.Request) bool {
			return strings.Contains(r.URL.Path, "/trees/") && do(w, r)
		}
	}
	tooBig := strings.Repeat(" ", maxForgeBody+1) + "{}"
	for _, tc := range []struct {
		name  string
		hook  func(http.ResponseWriter, *http.Request) bool
		setup func(t *testing.T, r *adoPushRun)
		why   string
	}{
		{name: "a tree read answers 500", why: "answered 500", hook: onTrees(func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusInternalServerError, `{}`)
		})},
		{name: "a tree read answers 401", why: "answered 401", hook: onTrees(func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusUnauthorized, `{}`)
		})},
		{name: "the diff answers 500", why: whyUnread, hook: func(w http.ResponseWriter, r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/diffs/commits") && writeRawJSON(w, http.StatusInternalServerError, `{}`)
		}},
		{name: "a sign-in page answers 203", why: "answered 203", hook: func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusNonAuthoritativeInfo, `<html>sign in</html>`)
		}},
		{name: "a sign-in page answers 200", why: whyUnread, hook: onTrees(func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusOK, `<html>sign in</html>`)
		})},
		{name: "a listing is paged", why: "only part of a listing", hook: onTrees(func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("x-ms-continuationtoken", "more")
			return writeRawJSON(w, http.StatusOK, `{"objectId":"`+strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/"):], "/")+`","treeEntries":[]}`)
		})},
		{name: "a listing is cut off mid-body", why: whyUnread, hook: onTrees(func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusOK, `{"objectId":"abc","treeEntries":[{"relative`)
		})},
		{name: "a listing names another tree", why: "did not list tree", hook: onTrees(func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusOK, `{"objectId":"abc","treeEntries":[]}`)
		})},
		{name: "a listing is larger than the broker reads", why: "exceeded", hook: onTrees(func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusOK, tooBig)
		})},
		{name: "the base commit names no tree", why: "named no tree", hook: func(w http.ResponseWriter, r *http.Request) bool {
			return strings.Contains(r.URL.Path, "/commits/") && writeRawJSON(w, http.StatusOK, `{}`)
		}},
		{name: "the server knows no such commit (404)", why: whyNoBase, hook: func(w http.ResponseWriter, r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/diffs/commits") && writeRawJSON(w, http.StatusNotFound, `{}`)
		}},
		{name: "the server knows no such commit (400)", why: whyNoBase, hook: func(w http.ResponseWriter, r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/diffs/commits") && writeRawJSON(w, http.StatusBadRequest, `{}`)
		}},
		{name: "the base is outside the default branch", why: whyNoBase, hook: func(w http.ResponseWriter, r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/diffs/commits") && writeRawJSON(w, http.StatusOK, `{"commonCommit":"`+strings.Repeat("a", 40)+`"}`)
		}},
		{name: "the read budget runs out", why: "more than 1 reads", setup: func(t *testing.T, _ *adoPushRun) {
			prev := maxForgeReads
			maxForgeReads = 1
			t.Cleanup(func() { maxForgeReads = prev })
		}},
		{name: "the forge does not answer in time", why: whyUnread, setup: func(t *testing.T, _ *adoPushRun) {
			prev := forgeReadWait
			forgeReadWait = 100 * time.Millisecond
			t.Cleanup(func() { forgeReadWait = prev })
		}, hook: func(_ http.ResponseWriter, r *http.Request) bool {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return false
		}},
		{name: "the credential cannot be resolved", why: "no Azure DevOps credential", setup: func(_ *testing.T, r *adoPushRun) {
			delete(r.p.inject.byHost, "dev.azure.com")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newADOPushRun(t, adoReview(), true)
			if tc.setup != nil {
				tc.setup(t, r)
			}
			if tc.hook != nil {
				r.rest.hook.Store(&tc.hook)
			}
			body := r.record(t, map[string]string{"lib/b.txt": "b\n"})
			r.wantHeld(t, postEntraPush(t, r.p, body), []string{adoWorkedOut, ".github/", tc.why}, adoTouches)
		})
	}

	t.Run("a force-push stays unknown though its base is in the default branch", func(t *testing.T) {
		r := newADOPushRun(t, adoReview(), true)
		r.record(t, map[string]string{"lib/b.txt": "b\n"})
		r.snapshot(t)
		writeFiles(t, r.work, map[string]string{"lib/b.txt": "rewritten\n"})
		runGit(t, r.work, "add", "-A")
		runGit(t, r.work, "commit", "-q", "--amend", "-m", "rewritten")
		r.pushHead(t, "--force")
		r.wantHeld(t, postEntraPush(t, r.p, r.f.pushBody()), []string{adoWorkedOut, whyForced}, adoTouches)
	})

	t.Run("an attended push asks, and its refusal says the same", func(t *testing.T) {
		shortPushHolds(t)
		r := newADOPushRun(t, adoReview(), false)
		newPushCP(t, types.ApprovalDenied).wire(t, r.p)
		fail := func(w http.ResponseWriter, _ *http.Request) bool {
			return writeRawJSON(w, http.StatusInternalServerError, `{}`)
		}
		r.rest.hook.Store(&fail)
		body := r.record(t, map[string]string{"lib/b.txt": "b\n"})
		rec := postEntraPush(t, r.p, body)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "whose changed paths could not be worked out") {
			t.Fatalf("status %d, want the denial to say the paths could not be worked out:\n%s", rec.Code, rec.Body)
		}
	})
}

// TestADOPushWithNoObjects: a push carrying nothing adds no content only when
// the forge holds every commit it sets and the branch moves forward.
func TestADOPushWithNoObjects(t *testing.T) {
	r := newADOPushRun(t, adoReview(), true)
	first := gitOut(t, r.work, "rev-parse", "HEAD")
	writeFiles(t, r.work, map[string]string{"lib/b.txt": "b\n"})
	commitAll(t, r.work)
	second := gitOut(t, r.work, "rev-parse", "HEAD")
	runGit(t, r.work, "push", "-q", r.bare, "main")
	r.snapshot(t)
	zero := strings.Repeat("0", 40)
	unknown := strings.Repeat("c", 40)
	dir := *r.rest.dir.Load()
	runGit(t, dir, "update-ref", "refs/heads/feature", second)

	const rest = "/acme/proj/_git/app/git-receive-pack"
	grant := ADOGrant{Organization: "acme"}
	for _, tc := range []struct {
		name string
		cmd  gitpack.Command
		want string
	}{
		{"a new branch at a held commit", gitpack.Command{Ref: "refs/heads/new", Old: zero, New: first}, ""},
		{"a held branch moved forward", gitpack.Command{Ref: "refs/heads/feature", Old: first, New: second}, ""},
		{"a held branch moved backward", gitpack.Command{Ref: "refs/heads/feature", Old: second, New: first}, whyHeldForced},
		{"a commit the server does not know", gitpack.Command{Ref: "refs/heads/new", Old: zero, New: unknown}, whyNotHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.p.adoEntraForge("dev.azure.com", rest, grant).held(context.Background(), []gitpack.Command{tc.cmd})
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("held = %q, want %q", got, tc.want)
			}
		})
	}
	r.finish(t)
}

func TestADORepoAPIBase(t *testing.T) {
	for _, tc := range []struct{ host, rest, org, want string }{
		{"dev.azure.com", "/acme/proj/_git/app/git-receive-pack", "acme", "/acme/proj/_apis/git/repositories/app"},
		{"dev.azure.com", "/acme/_git/app/git-receive-pack", "acme", "/acme/app/_apis/git/repositories/app"},
		{"dev.azure.com", "/acme/My Project/_git/My Repo/git-receive-pack", "acme", "/acme/My%20Project/_apis/git/repositories/My%20Repo"},
		{"acme.visualstudio.com", "/proj/_git/app/git-receive-pack", "acme", "/proj/_apis/git/repositories/app"},
		{"acme.visualstudio.com", "/_git/app/git-receive-pack", "acme", "/app/_apis/git/repositories/app"},
		{"acme.visualstudio.com", "/DefaultCollection/proj/_git/app/git-receive-pack", "acme", "/DefaultCollection/proj/_apis/git/repositories/app"},
		{"tfs.example.com", "/tfs/coll/proj/_git/app/git-receive-pack", "tfs/coll", "/tfs/coll/proj/_apis/git/repositories/app"},
		{"tfs.example.com", "/tfs/coll/_git/app/git-receive-pack", "tfs/coll", "/tfs/coll/app/_apis/git/repositories/app"},
		{"dev.azure.com", "/acme/proj/team/_git/app/git-receive-pack", "acme", ""},
		{"dev.azure.com", "/_git/app/git-receive-pack", "acme", ""},
		{"dev.azure.com", "/acme/proj/_git/_full/app/git-receive-pack", "acme", ""},
		{"dev.azure.com", "/acme/_apis/_git/app/git-receive-pack", "acme", ""},
		{"dev.azure.com", "/acme//proj/_git/app/git-receive-pack", "acme", ""},
		{"dev.azure.com", "/acme/proj/_git/app/info/refs", "acme", ""},
		{"dev.azure.com", "/acme/proj/app/git-receive-pack", "acme", ""},
	} {
		got, ok := adoRepoAPIBase(tc.host, tc.rest, tc.org)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("adoRepoAPIBase(%q, %q, %q) = %q, %v; want %q", tc.host, tc.rest, tc.org, got, ok, tc.want)
		}
	}
}

// TestPATForgeReadsAzureDevOps: the git_pat lane's reader for Azure DevOps
// Services names the same repository; a forge with no reader still has none.
func TestPATForgeReadsAzureDevOps(t *testing.T) {
	p := newProxy(Options{Policy: CompilePolicy(types.RunPolicySpec{}), Sink: &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 1)}})
	f := p.patForge("dev.azure.com", "/acme/proj/_git/app/git-receive-pack", PATGrant{})
	if f == nil || f.ado == nil || f.ado.base != adoAPIPrefix {
		t.Fatalf("patForge = %+v, want the Azure DevOps reader at %s", f, adoAPIPrefix)
	}
	if f := p.patForge("gitlab.com", "/group/app/git-receive-pack", PATGrant{}); f != nil {
		t.Errorf("patForge on another forge = %+v, want none", f)
	}
}

func TestRefusalLineIsOneBoundedLine(t *testing.T) {
	paths := []string{strings.Repeat("a", 4096) + "/"}
	line := refusalLine("headline", "why\nsecond line", "remedy", paths)
	if strings.ContainsAny(line, "\r\n") || len(line) > maxRefusalLine {
		t.Errorf("refusal line of %d bytes holds a line break or runs over %d:\n%s", len(line), maxRefusalLine, line)
	}
	if !strings.Contains(line, "why second line. remedy.") {
		t.Errorf("the cap cut the reason:\n%s", line)
	}
}

// TestADOClearsRestoresEachRuleToStrict: with its switch off, a rule refuses what
// the pack does not carry on Azure DevOps, whatever the REST API would have shown.
func TestADOClearsRestoresEachRuleToStrict(t *testing.T) {
	const pattern = ".github/workflows/**"
	for _, tc := range []struct {
		name string
		off  func()
		spec types.PushRulesSpec
		want string
	}{
		{"require_review_paths", func() { adoClears.reviewPaths = false }, types.PushRulesSpec{RequireReviewPaths: []string{pattern}, HoldSeconds: 1}, whyNoForgeReader},
		{"deny_paths", func() { adoClears.denyPaths = false }, types.PushRulesSpec{DenyPaths: []string{pattern}}, whyNoForgeReader},
		{"max_file_size_mib", func() { adoClears.maxFileSize = false }, types.PushRulesSpec{MaxFileSizeMiB: 1}, whyNoForgeReader},
		{"deny_new_executables", func() { adoClears.newExecutables = false }, types.PushRulesSpec{DenyNewExecutables: true}, whyNoForgeReader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			on := newADOPushRun(t, types.RunPolicySpec{PushRules: &spec}, true)
			on.wantPassed(t, postEntraPush(t, on.p, on.record(t, map[string]string{"lib/b.txt": "b\n"})))

			prev := adoClears
			t.Cleanup(func() { adoClears = prev })
			tc.off()
			off := newADOPushRun(t, types.RunPolicySpec{PushRules: &spec}, true)
			rec := postEntraPush(t, off.p, off.record(t, map[string]string{"lib/b.txt": "b\n"}))
			if off.rest.packs.Load() != 0 || off.rest.reads.Load() != 0 || !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("%d pack(s), %d read(s), want none, refused for %q:\n%s", off.rest.packs.Load(), off.rest.reads.Load(), tc.want, rec.Body)
			}
		})
	}
	t.Run("a push with no objects", func(t *testing.T) {
		prev := adoClears
		t.Cleanup(func() { adoClears = prev })
		adoClears.noObjects = false
		r := newADOPushRun(t, adoReview(), true)
		tip := gitOut(t, r.work, "rev-parse", "HEAD")
		f := r.p.adoEntraForge("dev.azure.com", "/acme/proj/_git/app/git-receive-pack", ADOGrant{Organization: "acme"})
		cmds := []gitpack.Command{{Ref: "refs/heads/new", Old: strings.Repeat("0", 40), New: tip}}
		if got := f.forRule(adoClears.noObjects).held(context.Background(), cmds); got != whyHeldNoForgeReader {
			t.Errorf("held = %q, want %q", got, whyHeldNoForgeReader)
		}
	})
}

// TestADOPushForwardIsProvedPerRef: a ref moves forward only when the history the
// pack carries for that ref's own new commit names the tip it states, and only a
// base that some ref's history names vouches for what the push leaves out. A
// commit no ref in the push sets, or one another ref sets, proves neither. The
// branches stand at a tip that changed a review path, as a reviewed push would.
func TestADOPushForwardIsProvedPerRef(t *testing.T) {
	type tips struct{ a, b, other string }
	for _, tc := range []struct {
		name string
		// push commits in r.work and returns the commands (old, new, ref) and the
		// revs whose objects the pack carries.
		push     func(t *testing.T, r *adoPushRun, g tips) ([][3]string, string)
		wants    []string // nil: the push is forwarded
		unwanted string
	}{
		{"a commit no ref sets, built on the tip, does not make a move forward", func(t *testing.T, r *adoPushRun, g tips) ([][3]string, string) {
			c, d := adoCommitOn(t, r, g.a, "", "lib/c.txt"), adoCommitOn(t, r, g.b, "", "lib/d.txt")
			return [][3]string{{g.b, c, r.ref}}, c + "\n" + d + "\n^" + g.b
		}, []string{adoWorkedOut, whyForced}, adoTouches},
		{"a commit another ref sets does not make this ref's move forward", func(t *testing.T, r *adoPushRun, g tips) ([][3]string, string) {
			c, d := adoCommitOn(t, r, g.a, "", "lib/c.txt"), adoCommitOn(t, r, g.b, "", "lib/d.txt")
			return [][3]string{{g.b, c, r.ref}, {g.b, d, g.other}}, c + "\n" + d + "\n^" + g.b
		}, []string{adoWorkedOut, whyForced}, adoTouches},
		{"a commit no ref sets vouches for nothing the push leaves out", func(t *testing.T, r *adoPushRun, g tips) ([][3]string, string) {
			c, d := adoCommitOn(t, r, g.b, g.a, "lib/c.txt"), adoCommitOn(t, r, g.a, "", "lib/d.txt")
			return [][3]string{{g.b, c, r.ref}}, c + "\n" + d + "\n^" + g.b
		}, []string{adoTouches, ".github/", whyChanged}, adoWorkedOut},
		{"a forward move passes", func(t *testing.T, r *adoPushRun, g tips) ([][3]string, string) {
			c := adoCommitOn(t, r, g.b, "", "lib/c.txt")
			return [][3]string{{g.b, c, r.ref}}, c + "\n^" + g.b
		}, nil, ""},
		{"two refs each moved forward pass", func(t *testing.T, r *adoPushRun, g tips) ([][3]string, string) {
			c, d := adoCommitOn(t, r, g.b, "", "lib/c.txt"), adoCommitOn(t, r, g.b, "", "lib/d.txt")
			return [][3]string{{g.b, c, r.ref}, {g.b, d, g.other}}, c + "\n" + d + "\n^" + g.b
		}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newADOPushRun(t, adoReview(), true)
			g := tips{a: gitOut(t, r.work, "rev-parse", "HEAD"), other: BranchNSPrefix(r.p.runID) + "other"}
			r.record(t, map[string]string{".github/workflows/ci.yml": "on: reviewed\n"})
			g.b = gitOut(t, r.work, "rev-parse", "HEAD")
			runGit(t, r.work, "push", "-q", r.bare, "HEAD:"+g.other)
			r.snapshot(t)
			cmds, revs := tc.push(t, r, g)
			rec := postEntraPush(t, r.p, adoHandPush(t, r, cmds, revs))
			if tc.wants == nil {
				r.wantPassed(t, rec)
				r.finish(t)
				return
			}
			r.wantHeld(t, rec, tc.wants, tc.unwanted)
		})
	}
}

// adoCommitOn commits one new file on parent in r.work, with .github as from
// holds it when from is set, and returns the commit.
func adoCommitOn(t *testing.T, r *adoPushRun, parent, from, file string) string {
	t.Helper()
	runGit(t, r.work, "checkout", "-q", "--detach", parent)
	if from != "" {
		runGit(t, r.work, "checkout", "-q", from, "--", ".github")
	}
	writeFiles(t, r.work, map[string]string{file: file + "\n"})
	commitAll(t, r.work)
	return gitOut(t, r.work, "rev-parse", "HEAD")
}

// adoHandPush is a receive-pack request with cmds (old, new, ref) and a pack of
// the objects revs selects: a client the sandbox owns chooses both.
func adoHandPush(t *testing.T, r *adoPushRun, cmds [][3]string, revs string) []byte {
	t.Helper()
	pk := exec.Command("git", "pack-objects", "--revs", "--stdout")
	pk.Dir, pk.Env, pk.Stdin = r.work, gitEnv(), strings.NewReader(revs+"\n")
	pack, err := pk.Output()
	if err != nil {
		t.Fatal(err)
	}
	var req strings.Builder
	for i, c := range cmds {
		line := c[0] + " " + c[1] + " " + c[2]
		if i == 0 {
			line += "\x00report-status object-format=sha1"
		}
		req.WriteString(pkt(line + "\n"))
	}
	return append([]byte(req.String()+"0000"), pack...)
}

// A walk cut short proves nothing more, so every move reads as not forward.
func TestFastForwardsReadsACutShortWalkAsNotForward(t *testing.T) {
	cmds := []gitpack.Command{{Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40), Ref: "refs/heads/x"}}
	named := []map[string]bool{{cmds[0].Old: true, cmds[0].New: true}}
	if !fastForwards(cmds, named, true) || fastForwards(cmds, named, false) {
		t.Fatal("fastForwards: want forward with a whole walk and not forward with one cut short")
	}
}

// A forward push whose walk was cut short is refused for that reason, not as a move other than forward.
func TestADOPushCutShortWalkSaysSo(t *testing.T) {
	prev := packReaches
	t.Cleanup(func() { packReaches = prev })
	packReaches = func(res gitpack.Result) ([]map[string]bool, bool) {
		named, _ := res.Reaches()
		return named, false
	}
	r := newADOPushRun(t, adoReview(), true)
	r.record(t, map[string]string{"lib/b.txt": "b\n"})
	r.snapshot(t)
	rec := postEntraPush(t, r.p, r.record(t, map[string]string{"lib/c.txt": "c\n"}))
	r.wantHeld(t, rec, []string{adoWorkedOut, whyCutShort}, "other than forward")
}

// TestADOPushKeepsWhatTheBranchTipHeld: a push that moves the run's branch must
// leave it holding, at a review path, what the branch's tip holds there. A merge
// naming an older commit of the branch as a parent, or a second branch the same
// push creates on one, no longer brings that commit's version back without a
// hold. Each push is a stock git client's. Building on an older commit and then
// merging the tip back in leaves the tip's version, and passes.
func TestADOPushKeepsWhatTheBranchTipHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		// push builds on the branch's tip b over the older commit a and returns what git sent.
		push     func(t *testing.T, r *adoPushRun, a, b string) []byte
		wants    []string // nil: the push is forwarded
		unwanted string
		// broken, when set, names the tree whose listing the forge fails.
		broken func(t *testing.T, r *adoPushRun, a string) string
	}{
		{"a merge whose other parent is an older commit of the branch is held", func(t *testing.T, r *adoPushRun, a, b string) []byte {
			return r.gitPush(t, adoMergeWith(t, r, b, a)+":"+r.ref)
		}, []string{adoTouches, ".github/workflows/ci.yml", whyTipChanged}, adoWorkedOut, nil},
		{"a second branch the same push creates on an older commit is held", func(t *testing.T, r *adoPushRun, a, b string) []byte {
			c, d := adoCommitOn(t, r, b, a, "lib/c.txt"), adoCommitOn(t, r, a, "", "lib/d.txt")
			return r.gitPush(t, c+":"+r.ref, d+":"+BranchNSPrefix(r.p.runID)+"older")
		}, []string{adoTouches, ".github/workflows/ci.yml", whyTipChanged}, adoWorkedOut, nil},
		{"a comparison the forge cannot finish holds the push", func(t *testing.T, r *adoPushRun, a, b string) []byte {
			return r.gitPush(t, adoMergeWith(t, r, b, a)+":"+r.ref)
		}, []string{adoWorkedOut, ".github/", whyTipUnread}, adoTouches, func(t *testing.T, r *adoPushRun, a string) string {
			return gitOut(t, r.work, "rev-parse", a+":.github")
		}},
		{"a push stating a tip the forge does not show is held", func(t *testing.T, r *adoPushRun, a, _ string) []byte {
			c := adoCommitOn(t, r, a, "", "lib/c.txt")
			return adoHandPush(t, r, [][3]string{{a, c, r.ref}}, c+"\n^"+a)
		}, []string{adoWorkedOut, ".github/", whyNoTip}, adoTouches, nil},
		{"a commit on the tip passes", func(t *testing.T, r *adoPushRun, _, b string) []byte {
			return r.gitPush(t, adoCommitOn(t, r, b, "", "lib/c.txt")+":"+r.ref)
		}, nil, "", nil},
		{"a commit on an older base, merged with the tip, passes", func(t *testing.T, r *adoPushRun, a, b string) []byte {
			adoCommitOn(t, r, a, "", "lib/e.txt")
			runGit(t, r.work, "merge", "-q", "--no-edit", b)
			return r.gitPush(t, "HEAD:"+r.ref)
		}, nil, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newADOPushRun(t, adoReview(), true)
			a := gitOut(t, r.work, "rev-parse", "HEAD")
			r.record(t, map[string]string{".github/workflows/ci.yml": "on: reviewed\n"})
			b := gitOut(t, r.work, "rev-parse", "HEAD")
			r.snapshot(t)
			if tc.broken != nil {
				tree := "/trees/" + tc.broken(t, r, a)
				fail := func(w http.ResponseWriter, req *http.Request) bool {
					if !strings.HasSuffix(req.URL.Path, tree) {
						return false
					}
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return true
				}
				r.rest.hook.Store(&fail)
			}
			rec := postEntraPush(t, r.p, tc.push(t, r, a, b))
			if tc.wants == nil {
				r.wantPassed(t, rec)
				r.finish(t)
				return
			}
			r.wantHeld(t, rec, tc.wants, tc.unwanted)
		})
	}
}

// gitPush pushes refspecs with a stock git client, no deltas as pushHead, and
// returns the bytes it sent.
func (r *adoPushRun) gitPush(t *testing.T, refspecs ...string) []byte {
	t.Helper()
	runGit(t, r.work, append([]string{"-c", "pack.window=0", "push", "-q", r.f.srv.URL + "/octocat/hello-world.git"}, refspecs...)...)
	return r.f.pushBody()
}

// adoMergeWith commits, with git commit-tree, a merge of the branch's tip and an
// older commit whose tree takes .github from the older one, as a person
// resolving such a merge by hand would, and returns it.
func adoMergeWith(t *testing.T, r *adoPushRun, tip, older string) string {
	t.Helper()
	runGit(t, r.work, "checkout", "-q", "--detach", tip)
	runGit(t, r.work, "checkout", "-q", older, "--", ".github")
	writeFiles(t, r.work, map[string]string{"lib/m.txt": "m\n"})
	runGit(t, r.work, "add", "-A")
	return gitOut(t, r.work, "commit-tree", gitOut(t, r.work, "write-tree"), "-p", tip, "-p", older, "-m", "merge")
}
