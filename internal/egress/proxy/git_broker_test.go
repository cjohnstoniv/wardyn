// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gitBrokerUpstream is one TLS server standing in for BOTH the control-plane mint
// route and github.com — redirectDial funnels every dial to a single addr, so the
// server routes by path: /api/v1/internal/credentials/mint mints a token (counting
// calls, to prove the per-grant cache), everything else is the git-smart-HTTP
// upstream (capturing the injected Authorization + rewritten path).
type gitBrokerUpstream struct {
	srv       *httptest.Server
	mu        sync.Mutex
	mintCalls int
	gitAuth   string // Authorization the upstream github request carried
	gitPath   string
	gitQuery  string
	gitProto  string
	gitBody   []byte // body github received (proves byte-for-byte forwarding)
	gitHits   int
}

func newGitBrokerUpstream(t *testing.T, token string) *gitBrokerUpstream {
	t.Helper()
	u := &gitBrokerUpstream{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			u.mintCalls++
			exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"github_token","token":"`+token+`","username":"x-access-token","jti":"j","expires_at":"`+exp+`"}`)
			return
		}
		u.gitHits++
		u.gitAuth = r.Header.Get("Authorization")
		u.gitPath = r.URL.Path
		u.gitQuery = r.URL.RawQuery
		u.gitProto = r.Header.Get("Git-Protocol")
		u.gitBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "git-pack-data")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// newGitBrokerProxy builds a Proxy with the git-broker allowlist wired and both the
// control plane and github reachable at the single upstream (HTTPS so the mint
// forward and the github re-origination share one TLS server).
func newGitBrokerProxy(t *testing.T, grants map[string]uuid.UUID, upstreamAddr string) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            sink,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		GitGrants:       grants,
	})
	return p, buf
}

// TestGitBrokerClonesGrantedRepo: a granted repo's info/refs is re-originated to
// github with a Basic x-access-token:<token> auth (the sandbox's smuggled
// Authorization stripped), the path/query/Git-Protocol preserved, and the response
// streamed back. A following git-upload-pack reuses the cached token (one mint).
func TestGitBrokerClonesGrantedRepo(t *testing.T) {
	grantID := uuid.New()
	up := newGitBrokerUpstream(t, "gh-inst-token")
	// The allowlist key is the canonical lowercased "<org>/<repo>".
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": grantID}, upstreamAddr(up.srv))

	// info/refs (note mixed-case in the request path — github is case-insensitive).
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet,
		"/wardyn/gh/octocat/Hello-World.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	req.Header.Set("Git-Protocol", "version=2")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("info/refs status = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "git-pack-data" {
		t.Fatalf("info/refs body = %q, want streamed git-pack-data", rec.Body.String())
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:gh-inst-token"))
	if up.gitAuth != wantAuth {
		t.Fatalf("upstream Authorization = %q, want %q (Basic x-access-token; smuggled stripped)", up.gitAuth, wantAuth)
	}
	if up.gitAuth == "Bearer SANDBOX-SMUGGLED" {
		t.Fatal("sandbox-smuggled Authorization reached github")
	}
	if up.gitPath != "/octocat/hello-world.git/info/refs" {
		t.Fatalf("upstream path = %q", up.gitPath)
	}
	if up.gitQuery != "service=git-upload-pack" {
		t.Fatalf("upstream query = %q, want service preserved", up.gitQuery)
	}
	if up.gitProto != "version=2" {
		t.Fatalf("upstream Git-Protocol = %q, want version=2 preserved", up.gitProto)
	}

	// The following git-upload-pack POST must reuse the cached token — one mint for
	// the whole clone (mandatory for single-use approval-gated grants).
	rec2 := httptest.NewRecorder()
	req2 := mustLocalReq(t, http.MethodPost,
		"/wardyn/gh/octocat/Hello-World.git/git-upload-pack", strings.NewReader("0011command=fetch"))
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("git-upload-pack status = %d body=%q", rec2.Code, rec2.Body.String())
	}
	if up.mintCalls != 1 {
		t.Fatalf("mintCalls = %d, want 1 (token cached across info/refs + upload-pack)", up.mintCalls)
	}
}

// TestGitBrokerDeniesUngrantedRepo: a repo not in the allowlist is 403'd BEFORE any
// github URL is formed or token minted (repo is the unit of trust).
func TestGitBrokerDeniesUngrantedRepo(t *testing.T) {
	up := newGitBrokerUpstream(t, "unused")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet,
		"/wardyn/gh/attacker/evil.git/info/refs?service=git-upload-pack", nil)
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted repo status = %d, want 403", rec.Code)
	}
	if up.gitHits != 0 {
		t.Fatalf("github upstream was hit %d times for an ungranted repo", up.gitHits)
	}
	if up.mintCalls != 0 {
		t.Fatalf("mint was called %d times for an ungranted repo (must not mint)", up.mintCalls)
	}
}

// TestGitBrokerRejectsBadRequests: traversal / short / unknown-verb / bad-service
// requests never reach github, whether they 403 (matched-repo, bad rest) or 404
// (malformed path that doesn't even parse to a key).
func TestGitBrokerRejectsBadRequests(t *testing.T) {
	grants := map[string]uuid.UUID{"octocat/hello-world": uuid.New()}
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"traversal", http.MethodGet, "/wardyn/gh/octocat/../secret.git/info/refs?service=git-upload-pack"},
		{"too-short", http.MethodGet, "/wardyn/gh/onlyorg"},
		{"bad-service", http.MethodGet, "/wardyn/gh/octocat/Hello-World.git/info/refs?service=evil"},
		{"unknown-verb", http.MethodPost, "/wardyn/gh/octocat/Hello-World.git/git-evil-pack"},
		{"info-refs-wrong-method", http.MethodPost, "/wardyn/gh/octocat/Hello-World.git/info/refs?service=git-upload-pack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "unused")
			p, _ := newGitBrokerProxy(t, grants, upstreamAddr(up.srv))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.path, nil))
			if rec.Code == http.StatusOK {
				t.Fatalf("%s: status = 200, want a rejection", tc.name)
			}
			if up.gitHits != 0 {
				t.Fatalf("%s: github upstream was hit for a rejected request", tc.name)
			}
		})
	}
}

// ── push branch-namespace confinement ────────────────────────────────────────

// pkt frames one git pkt-line: 4 hex length digits that count themselves.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

const (
	zeroOID  = "0000000000000000000000000000000000000000"
	someOID  = "1111111111111111111111111111111111111111"
	otherOID = "2222222222222222222222222222222222222222"
	// firstCaps is the "\0<capability-list>" suffix git puts on the FIRST command.
	firstCaps = "\x00report-status side-band-64k agent=git/2.45.0\n"
)

// TestReceivePackCommandParser: the pkt-line command-section parser accepts ONLY
// ref updates inside the run's namespace and fails closed on everything else.
func TestReceivePackCommandParser(t *testing.T) {
	const prefix = "refs/heads/wardyn/run-1/"
	inNS := prefix + "feature"

	var oversized strings.Builder
	for i := 0; i < 600; i++ { // ~136 B/command => well past the 64 KiB cap
		oversized.WriteString(pkt(fmt.Sprintf("%s %s %sb%03d\n", someOID, otherOID, prefix, i)))
	}

	cases := []struct {
		name    string
		section string
		wantErr string // "" == must be accepted
	}{
		{"in-namespace", pkt(someOID+" "+otherOID+" "+inNS+firstCaps) + "0000", ""},
		{"caps-suffix-stripped-not-treated-as-ref",
			pkt(zeroOID+" "+otherOID+" "+inNS+"\x00report-status refs/heads/main\n") + "0000", ""},
		{"delete-inside-namespace", pkt(someOID+" "+zeroOID+" "+inNS+firstCaps) + "0000", ""},
		{"shallow-line-then-command",
			pkt("shallow "+someOID+"\n") + pkt(someOID+" "+otherOID+" "+inNS+firstCaps) + "0000", ""},
		{"multi-ref-all-inside",
			pkt(someOID+" "+otherOID+" "+prefix+"a"+firstCaps) + pkt(someOID+" "+otherOID+" "+prefix+"b\n") + "0000", ""},

		{"default-branch", pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000", "outside this run's branch namespace"},
		{"other-run-namespace", pkt(someOID+" "+otherOID+" refs/heads/wardyn/run-2/x"+firstCaps) + "0000", "outside this run's branch namespace"},
		{"tag", pkt(someOID+" "+otherOID+" refs/tags/v1"+firstCaps) + "0000", "outside this run's branch namespace"},
		{"pull-ref", pkt(someOID+" "+otherOID+" refs/pull/1/head"+firstCaps) + "0000", "outside this run's branch namespace"},
		{"namespace-root-has-no-branch", pkt(someOID+" "+otherOID+" "+prefix+firstCaps) + "0000", "outside this run's branch namespace"},
		{"multi-ref-mixed-rejects-whole-push",
			pkt(someOID+" "+otherOID+" "+inNS+firstCaps) + pkt(someOID+" "+otherOID+" refs/heads/main\n") + "0000",
			"outside this run's branch namespace"},
		{"delete-outside-namespace",
			pkt(someOID+" "+zeroOID+" refs/heads/main"+firstCaps) + "0000", "outside this run's branch namespace"},
		{"traversal-refname",
			pkt(someOID+" "+otherOID+" "+prefix+"../../heads/main"+firstCaps) + "0000", "malformed refname"},
		{"embedded-second-ref",
			pkt(someOID+" "+otherOID+" "+inNS+" refs/heads/main"+firstCaps) + "0000", "malformed refname"},
		{"push-cert", pkt("push-cert"+firstCaps) + "0000", "unsupported receive-pack command"},
		{"malformed-length", "zzzz" + "0000", "malformed pkt-line length"},
		{"delim-pkt", "0001" + "0000", "unexpected pkt-line length"},
		{"truncated-payload", "0040" + someOID, "truncated pkt-line"},
		{"no-flush-pkt", pkt(someOID + " " + otherOID + " " + inNS + firstCaps), "unreadable pkt-line length"},
		{"oversized-command-section", oversized.String() + "0000", "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, err := readReceivePackCommands(strings.NewReader(tc.section), prefix)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("readReceivePackCommands: unexpected error %v", err)
				}
				if string(head) != tc.section {
					t.Fatalf("buffered section = %q, want the input verbatim", head)
				}
				return
			}
			if err == nil {
				t.Fatalf("readReceivePackCommands: want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestReceivePackParserStopsAtFlushPkt: the parser consumes the command section
// and NOT one byte more — the packfile is left on the reader for streaming, and
// the buffered head + the remainder reassemble the original body byte-for-byte.
func TestReceivePackParserStopsAtFlushPkt(t *testing.T) {
	const prefix = "refs/heads/wardyn/run-1/"
	section := pkt(someOID+" "+otherOID+" "+prefix+"feature"+firstCaps) + "0000"
	pack := "PACK\x00\x02\x00\x00\x00\x01\xff\xfe binary bytes \x00\x00"

	body := strings.NewReader(section + pack)
	head, err := readReceivePackCommands(body, prefix)
	if err != nil {
		t.Fatalf("readReceivePackCommands: %v", err)
	}
	if string(head) != section {
		t.Fatalf("buffered head = %q, want the command section verbatim", head)
	}
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read remainder: %v", err)
	}
	if string(rest) != pack {
		t.Fatalf("remainder = %q, want the packfile untouched", rest)
	}
}

// TestBranchNSEnforcedEnv: the opt-in env parses loudly — off by default and on
// explicit disable words, on for enable words, and FAIL CLOSED (on) for garbage.
func TestBranchNSEnforcedEnv(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false}, {"off", false}, {"0", false}, {"false", false}, {"disabled", false},
		{"1", true}, {"true", true}, {"on", true}, {"enforce", true},
		{"maybe", true}, // garbage => enforce, never silently off
	} {
		t.Run("val="+tc.val, func(t *testing.T) {
			t.Setenv(envEnforceBranchNS, tc.val)
			if got := branchNSEnforced(); got != tc.want {
				t.Fatalf("branchNSEnforced(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestGitBrokerDeniesOutOfNamespacePush: with enforcement on, a push to a branch
// outside `wardyn/<run-id>/*` is 403'd BEFORE the token is minted and before any
// byte reaches github; audit gets a brokered:git:branch-ns deny row.
func TestGitBrokerDeniesOutOfNamespacePush(t *testing.T) {
	t.Setenv(envEnforceBranchNS, "1")
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	body := pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000" + "PACKDATA"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/gh/octocat/Hello-World.git/git-receive-pack", strings.NewReader(body)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "refs/heads/main") ||
		!strings.Contains(rec.Body.String(), "refs/heads/wardyn/"+p.runID.String()+"/") {
		t.Fatalf("body = %q, want it to name the offending ref and the allowed namespace", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gh-inst-token") {
		t.Fatal("the installation token leaked into the denial body")
	}
	if up.gitHits != 0 {
		t.Fatalf("github upstream was hit %d times for a denied push", up.gitHits)
	}
	if up.mintCalls != 0 {
		t.Fatalf("mint was called %d times for a denied push (deny must precede the mint)", up.mintCalls)
	}
	if !strings.Contains(sink.String(), ruleSourceGitRef) || !strings.Contains(sink.String(), `"decision":"deny"`) {
		t.Fatalf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitRef)
	}
}

// TestGitBrokerForwardsInNamespacePush: an in-namespace push is forwarded, and the
// buffered command section + streamed packfile arrive byte-for-byte identical.
func TestGitBrokerForwardsInNamespacePush(t *testing.T) {
	t.Setenv(envEnforceBranchNS, "on")
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	ref := "refs/heads/wardyn/" + p.runID.String() + "/feature"
	body := pkt(someOID+" "+otherOID+" "+ref+firstCaps) + "0000" +
		"PACK\x00\x02\x00\x00\x00\x01\xff\xfe\x00 binary"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/gh/octocat/Hello-World.git/git-receive-pack", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if string(up.gitBody) != body {
		t.Fatalf("upstream body = %q, want the request forwarded byte-for-byte (%q)", up.gitBody, body)
	}
}

// TestGitBrokerRejectsEncodedPushWhenEnforcing: a content-encoded push body cannot
// be ref-checked, so it is refused rather than waved through unparsed.
func TestGitBrokerRejectsEncodedPushWhenEnforcing(t *testing.T) {
	t.Setenv(envEnforceBranchNS, "1")
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	req := mustLocalReq(t, http.MethodPost,
		"/wardyn/gh/octocat/Hello-World.git/git-receive-pack", strings.NewReader("gzipped-bytes"))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if up.gitHits != 0 {
		t.Fatalf("github upstream was hit for an unparseable push body")
	}
}

// TestGitBrokerPushUnenforcedByDefault: confinement is OPT-IN — with the env unset
// an out-of-namespace push still forwards (fetch/clone are never touched at all).
// This pins the honest default; flip it when agent-run names branches itself.
func TestGitBrokerPushUnenforcedByDefault(t *testing.T) {
	t.Setenv(envEnforceBranchNS, "") // never inherit an operator's setting
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	body := pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000" + "PACKDATA"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/gh/octocat/Hello-World.git/git-receive-pack", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (enforcement is off by default)", rec.Code)
	}
	if string(up.gitBody) != body {
		t.Fatalf("upstream body = %q, want the unenforced push streamed through unchanged", up.gitBody)
	}
}
