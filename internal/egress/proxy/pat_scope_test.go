// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
