// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// adoResolveFixture is a captured sign-in (against the entra fake) plus a run
// whose dispatch authored one grant for dev.azure.com.
type adoResolveFixture struct {
	*adoFixture
	st      *adoTestStore
	runID   uuid.UUID
	grantID uuid.UUID
	subject string
}

func newADOResolveFixture(t *testing.T) *adoResolveFixture {
	t.Helper()
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	row := adoEntraTestRow()
	row.ID = f.cfg.RowID
	row.Entra.TenantID, row.Entra.ClientID = f.cfg.TenantID, f.cfg.ClientID
	st := &adoTestStore{site: adoSite(row)}
	f.srv.cfg.Store = st

	rf := &adoResolveFixture{adoFixture: f, st: st, runID: uuid.New(), subject: subject}
	ado, ok := resolveADOEntraRun(st.site, []string{adoTestRepo}, subject)
	if !ok {
		t.Fatal("fixture resolved no lane")
	}
	if _, _, ok := f.srv.authorADOEntraInjection(context.Background(), types.AgentRun{ID: rf.runID}, ado,
		"CERT", "KEY", &types.RunPolicySpec{}, map[string]string{}, nil); !ok {
		t.Fatal("fixture dispatch refused")
	}
	rf.grantID = st.grants[0].ID // dev.azure.com
	f.audit.rows = nil
	return rf
}

func (rf *adoResolveFixture) resolve(t *testing.T, sub, host string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+rf.grantID.String(), nil)
	handled := rf.srv.resolveADOInjection(w, r,
		&identity.Claims{RunID: rf.runID, Sub: sub, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-1", Injection: &egress.InjectionRule{Host: host, SecretName: types.ADOEntraAccessTokenSecret}},
		rf.grantID)
	if !handled {
		t.Fatal("the Azure DevOps sentinel was not handled by its own arm")
	}
	return w
}

// failureReason is the reason on the single secret.read failure row.
func (rf *adoResolveFixture) failureReason(t *testing.T) map[string]any {
	t.Helper()
	rows := rf.audit.find("secret.read")
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("secret.read rows = %+v, want exactly one failure", rows)
	}
	var d map[string]any
	_ = json.Unmarshal(rows[0].Data, &d)
	return d
}

// The granted scope string reaches the audit row, and the response carries the
// organisation and capabilities the proxy pins and gates on.
func TestResolveADOInjection_LiveRecordsGrantedScope(t *testing.T) {
	rf := newADOResolveFixture(t)
	var issued []string
	rf.fake.OnIssue(func(tok entrafake.IssuedToken) { issued = tok.Scopes })
	w := rf.resolve(t, rf.subject, "dev.azure.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Header != "Authorization" || !strings.HasPrefix(resp.Value, "Bearer fake-entra-access-") ||
		resp.Organisation != "contoso" || strings.Join(resp.Capabilities, ",") != "read,code_write" || resp.ExpiresAt == 0 {
		t.Errorf("response = %+v, want a bearer, organisation contoso, capabilities read,code_write, an expiry", resp)
	}
	rows := rf.audit.find("secret.read")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("secret.read rows = %+v, want one success", rows)
	}
	var d map[string]any
	_ = json.Unmarshal(rows[0].Data, &d)
	// The fake, like the real service, answers every consented scope (the
	// OIDC ones too, which the stored capture does not keep); the audit must
	// carry exactly that answer.
	want := strings.Join(issued, " ")
	if d["granted_scope"] != want {
		t.Errorf("audit granted_scope = %v, want the authority's own granted string %q", d["granted_scope"], want)
	}
	if strings.Contains(string(rows[0].Data), strings.TrimPrefix(resp.Value, "Bearer ")) {
		t.Fatal("the access token reached the audit row")
	}
}

// §2.8 (#1083): an access token lasting an hour is still advertised for no
// longer than the stored-key lease, so the proxy re-resolves within it.
func TestResolveADOInjection_AdvertisesTheStoredKeyLease(t *testing.T) {
	rf := newADOResolveFixture(t)
	w := rf.resolve(t, rf.subject, "dev.azure.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if lease := adoTestNow.Add(storedKeyTTL).UnixMilli(); resp.ExpiresAt == 0 || resp.ExpiresAt > lease {
		t.Fatalf("expires_at = %d, want at most now + storedKeyTTL (%d): the token's own hour outlives a deleted sign-in", resp.ExpiresAt, lease)
	}
}

// erasePerson drives DELETE /people/{principal}/credentials.
func erasePerson(t *testing.T, srv *Server, principal string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/people/"+principal+"/credentials", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("principal", principal)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	srv.handleErasePersonCredentials(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("erase %s: status %d body %q", principal, w.Code, w.Body.String())
	}
}

// Erasing the person's credentials evicts the access token cached from their
// sign-in: the next resolve reads the store and is refused, not served.
func TestResolveADOInjection_EraseEvictsTheCachedToken(t *testing.T) {
	rf := newADOResolveFixture(t)
	rf.srv.cfg.Store = secretOwnerDirectory{Store: rf.st, toks: []types.APIToken{{Principal: rf.subject}}}
	issued := 0
	rf.fake.OnIssue(func(entrafake.IssuedToken) { issued++ })
	for range 2 {
		if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
			t.Fatalf("warm: status %d body %q", w.Code, w.Body.String())
		}
	}
	if issued != 1 {
		t.Fatalf("redemptions = %d, want 1: the second resolve must come from the cache", issued)
	}

	erasePerson(t, rf.srv, rf.subject)
	rf.audit.rows = nil
	w := rf.resolve(t, rf.subject, "dev.azure.com")
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "fake-entra-access-") {
		t.Fatalf("after erase: status %d body %q, want 403 with no token: the cached token outlived the sign-in", w.Code, w.Body.String())
	}
	if d := rf.failureReason(t); d["reason"] != string(ADOEntraFailureNotCaptured) {
		t.Errorf("reason = %v, want %s", d["reason"], ADOEntraFailureNotCaptured)
	}
}

// forget drops one person's tokens and refusals, and nobody else's — not even
// an owner whose subject merely starts with theirs.
func TestADOEntraAccessCache_ForgetIsPerOwner(t *testing.T) {
	var c adoEntraAccessCache
	for _, owner := range []string{"al", "alice"} {
		c.put(owner+"\x00row\x00t\x00c\x00s", ADOEntraAccess{ExpiresAt: adoTestNow.Add(time.Hour)})
	}
	c.refused = map[string]time.Time{"al\x00row\x00t\x00c\x00s": adoTestNow, "alice\x00row\x00t\x00c\x00s": adoTestNow}
	c.forget("al")
	if _, ok := c.get("al\x00row\x00t\x00c\x00s", adoTestNow); ok || !c.refused["al\x00row\x00t\x00c\x00s"].IsZero() {
		t.Fatalf("al still cached (token %v, refusals %v)", ok, c.refused)
	}
	if _, ok := c.get("alice\x00row\x00t\x00c\x00s", adoTestNow); !ok || c.refused["alice\x00row\x00t\x00c\x00s"].IsZero() {
		t.Fatal("forgetting al evicted alice")
	}
}

func TestResolveADOInjection_RefusesOwnerMismatch(t *testing.T) {
	rf := newADOResolveFixture(t)
	w := rf.resolve(t, "mallory-sub", "dev.azure.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	if d := rf.failureReason(t); d["reason"] != "owner_not_caller" {
		t.Errorf("reason = %v, want owner_not_caller", d["reason"])
	}
	// #204: the audited reason must also reach the wire body, not just the
	// audit row — the proxy has no audit access and used to see only the
	// human sentence.
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason != "owner_not_caller" {
		t.Errorf("wire reason = %q, want owner_not_caller", body.Reason)
	}
}

// SNAPSHOT DRIFT is a refusal, never a substitution: each live-row change
// below answers 403 with the drifted field on the audit row.
func TestResolveADOInjection_RefusesSnapshotDrift(t *testing.T) {
	for drift, mutate := range map[string]func(*types.GitProvider){
		"tenant_id":          func(r *types.GitProvider) { r.Entra.TenantID = "99999999-0000-0000-0000-000000000001" },
		"client_id":          func(r *types.GitProvider) { r.Entra.ClientID = "99999999-0000-0000-0000-000000000002" },
		"organisation":       func(r *types.GitProvider) { r.BaseURLs = []string{"https://dev.azure.com/fabrikam"} },
		"capability_ceiling": func(r *types.GitProvider) { r.Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapRead} },
		"credential_source":  func(r *types.GitProvider) { r.CredentialSource = types.CredentialSourceShared },
		"token_mode":         func(r *types.GitProvider) { r.Entra.TokenMode = types.ADOTokenModeMintedPAT },
		"lane_withdrawn":     func(r *types.GitProvider) { r.Lanes = []types.GitLane{types.GitLanePAT} },
		"row_withdrawn":      func(r *types.GitProvider) { r.Disabled = true },
	} {
		t.Run(drift, func(t *testing.T) {
			rf := newADOResolveFixture(t)
			mutate(&rf.st.site.WorkspaceProviders.Git[0])
			w := rf.resolve(t, rf.subject, "dev.azure.com")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", w.Code)
			}
			if d := rf.failureReason(t); d["reason"] != "scope_changed" || d["drift"] != drift {
				t.Errorf("audit = %v, want scope_changed/%s", d, drift)
			}
		})
	}
}

func TestResolveADOInjection_PinsHostToOrganisation(t *testing.T) {
	for _, host := range []string{"fabrikam.visualstudio.com", "evil.example"} {
		rf := newADOResolveFixture(t)
		w := rf.resolve(t, rf.subject, host)
		if w.Code != http.StatusForbidden {
			t.Errorf("host %q: status %d, want 403", host, w.Code)
		}
		var body errorBody
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Reason != "host_not_organisation" {
			t.Errorf("host %q: wire reason = %q (%v), want host_not_organisation", host, body.Reason, err)
		}
	}
}

// Each redemption failure class answers with its own reason.
func TestResolveADOInjection_ClassifiesFailures(t *testing.T) {
	for want, arm := range map[ADOEntraFailure]func(*adoResolveFixture){
		ADOEntraFailureDeadCredential:      func(rf *adoResolveFixture) { rf.fake.SetInvalidGrant(true) },
		ADOEntraFailureConsentRequired:     func(rf *adoResolveFixture) { rf.fake.SetConsentRequired(true) },
		ADOEntraFailureInteractionRequired: func(rf *adoResolveFixture) { rf.fake.SetInteractionRequired(true) },
		ADOEntraFailureUnavailable:         func(rf *adoResolveFixture) { rf.fake.Close() },
		ADOEntraFailureNotCaptured: func(rf *adoResolveFixture) {
			rf.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
		},
	} {
		t.Run(string(want), func(t *testing.T) {
			rf := newADOResolveFixture(t)
			arm(rf)
			w := rf.resolve(t, rf.subject, "dev.azure.com")
			wantStatus := http.StatusForbidden
			if want == ADOEntraFailureUnavailable {
				wantStatus = http.StatusServiceUnavailable
			}
			if w.Code != wantStatus {
				t.Fatalf("status %d, want %d", w.Code, wantStatus)
			}
			if d := rf.failureReason(t); d["reason"] != string(want) {
				t.Errorf("reason = %v, want %s", d["reason"], want)
			}
		})
	}
}
