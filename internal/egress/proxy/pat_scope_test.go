// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPATRequestRepoKey pins the key the broker compares: what a smart-HTTP
// request's upstream path names, read with the forge's path table through
// types.PATRepoKey. Nothing is case-folded and no ".git" is stripped, because
// the path the proxy forwards is the path it compared.
func TestPATRequestRepoKey(t *testing.T) {
	for _, tc := range []struct {
		name, forge, rest, verb string
		want                    string
		ok                      bool
	}{
		{"generic info/refs", "generic", "/team/app/info/refs", "info/refs", "team/app", true},
		{"generic upload-pack", "generic", "/team/app/git-upload-pack", "git-upload-pack", "team/app", true},
		{"generic receive-pack", "generic", "/team/app/git-receive-pack", "git-receive-pack", "team/app", true},
		{"generic keeps case and .git", "generic", "/Team/App.git/info/refs", "info/refs", "Team/App.git", true},
		{"empty forge is generic", "", "/team/app/info/refs", "info/refs", "team/app", true},
		{"gitlab subgroup", "gitlab", "/g/sub/app/info/refs", "info/refs", "g/sub/app", true},
		{"gitlab keeps .git", "gitlab", "/g/app.git/info/refs", "info/refs", "g/app.git", true},
		{"gitea", "gitea", "/org/repo/git-upload-pack", "git-upload-pack", "org/repo", true},
		{"bitbucket reads /scm/", "bitbucket_server", "/scm/PROJ/repo.git/info/refs", "info/refs", "PROJ/repo.git", true},
		{"bitbucket outside /scm/", "bitbucket_server", "/PROJ/repo.git/info/refs", "info/refs", "", false},
		{"bitbucket context path keys as /scm/", "bitbucket_server", "/bitbucket/scm/PROJ/repo.git/info/refs", "info/refs", "PROJ/repo.git", true},
		{"no repository", "generic", "/info/refs", "info/refs", "", false},
		{"empty segment", "generic", "/team//app/info/refs", "info/refs", "", false},
		{"double slash prefix", "generic", "//team/app/info/refs", "info/refs", "", false},
		{"trailing slash before verb", "generic", "/team/app//info/refs", "info/refs", "", false},
		{"wildcard is never a request", "generic", "/group/*/info/refs", "info/refs", "", false},
		{"verb is not the tail", "generic", "/team/app/info/refs/x", "info/refs", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := patRequestRepoKey(tc.forge, tc.rest, tc.verb)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("patRequestRepoKey(%q, %q, %q) = (%q, %v), want (%q, %v)", tc.forge, tc.rest, tc.verb, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// patScopeProxy is a proxy with one narrowed git_pat grant and a forge that
// answers every request.
func patScopeProxy(t *testing.T, host string, g PATGrant) (*Proxy, *gitBrokerUpstream, func() egress.DecisionLog) {
	t.Helper()
	up := newPATBrokerUpstream(t, "T", "oauth2")
	g.GrantID = uuid.New()
	p, sink := newPATBrokerProxy(t, map[string]PATGrant{host: g}, upstreamAddr(up.srv))
	return p, up, func() egress.DecisionLog { return lastDecision(t, sink) }
}

// TestPATBrokerRepoPathTables drives the broker with narrowed grants and asserts,
// for every case, both what the check decided and what the forge received. A
// normaliser that folded case or stripped ".git" while the forwarded path kept
// the original would pass a same-string test and alias two repositories, so an
// allowed request must reach the forge at exactly the path compared, and a refused
// one must reach it not at all and spend no mint.
func TestPATBrokerRepoPathTables(t *testing.T) {
	const info = "/info/refs?service=git-upload-pack"
	for _, tc := range []struct {
		name  string
		forge string
		repos []string
		// path is the request path after the host, as the sandbox sends it
		// (still percent-encoded); upstream is the path the forge must receive,
		// "" for a refusal.
		path     string
		upstream string
	}{
		// generic: the path is preserved exactly.
		{"generic: exact entry passes", "generic", []string{"team/app"}, "team/app" + info, "/team/app/info/refs"},
		{"generic: Team/App.git is not team/app.git (request)", "generic", []string{"team/app.git"}, "Team/App.git" + info, ""},
		{"generic: Team/App.git is not team/app.git (entry)", "generic", []string{"Team/App.git"}, "team/app.git" + info, ""},
		{"generic: exact Team/App.git passes unfolded", "generic", []string{"Team/App.git"}, "Team/App.git" + info, "/Team/App.git/info/refs"},
		{"generic: team/foo is not team/foo.git", "generic", []string{"team/foo"}, "team/foo.git" + info, ""},
		{"generic: team/foo.git is not team/foo", "generic", []string{"team/foo.git"}, "team/foo" + info, ""},
		{"generic: team/foo.git passes unstripped", "generic", []string{"team/foo.git"}, "team/foo.git" + info, "/team/foo.git/info/refs"},
		// encoding: the decoded path is compared and is what is forwarded.
		{"encoded slash decodes into the entry", "generic", []string{"team/app"}, "team%2Fapp" + info, "/team/app/info/refs"},
		{"encoded slash cannot reach a deeper repository", "generic", []string{"team/app"}, "team%2Fapp%2Fsub" + info, ""},
		{"encoded slash cannot reach a shorter entry", "generic", []string{"team"}, "team%2Fapp" + info, ""},
		{"encoded dot is compared decoded", "generic", []string{"team/app"}, "team/app%2Egit" + info, ""},
		{"encoded dot decoded onto the entry", "generic", []string{"team/app.git"}, "team/app%2Egit" + info, "/team/app.git/info/refs"},
		{"encoded dot segment is refused before the scope", "generic", []string{"team/app"}, "team/%2e%2e/app" + info, ""},
		{"dot-dot inside a name is refused by the key function", "generic", []string{"team/a..b"}, "team/a..b" + info, ""},
		// boundaries.
		{"boundary: team/app-x", "generic", []string{"team/app"}, "team/app-x" + info, ""},
		{"boundary: team/app/sub", "generic", []string{"team/app"}, "team/app/sub" + info, ""},
		{"boundary: parent of the entry", "generic", []string{"team/app"}, "team" + info, ""},
		// wildcard: exactly one segment.
		{"wildcard: one segment passes", "gitlab", []string{"group/*"}, "group/app" + info, "/group/app/info/refs"},
		{"wildcard: not two segments", "gitlab", []string{"group/*"}, "group/sub/app" + info, ""},
		{"wildcard: not the group itself", "gitlab", []string{"group/*"}, "group" + info, ""},
		{"wildcard: not a literal star", "gitlab", []string{"group/*"}, "group/*" + info, ""},
		{"wildcard: a subgroup needs its own entry", "gitlab", []string{"group/*", "group/sub/*"}, "group/sub/app" + info, "/group/sub/app/info/refs"},
		// the empty list is "none", never "all".
		{"empty repos refuse everything", "generic", []string{}, "team/app" + info, ""},
		// per-forge tables.
		{"gitea: exact", "gitea", []string{"org/repo"}, "org/repo" + info, "/org/repo/info/refs"},
		{"gitea: case is not folded", "gitea", []string{"org/repo"}, "Org/Repo" + info, ""},
		{"bitbucket: /scm/ is part of the form", "bitbucket_server", []string{"PROJ/repo.git"}, "scm/PROJ/repo.git" + info, "/scm/PROJ/repo.git/info/refs"},
		{"bitbucket: outside /scm/ is refused", "bitbucket_server", []string{"PROJ/repo.git"}, "PROJ/repo.git" + info, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, up, last := patScopeProxy(t, "gitlab.com", PATGrant{Repos: &tc.repos, Forge: tc.forge})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/"+tc.path, nil))

			if tc.upstream == "" {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
				if up.gitHits != 0 || up.mintCalls != 0 {
					t.Fatalf("forge saw %d request(s) and %d mint(s), want none of either", up.gitHits, up.mintCalls)
				}
				if d := last(); d.Decision != egress.Deny {
					t.Fatalf("decision = %v, want a deny", d.Decision)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%q, want 200", rec.Code, rec.Body.String())
			}
			if up.gitPath != tc.upstream {
				t.Fatalf("forwarded path = %q, want %q: the path compared is the path forwarded", up.gitPath, tc.upstream)
			}
			if d := last(); d.RuleSource != ruleSourcePAT {
				t.Fatalf("rule source = %q, want %q", d.RuleSource, ruleSourcePAT)
			}
		})
	}
}

// A refused repository is its own rule source, so the audit row says which axis
// refused.
func TestPATBrokerRepoRefusalIsItsOwnRuleSource(t *testing.T) {
	p, _, last := patScopeProxy(t, "gitlab.com", PATGrant{Repos: types.PATRepoSet("team/app")})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/other/repo/info/refs?service=git-upload-pack", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if d := last(); d.RuleSource != ruleSourcePATRepo || d.Decision != egress.Deny {
		t.Fatalf("decision = %v / %q, want a deny from %q", d.Decision, d.RuleSource, ruleSourcePATRepo)
	}
}

// Read-only shuts both doors of a push: the ref advertisement a push starts with
// and the POST. Refusing only the POST would still advertise refs for a push the
// run can never make. Fetch and clone are unaffected.
func TestPATBrokerReadOnlyShutsBothReceivePackDoors(t *testing.T) {
	const repo = "/wardyn/git/gitlab.com/team/app"
	for _, tc := range []struct {
		name, method, path string
		refused            bool
	}{
		{"push advertisement", http.MethodGet, repo + "/info/refs?service=git-receive-pack", true},
		{"push", http.MethodPost, repo + "/git-receive-pack", true},
		{"fetch advertisement", http.MethodGet, repo + "/info/refs?service=git-upload-pack", false},
		{"fetch", http.MethodPost, repo + "/git-upload-pack", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, up, last := patScopeProxy(t, "gitlab.com", PATGrant{Access: types.PATAccessRead})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.path, strings.NewReader("0000")))

			if !tc.refused {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%q, want 200: a read-only grant still fetches", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if up.gitHits != 0 || up.mintCalls != 0 {
				t.Fatalf("forge saw %d request(s) and %d mint(s), want none of either", up.gitHits, up.mintCalls)
			}
			if d := last(); d.RuleSource != ruleSourcePATReadOnly || d.Decision != egress.Deny {
				t.Fatalf("decision = %v / %q, want a deny from %q", d.Decision, d.RuleSource, ruleSourcePATReadOnly)
			}
		})
	}
}

// A repeated service key cannot carry an unchecked value to the forge: refs
// discovery forwards only the one service value the checks read.
func TestPATBrokerReadOnlyForwardsOnlyTheCheckedService(t *testing.T) {
	p, up, _ := patScopeProxy(t, "gitlab.com", PATGrant{Access: types.PATAccessRead})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/git/gitlab.com/team/app/info/refs?service=git-upload-pack&service=git-receive-pack", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q, want the fetch advertisement served", rec.Code, rec.Body.String())
	}
	if up.gitQuery != "service=git-upload-pack" {
		t.Fatalf("upstream query = %q, want only the checked service", up.gitQuery)
	}
}

// A grant that is write (the default) and sets no repos is the unnarrowed grant
// of every earlier release: a push passes.
func TestPATBrokerUnnarrowedGrantStillPushes(t *testing.T) {
	p, _, _ := patScopeProxy(t, "gitlab.com", PATGrant{})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/any/repo/info/refs?service=git-receive-pack", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// The checks sit before the mint. On an approval-gated single-use grant a
// refusal that came after patToken would consume the one approval and leave the
// next, allowed, request with nothing.
func TestPATBrokerRefusalLeavesTheMintUnspent(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })

	up := newPATApprovalUpstream(t, "pat-after-approval", uuid.New(), 1)
	p, _ := newPATBrokerProxy(t, map[string]PATGrant{"gitlab.com": {
		GrantID: uuid.New(), Repos: types.PATRepoSet("team/app"), Access: types.PATAccessRead,
	}}, upstreamAddr(up.srv))

	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, method, path, strings.NewReader("0000")))
		return rec.Code
	}
	if code := do(http.MethodGet, "/wardyn/git/gitlab.com/other/repo/info/refs?service=git-upload-pack"); code != http.StatusForbidden {
		t.Fatalf("other repository: status = %d, want 403", code)
	}
	if code := do(http.MethodGet, "/wardyn/git/gitlab.com/team/app/info/refs?service=git-receive-pack"); code != http.StatusForbidden {
		t.Fatalf("push advertisement: status = %d, want 403", code)
	}
	if code := do(http.MethodPost, "/wardyn/git/gitlab.com/team/app/git-receive-pack"); code != http.StatusForbidden {
		t.Fatalf("push: status = %d, want 403", code)
	}
	if up.mintCalls != 0 || up.pollCalls != 0 {
		t.Fatalf("after three refusals: %d mint call(s) and %d approval poll(s), want none: a refused request must not touch the grant", up.mintCalls, up.pollCalls)
	}

	if code := do(http.MethodGet, "/wardyn/git/gitlab.com/team/app/info/refs?service=git-upload-pack"); code != http.StatusOK {
		t.Fatalf("allowed request: status = %d, want 200", code)
	}
	// One approval-gated mint: the 409 that raises the approval, then the re-mint
	// once it is approved.
	if up.mintCalls != 2 || up.gitHits != 1 {
		t.Fatalf("after the allowed request: %d mint call(s) and %d forge hit(s), want 2 and 1", up.mintCalls, up.gitHits)
	}
}

// The proxy refuses a config whose scope a control plane could not have written,
// at start, rather than enforce a different scope than dispatch meant.
func TestLoadConfigRefusesAnUnreadablePATScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grant string
		want  string // "" = accepted
	}{
		{"unnarrowed", `{"grant_id":"` + uuid.NewString() + `"}`, ""},
		{"narrowed", `{"grant_id":"` + uuid.NewString() + `","repos":["team/app","group/*"],"access":"read","forge":"gitlab"}`, ""},
		{"empty repos", `{"grant_id":"` + uuid.NewString() + `","repos":[]}`, ""},
		{"access outside the enum", `{"grant_id":"` + uuid.NewString() + `","access":"admin"}`, "access"},
		{"forge outside the enum", `{"grant_id":"` + uuid.NewString() + `","forge":"svn"}`, "forge"},
		{"malformed repo", `{"grant_id":"` + uuid.NewString() + `","repos":["a/../b"]}`, "malformed"},
		{"unknown key", `{"grant_id":"` + uuid.NewString() + `","branch":"main"}`, "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:8080","run_token":"t","pat_grants":{"gitlab.com":` + tc.grant + `}}`))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadConfigBytes: %v, want it accepted", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("LoadConfigBytes error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// An unnarrowed grant renders as it always did, so a proxy image from before
// this release still starts on it.
func TestUnnarrowedPATGrantRendersNoScopeKeys(t *testing.T) {
	b, err := json.Marshal(PATGrant{GrantID: uuid.MustParse("00000000-0000-0000-0000-000000000001")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"grant_id":"00000000-0000-0000-0000-000000000001"}`; got != want {
		t.Fatalf("PATGrant renders %s, want %s", got, want)
	}
}

// routePathOverrideProbes are spellings of path, prefix and host overrides (armscope.RouteOverrideHeader),
// canonical and folded; TestRoutePathOverrideProbes keeps them in its set.
var routePathOverrideProbes = []string{"X-Original-URL", "X-Rewrite-URL", "X-Override-URL", "X-Forwarded-Prefix",
	"X-Script-Name", "X-Forwarded-Host", "X_Script_Name", "X_Forwarded_Host", "X-Original-URI", "X-Forwarded-Uri", "X-Forwarded-Path",
	"X-Original-Host", "X-HTTP-Host-Override", "Forwarded", "X-Matched-Path", "X-Invoke-Path", "X_Replaced_Path",
	"X-Host", "x_host", "X-Forwarded-Server", "x-forwarded-server"}

// methodOverrideProbes are spellings of the method overrides (armscope.MethodOverrideHeader), canonical
// and folded.
var methodOverrideProbes = []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override",
	"X_HTTP_Method_Override", "x-http-method", "X_Method_Override",
	"X-Method", "x-method", "X-METHOD", "X-Ms-Http-Method", "x_ms_http_method", "X-Original-Method", "X-Forwarded-Method"}

// routeOverrideHeaders are the method- and path-override headers neither git
// broker lane forwards.
var routeOverrideHeaders = slices.Concat(methodOverrideProbes, routePathOverrideProbes)

func TestRoutePathOverrideProbes(t *testing.T) {
	for _, h := range routePathOverrideProbes {
		if !armscope.RouteOverrideHeader(h) {
			t.Errorf("%s is no route override to armscope.RouteOverrideHeader", h)
		}
	}
	for _, h := range methodOverrideProbes {
		if !armscope.MethodOverrideHeader(h) {
			t.Errorf("%s is no method override to armscope.MethodOverrideHeader", h)
		}
	}
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Port", "Host", "X-Host-Hint", "X-Hosted-By",
		"X-Portkey-Custom-Host", "X-Portkey-Provider", "Authorization", "Content-Type", "Content-Length", "anthropic-version", "openai-organization"} {
		if armscope.RouteOverrideHeader(h) {
			t.Errorf("%s is a route override to armscope.RouteOverrideHeader, want it left alone", h)
		}
	}
	for _, h := range []string{"X-HTTP-Method-Hint", "X-Methods", "Method", "Allow", "Access-Control-Request-Method"} {
		if armscope.MethodOverrideHeader(h) {
			t.Errorf("%s is a method override to armscope.MethodOverrideHeader, want only the listed names", h)
		}
	}
}

// A method override is judged by every key that spells one and every value it carries: two keys in
// different spellings, or one key with several values, are a mismatch when any names another method.
func TestPATAPIMethodMismatchReadsEveryKeyAndValue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   http.Header
		mismatch bool
	}{
		{"two spellings, one names another method", http.Header{"X-Http-Method-Override": {"GET"}, "x_http_method": {"DELETE"}}, true},
		{"two spellings, both the request's", http.Header{"X-Http-Method-Override": {"GET"}, "x_http_method": {"get"}}, false},
		{"one key, a second value names another method", http.Header{"X-Method": {"GET", "DELETE"}}, true},
		{"one key, every value the request's", http.Header{"X-Method": {"GET", " get "}}, false},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://"+hrHost+"/repos/a/b", nil)
		r.Header = tc.header
		if got := patAPIMethodMismatch(r); got != tc.mismatch {
			t.Errorf("%s: mismatch %v, want %v", tc.name, got, tc.mismatch)
		}
	}
}

// patGitRepoForges is the column order of TestPATGitRepoPerForge's verdicts.
var patGitRepoForges = []string{"generic", "gitlab", "gitea", "bitbucket_server"}

// TestPATGitRepoPerForge is the value list of the broker's path predicate: for
// each decoded upstream path, the verdict per forge in patGitRepoForges order,
// A for admitted and R for refused. Each row whose name starts "only:" is
// refused by the one reading it names (case fold, ";" cut, trailing dot or space
// trim, "%"), so removing that reading turns it red.
func TestPATGitRepoPerForge(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		// The reported exploit and its siblings: every forge refuses.
		{"Gitea contents API commit", "api/v1/repos/o/r/contents/dir/git-upload-pack", "RRRR"},
		{"Gitea contents API receive-pack", "api/v1/repos/o/r/contents/dir/git-receive-pack", "RRRR"},
		{"Gitea raw API refs", "api/v1/repos/o/r/raw/main/info/refs", "RRRR"},
		{"API mount under a sub-path", "gitea/api/v1/repos/o/r/contents/dir/git-upload-pack", "RRRR"},
		{"GitLab API under a relative root", "gitlab/api/v4/projects/1/repository/files/x/git-upload-pack", "RRRR"},
		{"GitHub Enterprise GraphQL mount", "ghe/api/graphql/x/git-upload-pack", "RRRR"},
		{"Bitbucket Server REST", "rest/api/1.0/projects/P/repos/r/browse/dir/git-upload-pack", "RRRR"},
		{"Bitbucket Server REST plugin route", "bitbucket/rest/git/latest/projects/P/repos/r/tags/git-upload-pack", "RRRR"},
		{"Azure DevOps _apis", "org/project/_apis/git/repositories/r/git-upload-pack", "RRRR"},
		{"GitLab route scope", "group/project/-/raw/main/info/refs", "RRRR"},
		{"Bitbucket Cloud !api", "!api/2.0/repositories/ws/git-upload-pack", "RRRR"},
		{"no repository", "info/refs", "RRRR"},
		{"empty repository", "/info/refs", "RRRR"},
		// Only the reading named refuses these.
		{"only: case fold", "API/v1/user/git-upload-pack", "RRRR"},
		{"only: case fold on _apis", "org/_APIS/x/git-upload-pack", "RRRR"},
		{"only: case fold on the version", "gitlab/API/V4/user/git-upload-pack", "RRRR"},
		{"only: Unicode fold (U+017F)", "org/_api\u017f/x/git-upload-pack", "RRRR"},
		{"only: ASCII case mapping (U+0131)", "org/_ap\u0131s/x/git-upload-pack", "RRRR"},
		{"only: ASCII case mapping (U+0130)", "org/_ap\u0130s/x/git-upload-pack", "RRRR"},
		{"only: empty segment before the mount", "/api/2.0/repositories/ws/git-upload-pack", "RRRR"},
		{"only: segment read as empty before the mount", ";x/api/2.0/repositories/ws/git-upload-pack", "RRRR"},
		{"empty segment inside", "team//app/git-upload-pack", "RRRR"},
		{"trailing slash before the verb", "team/app//git-upload-pack", "RRRR"},
		{"segment of dots", "team/.../app/git-upload-pack", "RRRR"},
		{"only: ; cut", "api;v=1/v1/user/git-upload-pack", "RRRR"},
		{"only: ; cut on -", "team/-;x/y/git-upload-pack", "RRRR"},
		{"only: trailing dot", "api./v1/user/git-upload-pack", "RRRR"},
		{"only: trailing dot on -", "group/project/-./raw/main/info/refs", "RRRR"},
		{"only: trailing space", "api /v1/user/git-upload-pack", "RRRR"},
		{"only: percent escape", "o/r%2Fx/git-upload-pack", "RRRR"},
		{"only: percent", "o/r%x/git-upload-pack", "ARRR"},
		{"Azure DevOps name with a percent", "org/100% Done/_git/Half 50%/info/refs", "ARRR"},
		// Bitbucket Server: scm/ with an API tail or a route before it.
		{"scm under REST", "rest/api/1.0/projects/P/repos/r/browse/scm/x/y/git-upload-pack", "RRRR"},
		{"scm under the web browse route", "projects/P/repos/r/browse/scm/x/y/info/refs", "AAAR"},
		{"scm under a plugin servlet", "plugins/servlet/x/scm/P/r/git-upload-pack", "AAAR"},
		{"scm with a route after the repository", "scm/P/r/raw/x/git-upload-pack", "AAAR"},
		{"scm under an API mount", "api/scm/P/r/git-upload-pack", "RRRR"},
		{"only: case fold in the context path", "bitbucket/REST/scm/P/r.git/info/refs", "AAAR"},
		{"scm under a doubled slash", "bitbucket//scm/P/r.git/info/refs", "RRRR"},
		// generic only: Azure DevOps _api.
		{"Azure DevOps _api", "org/_api/x/git-upload-pack", "RAAR"},
		// Real repositories: admitted wherever the forge allows the name.
		{"GitLab subgroup api", "group/api/project.git/info/refs", "AARR"},
		{"GitLab subgroup api, project v4", "group/api/v4/info/refs", "AARR"},
		{"namespace contents", "contents/project.git/info/refs", "AAAR"},
		{"namespace rest", "rest/project.git/git-upload-pack", "AAAR"},
		{"a former guard row: contents mid-path", "o/r/contents/dir/git-upload-pack", "AAAR"},
		{"Azure DevOps project API", "org/API/_git/repo/info/refs", "AARR"},
		{"a repository named api", "team/api/git-upload-pack", "AAAR"},
		{"Gerrit authenticated clone", "a/project.git/info/refs", "AAAR"},
		// Gerrit: its git servlet takes every [a/]<project>/<git verb> path ahead of REST.
		{"Gerrit project named projects, through a/", "a/projects/git-upload-pack", "AAAR"},
		{"Gerrit project under a collection name, through a/", "a/plugins/replication/info/refs", "AAAR"},
		{"Gerrit project named config, through a/", "a/config/info/refs", "AAAR"},
		{"Gerrit project under projects/, through a/", "a/projects/p/branches/git-upload-pack", "AAAR"},
		{"Gerrit project named projects, without a/", "projects/info/refs", "AAAR"},
		{"GitLab subgroups api and v4 (refused: no deployment root is configured)", "group/api/v4/project.git/info/refs", "RRRR"},
		{"GitLab subgroups api and graphql (refused: no deployment root is configured)", "group/api/graphql/project.git/info/refs", "RRRR"},
		{"Bitbucket Server", "scm/PROJ/repo.git/git-upload-pack", "AAAA"},
		{"Bitbucket Server project API", "scm/API/repo.git/git-upload-pack", "AARA"},
		{"Bitbucket Server personal", "scm/~user/repo.git/info/refs", "AAAA"},
		{"Bitbucket Server context path", "bitbucket/scm/PROJ/repo.git/info/refs", "AAAA"},
	} {
		path := tc.path
		verb := path[strings.LastIndex(path, "/")+1:]
		if strings.HasSuffix(path, "/info/refs") || path == "info/refs" {
			verb = patInfoRefs
		}
		for i, forge := range patGitRepoForges {
			t.Run(tc.name+"/"+forge, func(t *testing.T) {
				_, ok := patGitRepo(forge, "/"+path, verb)
				if got := map[bool]byte{true: 'A', false: 'R'}[ok]; got != tc.want[i] {
					t.Fatalf("patGitRepo(%q, %q) = %c, want %c", forge, "/"+path, got, tc.want[i])
				}
			})
		}
	}
}

// The broker route judges the whole path, not its last segment: a forge API
// route whose tail happens to spell a smart-HTTP verb (Gitea's contents API
// commits a file on POST and takes a token over Basic auth) is refused on every
// forge, with no repos set and read-only access, before anything mints. The
// path arrives percent-encoded, as a sandbox can send it.
func TestPATBrokerRefusesForgeAPIPathsEndingInAGitVerb(t *testing.T) {
	const up = "?service=git-upload-pack"
	for _, tc := range []struct{ name, method, path string }{
		{"contents API commit", http.MethodPost, "api/v1/repos/o/r/contents/dir/git-upload-pack"},
		{"contents API receive-pack", http.MethodPost, "api/v1/repos/o/r/contents/dir/git-receive-pack"},
		{"raw API refs", http.MethodGet, "api/v1/repos/o/r/raw/main/info/refs" + up},
		{"api prefix any case", http.MethodPost, "API/v1/repos/o/r/contents/dir/git-upload-pack"},
		{"api under a sub-path", http.MethodPost, "gitea/api/v1/repos/o/r/contents/dir/git-upload-pack"},
		{"api with a path parameter", http.MethodPost, "api;v=1/v1/repos/o/r/contents/dir/git-upload-pack"},
		{"api with a trailing dot", http.MethodPost, "api./v1/repos/o/r/contents/dir/git-upload-pack"},
		{"Bitbucket Server REST", http.MethodPost, "rest/api/1.0/projects/P/repos/r/browse/dir/git-upload-pack"},
		{"Azure DevOps _apis", http.MethodPost, "org/project/_apis/git/repositories/r/git-upload-pack"},
		{"GitLab route scope", http.MethodGet, "group/project/-/raw/main/info/refs" + up},
		{"no repository", http.MethodGet, "info/refs" + up},
		// Encodings: each decodes onto a refused path, or leaves a "%".
		{"encoded slashes", http.MethodPost, "api%2Fv1%2Frepos%2Fo%2Fr%2Fcontents%2Fdir/git-upload-pack"},
		{"encoded slashes, mixed case", http.MethodPost, "Api%2fV1%2Frepos%2fo%2Fr%2Fcontents%2fdir/git-upload-pack"},
		{"doubly encoded slashes", http.MethodPost, "api%252Fv1%252Frepos%252Fo%252Fr%252Fcontents%252Fdir/git-upload-pack"},
		{"doubly encoded, one segment", http.MethodPost, "team/app%252Fx/git-upload-pack"},
		{"encoded trailing space", http.MethodPost, "api%20/v1/user/git-upload-pack"},
		{"encoded long s", http.MethodPost, "org/_api%C5%BF/x/git-upload-pack"},
		{"trailing slash after the verb", http.MethodPost, "api/v1/repos/o/r/contents/dir/git-upload-pack/"},
		{"trailing slash before the verb", http.MethodPost, "api/v1/repos/o/r/contents/dir//git-upload-pack"},
	} {
		for _, forge := range patGitRepoForges {
			t.Run(tc.name+"/"+forge, func(t *testing.T) {
				p, upstream, last := patScopeProxy(t, "git.corp", PATGrant{Forge: forge, Access: types.PATAccessRead})
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, tc.method, "/wardyn/git/git.corp/"+tc.path,
					strings.NewReader(`{"content":"eA==","branch":"main","message":"x"}`)))
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
				if upstream.gitHits != 0 || upstream.mintCalls != 0 {
					t.Fatalf("forge saw %d request(s) at %q and %d mint(s), want none: the PAT reached a forge API route",
						upstream.gitHits, upstream.gitPath, upstream.mintCalls)
				}
				if d := last(); d.Decision != egress.Deny || d.RuleSource != ruleSourcePATDenied {
					t.Fatalf("decision = %v %q, want a deny from %q", d.Decision, d.RuleSource, ruleSourcePATDenied)
				}
			})
		}
	}
}

// The same check must leave every forge's real clone, fetch and push paths
// alone: each is forwarded at exactly the path the sandbox sent.
func TestPATBrokerForwardsEachForgesGitPaths(t *testing.T) {
	for _, tc := range []struct{ forge, repo string }{
		{"generic", "octocat/hello-world.git"},                 // GitHub / GitHub Enterprise
		{"generic", "octocat/hello-world"},                     // without .git
		{"generic", "workspace/repo.git"},                      // Bitbucket Cloud
		{"generic", "org/project/_git/repo"},                   // Azure DevOps Services
		{"generic", "org/_git/repo"},                           // Azure DevOps, project named as the repository
		{"generic", "DefaultCollection/project/_git/repo"},     // <org>.visualstudio.com
		{"generic", "tfs/DefaultCollection/project/_git/repo"}, // Azure DevOps Server
		{"generic", "team/api"},                                // a repository named api
		{"generic", "team/rest.git"},                           // a repository named rest
		{"generic", "org/API/_git/repo"},                       // Azure DevOps project named API
		{"generic", "a/project.git"},                           // Gerrit, authenticated
		{"gitlab", "group/project.git"},
		{"gitlab", "group/api/project.git"}, // subgroup api
		{"gitlab", "contents/project.git"},
		{"gitlab", "rest/project.git"},
		{"gitlab", "group/sub/project"},
		{"gitlab", "group/project.wiki.git"},
		{"gitea", "owner/repo.git"},
		{"gitea", "owner/repo"},
		{"gitea", "owner/contents"},       // a repository named contents
		{"gitea", "gitea/owner/repo.git"}, // served under a sub-path
		{"bitbucket_server", "scm/PROJ/repo.git"},
		{"bitbucket_server", "scm/~user/repo.git"},
		{"bitbucket_server", "scm/API/repo.git"},
		{"bitbucket_server", "bitbucket/scm/PROJ/repo.git"}, // under a context path
	} {
		for _, rq := range []struct{ method, tail, query string }{
			{http.MethodGet, "info/refs", "?service=git-upload-pack"},
			{http.MethodGet, "info/refs", "?service=git-receive-pack"},
			{http.MethodPost, "git-upload-pack", ""},
			{http.MethodPost, "git-receive-pack", ""},
		} {
			t.Run(tc.forge+"/"+tc.repo+"/"+rq.tail+rq.query, func(t *testing.T) {
				p, upstream, _ := patScopeProxy(t, "git.corp", PATGrant{Forge: tc.forge})
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, rq.method, "/wardyn/git/git.corp/"+tc.repo+"/"+rq.tail+rq.query,
					strings.NewReader("0000")))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%q, want 200", rec.Code, rec.Body.String())
				}
				if want := "/" + tc.repo + "/" + rq.tail; upstream.gitPath != want {
					t.Fatalf("forwarded path = %q, want %q", upstream.gitPath, want)
				}
			})
		}
	}
}

// A method override is never forwarded: the forge would judge the request by a
// method the verb check never saw. A pack POST carries no query, and refs
// discovery carries only the service the check read.
func TestPATBrokerForwardsNoMethodOverrideAndOnlyTheServiceQuery(t *testing.T) {
	p, upstream, _ := patScopeProxy(t, "git.corp", PATGrant{})
	req := mustLocalReq(t, http.MethodPost, "/wardyn/git/git.corp/team/app/git-upload-pack?x=1", strings.NewReader("0000"))
	for _, h := range routeOverrideHeaders {
		req.Header.Set(h, http.MethodPut)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	for _, h := range routeOverrideHeaders {
		if v := upstream.gitHeaders.Values(h); len(v) != 0 {
			t.Errorf("forge saw %s: %q, want it stripped", h, v)
		}
	}
	if upstream.gitQuery != "" {
		t.Errorf("forwarded query = %q, want none on a pack POST", upstream.gitQuery)
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/git/git.corp/team/app/info/refs?service=git-upload-pack&ref=main", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("info/refs status = %d, want 200", rec.Code)
	}
	if upstream.gitQuery != "service=git-upload-pack" {
		t.Errorf("forwarded query = %q, want only service", upstream.gitQuery)
	}
}
