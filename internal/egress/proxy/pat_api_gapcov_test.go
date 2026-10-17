// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net"
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

// gapCovForge is a forge that records the Host and Authorization of each request
// it receives and answers the control plane's mint route with a git_pat.
type gapCovForge struct {
	srv  *httptest.Server
	mu   sync.Mutex
	host []string
	auth []string
}

func gapCovNewForge(t *testing.T, mintStatus int, mintBody string) *gapCovForge {
	t.Helper()
	f := &gapCovForge{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(mintStatus)
			_, _ = io.WriteString(w, mintBody)
			return
		}
		f.mu.Lock()
		f.host = append(f.host, r.Host)
		f.auth = append(f.auth, r.Header.Get("Private-Token"))
		f.mu.Unlock()
		_, _ = io.WriteString(w, "forge-ok")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *gapCovForge) hits() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.host...), append([]string(nil), f.auth...)
}

func gapCovMintOK() string {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return `{"kind":"git_pat","token":"` + apiToken + `","username":"oauth2","jti":"j","expires_at":"` + exp + `"}`
}

func gapCovProxy(t *testing.T, f *gapCovForge) (*Proxy, *apiHarness) {
	t.Helper()
	grant := PATGrant{GrantID: uuid.New(), API: true, Forge: types.PATForgeGitLab, Repos: types.PATRepoSet("team/app")}
	p, buf := newPATBrokerProxy(t, map[string]PATGrant{apiHost: grant}, upstreamAddr(f.srv))
	return p, &apiHarness{p: p, log: buf}
}

func gapCovGet(t *testing.T) *http.Request {
	t.Helper()
	return rawAPIRequest(t, "GET", glBase+"/issues", "", "", nil)
}

// A mint the control plane refuses is a 502 to the sandbox and a denied
// decision; the forge never sees the request.
func TestGapCovPATAPIMintFailureDeniesAndSendsNothing(t *testing.T) {
	f := gapCovNewForge(t, http.StatusInternalServerError, `{"error":"mint refused"}`)
	p, h := gapCovProxy(t, f)

	rec := httptest.NewRecorder()
	p.servePATAPI(rec, gapCovGet(t), apiHost, 443, p.patAPI[apiHost])

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "mint git_pat") {
		t.Fatalf("status %d body %q, want a 502 naming the failed mint", rec.Code, rec.Body.String())
	}
	if d := findDecision(t, h.log, ruleSourcePATAPIDenied); d.Decision != egress.Deny {
		t.Fatalf("decision %v, want deny", d.Decision)
	}
	if hosts, _ := f.hits(); len(hosts) != 0 {
		t.Fatalf("forge saw %d request(s) after a failed mint, want none", len(hosts))
	}
}

// A grant host the SSRF guard resolves to a private address is refused with a
// 403 after the mint and before anything is sent to it.
func TestGapCovPATAPIBlockedForgeHostIsRefusedBeforeSend(t *testing.T) {
	f := gapCovNewForge(t, http.StatusOK, gapCovMintOK())
	p, h := gapCovProxy(t, f)
	p.res = gapCovPrivateResolver{}

	rec := httptest.NewRecorder()
	p.servePATAPI(rec, gapCovGet(t), apiHost, 443, p.patAPI[apiHost])

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "vet forge host") {
		t.Fatalf("status %d body %q, want a 403 naming the host vet", rec.Code, rec.Body.String())
	}
	if d := findDecision(t, h.log, ruleSourcePATAPIDenied); d.Decision != egress.Deny {
		t.Fatalf("decision %v, want deny", d.Decision)
	}
	if hosts, _ := f.hits(); len(hosts) != 0 {
		t.Fatalf("forge saw %d request(s) for a blocked host, want none", len(hosts))
	}
}

type gapCovPrivateResolver struct{}

func (gapCovPrivateResolver) LookupIP(string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("10.1.2.3")}, nil
}

// A host that cannot form a URL is answered 502 as a failed request build, and
// is denied in the decision log.
func TestGapCovPATAPIUnbuildableUpstreamRequestIs502(t *testing.T) {
	f := gapCovNewForge(t, http.StatusOK, gapCovMintOK())
	p, h := gapCovProxy(t, f)

	rec := httptest.NewRecorder()
	p.servePATAPI(rec, gapCovGet(t), "bad host", 443, p.patAPI[apiHost])

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "build forge request") {
		t.Fatalf("status %d body %q, want a 502 naming the request build", rec.Code, rec.Body.String())
	}
	if d := findDecision(t, h.log, ruleSourcePATAPIDenied); d.Decision != egress.Deny {
		t.Fatalf("decision %v, want deny", d.Decision)
	}
	if hosts, _ := f.hits(); len(hosts) != 0 {
		t.Fatalf("forge saw %d request(s), want none", len(hosts))
	}
}

// A port other than the scheme's default is carried in the upstream Host, and
// the PAT rides in the forge's own header.
func TestGapCovPATAPINonDefaultPortIsKeptInTheUpstreamHost(t *testing.T) {
	f := gapCovNewForge(t, http.StatusOK, gapCovMintOK())
	p, _ := gapCovProxy(t, f)

	rec := httptest.NewRecorder()
	p.servePATAPI(rec, gapCovGet(t), apiHost, 8443, p.patAPI[apiHost])

	if rec.Code != http.StatusOK || rec.Body.String() != "forge-ok" {
		t.Fatalf("status %d body %q, want the forge's 200", rec.Code, rec.Body.String())
	}
	hosts, auth := f.hits()
	if len(hosts) != 1 || hosts[0] != apiHost+":8443" || auth[0] != apiToken {
		t.Fatalf("forge saw hosts %v, Private-Token %v; want one request for %s:8443 carrying the PAT", hosts, auth, apiHost)
	}
}

// With the token cached, a forge that stops answering is a 502 and a dial-failed
// decision, not a hang or a leaked error.
func TestGapCovPATAPIUpstreamFailureIs502WithDialFailedDecision(t *testing.T) {
	f := gapCovNewForge(t, http.StatusOK, gapCovMintOK())
	p, h := gapCovProxy(t, f)

	first := httptest.NewRecorder()
	p.servePATAPI(first, gapCovGet(t), apiHost, 443, p.patAPI[apiHost])
	if first.Code != http.StatusOK {
		t.Fatalf("priming request: status %d (%s), want 200", first.Code, first.Body.String())
	}
	f.srv.Close()

	rec := httptest.NewRecorder()
	p.servePATAPI(rec, gapCovGet(t), apiHost, 443, p.patAPI[apiHost])

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "forge api upstream error") {
		t.Fatalf("status %d body %q, want a 502 naming the upstream error", rec.Code, rec.Body.String())
	}
	if d := findDecision(t, h.log, "builtin:dial-failed"); d.Decision != egress.Deny {
		t.Fatalf("decision %v, want deny", d.Decision)
	}
}

// A grant whose forge has no API table is refused in the gate itself.
func TestGapCovPATAPIAdmitRefusesAForgeWithoutATable(t *testing.T) {
	r := rawAPIRequest(t, "GET", "/api/v4/user", "", "", nil)
	got := patAPIAdmit(r, PATGrant{Forge: types.PATForgeGeneric, API: true})
	if got != "this grant's forge has no API table" {
		t.Fatalf("patAPIAdmit = %q, want the no-table refusal", got)
	}
}

// bitbucketCheck skips a ref that names no repository and refuses one whose
// repository is another than the path's.
func TestGapCovBitbucketCheckRefs(t *testing.T) {
	create := patAPIRow{kind: patAPICreate}
	f := func(from, to any) *patAPIFields {
		return &patAPIFields{obj: map[string]any{"fromRef": from, "toRef": to}}
	}
	repo := func(key, slug string) any {
		return map[string]any{"repository": map[string]any{"slug": slug, "project": map[string]any{"key": key}}}
	}
	cases := []struct {
		name string
		row  patAPIRow
		f    *patAPIFields
		want string
	}{
		{"refs without a repository object", create, f(map[string]any{"id": "refs/heads/a"}, "not an object"), ""},
		{"both refs name the path's repository", create, f(repo("PRJ", "app"), repo("prj", "APP")), ""},
		{"from ref names another slug", create, f(repo("PRJ", "other"), repo("PRJ", "app")), "the pull request's fromRef names another repository"},
		{"to ref names another project", create, f(repo("PRJ", "app"), repo("ELSE", "app")), "the pull request's toRef names another repository"},
		{"a non-create row is not judged", patAPIRow{kind: patAPIRead}, f(repo("ELSE", "x"), nil), ""},
	}
	for _, c := range cases {
		if got := bitbucketCheck(c.row, nil, "PRJ/app", c.f); got != c.want {
			t.Errorf("%s: bitbucketCheck = %q, want %q", c.name, got, c.want)
		}
	}
}

func gapCovCollect(t *testing.T, target, ctype, body string) (*patAPIFields, string) {
	t.Helper()
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	return collectPATAPIFields(r)
}

// Every body the door cannot read whole is refused with its own clause.
func TestGapCovCollectPATAPIFieldsRefusals(t *testing.T) {
	deep := strings.Repeat("[", 40) + strings.Repeat("]", 40)
	mp := func(parts ...string) string { return strings.Join(parts, "") + "--B--\r\n" }
	field := func(name, val string) string {
		return "--B\r\nContent-Disposition: form-data; name=\"" + name + "\"\r\n\r\n" + val + "\r\n"
	}
	manyParts := strings.Repeat(field("a", "1"), patAPIMaxParts+1)
	cases := []struct {
		name, target, ctype, body, why string
	}{
		{"query that does not parse", "/x?a=%zz", "", "", "the query string cannot be parsed"},
		{"data after the json value", "/x", jsonCT, `{"a":1} {"b":2}`, "its body cannot be parsed: data after the JSON value"},
		{"whitespace-only json", "/x", jsonCT, " ", "its body cannot be parsed: EOF"},
		{"truncated json", "/x", jsonCT, `{"a":1,`, "its body cannot be parsed: "},
		{"json nested too deeply", "/x", jsonCT, deep, "its body cannot be parsed: the JSON is nested too deeply"},
		{"repeated key inside a nested object", "/x", jsonCT, `{"a":{"b":1,"b":2}}`, `its body cannot be parsed: the key "b" appears twice`},
		{"json scalar", "/x", jsonCT, `7`, "its body cannot be parsed: the body is not a JSON object"},
		{"multipart with no boundary", "/x", "multipart/form-data", field("a", "1"), "its body cannot be parsed: no multipart boundary"},
		{"multipart with malformed part headers", "/x", "multipart/form-data; boundary=B", "--B\r\nbroken header line\r\n\r\nx\r\n--B--\r\n", "its body cannot be parsed: "},
		{"multipart part cut short", "/x", "multipart/form-data; boundary=B", "--B\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\ntruncated", "its body cannot be parsed: "},
		{"multipart over the part cap", "/x", "multipart/form-data; boundary=B", mp(manyParts), "its body cannot be parsed: too many parts"},
	}
	for _, c := range cases {
		f, why := gapCovCollect(t, c.target, c.ctype, c.body)
		if why == "" || !strings.HasPrefix(why, c.why) {
			t.Errorf("%s: why = %q, want it to start %q", c.name, why, c.why)
		}
		if f != nil {
			t.Errorf("%s: a refused body returned fields", c.name)
		}
	}
}

// A multipart file part keeps its field name but not its bytes.
func TestGapCovCollectPATAPIFieldsMultipartFileKeepsOnlyTheName(t *testing.T) {
	body := "--B\r\nContent-Disposition: form-data; name=\"Upload\"; filename=\"a.txt\"\r\n\r\nsecret-bytes\r\n--B--\r\n"
	f, why := gapCovCollect(t, "/x", "multipart/form-data; boundary=B", body)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	if !f.keys["upload"] || len(f.vals["upload"]) != 1 || f.vals["upload"][0] != "" {
		t.Fatalf("keys %v vals %v, want the field name kept with an empty value", f.keys, f.vals)
	}
}
