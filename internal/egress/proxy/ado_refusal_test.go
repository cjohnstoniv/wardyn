// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// The phrases each class's message must carry, and the one the 401 must never.
const (
	adoMsgNotSignedIn = "not signed in to Azure DevOps"
	adoMsgRefused     = "may have expired, it may have been revoked, or it may be missing a permission"
	adoMsgNoAccess    = "your Azure DevOps account lacks access to "
	adoMsgScopeClaim  = "lacks the scope"
)

// adoRefusalCase is one live Azure DevOps refusal shape (F-LIVE-7) and what
// each door must make of it.
type adoRefusalCase struct {
	name   string
	fault  adofake.Fault
	status int // the status Azure DevOps answered
	class  adoUpstreamClass
	msg    string
}

var adoRefusalCases = []adoRefusalCase{
	{"SignInPage203", adofake.FaultSignInPage, http.StatusNonAuthoritativeInfo, adoUpstreamNotSignedIn, adoMsgNotSignedIn},
	{"Basic401", adofake.FaultBasicUnauthorized, http.StatusUnauthorized, adoUpstreamRefused, adoMsgRefused},
	{"BearerTF400813", adofake.FaultBearerTF400813, http.StatusUnauthorized, adoUpstreamRefused, adoMsgRefused},
	{"RepoNotFoundTF401019", adofake.FaultRepoNotFound, http.StatusNotFound, adoUpstreamNoAccess, adoMsgNoAccess},
	{"PolicyTF402455", adofake.FaultPolicyRejected, http.StatusForbidden, adoUpstreamNoAccess, adoMsgNoAccess},
}

// captureSlog routes slog into a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// The REST door: every refusal shape is recorded under its class and carries
// its sentence; the body of a 401/403/404 reaches the client unchanged, and a
// 203 sign-in page reaches it as a 401 in Azure DevOps' JSON error shape, so no
// client reads HTML as success.
func TestADORefusal_REST(t *testing.T) {
	for _, tc := range adoRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			h := newADOHarness(t, adoscope.CapRead)
			h.fake.SetFault(adofake.EndpointProjectsGet, tc.fault)
			rec := h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", nil)
			body := rec.Body.String()

			var said string
			if tc.class == adoUpstreamNotSignedIn {
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 for a 203 sign-in page. body=%s", rec.Code, body)
				}
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("Content-Type = %q, want JSON", ct)
				}
				var got adoRefusal
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.TypeKey != "NotSignedInException" {
					t.Fatalf("body is not Azure DevOps' error shape: %v %s", err, body)
				}
				if strings.Contains(body, "<html") {
					t.Errorf("the sign-in page reached the client: %s", body)
				}
				said = got.Message
			} else {
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want Azure DevOps' own %d. body=%s", rec.Code, tc.status, body)
				}
				want := h.fake.Requests()
				if len(want) != 1 {
					t.Fatalf("upstream saw %d requests, want 1", len(want))
				}
				if tc.fault == adofake.FaultBasicUnauthorized && body != "" {
					t.Errorf("the empty 401 body was changed to %q", body)
				}
				if tc.fault == adofake.FaultRepoNotFound && !strings.Contains(body, "TF401019") ||
					tc.fault == adofake.FaultPolicyRejected && !strings.Contains(body, "TF402455") ||
					tc.fault == adofake.FaultBearerTF400813 && !strings.Contains(body, "TF400813") {
					t.Errorf("Azure DevOps' body did not pass through: %q", body)
				}
				said = rec.Header().Get(egressHeaderDetail)
			}
			if !strings.Contains(said, tc.msg) {
				t.Errorf("message %q does not say %q", said, tc.msg)
			}
			if strings.Contains(said, adoMsgScopeClaim) {
				t.Errorf("message %q claims a scope problem", said)
			}
			if tc.class == adoUpstreamNoAccess && !strings.Contains(said, adoHost+"/acme/_apis/projects") {
				t.Errorf("message %q does not name what was refused", said)
			}
			log := h.log()
			if !strings.Contains(log, `"`+tc.class.ruleSource(false)+`"`) {
				t.Errorf("decision log does not name %s: %s", tc.class.ruleSource(false), log)
			}
			for name, s := range map[string]string{"body": body, "detail": said, "decision log": log, "slog": logs.String()} {
				if strings.Contains(s, adoToken) {
					t.Errorf("%s carries the token: %s", name, s)
				}
			}
		})
	}
}

// Shapes that are NOT a classified refusal pass through untouched on the REST
// door: a 203 that is not a sign-in page, and a 404 no identity stands behind.
func TestADORefusal_RESTUnclassifiedPassesThrough(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"JSON203", http.StatusNonAuthoritativeInfo}, {"Anonymous404", http.StatusNotFound}, {"Anonymous403", http.StatusForbidden}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newADOHarness(t, adoscope.CapRead)
			h.fake.SetOverride(adofake.EndpointProjectsGet, tc.status, []byte(`{"value":[]}`))
			rec := h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", nil)
			if rec.Code != tc.status || rec.Body.String() != `{"value":[]}` || rec.Header().Get(egressHeaderDetail) != "" {
				t.Fatalf("got %d %q detail=%q, want %d passed through untouched",
					rec.Code, rec.Body.String(), rec.Header().Get(egressHeaderDetail), tc.status)
			}
			if log := h.log(); strings.Contains(log, ":upstream-") {
				t.Errorf("an unclassified answer was recorded as a refusal: %s", log)
			}
		})
	}
}

// The git door, upload-pack side: every refusal shape on the advertisement
// reaches git as its sentence in plain text — never a credential prompt, never
// a 203 page taken as an answer — under its class's rule source.
func TestADORefusal_GitUploadPack(t *testing.T) {
	for _, tc := range adoRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newADOGitHarness(t, adoscope.CapRead)
			h.fake.SetFault(adofake.EndpointGitAdvertise, tc.fault)
			out, err := h.git(t, "clone", "https://dev.azure.com/acme/proj/_git/app", "app")
			mustBeGitRefusal(t, out, err, tc.msg)
			if strings.Contains(out, adoMsgScopeClaim) {
				t.Errorf("git output claims a scope problem:\n%s", out)
			}
			if tc.class == adoUpstreamNoAccess && !strings.Contains(out, "dev.azure.com/acme/proj/_git/app") {
				t.Errorf("git output does not name what was refused:\n%s", out)
			}
			if log := h.finish(t); !strings.Contains(log, `"`+tc.class.ruleSource(true)+`"`) {
				t.Errorf("decision log does not name %s: %s", tc.class.ruleSource(true), log)
			}
		})
	}
}

// The git door, receive-pack side: a refusal of the pack upload reaches git
// through the receive-pack result, as a rejected ref carrying the sentence.
func TestADORefusal_GitReceivePack(t *testing.T) {
	for _, tc := range adoRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
			dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
			h.fake.SetFault(adofake.EndpointGitReceivePack, tc.fault)
			out, err := h.push(t, dir, h.runBranch())
			mustBeGitRefusal(t, out, err, tc.msg)
			if !strings.Contains(out, "remote rejected") {
				t.Errorf("git did not report the rejected ref:\n%s", out)
			}
			if log := h.finish(t); !strings.Contains(log, `"`+tc.class.ruleSource(true)+`"`) {
				t.Errorf("decision log does not name %s: %s", tc.class.ruleSource(true), log)
			}
		})
	}
}
