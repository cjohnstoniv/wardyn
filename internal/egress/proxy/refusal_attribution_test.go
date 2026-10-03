// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	teamA = &policyref.Ref{Source: policyref.SourceProfile, Name: "Team A", Owner: "Pat Owner",
		Email: "pat@example.com", RequestURL: "https://help.example.com/access"}
	deploymentRef = &policyref.Ref{Source: policyref.SourceDeployment, RequestURL: "https://help.example.com/access"}
)

// attributedProxy is a proxy with no listeners, enough to call a refusal writer.
func attributedProxy(ref *policyref.Ref) *Proxy {
	return newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Attribution: ref})
}

func denyWith(p *Proxy, reason string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.writeEgressDeny(rec, "blocked.test", 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: reason}, false)
	return rec
}

// The four reasons the policy decides carry both headers and the one line; every
// fault reason, and the evaluator error, answers exactly as it does with no
// attribution at all.
func TestWriteEgressDenyAttributesOnlyPolicyDecisions(t *testing.T) {
	const golden = "egress denied by policy\n" +
		`This run is governed by the profile "Team A". To request a change: https://help.example.com/access` + "\n"
	for _, reason := range []string{"policy:denied", "policy:default-deny", "policy:method", "approval:denied"} {
		t.Run(reason, func(t *testing.T) {
			rec := denyWith(attributedProxy(teamA), reason)
			if rec.Code != http.StatusForbidden || rec.Body.String() != golden {
				t.Errorf("code %d body %q, want 403 and %q", rec.Code, rec.Body.String(), golden)
			}
			if got := rec.Header().Get("X-Wardyn-Policy"); got != `profile; name="Team%20A"` {
				t.Errorf("X-Wardyn-Policy = %q", got)
			}
			if got := rec.Header().Get("X-Wardyn-Policy-Request"); got != "https://help.example.com/access" {
				t.Errorf("X-Wardyn-Policy-Request = %q", got)
			}
			if strings.Contains(rec.Body.String(), "Pat Owner") || strings.Contains(rec.Body.String(), "pat@example.com") {
				t.Errorf("the body names the owner: %q", rec.Body.String())
			}
		})
	}
	for _, reason := range []string{"builtin:private-ip", "builtin:resolve-failed", "builtin:dial-failed", "policy:evaluator-error", ""} {
		t.Run("fault "+reason, func(t *testing.T) {
			got, want := denyWith(attributedProxy(teamA), reason), denyWith(attributedProxy(nil), reason)
			if got.Body.String() != want.Body.String() || got.Code != want.Code {
				t.Errorf("body %q code %d, want %q code %d", got.Body.String(), got.Code, want.Body.String(), want.Code)
			}
			if !headersEqual(got.Header(), want.Header()) {
				t.Errorf("headers %v, want %v", got.Header(), want.Header())
			}
		})
	}
}

func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if strings.Join(v, ",") != strings.Join(b[k], ",") {
			return false
		}
	}
	return true
}

// A run with no attribution answers byte for byte as before, and the deny paths
// the proxy serves (plain HTTP and CONNECT) both reach the attributed writer.
func TestEgressDenyThroughTheProxyIsAttributed(t *testing.T) {
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}, DeniedDomains: []string{"bad.test"}}, "127.0.0.1:1", nil, nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://denied.test/"))
	if rec.Body.String() != "egress denied by policy\n" || rec.Header().Get("X-Wardyn-Policy") != "" {
		t.Errorf("an unattributed run changed: %q %v", rec.Body.String(), rec.Header())
	}

	p.attribution = teamA
	for name, req := range map[string]*http.Request{
		"default-deny": mustProxyReq(t, http.MethodGet, "http://denied.test/"),
		"deny list":    mustProxyReq(t, http.MethodGet, "http://bad.test/"),
		"connect":      connectReq(t, "denied.test:443"),
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Header().Get("X-Wardyn-Policy") != `profile; name="Team%20A"` ||
			!strings.Contains(rec.Body.String(), `governed by the profile "Team A"`) {
			t.Errorf("%s: not attributed: %q %v", name, rec.Body.String(), rec.Header())
		}
	}
}

func TestAttributionRendering(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ref        *policyref.Ref
		wantPolicy string
		wantReq    string
		wantLine   string
	}{
		{"non-ASCII name travels percent-encoded", &policyref.Ref{Source: policyref.SourceProfile, Name: "Équipe ☂", RequestURL: "https://h.example.com/x"},
			"profile; name=\"%C3%89quipe%20%E2%98%82\"", "https://h.example.com/x", `This run is governed by the profile "Équipe ☂". To request a change: https://h.example.com/x`},
		{"deployment default", deploymentRef, "deployment", "https://help.example.com/access",
			"This run is governed by this deployment's default policy. To request a change: https://help.example.com/access"},
		{"mailto from the email", &policyref.Ref{Source: policyref.SourceProfile, Name: "T", Email: "pat@example.com"},
			`profile; name="T"`, "mailto:pat@example.com", `This run is governed by the profile "T". To request a change: mailto:pat@example.com`},
		{"request text when there is no route", &policyref.Ref{Source: policyref.SourceDeployment, RequestText: "ask the platform desk"},
			"deployment", "", "This run is governed by this deployment's default policy. To request a change: ask the platform desk"},
		{"nothing to say about a route", &policyref.Ref{Source: policyref.SourceProfile, Name: "T"},
			`profile; name="T"`, "", `This run is governed by the profile "T".`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			line := attributedProxy(tc.ref).attributeRefusal(rec)
			if line != tc.wantLine {
				t.Errorf("line = %q, want %q", line, tc.wantLine)
			}
			if got := rec.Header().Get("X-Wardyn-Policy"); got != tc.wantPolicy {
				t.Errorf("X-Wardyn-Policy = %q, want %q", got, tc.wantPolicy)
			}
			if got := rec.Header().Get("X-Wardyn-Policy-Request"); got != tc.wantReq {
				t.Errorf("X-Wardyn-Policy-Request = %q, want %q", got, tc.wantReq)
			}
			for k, v := range rec.Header() {
				for _, s := range v {
					for _, r := range s {
						if r > 0x7e || r < 0x20 {
							t.Errorf("header %s carries a non-ASCII or control rune in %q", k, s)
						}
					}
				}
			}
		})
	}
	rec := httptest.NewRecorder()
	if line := attributedProxy(nil).attributeRefusal(rec); line != "" || len(rec.Header()) != 0 {
		t.Errorf("no attribution wrote %q %v", line, rec.Header())
	}
}

func assertAttributed(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body %q", rec.Code, wantStatus, rec.Body.String())
	}
	if rec.Header().Get("X-Wardyn-Policy") != `profile; name="Team%20A"` || rec.Header().Get("X-Wardyn-Policy-Request") != "https://help.example.com/access" {
		t.Errorf("headers = %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), `This run is governed by the profile \"Team A\". To request a change: https://help.example.com/access`) &&
		!strings.Contains(rec.Body.String(), `This run is governed by the profile "Team A". To request a change: https://help.example.com/access`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func assertUntouched(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Header().Get("X-Wardyn-Policy") != "" || strings.Contains(rec.Body.String(), "governed by") {
		t.Errorf("a fault refusal names a policy: %v %q", rec.Header(), rec.Body.String())
	}
}

func TestGitBrokerRefusalsAreAttributed(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	notGranted := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/gh/other/repo/info/refs?service=git-upload-pack", nil))
		return rec
	}
	if rec := notGranted(); rec.Body.String() != "repository not granted to this run\n" || rec.Header().Get("X-Wardyn-Policy") != "" {
		t.Errorf("an unattributed refusal changed: %q %v", rec.Body.String(), rec.Header())
	}
	p.attribution = teamA
	assertAttributed(t, notGranted(), http.StatusForbidden)

	// branch-namespace: a push to the default branch.
	body := recordedPush(t, "refs/heads/main", map[string]string{"a.txt": "x\n"})
	rec := postPush(t, p, string(body))
	assertAttributed(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "outside this run's branch namespace") {
		t.Errorf("not the branch-namespace refusal: %q", rec.Body.String())
	}

	// the 415 encoding refusal is a fault and stays byte-identical.
	enc := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack", strings.NewReader("x"))
	req.Header.Set("Content-Encoding", "gzip")
	p.ServeHTTP(enc, req)
	if enc.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", enc.Code)
	}
	assertUntouched(t, enc)
	if enc.Body.String() != "wardyn: cannot enforce branch-namespace confinement on a gzip-encoded push body\n" {
		t.Errorf("encoding refusal changed: %q", enc.Body.String())
	}
}

func TestPushRuleRefusalsAreAttributed(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	grants := map[string]uuid.UUID{"octocat/hello-world": uuid.New()}

	denied := contentRulesSpec(".github/workflows/**")
	p, _ := newGitBrokerProxyWithSpec(t, grants, upstreamAddr(up.srv), denied)
	p.attribution = teamA
	push := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{".github/workflows/ci.yml": "on: push\n"})
	assertAttributed(t, postPush(t, p, string(push)), http.StatusForbidden)

	big := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{"nothing/"}, MaxFileSizeMiB: 1}}
	pb, _ := newGitBrokerProxyWithSpec(t, grants, upstreamAddr(up.srv), big)
	pb.attribution = teamA
	large := recordedPush(t, BranchNSPrefix(pb.runID)+"work", map[string]string{"big.bin": strings.Repeat("0123456789abcdef", 1<<17)})
	assertAttributed(t, postPush(t, pb, string(large)), http.StatusForbidden)

	// An uninspectable push is the inspector's fault: no policy named.
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack", strings.NewReader("x"))
	req.Header.Set("Content-Encoding", "gzip")
	p.ServeHTTP(rec, req)
	assertUntouched(t, rec)
}

func TestPATBrokerRefusalIsAttributed(t *testing.T) {
	up := newPATBrokerUpstream(t, "pat", "oauth2")
	p, _ := newPATBrokerProxy(t, map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))
	notGranted := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/git/other.example/org/repo.git/info/refs?service=git-upload-pack", nil))
		return rec
	}
	if rec := notGranted(); rec.Body.String() != "host not granted to this run\n" || rec.Header().Get("X-Wardyn-Policy") != "" {
		t.Errorf("an unattributed refusal changed: %q %v", rec.Body.String(), rec.Header())
	}
	p.attribution = teamA
	assertAttributed(t, notGranted(), http.StatusForbidden)
}

func TestADORefusalsAreAttributed(t *testing.T) {
	h := newADOHarness(t, adoscope.CapCodeRead)
	patch := func() *httptest.ResponseRecorder {
		return h.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1", `[{"op":"add","path":"/fields/System.Title","value":"x"}]`, nil)
	}
	plain := patch()
	h.p.attribution = teamA
	rec := patch()

	var before, after adoRefusal
	if err := json.Unmarshal(plain.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("not Azure DevOps' shape: %v: %s", err, rec.Body.String())
	}
	if after.TypeKey != before.TypeKey || rec.Code != plain.Code {
		t.Errorf("typeKey %q status %d, want %q %d", after.TypeKey, rec.Code, before.TypeKey, plain.Code)
	}
	if after.Message != before.Message+` This run is governed by the profile "Team A". To request a change: https://help.example.com/access` {
		t.Errorf("message = %q", after.Message)
	}
	if rec.Header().Get("X-Wardyn-Policy") != `profile; name="Team%20A"` || rec.Header().Get("X-Wardyn-Policy-Request") == "" {
		t.Errorf("headers = %v", rec.Header())
	}
	if plain.Header().Get("X-Wardyn-Policy") != "" {
		t.Errorf("an unattributed refusal carries a policy header: %v", plain.Header())
	}
}

func TestADOGitRefusalIsAttributed(t *testing.T) {
	refuse := func(p *Proxy) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := mustLocalReq(t, http.MethodGet, "/wardyn/git/dev.azure.com/acme/proj/_git/app/info/refs", nil)
		if p.refuseADOGit(rec, r, "dev.azure.com", nil, nil, "Wardyn refused this git request: no capability") {
			t.Fatal("refused request reported as forwarded")
		}
		return rec
	}
	plain, got := refuse(attributedProxy(nil)), refuse(attributedProxy(teamA))
	if plain.Code != got.Code || plain.Code != http.StatusForbidden {
		t.Fatalf("status %d vs %d, want 403", got.Code, plain.Code)
	}
	if got.Body.String() != strings.TrimSuffix(plain.Body.String(), "\n")+
		` This run is governed by the profile "Team A". To request a change: https://help.example.com/access`+"\n" {
		t.Errorf("body = %q (unattributed %q)", got.Body.String(), plain.Body.String())
	}
	if got.Header().Get("X-Wardyn-Policy") != `profile; name="Team%20A"` || plain.Header().Get("X-Wardyn-Policy") != "" {
		t.Errorf("headers = %v / %v", got.Header(), plain.Header())
	}
}

// reprojectAttribution drops what Project refuses and never stops the sidecar.
func TestConfigAttributionIsReprojected(t *testing.T) {
	base := `{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:1","run_token":"t","policy":{},` +
		`"attribution":`
	cfg, err := LoadConfigBytes([]byte(base + `{"source":"profile","name":"Team A","request_url":"javascript:alert(1)"}}`))
	if err != nil {
		t.Fatalf("a bad request_url stopped the sidecar: %v", err)
	}
	if cfg.Attribution == nil || cfg.Attribution.Name != "Team A" || cfg.Attribution.RequestURL != "" {
		t.Fatalf("attribution = %+v, want the name kept and the bad url dropped", cfg.Attribution)
	}
	p := attributedProxy(cfg.Attribution)
	rec := httptest.NewRecorder()
	p.attributeRefusal(rec)
	if rec.Header().Get("X-Wardyn-Policy") == "" || rec.Header().Get("X-Wardyn-Policy-Request") != "" {
		t.Errorf("headers = %v, want the policy header and no request header", rec.Header())
	}

	cfg, err = LoadConfigBytes([]byte(base + `{"source":"admin","name":"x"}}`))
	if err != nil || cfg.Attribution != nil {
		t.Errorf("an unknown source: %+v, %v; want it dropped and the sidecar started", cfg.Attribution, err)
	}
	if _, err = LoadConfigBytes([]byte(base + `{"source":"profile","bogus":1}}`)); err == nil {
		t.Error("an unknown attribution key was accepted; the decode must stay strict")
	}
}
