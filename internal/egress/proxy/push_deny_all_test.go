// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// denyAllSpec refuses every push. git_push_any_branch is on so no branch rule
// refuses first: what refuses is deny_all, whatever the WHERE opt-out says.
func denyAllSpec() types.RunPolicySpec {
	return types.RunPolicySpec{GitPushAnyBranch: true, PushRules: &types.PushRulesSpec{DenyAll: true}}
}

// denyAllLane is one receive-pack lane: base is its broker path up to the
// verb, post sends one request there, and hits is how many requests reached
// the forge or minted a credential.
type denyAllLane struct {
	name string
	new  func(t *testing.T) (p *Proxy, sink *bytes.Buffer, base string, hits func() int)
}

func denyAllLanes() []denyAllLane {
	pat := func(host, repo string) func(t *testing.T) (*Proxy, *bytes.Buffer, string, func() int) {
		return func(t *testing.T) (*Proxy, *bytes.Buffer, string, func() int) {
			up := newPATBrokerUpstream(t, "pat-token", "x-access-token")
			p, sink := newPATBrokerProxySpec(t, denyAllSpec(),
				map[string]PATGrant{host: {GrantID: uuid.New(), Username: "x-access-token"}}, upstreamAddr(up.srv))
			return p, sink, "/wardyn/git/" + host + "/" + repo, func() int { return up.mintCalls + up.gitHits }
		}
	}
	return []denyAllLane{
		{"GitHub App (git_broker)", func(t *testing.T) (*Proxy, *bytes.Buffer, string, func() int) {
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, sink := newGitBrokerProxyWithSpec(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
				upstreamAddr(up.srv), denyAllSpec())
			return p, sink, "/wardyn/gh/octocat/hello-world", func() int { return up.mintCalls + up.gitHits }
		}},
		{"git_pat on github.com (pat_broker)", pat("github.com", "octocat/hello-world.git")},
		{"git_pat on dev.azure.com (pat_broker)", pat("dev.azure.com", "acme/proj/_git/app")},
		{"Azure DevOps Entra (pat_broker_entra)", func(t *testing.T) (*Proxy, *bytes.Buffer, string, func() int) {
			p, sink, up, cp, _ := newEntraLane(t, denyAllSpec(), types.ApprovalApproved, adoscope.CapCodeRead, adoscope.CapCodeWrite)
			return p, sink, "/wardyn/git/dev.azure.com/acme/proj/_git/app", func() int {
				raises, polls := cp.counts()
				return up.gitHits + raises + polls
			}
		}},
	}
}

// TestPushDenyAllRefusesEveryPushOnEveryLane: under push_rules.deny_all a
// content push, a ref-only update (an empty pack) and a delete-only push (no
// pack at all) are each refused 403 on every receive-pack lane, with the
// deny_all rule source and sentence, before anything reaches the forge.
func TestPushDenyAllRefusesEveryPushOnEveryLane(t *testing.T) {
	const ref = "refs/heads/main"
	pushes := map[string]func(t *testing.T) string{
		"a content push": func(t *testing.T) string {
			return string(recordedPush(t, ref, map[string]string{"src/main.go": "package main\n"}))
		},
		"a ref-only update": func(*testing.T) string {
			return pkt(zeroOID+" "+someOID+" "+ref+firstCaps) + "0000" + emptyPack()
		},
		"a delete-only push": func(*testing.T) string {
			return pkt(someOID+" "+zeroOID+" "+ref+"\x00report-status delete-refs\n") + "0000"
		},
	}
	for _, lane := range denyAllLanes() {
		for name, body := range pushes {
			t.Run(lane.name+"/"+name, func(t *testing.T) {
				p, sink, base, hits := lane.new(t)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, base+"/git-receive-pack", strings.NewReader(body(t))))
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), pushDenyAllRefusal) {
					t.Fatalf("status = %d body %q, want 403 with %q", rec.Code, rec.Body.String(), pushDenyAllRefusal)
				}
				if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPushDenyAll+`"`) {
					t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPushDenyAll)
				}
				if n := hits(); n != 0 {
					t.Errorf("a refused push reached the forge, minted or asked %d time(s)", n)
				}
			})
		}
	}
}

// TestPushDenyAllLetsAFetchThrough: deny_all refuses pushes only. The upload
// advertisement and git-upload-pack pass on every lane.
func TestPushDenyAllLetsAFetchThrough(t *testing.T) {
	for _, lane := range denyAllLanes() {
		t.Run(lane.name, func(t *testing.T) {
			p, sink, base, _ := lane.new(t)
			for _, req := range []*http.Request{
				mustLocalReq(t, http.MethodGet, base+"/info/refs?service=git-upload-pack", nil),
				mustLocalReq(t, http.MethodPost, base+"/git-upload-pack", strings.NewReader(pkt("want "+someOID+"\n")+"0000")),
			} {
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s: status = %d body %q, want 200", req.Method, req.URL, rec.Code, rec.Body.String())
				}
			}
			if strings.Contains(sink.String(), ruleSourceGitPushDenyAll) {
				t.Errorf("a fetch met deny_all: %s", sink.String())
			}
		})
	}
}

// TestPushDenyAllWithRealGit drives real git through the App and git_pat
// lanes: the clone (a fetch) passes, and a commit, a branch made at a commit
// the forge has (no new objects) and a branch delete are all refused and land
// nothing.
func TestPushDenyAllWithRealGit(t *testing.T) {
	for _, patHost := range []string{"", "gitlab.com"} {
		t.Run("lane "+patHost, func(t *testing.T) {
			r := newForgeRunSpec(t, patHost, denyAllSpec(), map[string]string{"README.md": "seed\n"}, nil)
			gitPush := func(spec string) (string, error) {
				cmd := exec.Command("git", "push", "origin", spec)
				cmd.Dir, cmd.Env = r.work, gitEnv()
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			refused := func(what, out string, err error) {
				t.Helper()
				if err == nil {
					t.Fatalf("%s: git push succeeded, want a refusal\n%s", what, out)
				}
				if !strings.Contains(r.lastRefusal(), pushDenyAllRefusal) {
					t.Errorf("%s: refusal %q, want %q", what, r.lastRefusal(), pushDenyAllRefusal)
				}
			}
			out, err := gitPush("HEAD:" + r.ref)
			refused("a branch at a commit the forge has", out, err)
			writeFiles(t, r.work, map[string]string{"src/a.go": "package a\n"})
			r.commit(t, "work")
			out, err = r.push()
			refused("a commit", out, err)
			if r.landed(t) {
				t.Error("BYPASS: the commit landed on the forge")
			}
			out, err = gitPush(":refs/heads/main")
			refused("a branch delete", out, err)
			if exec.Command("git", "-C", r.bare, "rev-parse", "-q", "--verify", "refs/heads/main").Run() != nil {
				t.Error("BYPASS: the forge's main was deleted")
			}
		})
	}
}

// TestPushDenyAllOnTheEntraLaneWithRealGit: the Azure DevOps Entra lane under
// real git: the clone passes and a push is refused, nothing reaching the forge.
// git drops a refused receive-pack's body, as on the other lanes, so the
// sentence is asserted by TestPushDenyAllRefusesEveryPushOnEveryLane.
func TestPushDenyAllOnTheEntraLaneWithRealGit(t *testing.T) {
	h := newADOGitHarnessWith(t, func(p *Proxy, _ string) { p.policy = CompilePolicy(denyAllSpec()) },
		adoscope.CapCodeRead, adoscope.CapCodeWrite)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	out, err := h.push(t, dir, h.runBranch())
	mustBeGitRefusal(t, out, err, "403")
	if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
		t.Errorf("the forge saw %d receive-pack request(s)", n)
	}
	if log := h.finish(t); !strings.Contains(log, ruleSourceGitPushDenyAll) {
		t.Errorf("no %s row:\n%s", ruleSourceGitPushDenyAll, log)
	}
}

// TestADORESTPushDenyAll is the Azure DevOps REST door under deny_all: every
// request that writes a repository is refused before its paths are read, a
// ref delete and Git Pushes - Create included, and reads and writes that move
// no ref (a pull request opened, a comment) pass to the capability gate.
func TestADORESTPushDenyAll(t *testing.T) {
	caps := adoscope.GrantableCapabilities()
	const repo = "/acme/proj/_apis/git/repositories/app"
	refused := []struct {
		name, method, target string
		body                 func(h *adoHarness) string
	}{
		{"Git Pushes - Create", http.MethodPost, restPushTarget, func(h *adoHarness) string { return restPush(h.branch(), "/src/main.go") }},
		{"Update Refs moving a ref", http.MethodPost, repo + "/refs?api-version=7.1", func(h *adoHarness) string {
			return `[{"name":"` + h.branch() + `","oldObjectId":"` + zeroOID + `","newObjectId":"` + strings.Repeat("a", 40) + `"}]`
		}},
		{"Update Refs deleting a ref", http.MethodPost, repo + "/refs?api-version=7.1", func(h *adoHarness) string {
			return `[{"name":"` + h.branch() + `","oldObjectId":"` + strings.Repeat("a", 40) + `","newObjectId":"` + zeroOID + `"}]`
		}},
		{"Update Ref", http.MethodPatch, repo + "/refs?filter=", func(h *adoHarness) string { return `{"isLocked":false}` }},
		{"a pull request completion", http.MethodPatch, repo + "/pullrequests/7?api-version=7.1", func(*adoHarness) string {
			return `{"status":"completed","lastMergeSourceCommit":{"commitId":"` + strings.Repeat("a", 40) + `"}}`
		}},
		{"a commit by the items route", http.MethodPost, repo + "/items?api-version=7.1", func(*adoHarness) string { return `{}` }},
		{"an import request", http.MethodPost, repo + "/importrequests?api-version=7.1", func(*adoHarness) string {
			return `{"parameters":{"gitSource":{"url":"https://example.com/x.git"}}}`
		}},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			h, cp := newADORESTRules(t, denyAllSpec(), types.ApprovalApproved, caps...)
			target := c.target
			if strings.HasSuffix(target, "?filter=") {
				target += strings.TrimPrefix(h.branch(), "refs/") + "&api-version=7.1"
			}
			rec := h.do(t, c.method, target, c.body(h), nil)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "every push is refused for this run") {
				t.Fatalf("status = %d body %s, want 403 with the deny_all sentence", rec.Code, rec.Body.String())
			}
			if n := len(h.fake.Requests()); n != 0 {
				t.Errorf("the upstream saw %d request(s)", n)
			}
			if raises, _ := cp.counts(); raises != 0 || !strings.Contains(h.log(), `"`+ruleSourceGitPushDenyAll+`"`) {
				t.Errorf("raises = %d log %s; want none and a %s row", raises, h.log(), ruleSourceGitPushDenyAll)
			}
		})
	}
	passes := []struct{ name, method, target, body string }{
		{"a file read", http.MethodGet, repo + "/items?path=/README.md&api-version=7.1", ""},
		{"a refs read", http.MethodGet, repo + "/refs?api-version=7.1", ""},
		{"a pull request opened", http.MethodPost, repo + "/pullrequests?api-version=7.1",
			`{"sourceRefName":"refs/heads/feature","targetRefName":"refs/heads/main","title":"x"}`},
	}
	for _, c := range passes {
		t.Run(c.name+" passes", func(t *testing.T) {
			h, _ := newADORESTRules(t, denyAllSpec(), types.ApprovalApproved, caps...)
			h.do(t, c.method, c.target, c.body, nil)
			if log := h.log(); strings.Contains(log, ruleSourceGitPushDenyAll) || !strings.Contains(log, `"decision":"allow"`) {
				t.Errorf("log %s; want it allowed and no deny_all row", log)
			}
		})
	}
}

// TestGitHubAPIDoorRefusesContentWritesUnderDenyAll: the GitHub API door
// never grants a REST write to a repository's git data (ghscope classifies it
// CapRepoContentWriteREST, a denied capability no grant can hold), so a
// contents PUT/DELETE, a git data ref, commit or tree, a merge and a pull
// request merge are refused under deny_all too, before any mint, even on a
// grant holding every grantable capability.
func TestGitHubAPIDoorRefusesContentWritesUnderDenyAll(t *testing.T) {
	writes := []struct{ method, target string }{
		{http.MethodPut, "/repos/acme/app/contents/README.md"},
		{http.MethodDelete, "/repos/acme/app/contents/README.md"},
		{http.MethodPost, "/repos/acme/app/git/refs"},
		{http.MethodPatch, "/repos/acme/app/git/refs/heads/main"},
		{http.MethodDelete, "/repos/acme/app/git/refs/heads/main"},
		{http.MethodPost, "/repos/acme/app/git/commits"},
		{http.MethodPost, "/repos/acme/app/git/trees"},
		{http.MethodPost, "/repos/acme/app/merges"},
		{http.MethodPut, "/repos/acme/app/pulls/7/merge"},
		{http.MethodPut, "/repos/acme/app/pulls/7/update-branch"},
	}
	for _, w := range writes {
		t.Run(w.method+" "+w.target, func(t *testing.T) {
			up := newGHUpstream(t)
			spec := denyAllSpec()
			spec.AllowedDomains = []string{"api.github.com:443"}
			p, g := newGHProxySpec(t, up, spec)
			g.Capabilities = ghscope.GrantableCapabilities()
			v, _ := gitHubAPIAdmit(ghTunnelRequest(t, w.method, w.target), g)
			if v.Capability != ghscope.CapRepoContentWriteREST || v.Capability.Grantable() {
				t.Fatalf("classified %q (grantable %v), want the never-grantable %s", v.Capability, v.Capability.Grantable(), ghscope.CapRepoContentWriteREST)
			}
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, ghTunnelRequest(t, w.method, w.target), ghscope.APIHost, 443)
			ghRefused(t, rec)
			if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
				t.Fatalf("a refused write reached GitHub: mints %d seen %q", mints, seen)
			}
		})
	}
}

// ghTunnelRequestBody is ghTunnelRequest carrying a JSON body, as gh sends a
// GraphQL document.
func ghTunnelRequestBody(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\n" +
		"Authorization: token wardyn-proxy-injects-github-token\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// denyAllGHDoor is the GitHub API door on a deny_all run whose grant holds
// every grantable capability, so a refusal below is the catalogue's, not a
// missing grant.
func denyAllGHDoor(t *testing.T) (*Proxy, *GitHubAPIGrant, *ghUpstream) {
	t.Helper()
	up := newGHUpstream(t)
	spec := denyAllSpec()
	spec.AllowedDomains = []string{"api.github.com:443"}
	p, g := newGHProxySpec(t, up, spec)
	g.Capabilities = ghscope.GrantableCapabilities()
	return p, g, up
}

// TestGitHubAPIDoorRefusesGraphQLUnderDenyAll: the door refuses the GraphQL
// endpoint as a class (ghscope.CapDeniedGraphQL, never grantable, whatever the
// method, query string or document), so every mutation that moves a ref or
// writes a commit, and a read query too, is refused under deny_all before any
// mint, even on a grant holding every grantable capability.
func TestGitHubAPIDoorRefusesGraphQLUnderDenyAll(t *testing.T) {
	docs := []struct{ name, doc string }{
		{"createCommitOnBranch", `{"query":"mutation($i:CreateCommitOnBranchInput!){createCommitOnBranch(input:$i){commit{oid}}}"}`},
		{"createRef", `{"query":"mutation($i:CreateRefInput!){createRef(input:$i){ref{id}}}"}`},
		{"updateRef", `{"query":"mutation($i:UpdateRefInput!){updateRef(input:$i){ref{id}}}"}`},
		{"mergePullRequest", `{"query":"mutation($i:MergePullRequestInput!){mergePullRequest(input:$i){pullRequest{id}}}"}`},
		{"mergeBranch", `{"query":"mutation($i:MergeBranchInput!){mergeBranch(input:$i){mergeCommit{oid}}}"}`},
		{"a read query", `{"query":"{repository(owner:\"acme\",name:\"app\"){defaultBranchRef{name}}}"}`},
	}
	for _, d := range docs {
		for _, target := range []string{"/graphql", "/graphql?x=1"} {
			t.Run(d.name+" "+target, func(t *testing.T) {
				p, g, up := denyAllGHDoor(t)
				v, why := gitHubAPIAdmit(ghTunnelRequestBody(t, http.MethodPost, target, d.doc), g)
				if v.Capability != ghscope.CapDeniedGraphQL || v.Capability.Grantable() || why != gitHubAPIRESTOnly {
					t.Fatalf("classified %q (grantable %v) with %q, want the never-grantable %s and the REST-only sentence",
						v.Capability, v.Capability.Grantable(), why, ghscope.CapDeniedGraphQL)
				}
				rec := httptest.NewRecorder()
				p.serveMITMRequest(rec, ghTunnelRequestBody(t, http.MethodPost, target, d.doc), ghscope.APIHost, 443)
				ghRefused(t, rec)
				if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
					t.Fatalf("a refused GraphQL request reached GitHub: mints %d seen %q", mints, seen)
				}
			})
		}
	}
}

// TestGitHubAPIDoorRefusesReleaseAndTagWritesUnderDenyAll: a release or a tag
// object is a git-data write the door never grants (CapRepoContentWriteREST),
// so creating, editing or deleting a release, uploading or removing a release
// asset through the API, and creating a tag object are refused under deny_all
// before any mint. Reading releases and tags still passes.
func TestGitHubAPIDoorRefusesReleaseAndTagWritesUnderDenyAll(t *testing.T) {
	writes := []struct{ method, target string }{
		{http.MethodPost, "/repos/acme/app/releases"},
		{http.MethodPatch, "/repos/acme/app/releases/7"},
		{http.MethodDelete, "/repos/acme/app/releases/7"},
		{http.MethodPost, "/repos/acme/app/releases/7/assets"},
		{http.MethodPatch, "/repos/acme/app/releases/assets/9"},
		{http.MethodDelete, "/repos/acme/app/releases/assets/9"},
		{http.MethodPost, "/repos/acme/app/git/tags"},
	}
	for _, w := range writes {
		t.Run(w.method+" "+w.target, func(t *testing.T) {
			p, g, up := denyAllGHDoor(t)
			v, _ := gitHubAPIAdmit(ghTunnelRequest(t, w.method, w.target), g)
			if v.Capability != ghscope.CapRepoContentWriteREST || v.Capability.Grantable() {
				t.Fatalf("classified %q (grantable %v), want the never-grantable %s", v.Capability, v.Capability.Grantable(), ghscope.CapRepoContentWriteREST)
			}
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, ghTunnelRequest(t, w.method, w.target), ghscope.APIHost, 443)
			ghRefused(t, rec)
			if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
				t.Fatalf("a refused write reached GitHub: mints %d seen %q", mints, seen)
			}
		})
	}
	for _, target := range []string{"/repos/acme/app/releases", "/repos/acme/app/git/tags/" + someOID} {
		t.Run("GET "+target+" passes", func(t *testing.T) {
			p, _, up := denyAllGHDoor(t)
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, ghTunnelRequest(t, http.MethodGet, target), ghscope.APIHost, 443)
			if mints, seen := up.counts(); rec.Code != http.StatusOK || mints != 1 || len(seen) != 1 {
				t.Fatalf("status %d mints %d seen %q, want 200 and one forwarded read", rec.Code, mints, seen)
			}
		})
	}
}

// TestGitHubUploadsHostIsNotOpenedByDenyAll: release assets upload to
// uploads.github.com, which the door neither serves nor classifies. A run that
// allows only the API host cannot open a tunnel there under deny_all, and
// nothing reaches the forge.
func TestGitHubUploadsHostIsNotOpenedByDenyAll(t *testing.T) {
	p, _, up := denyAllGHDoor(t)
	if p.isGitHubAPIHost("uploads.github.com") {
		t.Fatal("the door serves uploads.github.com")
	}
	req := httptest.NewRequest(http.MethodConnect, "uploads.github.com:443", nil)
	req.Host = "uploads.github.com:443"
	rec := httptest.NewRecorder()
	p.handleConnect(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"rule_source":"policy:default-deny"`) {
		t.Fatalf("status %d body %s, want 403 from the default-deny policy", rec.Code, rec.Body)
	}
	if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
		t.Fatalf("a refused tunnel reached upstream: mints %d seen %q", mints, seen)
	}
}

// TestPushDenyAllRefusesLFSOnEveryLane: the git brokers serve the smart-HTTP
// verbs only (info/refs, git-upload-pack, git-receive-pack), so the LFS batch
// API, with upload or download, and an LFS object PUT never get past the
// broker on any lane under deny_all: 403, and nothing reaches the forge or
// mints a credential.
func TestPushDenyAllRefusesLFSOnEveryLane(t *testing.T) {
	const oid = "4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393"
	reqs := []struct{ name, method, tail, body string }{
		{"batch upload", http.MethodPost, "/info/lfs/objects/batch",
			`{"operation":"upload","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":12}]}`},
		{"batch download", http.MethodPost, "/info/lfs/objects/batch",
			`{"operation":"download","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":12}]}`},
		{"object upload", http.MethodPut, "/info/lfs/objects/" + oid, "hello world!"},
	}
	for _, lane := range denyAllLanes() {
		for _, c := range reqs {
			t.Run(lane.name+"/"+c.name, func(t *testing.T) {
				p, _, base, hits := lane.new(t)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, c.method, base+c.tail, strings.NewReader(c.body)))
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d body %q, want 403", rec.Code, rec.Body.String())
				}
				if n := hits(); n != 0 {
					t.Errorf("a refused LFS request reached the forge, minted or asked %d time(s)", n)
				}
			})
		}
	}
}

// TestPushDenyAllCompiles: deny_all alone is a rule (IsSet), compiles to a
// rule set that refuses, and false compiles to exactly what an absent field
// does.
func TestPushDenyAllCompiles(t *testing.T) {
	if rs := compilePushRules(&types.PushRulesSpec{DenyAll: true}); rs == nil || !rs.denyAll {
		t.Fatalf("deny_all alone compiled to %+v, want a refusing rule set", rs)
	}
	if rs := compilePushRules(&types.PushRulesSpec{DenyAll: false}); rs != nil {
		t.Errorf("deny_all false compiled to %+v, want no rules", rs)
	}
}

// TestPushDenyAllSentenceNamesNoSource: a policy, a person's walls and a wall preset all set deny_all,
// so what git and the Azure DevOps REST door show is true for each and blames none of them.
func TestPushDenyAllSentenceNamesNoSource(t *testing.T) {
	for _, s := range []string{pushDenyAllRefusal, adoPushDenyAllRefusal} {
		if !strings.Contains(s, "every push is refused for this run") || strings.Contains(s, "policy") {
			t.Errorf("refusal %q, want it to say every push is refused for this run, naming no policy", s)
		}
	}
	if !strings.HasPrefix(pushDenyAllRefusal, "wardyn: ") || !strings.HasPrefix(adoPushDenyAllRefusal, "Wardyn refused this Azure DevOps request: ") {
		t.Errorf("refusals %q and %q lost their door's prefix", pushDenyAllRefusal, adoPushDenyAllRefusal)
	}
}
