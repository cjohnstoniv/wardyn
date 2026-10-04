// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	apiHost  = "forge.test"
	apiToken = "PAT-SECRET"
)

// apiHarness is a proxy with one git_pat API grant, terminating its host, and a
// forge behind it that answers every request and counts the mints it saw.
type apiHarness struct {
	p   *Proxy
	up  *gitBrokerUpstream
	log *bytes.Buffer
}

func newAPIHarness(t *testing.T, forge string, g PATGrant) *apiHarness {
	t.Helper()
	up := newPATBrokerUpstream(t, apiToken, "oauth2")
	g.GrantID, g.API, g.Forge = uuid.New(), true, forge
	p, buf := newPATBrokerProxy(t, map[string]PATGrant{apiHost: g}, upstreamAddr(up.srv))
	return &apiHarness{p: p, up: up, log: buf}
}

// rawAPIRequest parses a request the way the MITM listener would: the target and
// headers exactly as given.
func rawAPIRequest(t *testing.T, method, target, ctype, body string, hdr map[string]string) *http.Request {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + apiHost + "\r\n"
	for k, v := range hdr {
		raw += k + ": " + v + "\r\n"
	}
	if ctype != "" {
		raw += "Content-Type: " + ctype + "\r\n"
	}
	if body != "" {
		raw += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n" + body)))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	return req
}

type apiReq struct {
	method, target, ctype, body string
	hdr                         map[string]string
}

func (h *apiHarness) serve(t *testing.T, q apiReq) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, rawAPIRequest(t, q.method, q.target, q.ctype, q.body, q.hdr), apiHost, 443)
	return rec
}

func (h *apiHarness) forgeHits() int { h.up.mu.Lock(); defer h.up.mu.Unlock(); return h.up.gitHits }
func (h *apiHarness) mints() int     { h.up.mu.Lock(); defer h.up.mu.Unlock(); return h.up.mintCalls }

const (
	jsonCT = "application/json"
	formCT = "application/x-www-form-urlencoded"
	glBase = "/api/v4/projects/team%2Fapp"
	gtBase = "/api/v1/repos/team/app"
)

var apiGrants = map[string]PATGrant{
	types.PATForgeGitLab: {Repos: types.PATRepoSet("team/app")},
	types.PATForgeGitea:  {Repos: types.PATRepoSet("team/app")},
}

// Every admitted row reaches the forge once, with the PAT in the forge's header,
// the sandbox's own credential headers gone, and the path forwarded as the path
// compared (an encoded "/" in a GitLab project path survives).
func TestPATAPIAdmittedRows(t *testing.T) {
	gl := []apiReq{
		{"GET", glBase + "/merge_requests?state=opened", "", "", nil},
		{"GET", glBase + "/merge_requests/7", "", "", nil},
		{"GET", glBase + "/merge_requests/7/changes", "", "", nil},
		{"GET", glBase + "/merge_requests/7/notes", "", "", nil},
		{"GET", glBase + "/issues", "", "", nil},
		{"GET", glBase + "/issues/3/notes/9", "", "", nil},
		{"GET", glBase + "/repository/branches", "", "", nil},
		{"GET", glBase + "/repository/tags", "", "", nil},
		{"GET", glBase + "/repository/commits/abc123", "", "", nil},
		{"GET", glBase + "/repository/compare?from=main&to=feature", "", "", nil},
		{"GET", glBase + "/pipelines/4/jobs", "", "", nil},
		{"GET", glBase + "/jobs/5", "", "", nil},
		{"GET", glBase + "/repository/tree?path=src", "", "", nil},
		{"GET", glBase + "/repository/files/src%2Fmain.go?ref=main", "", "", nil},
		{"GET", glBase + "/repository/files/src%2Fmain.go/raw?ref=main", "", "", nil},
		{"GET", glBase + "/repository/archive.zip", "", "", nil},
		{"POST", glBase + "/merge_requests", jsonCT, `{"source_branch":"f","target_branch":"main","title":"t","description":"fixes the thing"}`, nil},
		{"POST", glBase + "/merge_requests", formCT, `source_branch=f&target_branch=main&title=t`, nil},
		{"POST", glBase + "/merge_requests/7/notes", jsonCT, `{"body":"looks good"}`, nil},
		{"POST", glBase + "/issues/3/notes", jsonCT, `{"body":"see the log"}`, nil},
	}
	gt := []apiReq{
		{"GET", gtBase + "/pulls?state=open", "", "", nil},
		{"GET", gtBase + "/pulls/7", "", "", nil},
		{"GET", gtBase + "/pulls/7.diff", "", "", nil},
		{"GET", gtBase + "/pulls/7/files", "", "", nil},
		{"GET", gtBase + "/issues/3/comments", "", "", nil},
		{"GET", gtBase + "/branches/feature/x", "", "", nil},
		{"GET", gtBase + "/commits/abc/status", "", "", nil},
		{"GET", gtBase + "/compare/main...feature", "", "", nil},
		{"GET", gtBase + "/actions/runs/4/jobs", "", "", nil},
		{"GET", gtBase + "/contents/src/main.go?ref=main", "", "", nil},
		{"GET", gtBase + "/raw/src/main.go", "", "", nil},
		{"GET", gtBase + "/archive/main.zip", "", "", nil},
		{"POST", gtBase + "/pulls", jsonCT, `{"head":"feature","base":"main","title":"t"}`, nil},
		{"POST", gtBase + "/issues/3/comments", jsonCT, `{"body":"ok"}`, nil},
	}
	wantHdr := map[string]func(http.Header) string{
		types.PATForgeGitLab: func(h http.Header) string { return h.Get("Private-Token") },
		types.PATForgeGitea:  func(h http.Header) string { return h.Get("Authorization") },
	}
	wantVal := map[string]string{types.PATForgeGitLab: apiToken, types.PATForgeGitea: "token " + apiToken}
	// One synthesised request per table row, so a row whose pattern the matcher
	// cannot match is caught, and a new row cannot go untested.
	for forge, hand := range map[string]*[]apiReq{types.PATForgeGitLab: &gl, types.PATForgeGitea: &gt} {
		rows := patAPIForges[forge].rows
		synth := synthPATAPIRows(forge, rows)
		if len(synth) != len(rows) {
			t.Fatalf("%s: synthesised %d requests for %d rows", forge, len(synth), len(rows))
		}
		*hand = append(*hand, synth...)
	}
	for forge, reqs := range map[string][]apiReq{types.PATForgeGitLab: gl, types.PATForgeGitea: gt} {
		for _, q := range reqs {
			t.Run(forge+" "+q.method+" "+q.target, func(t *testing.T) {
				h := newAPIHarness(t, forge, apiGrants[forge])
				q.hdr = map[string]string{"Authorization": "Bearer SANDBOX", "Private-Token": "SANDBOX", "Cookie": "s=1"}
				rec := h.serve(t, q)
				if rec.Code != http.StatusOK || h.forgeHits() != 1 {
					t.Fatalf("status %d (%s), forge hits %d, want 200 and 1", rec.Code, rec.Body.String(), h.forgeHits())
				}
				if got := wantHdr[forge](h.up.gitHeaders); got != wantVal[forge] {
					t.Fatalf("forge saw credential %q, want %q", got, wantVal[forge])
				}
				if h.up.gitHeaders.Get("Cookie") != "" || (forge == types.PATForgeGitLab && h.up.gitHeaders.Get("Authorization") != "") {
					t.Fatalf("a sandbox credential header reached the forge: %v", h.up.gitHeaders)
				}
				if want, _, _ := strings.Cut(q.target, "?"); !strings.HasPrefix(h.up.gitURI, want) {
					t.Fatalf("forge saw %q, want the compared path %q", h.up.gitURI, want)
				}
				if d := findDecision(t, h.log, ruleSourcePATAPI); d.Decision != egress.Allow {
					t.Fatalf("decision %v, want allow", d.Decision)
				}
			})
		}
	}
}

// synthPATAPIRows builds one admissible request per row: each tail token is
// replaced by a value it matches, and a write carries the minimal body.
func synthPATAPIRows(forge string, rows []patAPIRow) []apiReq {
	base, create := glBase, `{"source_branch":"f","target_branch":"main","title":"t"}`
	if forge == types.PATForgeGitea {
		base, create = gtBase, `{"head":"f","base":"main","title":"t"}`
	}
	subst := map[string]string{"*": "x", "**": "a/b", "{f}": "src%2Fmain.go", "{archive}": "archive.zip"}
	out := make([]apiReq, len(rows))
	for i, r := range rows {
		toks := strings.Split(r.tail, "/")
		for j, tok := range toks {
			if v, ok := subst[tok]; ok {
				toks[j] = v
			} else if suffix, ok := strings.CutPrefix(tok, "{n}"); ok {
				toks[j] = "7" + suffix
			}
		}
		q := apiReq{method: r.method, target: base + "/" + strings.Join(toks, "/")}
		if r.method == "POST" {
			q.ctype, q.body = jsonCT, `{"body":"ok"}`
			if r.kind == patAPICreate {
				q.body = create
			}
		}
		out[i] = q
	}
	return out
}

func apiMultipart(t *testing.T, fields map[string]string) (ctype, body string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	_ = mw.Close()
	return mw.FormDataContentType(), buf.String()
}

// Each of these is refused before the mint and before anything is sent upstream.
func TestPATAPIRefusals(t *testing.T) {
	mpCT, mpBody := apiMultipart(t, map[string]string{"title": "t", "target_project_id": "5"})
	type tc struct {
		name string
		apiReq
		why string // a clause of the refusal
	}
	gl := []tc{
		{"other repository in the path", apiReq{"GET", "/api/v4/projects/team%2Fother/merge_requests", "", "", nil}, "not granted"},
		{"numeric id", apiReq{"GET", "/api/v4/projects/42/merge_requests", "", "", nil}, "numeric id"},
		{"unencoded project path", apiReq{"GET", "/api/v4/projects/team/app/merge_requests", "", "", nil}, "URL-encoded path"},
		{"target in the json body", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"title":"t","target_project_id":5}`, nil}, "target_project_id"},
		{"nested target in the json body", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"mr":{"target_project_id":5}}`, nil}, "target_project_id"},
		{"target in the query", apiReq{"POST", glBase + "/merge_requests?target_project_id=5", jsonCT, `{"title":"t"}`, nil}, "target_project_id"},
		{"target in a get query", apiReq{"GET", glBase + "/repository/compare?from=a&to=b&from_project_id=5", "", "", nil}, "from_project_id"},
		{"target in a form body", apiReq{"POST", glBase + "/merge_requests", formCT, `title=t&target_project_id=5`, nil}, "target_project_id"},
		{"target in a bracketed form key", apiReq{"POST", glBase + "/merge_requests", formCT, `title=t&merge_request%5Btarget_project_id%5D=5`, nil}, "target_project_id"},
		{"target in a multipart body", apiReq{"POST", glBase + "/merge_requests", mpCT, mpBody, nil}, "target_project_id"},
		{"encoded body", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"title":"t"}`, map[string]string{"Content-Encoding": "gzip"}}, "encoded body"},
		{"unparsed content type", apiReq{"POST", glBase + "/merge_requests", "text/plain", `title=t`, nil}, "does not parse"},
		{"two content types", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"title":"t"}`, map[string]string{"Content-Type": formCT}}, "no content type"},
		{"repeated json key", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"title":"a","title":"b"}`, nil}, "appears twice"},
		{"json array body", apiReq{"POST", glBase + "/merge_requests", jsonCT, `[1]`, nil}, "cannot be parsed"},
		{"body over the peek cap", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"title":"` + strings.Repeat("x", 300<<10) + `"}`, nil}, "too large"},
		{"project metadata", apiReq{"GET", glBase, "", "", nil}, "not one of"},
		{"variables", apiReq{"GET", glBase + "/variables", "", "", nil}, "not one of"},
		{"hooks", apiReq{"GET", glBase + "/hooks", "", "", nil}, "not one of"},
		{"deploy keys", apiReq{"GET", glBase + "/deploy_keys", "", "", nil}, "not one of"},
		{"deploy tokens", apiReq{"GET", glBase + "/deploy_tokens", "", "", nil}, "not one of"},
		{"access tokens", apiReq{"GET", glBase + "/access_tokens", "", "", nil}, "not one of"},
		{"members", apiReq{"GET", glBase + "/members", "", "", nil}, "not one of"},
		{"export", apiReq{"GET", glBase + "/export", "", "", nil}, "not one of"},
		{"trace of a job", apiReq{"GET", glBase + "/jobs/5/trace", "", "", nil}, "not one of"},
		{"json suffix", apiReq{"GET", glBase + "/merge_requests.json", "", "", nil}, "not one of"},
		{"merge", apiReq{"PUT", glBase + "/merge_requests/7/merge", jsonCT, `{}`, nil}, "merging"},
		{"merge by post", apiReq{"POST", glBase + "/merge_requests/7/merge", "", "", nil}, "merging"},
		{"auto-merge at creation", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"title":"t","merge_when_pipeline_succeeds":true}`, nil}, "merge_when_pipeline_succeeds"},
		{"auto-merge at creation in a form", apiReq{"POST", glBase + "/merge_requests", formCT, `title=t&auto_merge=true`, nil}, "auto_merge"},
		{"auto-merge by update", apiReq{"PUT", glBase + "/merge_requests/7", jsonCT, `{"merge_when_pipeline_succeeds":true}`, nil}, "not one of"},
		{"repository files write", apiReq{"POST", glBase + "/repository/files/a.txt", jsonCT, `{"branch":"main","content":"x","commit_message":"m"}`, nil}, "repository files or commits"},
		{"repository files put", apiReq{"PUT", glBase + "/repository/files/a.txt", jsonCT, `{}`, nil}, "repository files or commits"},
		{"commits write", apiReq{"POST", glBase + "/repository/commits", jsonCT, `{"branch":"main","actions":[]}`, nil}, "repository files or commits"},
		{"graphql", apiReq{"POST", "/api/graphql", jsonCT, `{"query":"{currentUser{id}}"}`, nil}, "GraphQL"},
		{"search", apiReq{"GET", "/api/v4/search?scope=blobs&search=x", "", "", nil}, "search"},
		{"project search", apiReq{"GET", glBase + "/search?scope=blobs&search=x", "", "", nil}, "search"},
		{"endpoint outside a repository", apiReq{"GET", "/api/v4/user", "", "", nil}, "not under a repository"},
		{"method override read to write", apiReq{"POST", glBase + "/issues", "", "", map[string]string{"X-HTTP-Method-Override": "GET"}}, "method override"},
		{"method override write from get", apiReq{"GET", glBase + "/issues", "", "", map[string]string{"X-HTTP-Method-Override": "DELETE"}}, "method override"},
		{"method override field", apiReq{"POST", glBase + "/issues/3/notes", formCT, `body=x&_method=PUT`, nil}, "_method"},
		{"quick action in a note", apiReq{"POST", glBase + "/merge_requests/7/notes", jsonCT, `{"body":"thanks\n/merge"}`, nil}, "quick action"},
		{"quick action in a description", apiReq{"POST", glBase + "/merge_requests", jsonCT, `{"title":"t","description":"  /target_branch main"}`, nil}, "quick action"},
		{"creating an issue", apiReq{"POST", glBase + "/issues", jsonCT, `{"title":"t"}`, nil}, "not one of"},
		{"encoded dot segments", apiReq{"GET", glBase + "/repository/files/%2e%2e%2fsecret/raw", "", "", nil}, "segment"},
		{"double encoding", apiReq{"GET", glBase + "/repository/files/a%252Fb/raw", "", "", nil}, "percent sign"},
		{"sudo in the query", apiReq{"GET", glBase + "/merge_requests?sudo=root", "", "", nil}, "sudo"},
		{"sudo in the json body", apiReq{"POST", glBase + "/merge_requests/7/notes", jsonCT, `{"body":"x","sudo":"root"}`, nil}, "sudo"},
		{"sudo header", apiReq{"POST", glBase + "/merge_requests/7/notes", jsonCT, `{"body":"x"}`, map[string]string{"Sudo": "root"}}, "Sudo header"},
	}
	gt := []tc{
		{"other repository in the path", apiReq{"GET", "/api/v1/repos/team/other/pulls", "", "", nil}, "not granted"},
		{"case differs", apiReq{"GET", "/api/v1/repos/Team/App/pulls", "", "", nil}, "not granted"},
		{"repository by numeric id", apiReq{"GET", "/api/v1/repositories/42", "", "", nil}, "not under a repository"},
		{"cross-repository head", apiReq{"POST", gtBase + "/pulls", jsonCT, `{"head":"fork:feature","base":"main","title":"t"}`, nil}, "another repository"},
		{"cross-repository head in a form", apiReq{"POST", gtBase + "/pulls", formCT, `head=fork%3Afeature&base=main`, nil}, "another repository"},
		{"cross-repository compare", apiReq{"GET", gtBase + "/compare/main...fork:feature", "", "", nil}, "across repositories"},
		{"target in the query", apiReq{"POST", gtBase + "/pulls?project_id=5", jsonCT, `{"head":"f","base":"main"}`, nil}, "project_id"},
		{"target in a multipart body", apiReq{"POST", gtBase + "/pulls", mpCT, mpBody, nil}, "target_project_id"},
		{"encoded body", apiReq{"POST", gtBase + "/pulls", jsonCT, `{"head":"f","base":"main"}`, map[string]string{"Content-Encoding": "br"}}, "encoded body"},
		{"repository settings", apiReq{"GET", gtBase, "", "", nil}, "not one of"},
		{"hooks", apiReq{"GET", gtBase + "/hooks", "", "", nil}, "not one of"},
		{"keys", apiReq{"GET", gtBase + "/keys", "", "", nil}, "not one of"},
		{"collaborators", apiReq{"GET", gtBase + "/collaborators", "", "", nil}, "not one of"},
		{"merge", apiReq{"POST", gtBase + "/pulls/7/merge", jsonCT, `{"Do":"merge"}`, nil}, "merging"},
		{"auto-merge", apiReq{"POST", gtBase + "/pulls/7/merge", jsonCT, `{"Do":"merge","merge_when_checks_succeed":true}`, nil}, "merging"},
		{"auto-merge at creation", apiReq{"POST", gtBase + "/pulls", jsonCT, `{"head":"f","base":"main","auto_merge":true}`, nil}, "auto_merge"},
		{"pull update", apiReq{"PATCH", gtBase + "/pulls/7", jsonCT, `{"state":"closed"}`, nil}, "not one of"},
		{"contents write", apiReq{"POST", gtBase + "/contents/a.txt", jsonCT, `{"content":"eA=="}`, nil}, "repository files or commits"},
		{"contents put", apiReq{"PUT", gtBase + "/contents/a.txt", jsonCT, `{"content":"eA=="}`, nil}, "repository files or commits"},
		{"graphql", apiReq{"POST", "/api/graphql", jsonCT, `{}`, nil}, "GraphQL"},
		{"search", apiReq{"GET", "/api/v1/repos/search?q=x", "", "", nil}, "search"},
		{"method override", apiReq{"POST", gtBase + "/issues/3/comments", jsonCT, `{"body":"x"}`, map[string]string{"X-HTTP-Method-Override": "DELETE"}}, "method override"},
		{"sudo in the query", apiReq{"GET", gtBase + "/pulls?sudo=root", "", "", nil}, "sudo"},
		{"sudo in a form body", apiReq{"POST", gtBase + "/issues/3/comments", formCT, `body=x&sudo=root`, nil}, "sudo"},
		{"sudo header", apiReq{"POST", gtBase + "/issues/3/comments", jsonCT, `{"body":"x"}`, map[string]string{"Sudo": "root"}}, "Sudo header"},
	}
	for forge, cases := range map[string][]tc{types.PATForgeGitLab: gl, types.PATForgeGitea: gt} {
		for _, c := range cases {
			t.Run(forge+" "+c.name, func(t *testing.T) {
				h := newAPIHarness(t, forge, apiGrants[forge])
				rec := h.serve(t, c.apiReq)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), patAPIRefused) || !strings.Contains(rec.Body.String(), c.why) {
					t.Fatalf("status %d body %s, want a 403 %s refusal naming %q", rec.Code, rec.Body.String(), patAPIRefused, c.why)
				}
				if h.forgeHits() != 0 || h.mints() != 0 {
					t.Fatalf("refused request cost %d forge hit(s) and %d mint call(s), want none", h.forgeHits(), h.mints())
				}
				if d := findDecision(t, h.log, ruleSourcePATAPIDenied); d.Decision != egress.Deny {
					t.Fatalf("decision %v, want deny", d.Decision)
				}
			})
		}
	}
}

// access: read admits the read rows only, and refuses a creation or a comment.
func TestPATAPIReadAccessRefusesCreation(t *testing.T) {
	for forge, creates := range map[string][]apiReq{
		types.PATForgeGitLab: {
			{"POST", glBase + "/merge_requests", jsonCT, `{"source_branch":"f","target_branch":"main","title":"t"}`, nil},
			{"POST", glBase + "/issues/3/notes", jsonCT, `{"body":"x"}`, nil},
		},
		types.PATForgeGitea: {
			{"POST", gtBase + "/pulls", jsonCT, `{"head":"f","base":"main","title":"t"}`, nil},
			{"POST", gtBase + "/issues/3/comments", jsonCT, `{"body":"x"}`, nil},
		},
	} {
		g := apiGrants[forge]
		g.Access = types.PATAccessRead
		h := newAPIHarness(t, forge, g)
		for _, q := range creates {
			rec := h.serve(t, q)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "read-only") {
				t.Fatalf("%s %s: status %d body %s, want the read-only refusal", forge, q.target, rec.Code, rec.Body.String())
			}
		}
		read := map[string]string{types.PATForgeGitLab: glBase + "/merge_requests", types.PATForgeGitea: gtBase + "/pulls"}[forge]
		if rec := h.serve(t, apiReq{method: "GET", target: read}); rec.Code != http.StatusOK {
			t.Fatalf("%s: read status %d (%s), want 200", forge, rec.Code, rec.Body.String())
		}
		if h.forgeHits() != 1 {
			t.Fatalf("%s: forge hits %d, want only the read", forge, h.forgeHits())
		}
	}
}

// An unnarrowed repos admits any repository path, but a project named by number
// and every unlisted operation stay refused.
func TestPATAPIUnnarrowedReposStillRefusesTheUnmappable(t *testing.T) {
	h := newAPIHarness(t, types.PATForgeGitLab, PATGrant{})
	if rec := h.serve(t, apiReq{method: "GET", target: "/api/v4/projects/any%2Fproject/issues"}); rec.Code != http.StatusOK {
		t.Fatalf("any repository: status %d, want 200", rec.Code)
	}
	for _, target := range []string{"/api/v4/projects/9/issues", "/api/v4/projects/any%2Fproject/variables", "/api/v4/projects"} {
		if rec := h.serve(t, apiReq{method: "GET", target: target}); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403", target, rec.Code)
		}
	}
	// "group/*" covers one further segment only.
	h = newAPIHarness(t, types.PATForgeGitLab, PATGrant{Repos: types.PATRepoSet("group/*")})
	for target, want := range map[string]int{
		"/api/v4/projects/group%2Fapp/issues":       http.StatusOK,
		"/api/v4/projects/group%2Fsub%2Fapp/issues": http.StatusForbidden,
		"/api/v4/projects/group%2F%2A/issues":       http.StatusForbidden,
	} {
		if rec := h.serve(t, apiReq{method: "GET", target: target}); rec.Code != want {
			t.Fatalf("%s: status %d, want %d", target, rec.Code, want)
		}
	}
}

func TestPATAPIBitbucketServerTable(t *testing.T) {
	const bb = "/rest/api/1.0/projects/TEAM/repos/app"
	g := PATGrant{Repos: types.PATRepoSet("TEAM/app")}
	pr := func(from, to string) string {
		return `{"title":"t","fromRef":{"id":"refs/heads/f","repository":{"slug":"` + from + `","project":{"key":"TEAM"}}},` +
			`"toRef":{"id":"refs/heads/main","repository":{"slug":"` + to + `","project":{"key":"TEAM"}}}}`
	}
	for _, c := range []struct {
		apiReq
		want int
	}{
		{apiReq{method: "GET", target: bb + "/pull-requests?state=OPEN"}, 200},
		{apiReq{method: "GET", target: bb + "/browse/src/main.go?at=main"}, 200},
		{apiReq{"POST", bb + "/pull-requests", jsonCT, pr("app", "app"), nil}, 200},
		{apiReq{"POST", bb + "/pull-requests/7/comments", jsonCT, `{"text":"ok"}`, nil}, 200},
		{apiReq{"POST", bb + "/pull-requests", jsonCT, pr("fork", "app"), nil}, 403},
		{apiReq{"POST", bb + "/pull-requests", jsonCT, pr("app", "other"), nil}, 403},
		{apiReq{"POST", bb + "/pull-requests/7/merge", jsonCT, `{}`, nil}, 403},
		{apiReq{method: "GET", target: "/rest/api/1.0/projects/TEAM/repos/other/pull-requests"}, 403},
		{apiReq{method: "GET", target: "/rest/api/1.0/projects/TEAM/repos/app/settings/hooks"}, 403},
		{apiReq{method: "GET", target: "/rest/api/1.0/projects/TEAM/repos/app/compare/changes?from=a&to=b&fromRepo=OTHER/app"}, 403},
		{apiReq{method: "PUT", target: bb + "/browse/a.txt"}, 403},
	} {
		h := newAPIHarness(t, types.PATForgeBitbucketServer, g)
		rec := h.serve(t, c.apiReq)
		if rec.Code != c.want {
			t.Fatalf("%s %s: status %d (%s), want %d", c.method, c.target, rec.Code, rec.Body.String(), c.want)
		}
		if want := map[bool]int{true: 1, false: 0}[c.want == 200]; h.forgeHits() != want {
			t.Fatalf("%s %s: forge hits %d, want %d", c.method, c.target, h.forgeHits(), want)
		}
		if c.want == 200 && h.up.gitHeaders.Get("Authorization") != "Bearer "+apiToken {
			t.Fatalf("forge saw %q, want the bearer PAT", h.up.gitHeaders.Get("Authorization"))
		}
	}
}

// Proxy start on an approval-gated API grant mints nothing, a refused request
// leaves the mint unspent, and the first admitted request mints once.
func TestPATAPIMintsLazilyAndOnlyForAnAdmittedRequest(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })

	up := newPATApprovalUpstream(t, apiToken, uuid.New(), 1)
	p, _ := newPATBrokerProxy(t, map[string]PATGrant{apiHost: {
		GrantID: uuid.New(), API: true, Forge: types.PATForgeGitLab, Repos: types.PATRepoSet("team/app"),
	}}, upstreamAddr(up.srv))
	h := &apiHarness{p: p}
	if up.mintCalls != 0 || up.pollCalls != 0 {
		t.Fatalf("proxy start: %d mint call(s) and %d approval poll(s), want none", up.mintCalls, up.pollCalls)
	}
	for _, q := range []apiReq{
		{method: "GET", target: "/api/v4/projects/team%2Fother/issues"},
		{method: "GET", target: glBase + "/variables"},
		{"POST", glBase + "/merge_requests", jsonCT, `{"target_project_id":5}`, nil},
	} {
		if rec := h.serve(t, q); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: status %d, want 403", q.method, q.target, rec.Code)
		}
	}
	if up.mintCalls != 0 || up.pollCalls != 0 || up.gitHits != 0 {
		t.Fatalf("after three refusals: %d mint call(s), %d poll(s), %d forge hit(s), want none", up.mintCalls, up.pollCalls, up.gitHits)
	}
	if rec := h.serve(t, apiReq{method: "GET", target: glBase + "/issues"}); rec.Code != http.StatusOK {
		t.Fatalf("admitted request: status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	// The 409 that raises the approval, then the re-mint once it is approved: one approval-gated mint.
	if up.mintCalls != 2 || up.gitHits != 1 || up.gitAuth != "" {
		t.Fatalf("after the admitted request: %d mint call(s), %d forge hit(s), basic auth %q, want 2, 1 and none", up.mintCalls, up.gitHits, up.gitAuth)
	}
	if rec := h.serve(t, apiReq{method: "GET", target: glBase + "/jobs/5"}); rec.Code != http.StatusOK || up.mintCalls != 2 {
		t.Fatalf("second admitted request: status %d, %d mint call(s), want 200 and the cached token", rec.Code, up.mintCalls)
	}
}

// An absolute-form plain-HTTP request to the API host is refused before any
// upstream send, whatever its path.
func TestPATAPIPlainLaneRefusesTheHost(t *testing.T) {
	h := newAPIHarness(t, types.PATForgeGitLab, apiGrants[types.PATForgeGitLab])
	for _, target := range []string{"http://" + apiHost + glBase + "/issues", "http://" + apiHost + ":8080/anything", "https://" + apiHost + "/api/v4/user"} {
		rec := httptest.NewRecorder()
		h.p.servePlain(rec, mustProxyReq(t, http.MethodGet, target))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CONNECT") {
			t.Fatalf("%s: status %d body %s, want the plain-lane refusal", target, rec.Code, rec.Body.String())
		}
	}
	if h.forgeHits() != 0 || h.mints() != 0 {
		t.Fatalf("plain lane cost %d forge hit(s) and %d mint call(s), want none", h.forgeHits(), h.mints())
	}
	if d := findDecision(t, h.log, ruleSourcePATAPIDenied); d.Decision != egress.Deny {
		t.Fatalf("decision %v, want deny", d.Decision)
	}
	// Another host is not the door's.
	if h.p.refusePATAPIPlain(httptest.NewRecorder(), mustProxyReq(t, http.MethodGet, "http://other.test/x")) {
		t.Fatal("the plain lane refused a host with no API grant")
	}
}

// The grant's host, on 443, is the only host the door terminates; any other
// CONNECT to it is refused rather than tunnelled blind.
func TestPATAPIHostIsTerminatedOnItsOwnPortOnly(t *testing.T) {
	h := newAPIHarness(t, types.PATForgeGitLab, apiGrants[types.PATForgeGitLab])
	if !h.p.isCorpMITMHost(apiHost) || !h.p.mitmPortAllowed(apiHost, 443) || h.p.mitmPortAllowed(apiHost, 8443) {
		t.Fatal("the API host must be terminated on 443 and on no other port")
	}
	if h.p.isCorpMITMHost("api."+apiHost) || h.p.isCorpMITMHost("gitlab.com") {
		t.Fatal("a derived or unrelated host became MITM-eligible")
	}
	rec := httptest.NewRecorder()
	if !h.p.refusePATAPITunnel(rec, mustProxyReq(t, http.MethodConnect, "http://"+apiHost+":8443"), apiHost, 8443) || rec.Code != http.StatusForbidden {
		t.Fatalf("a tunnel to the API host on another port: refused=%v status %d, want a 403", rec.Code == http.StatusForbidden, rec.Code)
	}
	if h.p.refusePATAPITunnel(httptest.NewRecorder(), mustProxyReq(t, http.MethodConnect, "http://other.test:443"), "other.test", 443) {
		t.Fatal("a tunnel to a host with no API grant was refused")
	}
	// No grant with api, no door: the host stays an ordinary egress host.
	plain := newAPIHarness(t, types.PATForgeGitLab, PATGrant{})
	plain.p.patAPI = nil
	if plain.p.isPATAPIHost(apiHost) {
		t.Fatal("a proxy with no API grant treats the host as an API host")
	}
}

// api needs a forge with a table and a MITM CA, so a config the door could not
// honour fails at start rather than as a credential riding an ungated path.
func TestLoadConfigRefusesAnAPIGrantTheDoorCannotHonour(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, _ := json.Marshal(string(certPEM))
	key, _ := json.Marshal(string(keyPEM))
	for _, tc := range []struct {
		name, grant, ca, want string
	}{
		{"gitlab with a CA", `{"grant_id":"` + uuid.NewString() + `","api":true,"forge":"gitlab"}`, `"mitm_ca_cert_pem":` + string(ca) + `,"mitm_ca_key_pem":` + string(key) + `,`, ""},
		{"gitea with a CA", `{"grant_id":"` + uuid.NewString() + `","api":true,"forge":"gitea","repos":["team/app"]}`, `"mitm_ca_cert_pem":` + string(ca) + `,"mitm_ca_key_pem":` + string(key) + `,`, ""},
		{"generic", `{"grant_id":"` + uuid.NewString() + `","api":true}`, `"mitm_ca_cert_pem":` + string(ca) + `,"mitm_ca_key_pem":` + string(key) + `,`, "no API table"},
		{"no CA", `{"grant_id":"` + uuid.NewString() + `","api":true,"forge":"gitlab"}`, ``, "mitm_ca_cert_pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:8080","run_token":"t",` + tc.ca +
				`"pat_grants":{"gitlab.com":` + tc.grant + `}}`))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadConfigBytes: %v, want it accepted", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("LoadConfigBytes error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}
