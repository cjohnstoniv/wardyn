// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"compress/gzip"
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

// realCaps is what git 2.43's receive-pack actually advertises — captured from
// `git receive-pack --advertise-refs`. Note what is NOT in it: no-thin. That
// absence is the whole reason this file exists.
const realCaps = "report-status report-status-v2 delete-refs side-band-64k quiet atomic ofs-delta object-format=sha1 agent=git/2.43.0"

// advert builds a smart-HTTP receive-pack reference advertisement whose first
// ref line carries caps.
func advert(caps string) string {
	return pkt("# service=git-receive-pack\n") + "0000" +
		pkt("14a62c227c53756c428e3ac67906280a7bf018da refs/heads/main\x00"+caps+"\n") +
		pkt("6a1b2c3d4e5f60718293a4b5c6d7e8f901234567 refs/heads/topic\n") + "0000"
}

// advertResponse wraps an advertisement body as the upstream response
// relayNoThinAdvert is handed.
func advertResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/x-git-receive-pack-advertisement"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func relayed(t *testing.T, resp *http.Response) string {
	t.Helper()
	rec := httptest.NewRecorder()
	relayNoThinAdvert(rec, resp)
	return rec.Body.String()
}

// TestPushAdvertAddsNoThin pins the rewrite itself: no-thin joins the first ref
// line's capability list, the 4-hex length prefix is recomputed to match, and
// every other byte of the advertisement is left exactly where it was.
func TestPushAdvertAddsNoThin(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "real git advertisement",
			in:   advert(realCaps),
			want: advert(realCaps + " no-thin"),
		},
		{
			// The trailing LF is the pkt-line's, not the capability list's: git
			// chomps one before parsing, so appending after it would fold a
			// newline into the agent= token.
			name: "capability goes before the trailing newline",
			in:   pkt("# service=git-receive-pack\n") + "0000" + pkt("aa refs/heads/main\x00quiet agent=git/2.43.0\n") + "0000",
			want: pkt("# service=git-receive-pack\n") + "0000" + pkt("aa refs/heads/main\x00quiet agent=git/2.43.0 no-thin\n") + "0000",
		},
		{
			name: "ref line without a trailing newline",
			in:   pkt("# service=git-receive-pack\n") + "0000" + pkt("aa refs/heads/main\x00quiet") + "0000",
			want: pkt("# service=git-receive-pack\n") + "0000" + pkt("aa refs/heads/main\x00quiet no-thin") + "0000",
		},
		{
			name: "empty capability list gains no leading space",
			in:   pkt("# service=git-receive-pack\n") + "0000" + pkt("aa refs/heads/main\x00\n") + "0000",
			want: pkt("# service=git-receive-pack\n") + "0000" + pkt("aa refs/heads/main\x00no-thin\n") + "0000",
		},
		{
			// An empty repository advertises its capabilities on a synthetic
			// "capabilities^{}" line in the same position.
			name: "empty repository",
			in: pkt("# service=git-receive-pack\n") + "0000" +
				pkt("0000000000000000000000000000000000000000 capabilities^{}\x00"+realCaps+"\n") + "0000",
			want: pkt("# service=git-receive-pack\n") + "0000" +
				pkt("0000000000000000000000000000000000000000 capabilities^{}\x00"+realCaps+" no-thin\n") + "0000",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relayed(t, advertResponse(c.in)); got != c.want {
				t.Errorf("relayed advertisement\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestPushAdvertRelaysUnexpectedShapeVerbatim is the fail-safe half, and the
// branch that matters most: a corrupted reference advertisement breaks EVERY
// push through the broker, including runs with no content rules, while an
// un-rewritten one merely yields a thin pack the enforcement path refuses on
// its own terms. So anything that is not exactly the expected shape must come
// back byte for byte.
func TestPushAdvertRelaysUnexpectedShapeVerbatim(t *testing.T) {
	// A capability list long enough that adding " no-thin" would push the
	// pkt-line past git's 65520-byte ceiling.
	huge := pkt("aa refs/heads/main\x00" + strings.Repeat("c", 65520-len("aa refs/heads/main\x00")))

	cases := []struct{ name, in string }{
		{"empty body", ""},
		{"dumb http", "<html><body>not a git server</body></html>"},
		{"truncated length prefix", "00"},
		{"non-hex length prefix", "zzzz# service=git-receive-pack\n"},
		{"fetch service line", pkt("# service=git-upload-pack\n") + "0000" + pkt("aa refs/heads/main\x00"+realCaps+"\n") + "0000"},
		// A sandbox is free to set Git-Protocol: version=2, which the broker
		// forwards like any other header. v2 has no push, so a forge answering
		// a receive-pack discovery in it is answering something this cannot
		// read — and an unread advertisement is the safe one.
		{"protocol v2 capability advertisement", pkt("version 2\n") + pkt("agent=git/2.43.0\n") + pkt("ls-refs=unborn\n") + "0000"},
		{"service line without its newline", pkt("# service=git-receive-pack") + "0000" + pkt("aa refs/heads/main\x00"+realCaps+"\n") + "0000"},
		{"no flush after the service line", pkt("# service=git-receive-pack\n") + pkt("aa refs/heads/main\x00"+realCaps+"\n") + "0000"},
		{"delim-pkt where the flush belongs", pkt("# service=git-receive-pack\n") + "0001" + pkt("aa refs/heads/main\x00"+realCaps+"\n") + "0000"},
		{"advertisement ends after the flush", pkt("# service=git-receive-pack\n") + "0000"},
		{"second flush instead of a ref line", pkt("# service=git-receive-pack\n") + "0000" + "0000"},
		{"first ref line carries no capability list", pkt("# service=git-receive-pack\n") + "0000" + pkt("aa refs/heads/main\n") + "0000"},
		{"ref line truncated mid-payload", pkt("# service=git-receive-pack\n") + "0000" + "0040aa refs/heads/main\x00quiet"},
		{"no-thin already advertised", advert(realCaps + " no-thin")},
		{"no-thin already advertised first", advert("no-thin " + realCaps)},
		{"rewrite would exceed the pkt-line ceiling", pkt("# service=git-receive-pack\n") + "0000" + huge + "0000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relayed(t, advertResponse(c.in)); got != c.in {
				t.Errorf("advertisement was not relayed verbatim\n got %q\nwant %q", got, c.in)
			}
		})
	}

	// Same posture for a response this code has no business parsing at all.
	t.Run("non-200 response", func(t *testing.T) {
		resp := advertResponse("repository not found")
		resp.StatusCode = http.StatusNotFound
		rec := httptest.NewRecorder()
		relayNoThinAdvert(rec, resp)
		if rec.Code != http.StatusNotFound || rec.Body.String() != "repository not found" {
			t.Errorf("relayed %d %q, want 404 and the body verbatim", rec.Code, rec.Body.String())
		}
	})
	t.Run("body still in a content-coding", func(t *testing.T) {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte(advert(realCaps)))
		_ = zw.Close()
		resp := advertResponse(gz.String())
		resp.Header.Set("Content-Encoding", "gzip")
		if got := relayed(t, resp); got != gz.String() {
			t.Error("a gzipped advertisement was not relayed verbatim")
		}
	})
}

// advertUpstream is one TLS endpoint standing in for both the control-plane
// mint route and the forge's advertisement, recording the headers the forge
// saw so a test can prove what the broker stripped.
type advertUpstream struct {
	srv  *httptest.Server
	body string
	mu   sync.Mutex
	hdrs http.Header
}

func newAdvertUpstream(t *testing.T, body string) *advertUpstream {
	t.Helper()
	u := &advertUpstream{body: body}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"github_token","token":"gh-tok","jti":"j","expires_at":"`+exp+`"}`)
			return
		}
		u.mu.Lock()
		u.hdrs = r.Header.Clone()
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = io.WriteString(w, u.body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *advertUpstream) acceptEncoding() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hdrs.Get("Accept-Encoding")
}

// TestPushAdvertOnlyRewritesTheReceivePackAdvertisement walks the gate in
// handleGitBroker: which request is rewritten, which policy turns it on, and
// that the sandbox's Accept-Encoding is dropped only on the one request whose
// response has to be parsed.
func TestPushAdvertOnlyRewritesTheReceivePackAdvertisement(t *testing.T) {
	rules := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}}
	cases := []struct {
		name    string
		spec    types.RunPolicySpec
		service string
		want    bool // is no-thin advertised to the sandbox?
	}{
		{"content rules set", rules, "git-receive-pack", true},
		{"no content rules", types.RunPolicySpec{}, "git-receive-pack", false},
		// The fetch advertisement is a different conversation: a thin FETCH
		// pack is git deltifying against objects the client already has.
		{"fetch advertisement", rules, "git-upload-pack", false},
		// push_rules:{} carries no rule, so it reads as absent here exactly as
		// it does in the clamp and the risk grade (types.PushRulesSpec.IsSet).
		{"all-zero content rules read as absent", types.RunPolicySpec{PushRules: &types.PushRulesSpec{}}, "git-receive-pack", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newAdvertUpstream(t, advert(realCaps))
			p, _ := newGitBrokerProxyWithSpec(t,
				map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), c.spec)

			rec := httptest.NewRecorder()
			req := mustLocalReq(t, http.MethodGet,
				"/wardyn/gh/octocat/hello-world/info/refs?service="+c.service, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
			}
			if got := strings.Contains(rec.Body.String(), " no-thin"); got != c.want {
				t.Errorf("no-thin advertised = %v, want %v (body %q)", got, c.want, rec.Body.String())
			}
			if !c.want && rec.Body.String() != advert(realCaps) {
				t.Errorf("advertisement altered: %q", rec.Body.String())
			}
			// The sandbox's Accept-Encoding is replaced ONLY where the response
			// must be parsed; every other request keeps the client's own
			// negotiation.
			wantAE := "gzip"
			if c.want {
				wantAE = "identity"
			}
			if got := up.acceptEncoding(); got != wantAE {
				t.Errorf("forge saw Accept-Encoding %q, want %q", got, wantAE)
			}
		})
	}
}

// TestPushAdvertLeavesTheReceivePackPostAlone: only the advertisement is
// rewritten. The POST that follows it carries the pack, and a byte added to
// that response would corrupt the report-status the client is waiting for.
func TestPushAdvertLeavesTheReceivePackPostAlone(t *testing.T) {
	up := newAdvertUpstream(t, advert(realCaps))
	p, _ := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		types.RunPolicySpec{
			GitPushAnyBranch: true,
			PushRules:        &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
		})

	rec := httptest.NewRecorder()
	body := pkt("aa bb refs/heads/main\x00report-status\n") + "0000"
	req := mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack",
		strings.NewReader(body))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != advert(realCaps) {
		t.Errorf("receive-pack POST response was altered: %q", rec.Body.String())
	}
}

// gitForge is a real git server standing in for the forge: one TLS endpoint
// answering the control-plane mint route and `git {upload,receive}-pack` over
// smart HTTP, keeping the body of every receive-pack POST so a test can read
// the pack the client actually sent.
type gitForge struct {
	srv  *httptest.Server
	root string
	mu   sync.Mutex
	push []byte
}

func newGitForge(t *testing.T) *gitForge {
	t.Helper()
	f := &gitForge{root: t.TempDir()}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"github_token","token":"gh-tok","jti":"j","expires_at":"`+exp+`"}`)
			return
		}
		dir, service, isAdvert, ok := f.route(r)
		if !ok {
			http.Error(w, "no such git resource", http.StatusNotFound)
			return
		}
		if isAdvert {
			f.advertise(w, r, dir, service)
			return
		}
		f.rpc(w, r, dir, service)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// route maps /<org>/<repo>.git/<verb> onto a repository directory under the
// forge root and the git subcommand that serves it.
func (f *gitForge) route(r *http.Request) (dir, service string, isAdvert, ok bool) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	i := strings.Index(p, ".git/")
	if i < 0 {
		return "", "", false, false
	}
	dir = filepath.Join(f.root, filepath.FromSlash(p[:i])+".git")
	switch rest := p[i+len(".git/"):]; rest {
	case "info/refs":
		service = r.URL.Query().Get("service")
		return dir, service, true, service == "git-upload-pack" || service == "git-receive-pack"
	case "git-upload-pack", "git-receive-pack":
		return dir, rest, false, true
	}
	return "", "", false, false
}

func (f *gitForge) advertise(w http.ResponseWriter, r *http.Request, dir, service string) {
	refs, err := f.git(dir, strings.TrimPrefix(service, "git-"), "--advertise-refs", dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var body bytes.Buffer
	body.WriteString(pkt("# service=" + service + "\n"))
	body.WriteString("0000")
	body.Write(refs)
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	// Honour the client's gzip: the broker must strip the sandbox's own
	// Accept-Encoding to be able to read what it rewrites, and a forge that
	// always answered in plaintext would never test that.
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer func() { _ = zw.Close() }()
		_, _ = zw.Write(body.Bytes())
		return
	}
	_, _ = w.Write(body.Bytes())
}

func (f *gitForge) rpc(w http.ResponseWriter, r *http.Request, dir, service string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if service == "git-receive-pack" {
		f.mu.Lock()
		f.push = body
		f.mu.Unlock()
	}
	cmd := exec.Command("git", strings.TrimPrefix(service, "git-"), "--stateless-rpc", dir)
	cmd.Dir, cmd.Stdin, cmd.Env = dir, bytes.NewReader(body), gitEnv()
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, fmt.Sprintf("%s: %v", service, err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-result")
	_, _ = w.Write(out)
}

func (f *gitForge) git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, gitEnv()
	return cmd.Output()
}

func (f *gitForge) pushBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.push
}

// gitEnv pins identity and ignores the developer's own git configuration, so
// the test behaves the same on a workstation and in CI.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=@1767225600 +0000", "GIT_COMMITTER_DATE=@1767225600 +0000")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// bigFile is content large enough that git certainly prefers a delta over a
// fresh copy when one line of it changes — the shape a real edit takes.
func bigFile(lines int) string {
	var b strings.Builder
	for i := range lines {
		fmt.Fprintf(&b, "line %05d: the quick brown fox jumps over the lazy dog\n", i)
	}
	return b.String()
}

// TestPushAdvertEndToEndPackIsSelfContained is the claim the whole feature
// rests on, driven by a real `git push` through the broker into a real git
// server.
//
// The sandbox clones SHALLOW, exactly as the agent images do
// (deploy/images/common/agent-run-lib.sh), then edits a large file and pushes.
// Without no-thin the pack it sends carries that blob as a delta against a base
// object the pack does not contain, so the inspector cannot read the push at
// all — that control case is asserted here too, because a test that only proves
// the fixed path would pass just as well if git had never sent a thin pack.
func TestPushAdvertEndToEndPackIsSelfContained(t *testing.T) {
	selfContained := func(t *testing.T, contentRules bool) (gitpack.Result, error) {
		t.Helper()
		forge := newGitForge(t)
		bare := filepath.Join(forge.root, "octocat", "hello-world.git")
		if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, forge.root, "init", "-q", "--bare", "--initial-branch=main", bare)

		// Seed the forge with a history the sandbox will clone away from.
		seed := t.TempDir()
		runGit(t, seed, "init", "-q", "--initial-branch=main", seed)
		if err := os.WriteFile(filepath.Join(seed, "data.txt"), []byte(bigFile(4000)), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, seed, "add", "data.txt")
		runGit(t, seed, "commit", "-qm", "seed")
		runGit(t, seed, "push", "-q", bare, "HEAD:refs/heads/main")

		spec := types.RunPolicySpec{}
		if contentRules {
			spec.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}
		}
		p, _ := newGitBrokerProxyWithSpec(t,
			map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(forge.srv), spec)
		broker := httptest.NewServer(p)
		t.Cleanup(broker.Close)
		repoURL := broker.URL + "/wardyn/gh/octocat/hello-world"

		// The shallow clone is the premise: the base objects the next push
		// deltifies against stay on the forge.
		work := filepath.Join(t.TempDir(), "work")
		runGit(t, t.TempDir(), "clone", "-q", "--depth", "1", repoURL, work)

		edited := strings.Replace(bigFile(4000), "line 02000:", "line 02000! edited", 1)
		if err := os.WriteFile(filepath.Join(work, "data.txt"), []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "commit", "-qam", "edit one line")
		runGit(t, work, "push", "-q", "origin",
			"HEAD:"+BranchNSPrefix(p.runID)+"work")

		body := forge.pushBody()
		if len(body) == 0 {
			t.Fatal("the forge received no receive-pack request")
		}
		return gitpack.Inspect(body)
	}

	// The control: with no content rules the broker advertises nothing extra,
	// git sends a thin pack, and the inspector cannot resolve its bases.
	t.Run("without content rules the pack is thin", func(t *testing.T) {
		_, err := selfContained(t, false)
		if err == nil {
			t.Fatal("expected a thin pack the inspector cannot read; got a readable one — " +
				"if git stopped sending thin packs by default this test no longer proves anything")
		}
		if !strings.Contains(err.Error(), "not in the pack") {
			t.Fatalf("inspect error = %v, want a delta against a base absent from the pack", err)
		}
	})

	// The claim: with content rules set the advertisement carries no-thin, so
	// the client sends every base object it deltified against.
	t.Run("with content rules the pack is self-contained", func(t *testing.T) {
		res, err := selfContained(t, true)
		if err != nil {
			t.Fatalf("inspect the pushed pack: %v", err)
		}
		if len(res.Commands) != 1 || !strings.HasSuffix(res.Commands[0].Ref, "/work") {
			t.Errorf("commands = %+v, want the one namespaced ref update", res.Commands)
		}
		if len(res.Changes) != 1 || res.Changes[0].Path != "data.txt" {
			t.Errorf("changes = %+v, want the one edited path", res.Changes)
		}
	})
}
