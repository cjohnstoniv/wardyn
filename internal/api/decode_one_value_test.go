// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// encoding/json reads a key into the field it names, or else into one it names ignoring case
// (Unicode folding), so two spellings of one field keep the last, and two objects merge: a body
// with both is refused before anything is stored. One spelling on its own is read as before.
func TestPersonalWallsPutRefusesAFieldWrittenInTwoCases(t *testing.T) {
	srv, st, _ := uvServer(t)
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)
	key := wallsSub + "|" + types.PersonalWallsPrefKey
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/walls", member, `{"spec":{"denied_domains":["evil.example"]}}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	stored := string(st.prefs[key])
	for _, body := range []string{
		`{"spec":{"denied_domains":["evil.example"],"Denied_Domains":[]}}`,
		`{"spec":{"denied_domains":["evil.example"],"DENIED_DOMAINſ":[]}}`,
		`{"spec":{"denied_domains":["other.example"]},"SPEC":{}}`,
	} {
		w := doSSO(t, srv, http.MethodPut, "/api/v1/me/walls", member, body)
		if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonInvalidRequestBody {
			t.Errorf("PUT %s = %d %s, want 400 %s", body, w.Code, w.Body, reasonInvalidRequestBody)
		}
	}
	if got := string(st.prefs[key]); got != stored {
		t.Errorf("stored walls = %s, want them unchanged: %s", got, stored)
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/walls", member, `{"spec":{"Denied_Domains":["other.example"]}}`); w.Code != http.StatusOK ||
		!strings.Contains(string(st.prefs[key]), "other.example") {
		t.Errorf("one spelling in another case = %d %s, stored %s; want it read", w.Code, w.Body, st.prefs[key])
	}
}

// A template whose document writes intent twice in two cases is refused, and nothing is stored:
// the decode would keep the second, an empty intent, over the deny list.
func TestTemplateSaveRefusesAFieldWrittenInTwoCases(t *testing.T) {
	e := newTemplateEnv(t)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	doc := strings.TrimSuffix(templateDocBody(`{"inline_policy":{"denied_domains":["example.com"]}}`), "}") + `,"Intent":{}}`
	status, body := e.call(alice, http.MethodPost, "/api/v1/templates", `{"scope":"person","name":"twice","document":`+doc+`}`)
	if want := `the body.document has the key \"intent\" twice, written \"intent\" and \"Intent\"`; status != http.StatusBadRequest || !strings.Contains(body, want) {
		t.Errorf("save = %d %s, want 400 naming both spellings", status, body)
	}
	e.ts.mu.Lock()
	stored := len(e.ts.rows)
	e.ts.mu.Unlock()
	if stored != 0 {
		t.Errorf("%d templates stored, want none", stored)
	}
}

// A site config key in another letter case sets its field, as the decode reads it, so the stored
// value is not carried forward over it; the same field written in two cases is refused.
func TestSiteConfigPutReadsAKeyInAnotherCase(t *testing.T) {
	stored := types.SiteConfig{SignInHelpText: "Ask in #it-helpdesk."}
	fake := &fakeSiteConfigStore{cfg: stored}
	srv, _ := newSiteConfigHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"Sign_In_Help_Text":"Call the service desk."}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.SignInHelpText != "Call the service desk." {
		t.Errorf("the key in another case was carried forward over: %+v", fake.putSeen)
	}
	fake = &fakeSiteConfigStore{cfg: stored}
	srv, _ = newSiteConfigHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"sign_in_help_text":"Call the service desk.","SIGN_IN_HELP_TEXT":""}`); w.Code != http.StatusBadRequest || fake.putSeen != nil {
		t.Errorf("the field in two cases = %d %s, put %+v; want 400 and no write", w.Code, w.Body.String(), fake.putSeen)
	}
}

// A request body is one JSON value with no field written twice: a reader that decodes it on its
// own refuses a second value and a key written twice, in one case or two, before acting on it.
func TestBodyReadersRefuseASecondValueAndAKeyTwice(t *testing.T) {
	for name, rd := range map[string]struct {
		read              func(http.ResponseWriter, *http.Request) bool
		one, twice, case2 string
	}{
		"approve or deny": {func(w http.ResponseWriter, r *http.Request) bool { _, ok := decodeDecisionRequest(w, r); return ok },
			`{"decision_scope":"once"}`, `{"decision_scope":"once","decision_scope":"always"}`, `{"decision_scope":"once","Decision_Scope":"always"}`},
		"reject reason": {func(w http.ResponseWriter, r *http.Request) bool {
			_, ok := (&Server{}).readRejectReason(w, r)
			return ok
		},
			`{"reason":"not now"}`, `{"reason":"not now","reason":""}`, `{"reason":"not now","REASON":""}`},
		"build refresh": {func(w http.ResponseWriter, r *http.Request) bool { _, ok := readBuildRefresh(w, r); return ok },
			`{"refresh":true}`, `{"refresh":false,"refresh":true}`, `{"refresh":false,"Refresh":true}`},
		"org runner": {func(w http.ResponseWriter, r *http.Request) bool { _, ok := readAddOrgRunnerBody(w, r); return ok },
			`{"runs_namespace":"team-a"}`, `{"runs_namespace":"team-a","runs_namespace":"team-b"}`, `{"runs_namespace":"team-a","Runs_Namespace":"team-b"}`},
	} {
		call := func(body string) (bool, int) {
			w := httptest.NewRecorder()
			ok := rd.read(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
			return ok, w.Code
		}
		if ok, code := call(rd.one); !ok {
			t.Errorf("%s: one value = %d, want it read", name, code)
		}
		for _, body := range []string{rd.one + " " + rd.one, rd.one + " {}", rd.twice, rd.case2} {
			if ok, code := call(body); ok || code != http.StatusBadRequest {
				t.Errorf("%s: %s = ok %v, %d; want 400", name, body, ok, code)
			}
		}
	}
	// The shipped shell callers approve with no body, and a field this build does not know is ignored.
	for _, body := range []string{"", `{"reason":"looked at it","decision_note":"x"}`} {
		w := httptest.NewRecorder()
		if _, ok := decodeDecisionRequest(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))); !ok {
			t.Errorf("approve body %q = %d %s, want it read", body, w.Code, w.Body)
		}
	}

	h := newHarness(t)
	token := h.mintRunToken(t, uuid.New())
	one, _ := json.Marshal(egress.DecisionLog{Request: egress.Request{Host: "registry.example.com", Port: 443}, Decision: egress.Deny, RuleSource: "policy"})
	for _, body := range []string{string(one) + " " + string(one), `{"request":{"Host":"a.example","host":"b.example"},"decision":"deny"}`} {
		if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", token, body); w.Code != http.StatusBadRequest || errorReason(w) != reasonInternalDecisionLogInvalid {
			t.Errorf("decision log %s = %d %s, want 400 %s", body, w.Code, w.Body.String(), reasonInternalDecisionLogInvalid)
		}
	}
}

// The raise route reads one value with no field written twice. A bad call scope is answered by
// the call scope's own readers first, with their reason and auth.fail row, also when text follows.
func TestInternalRaiseReadsOneValue(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	one := `{"kind":"egress_domain","requested_scope":{"host":"pkg.example.com"}}`
	for _, body := range []string{
		one + " " + one,
		one + " junk",
		`{"kind":"egress_domain","kind":"egress_domain","requested_scope":{"host":"pkg.example.com"}}`,
		`{"kind":"egress_domain","KIND":"egress_domain","requested_scope":{"host":"pkg.example.com"}}`,
	} {
		if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, body); w.Code != http.StatusBadRequest ||
			errorReason(w) != reasonInternalApprovalRequestInvalid {
			t.Errorf("raise %s = %d %s, want 400 %s", body, w.Code, w.Body.String(), reasonInternalApprovalRequestInvalid)
		}
	}
	if len(h.approvals.requested) != 0 {
		t.Fatalf("%d approvals raised, want none", len(h.approvals.requested))
	}
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, one); w.Code != http.StatusCreated {
		t.Fatalf("one value = %d %s, want 201", w.Code, w.Body.String())
	}

	h = newHarness(t)
	tok = h.mintRunToken(t, uuid.New())
	badScope := `{"kind":"tool_call","requested_scope":{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{}}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, badScope+" junk")
	if w.Code != http.StatusBadRequest || errorReason(w) != "mcp_call_scope_invalid" || len(h.approvals.requested) != 0 {
		t.Fatalf("a bad call scope with text after it = %d %s, want 400 mcp_call_scope_invalid", w.Code, w.Body.String())
	}
	if ev := lastAuditEvent(t, h.audit.snapshot(), "auth.fail"); !strings.Contains(string(ev.Data), "mcp_call_scope_invalid") {
		t.Errorf("auth.fail row %s does not name mcp_call_scope_invalid", ev.Data)
	}
}

// A dry run reads its body as the write it previews does: one value, with no field written twice.
func TestDryRunBodyIsOneValue(t *testing.T) {
	for _, dst := range []func() any{
		func() any { return &unitDryRunRequest{} },
		func() any { return &membershipMoveDryRunRequest{} },
		func() any {
			return &struct {
				dryRunTarget
				types.BoundaryWrite
			}{}
		},
	} {
		if msg := strictJSON([]byte(`{"target":"unit"}`), dst()); msg != "" {
			t.Errorf("%T: one value: %s", dst(), msg)
		}
		for body, want := range map[string]string{
			`{"target":"unit"} {}`:                  "invalid JSON body: more than one JSON value",
			`{"target":"unit","target":"unit"}`:     `invalid JSON body: the body has the key "target" twice`,
			`{"target":"unit","TARGET":"boundary"}`: `invalid JSON body: the body has the key "target" twice, written "target" and "TARGET"`,
		} {
			if msg := strictJSON([]byte(body), dst()); msg != want {
				t.Errorf("%T: %s: %q, want %q", dst(), body, msg, want)
			}
		}
	}
}

// Template import reads its request as every other door does: one value, no field written twice.
func TestTemplateImportRequestIsOneValue(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, failingStore{}))
	one := `{"format":"json","source":"{}"}`
	if w := do(t, srv, http.MethodPost, "/api/v1/templates/import", adminToken, one); w.Code != http.StatusOK {
		t.Fatalf("one value = %d %s, want 200", w.Code, w.Body.String())
	}
	for _, body := range []string{one + " " + one, `{"format":"json","source":"{}","source":"{}"}`, `{"format":"json","source":"{}","Source":"{}"}`} {
		if w := do(t, srv, http.MethodPost, "/api/v1/templates/import", adminToken, body); w.Code != http.StatusBadRequest ||
			errorReason(w) != reasonInvalidRequestBody {
			t.Errorf("import %s = %d %s, want 400 %s", body, w.Code, w.Body.String(), reasonInvalidRequestBody)
		}
	}
}

// The branding PUT reads one value with no field written twice: the decode would save the first
// value, or the last spelling of the name, and the record is replaced whole.
func TestBrandingPutIsOneValue(t *testing.T) {
	one := brandingBody(t, nil)
	for body, want := range map[string]string{
		one + " " + one: "invalid JSON body: more than one JSON value",
		strings.TrimSuffix(one, "}") + `,"org_name":"Other Corp"}`: `invalid JSON body: the body has the key "org_name" twice`,
		strings.TrimSuffix(one, "}") + `,"Org_Name":"Other Corp"}`: `invalid JSON body: the body has the key "org_name" twice, written "org_name" and "Org_Name"`,
	} {
		srv, _, st := brandingServer(t)
		w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, body)
		if reason, msg := errReason(t, w.Body.Bytes()); w.Code != http.StatusBadRequest || reason != reasonInvalidRequestBody || msg != want {
			t.Errorf("PUT %s = %d %s %q, want 400 %s %q", body, w.Code, reason, msg, reasonInvalidRequestBody, want)
		}
		if st.rec != nil {
			t.Errorf("a refused write stored a record: %+v", st.rec)
		}
	}
	srv, _, st := brandingServer(t)
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, one); w.Code != http.StatusOK || st.rec == nil {
		t.Fatalf("one value = %d %s, want 200 and a record", w.Code, w.Body.String())
	}
}

// The ground-truth batch counts its events before decoding any: a batch over the limit is a 413
// even when its events are not events, which the decode would have refused only after building them.
func TestGroundtruthBatchCountsEventsFirst(t *testing.T) {
	h := newHarness(t)
	for body, want := range map[string]int{
		`{"events":[` + strings.Repeat("1,", 1000) + `1]}`: http.StatusRequestEntityTooLarge,
		`{"Events":[` + strings.Repeat("1,", 1000) + `1]}`: http.StatusRequestEntityTooLarge,
		`{"events":[` + strings.Repeat("1,", 999) + `1]}`:  http.StatusBadRequest,
	} {
		w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", h.mintGroundtruthToken(t), body)
		reason := reasonGroundtruthBatchInvalid
		if want == http.StatusRequestEntityTooLarge {
			reason = reasonGroundtruthBatchTooLarge
		}
		if w.Code != want || errorReason(w) != reason {
			t.Errorf("batch of %d bytes = %d %s, want %d %s", len(body), w.Code, w.Body.String(), want, reason)
		}
	}
}

// Every request body this package reads off the wire goes through types.DecodeOneJSON, directly or
// by decodeStrict: a json.Decoder made on the body stream reads its first value only and keeps the
// last of a key written twice.
func TestNoRequestBodyIsDecodedOffTheStream(t *testing.T) {
	stream := regexp.MustCompile(`json\.NewDecoder\((http\.MaxBytesReader|io\.LimitReader)\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if stream.MatchString(line) {
				t.Errorf("%s:%d decodes a request body with its own json.Decoder: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
