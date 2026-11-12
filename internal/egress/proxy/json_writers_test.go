// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestJSONWriters_StatusContentTypeAndBodyPerDoor pins what each door's JSON
// writer puts on the wire (status, Content-Type, exact body), so the shared
// writer cannot change one door's answer.
func TestJSONWriters_StatusContentTypeAndBodyPerDoor(t *testing.T) {
	const plain, charset = "application/json", "application/json; charset=utf-8"
	adoBody := func(typeKey, msg string) string {
		return `{"$id":"1","innerException":null,"message":"` + msg + `","typeName":"Wardyn.Egress.` + typeKey + `, Wardyn","typeKey":"` + typeKey + `","errorCode":0,"eventId":3000}` + "\n"
	}
	r := httptest.NewRequest(http.MethodGet, "http://x.test/p", nil)
	p := &Proxy{now: func() time.Time { return time.Unix(0, 0) }}
	adoNotSignedIn := &http.Response{StatusCode: http.StatusNonAuthoritativeInfo, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader("<html>"))}

	cases := []struct {
		name   string
		write  func(w http.ResponseWriter)
		status int
		ctype  string
		body   string
	}{
		{"ado gate refusal", func(w http.ResponseWriter) {
			writeADORefusal(w, http.StatusForbidden, "CapabilityNotGrantedException", "no")
		}, 403, charset, adoBody("CapabilityNotGrantedException", "no")},
		{"ado upstream not signed in", func(w http.ResponseWriter) {
			p.relayUpstream(w, r, "dev.azure.com", 443, adoNotSignedIn, ruleSourceADO)
		}, 401, charset, adoBody("NotSignedInException", "Wardyn: "+adoUpstreamNotSignedIn.message("dev.azure.com/p", 203))},
		{"azure arm refusal", func(w http.ResponseWriter) {
			p.refuseAzureARM(w, r, "management.azure.com", 443, "c", "m")
		}, 403, plain, `{"error":{"code":"c","message":"m"},"wardyn":"c"}` + "\n"},
		{"azure login refusal", func(w http.ResponseWriter) {
			p.refuseAzureARM(w, r, AzureLoginHost, 443, "c", "m")
		}, 403, plain, `{"error":"access_denied","error_description":"m","wardyn":"c"}` + "\n"},
		{"azure gate", func(w http.ResponseWriter) { writeAzureJSON(w, http.StatusBadGateway, "c", "d") },
			502, plain, `{"detail":"d","wardyn":"c"}` + "\n"},
		{"github api", func(w http.ResponseWriter) { p.refuseGitHubAPI(w, r, "api.github.com", 443, "m") },
			403, plain, `{"message":"m","wardyn":"` + gitHubAPIRefused + `"}` + "\n"},
		{"kube api", func(w http.ResponseWriter) { p.refuseKubeAPI(w, r, "k.test", 443, "c", "m") },
			403, plain, `{"apiVersion":"v1","code":403,"kind":"Status","message":"m","metadata":{},"reason":"Forbidden","status":"Failure","wardyn":"c"}` + "\n"},
		{"scan blocked", func(w http.ResponseWriter) { writeScanBlocked(w, 2, []string{"secret"}, "why") },
			403, plain, `{"wardyn":"llm_content_blocked","findings":2,"categories":["secret"],"reason":"why"}` + "\n"},
		{"mcp json-rpc", func(w http.ResponseWriter) { writeJSONRPC(w, map[string]any{"jsonrpc": "2.0"}) },
			200, plain, `{"jsonrpc":"2.0"}` + "\n"},
		{"pat api", func(w http.ResponseWriter) { p.refusePATAPI(w, r, "h.test", 443, "why") },
			403, plain, `{"message":"Wardyn refused this forge API request: why.","wardyn":"` + patAPIRefused + `"}` + "\n"},
		{"tool decision", func(w http.ResponseWriter) { writeToolDecision(w, "APPROVED") },
			200, plain, `{"decided_by":"policy","state":"APPROVED"}` + "\n"},
		{"resident mint", func(w http.ResponseWriter) {
			p.answerResidentMint(w, r, ResidentMint{GrantID: uuid.Nil, Kind: types.GrantGitPAT, Token: "t", Username: "u",
				ExpiresAt: time.Unix(3600, 0).UTC()})
		}, 200, plain, `{"kind":"` + string(types.GrantGitPAT) + `","token":"t","username":"u","expires_at":"` + time.Unix(3600, 0).UTC().Format(time.RFC3339) + `"}` + "\n"},
		// The sites below keep their own writers: they differ in body bytes
		// (no trailing newline), headers or status handling.
		{"approval pending", func(w http.ResponseWriter) { writeApprovalPending(w, nil) },
			403, plain, `{"wardyn":"approval-pending","approval_id":""}`},
		{"aws sdk error", func(w http.ResponseWriter) { writeAWSSDKError(w, 400, "T", "m") },
			400, plain, `{"__type":"T","message":"m"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.write(rec)
			if rec.Code != c.status {
				t.Errorf("status = %d, want %d", rec.Code, c.status)
			}
			if got := rec.Header().Get("Content-Type"); got != c.ctype {
				t.Errorf("Content-Type = %q, want %q", got, c.ctype)
			}
			if got := rec.Body.String(); got != c.body {
				t.Errorf("body = %q, want %q", got, c.body)
			}
		})
	}
}
