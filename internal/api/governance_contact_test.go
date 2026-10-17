// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestDecodeGovernanceProfileRequest_Contact(t *testing.T) {
	const prefix = `{"name":"p","ceiling":{"min_confinement_class":"CC2"}`
	tests := []struct {
		name    string
		body    string
		want    *policyref.Contact
		wantSet bool
		wantErr string
	}{
		{"absent keeps", prefix + `}`, nil, false, ""},
		{"null clears", prefix + `,"contact":null}`, nil, true, ""},
		{"empty object clears", prefix + `,"contact":{}}`, nil, true, ""},
		{"all-empty fields clear", prefix + `,"contact":{"owner":"","email":""}}`, nil, true, ""},
		{"set", prefix + `,"contact":{"owner":"Platform","email":"a@example.com"}}`,
			&policyref.Contact{Owner: "Platform", Email: "a@example.com"}, true, ""},
		{"unknown member", prefix + `,"contact":{"phone":"1"}}`, nil, false, "invalid contact"},
		{"not an object", prefix + `,"contact":"a@example.com"}`, nil, false, "invalid contact"},
		{"bad email", prefix + `,"contact":{"email":"x?cc=evil@example.com"}}`, nil, false, "email"},
		{"bad url", prefix + `,"contact":{"request_url":"javascript:alert(1)"}}`, nil, false, "request_url"},
		{"bidi owner", prefix + `,"contact":{"owner":"a\u202eb"}}`, nil, false, "owner"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/api/v1/governance/profiles/x", strings.NewReader(tc.body))
			req, msg := decodeGovernanceProfileRequest(httptest.NewRecorder(), r)
			if tc.wantErr != "" {
				if !strings.Contains(msg, tc.wantErr) {
					t.Fatalf("msg = %q, want it to contain %q", msg, tc.wantErr)
				}
				return
			}
			if msg != "" {
				t.Fatalf("refused: %s", msg)
			}
			if req.contactSet != tc.wantSet {
				t.Errorf("contactSet = %v, want %v", req.contactSet, tc.wantSet)
			}
			if (req.contact == nil) != (tc.want == nil) || (tc.want != nil && *req.contact != *tc.want) {
				t.Errorf("contact = %+v, want %+v", req.contact, tc.want)
			}
		})
	}
}

func TestHandlePutSiteConfig_PolicyHelp(t *testing.T) {
	const help = `{"owner":"Platform security","email":"sec@example.com","request_url":"https://help.example.com/access?team=a","request_text":"Ask in the queue."}`
	want := policyref.Contact{
		Owner: "Platform security", Email: "sec@example.com",
		RequestURL: "https://help.example.com/access?team=a", RequestText: "Ask in the queue.",
	}

	t.Run("round trip, never audited with owner or email", func(t *testing.T) {
		fake := &fakeSiteConfigStore{}
		srv, audit := newSiteConfigHarness(t, fake)
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"policy_help":`+help+`}`); w.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
		}
		w := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
		var got types.SiteConfig
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.PolicyHelp == nil || *got.PolicyHelp != want {
			t.Fatalf("GET policy_help = %+v, want %+v", got.PolicyHelp, want)
		}
		var found bool
		for _, ev := range audit.events {
			if ev.Action != "site_config.write" {
				continue
			}
			found = true
			var d map[string]any
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatal(err)
			}
			if d["policy_help_request_url"] != want.RequestURL {
				t.Errorf("policy_help_request_url = %v, want %q", d["policy_help_request_url"], want.RequestURL)
			}
			if f, _ := json.Marshal(d["policy_help_fields"]); string(f) != `["owner","email","request_url","request_text"]` {
				t.Errorf("policy_help_fields = %s", f)
			}
			if raw := string(ev.Data); strings.Contains(raw, "Platform security") || strings.Contains(raw, "sec@example.com") {
				t.Errorf("audit row carries the owner or email: %s", raw)
			}
		}
		if !found {
			t.Fatal("no site_config.write event")
		}
	})

	t.Run("absent keeps, null and empty clear", func(t *testing.T) {
		for _, tc := range []struct {
			body     string
			wantKept bool
		}{
			{`{"scm_hosts":["github.example.com"]}`, true},
			{`{"policy_help":null}`, false},
			{`{"policy_help":{}}`, false},
		} {
			fake := &fakeSiteConfigStore{cfg: types.SiteConfig{PolicyHelp: &want}}
			srv, _ := newSiteConfigHarness(t, fake)
			if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body); w.Code != http.StatusOK {
				t.Fatalf("PUT %s = %d %s", tc.body, w.Code, w.Body.String())
			}
			if kept := fake.putSeen.PolicyHelp != nil && *fake.putSeen.PolicyHelp == want; kept != tc.wantKept {
				t.Errorf("PUT %s: policy_help = %+v, kept=%v want %v", tc.body, fake.putSeen.PolicyHelp, kept, tc.wantKept)
			}
			if !tc.wantKept && fake.putSeen.PolicyHelp != nil {
				t.Errorf("PUT %s stored %+v, want none", tc.body, fake.putSeen.PolicyHelp)
			}
		}
	})

	t.Run("a bad value is a 400 that never reaches the store", func(t *testing.T) {
		for _, bad := range []string{
			`{"request_url":"http://help.example.com"}`,
			`{"email":"Ann <ann@example.com>"}`,
			`{"owner":"a\nb"}`,
		} {
			fake := &fakeSiteConfigStore{}
			srv, _ := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"policy_help":`+bad+`}`)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "policy_help.") {
				t.Errorf("policy_help %s = %d %s, want 400 naming policy_help", bad, w.Code, w.Body.String())
			}
			if fake.putSeen != nil {
				t.Errorf("policy_help %s reached the store", bad)
			}
		}
	})
}

// policy_help names a person and is for signed-in members only: the anonymous
// /healthz must never carry it.
func TestHealthz_OmitsPolicyHelp(t *testing.T) {
	st := &healthzHelpStore{fakeSiteConfigStore{cfg: types.SiteConfig{
		SignInHelpText: "Ask IT.",
		PolicyHelp:     &policyref.Contact{Owner: "Platform security", Email: "sec@example.com", RequestURL: "https://help.example.com/a"},
	}}}
	w := do(t, New(Config{Store: st}), http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("healthz = %d", w.Code)
	}
	for _, leak := range []string{"policy_help", "Platform security", "sec@example.com", "help.example.com"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("/healthz body carries %q: %s", leak, w.Body.String())
		}
	}
}
