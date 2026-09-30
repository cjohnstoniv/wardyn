// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const adoDisconnectPath = "/api/v1/scm/azure-devops/connection"

// adoConsoleStore is the minted-token fixture's store with no capability
// grants (noGovernanceStore's answers), so the reads that filter rows by
// capability resolve.
type adoConsoleStore struct{ *adoPATStore }

func (adoConsoleStore) ListCapabilityGrantsFor(context.Context, []string, []string, string) ([]types.CapabilityGrant, error) {
	return nil, nil
}

func (adoConsoleStore) ListGroupDenyGrants(context.Context, string) ([]types.CapabilityGrant, error) {
	return nil, nil
}

func (adoConsoleStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (adoConsoleStore) ListCapabilityRestrictions(context.Context) (map[string]map[string]bool, error) {
	return map[string]map[string]bool{}, nil
}

// newADOConsoleLane is newADOPATLane on adoConsoleStore; dispatched runs the
// run's dispatch too.
func newADOConsoleLane(t *testing.T, dispatched bool) *adoPATFixture {
	t.Helper()
	fx := newADOPATLane(t)
	fx.srv.cfg.Store = adoConsoleStore{fx.st}
	if dispatched && !fx.dispatch(t) {
		t.Fatalf("dispatch refused: %q", fx.st.hint)
	}
	return fx
}

// scmAccessJSON is the fixture person's /me/scm-access row, as the wire has it.
func (fx *adoPATFixture) scmAccessJSON(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	ctx := withOIDCHuman(context.Background(), fx.subject)
	rows, err := fx.srv.computeSCMAccessRowsFor(ctx, fx.st.site, fx.subject)
	if err != nil || len(rows) != 1 {
		t.Fatalf("computeSCMAccessRowsFor = %+v, %v; want one row", rows, err)
	}
	b, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A minted row reads token_mode "minted_pat", the row's default profile, and
// the last token created for the person once there is one.
func TestADOPATConsole_SCMAccessOnAMintedRow(t *testing.T) {
	fx := newADOConsoleLane(t, false)
	m := fx.scmAccessJSON(t)
	if got := string(m["token_mode"]); got != `"minted_pat"` {
		t.Errorf("token_mode = %s, want \"minted_pat\" (a console cannot tell a minted row from a bearer one without it)", got)
	}
	if got, want := string(m["default_profile"]), `["code_read","code_write"]`; got != want {
		t.Errorf("default_profile = %s, want the row's %s", got, want)
	}
	if raw, ok := m["last_token"]; ok {
		t.Errorf("last_token = %s before any token was created, want it absent", raw)
	}

	if !fx.dispatch(t) {
		t.Fatalf("dispatch refused: %q", fx.st.hint)
	}
	var last struct {
		CreatedAt *time.Time `json:"created_at"`
		RevokedAt *time.Time `json:"revoked_at"`
	}
	if err := json.Unmarshal(fx.scmAccessJSON(t)["last_token"], &last); err != nil || last.CreatedAt == nil || last.RevokedAt != nil {
		t.Fatalf("last_token after dispatch = %+v (%v), want created_at and no revoked_at", last, err)
	}
	fx.srv.revokeRunPATs(context.Background(), fx.run.ID, adoPATRevokeRunEnd)
	last.CreatedAt, last.RevokedAt = nil, nil
	if err := json.Unmarshal(fx.scmAccessJSON(t)["last_token"], &last); err != nil || last.RevokedAt == nil {
		t.Fatalf("last_token after the run ended = %+v (%v), want revoked_at", last, err)
	}
}

// A bearer row carries no token_mode and no last_token; its default profile
// reads as the catalogue default when the row names none.
func TestADOPATConsole_SCMAccessOnABearerRow(t *testing.T) {
	sc := adoTestSiteConfig(false)
	s := newSCMTestServer(t, sc, true)
	rows := scmAccessRows(t, s, context.Background(), sc, "alice")
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	b, _ := json.Marshal(rows[0])
	for _, absent := range []string{`"token_mode"`, `"last_token"`} {
		if bytes.Contains(b, []byte(absent)) {
			t.Errorf("a bearer row carries %s: %s", absent, b)
		}
	}
	want, _ := json.Marshal(adoscope.ProfileDefault())
	if !bytes.Contains(b, []byte(`"default_profile":`+string(want))) {
		t.Errorf("default_profile missing or not the catalogue default %s: %s", want, b)
	}
}

// disconnect drives DELETE /scm/azure-devops/connection as subject.
func (fx *adoPATFixture) disconnect(t *testing.T, subject string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, adoDisconnectPath, nil)
	r = r.WithContext(withOIDCHuman(r.Context(), subject))
	w := httptest.NewRecorder()
	fx.srv.handleADODisconnect(w, r)
	return w
}

// Disconnect revokes every live token the caller holds (and no one else's),
// forgets their stored sign-in, audits ado_pat.disconnect, and answers 204;
// a second disconnect is a 204 that removes nothing.
func TestADODisconnect_RevokesTheCallersTokensAndForgetsTheSignIn(t *testing.T) {
	fx := newADOConsoleLane(t, true)
	ctx := context.Background()
	mine := fx.st.unrevoked(t)
	if len(mine) != 1 {
		t.Fatalf("live tokens after dispatch = %d, want 1", len(mine))
	}
	// Another person's live token on another run.
	other := store.RunPAT{RunID: uuid.New(), AuthorizationID: uuid.New(), Owner: "sub-someone-else",
		ProviderRowID: fx.cfg.RowID, Org: "contoso", Scope: "vso.code", ValidTo: adoTestNow.Add(time.Hour)}
	if err := fx.st.InsertRunPAT(ctx, other); err != nil {
		t.Fatal(err)
	}
	fx.pats.live[other.AuthorizationID.String()] = adoPAT{AuthorizationID: other.AuthorizationID.String()}

	if w := fx.disconnect(t, fx.subject); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("disconnect = %d %q, want 204 with no body", w.Code, w.Body.String())
	}
	if got := fx.pats.revokedIDs(); !slices.Equal(got, []string{mine[0].AuthorizationID.String()}) {
		t.Errorf("revoked at Azure DevOps = %v, want only the caller's %s", got, mine[0].AuthorizationID)
	}
	if left := fx.st.unrevoked(t); len(left) != 1 || left[0].AuthorizationID != other.AuthorizationID {
		t.Errorf("live rows after disconnect = %+v, want only the other person's", left)
	}
	if got := fx.auditReasons(adoPATAuditRevoke); !slices.Equal(got, []string{adoPATRevokeDisconnect}) {
		t.Errorf("ado_pat.revoke reasons = %v, want [disconnect]", got)
	}
	if _, found := fx.stored(t, fx.subject); found {
		t.Error("the stored sign-in survived the disconnect")
	}
	if got := string(fx.scmAccessJSON(t)["state"]); got != `"`+modelAccessNotConfigured+`"` {
		t.Errorf("scm access state after disconnect = %s, want %q", got, modelAccessNotConfigured)
	}
	rows := fx.audit.find(adoPATAuditDisconnect)
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != fx.subject || rows[0].Target != fx.cfg.RowID ||
		!strings.Contains(string(rows[0].Data), `"removed":true`) {
		t.Fatalf("ado_pat.disconnect rows = %+v, want one success row by the caller naming the row, removed true", rows)
	}

	if w := fx.disconnect(t, fx.subject); w.Code != http.StatusNoContent {
		t.Fatalf("second disconnect = %d %q, want 204", w.Code, w.Body.String())
	}
	if rows := fx.audit.find(adoPATAuditDisconnect); len(rows) != 2 || !strings.Contains(string(rows[1].Data), `"removed":false`) {
		t.Errorf("second ado_pat.disconnect row = %+v, want removed false", rows)
	}
}

// D-6: a member refused the row gets the byte-identical answer a deployment
// with no Azure DevOps sign-in gives; one it allows disconnects. A caller
// with no identity provider subject has no sign-in to disconnect.
func TestADODisconnect_RefusedRowReadsAsNone(t *testing.T) {
	deny := []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capWorkspaceProvider, scmTestRowID, types.CapabilityDeny)}
	refused := doSSO(t, carrierServer(t, adoTestSiteConfig(false), deny, nil), http.MethodDelete, adoDisconnectPath, memberCookie(t), "")
	missing := doSSO(t, carrierServer(t, types.SiteConfig{}, nil, nil), http.MethodDelete, adoDisconnectPath, memberCookie(t), "")
	if refused.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("refused = %d, missing = %d; want 404 both", refused.Code, missing.Code)
	}
	if !bytes.Equal(refused.Body.Bytes(), missing.Body.Bytes()) {
		t.Errorf("a refused row is distinguishable from none:\nrefused: %s\nmissing: %s", refused.Body, missing.Body)
	}
	if w := doSSO(t, carrierServer(t, adoTestSiteConfig(false), nil, nil), http.MethodDelete, adoDisconnectPath, memberCookie(t), ""); w.Code != http.StatusNoContent {
		t.Errorf("an allowed member's disconnect = %d %s, want 204", w.Code, w.Body.String())
	}
	w := do(t, carrierServer(t, adoTestSiteConfig(false), nil, nil), http.MethodDelete, adoDisconnectPath, adminToken, "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonADOSignInNoSession) {
		t.Errorf("admin-token disconnect = %d %s, want 403 %s", w.Code, w.Body.String(), reasonADOSignInNoSession)
	}
}

// runTokens drives GET /runs/{id}/ado-tokens as the fixture's person.
func (fx *adoPATFixture) runTokens(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+fx.run.ID.String()+"/ado-tokens", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fx.run.ID.String())
	r = r.WithContext(context.WithValue(withOIDCHuman(r.Context(), fx.subject), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	fx.srv.handleListRunADOTokens(w, r)
	return w
}

// Every token the run held, oldest first, with its dates and why it closed —
// and never a token value or an authorization id.
func TestRunADOTokens_OldestFirstWithoutSecrets(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.setClock(time.UnixMilli(first.ExpiresAt).Add(-adoRunPATRenewWindow + time.Minute))
	fx.ok(t, "dev.azure.com", nil) // renewal: a second token
	rows := fx.st.unrevoked(t)
	if len(rows) != 2 {
		t.Fatalf("live tokens = %d, want 2", len(rows))
	}
	// The older token's revoke was refused for good: the sweep closed it with
	// the error. The kill then revokes the newer one.
	if _, err := fx.st.MarkRunPATRevoked(context.Background(), fx.run.ID, rows[0].AuthorizationID,
		adoPATRevokeSweep, "HTTP 400"); err != nil {
		t.Fatal(err)
	}
	fx.srv.revokeRunPATs(context.Background(), fx.run.ID, adoPATRevokeKill)

	w := fx.runTokens(t)
	if w.Code != http.StatusOK {
		t.Fatalf("GET ado-tokens = %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range fx.pats.tokens {
		if strings.Contains(body, secret) {
			t.Fatalf("the answer carries a token value: %s", body)
		}
	}
	for _, p := range rows {
		if strings.Contains(body, p.AuthorizationID.String()) {
			t.Fatalf("the answer carries an authorization id: %s", body)
		}
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 2 {
		t.Fatalf("decode %s: %v; want two tokens", body, err)
	}
	for i, tok := range got {
		for k := range tok {
			if !slices.Contains([]string{"created_at", "valid_to", "revoked_at", "revoke_reason", "revoke_failed"}, k) {
				t.Errorf("token %d carries %q: %s", i, k, body)
			}
		}
		if want := []string{adoPATRevokeSweep, adoPATRevokeKill}[i]; tok["revoke_reason"] != want || tok["revoked_at"] == nil {
			t.Errorf("token %d = %v, want revoked with reason %s", i, tok, want)
		}
	}
	older, _ := time.Parse(time.RFC3339Nano, got[0]["created_at"].(string))
	newer, _ := time.Parse(time.RFC3339Nano, got[1]["created_at"].(string))
	if !older.Before(newer) || got[0]["valid_to"].(string) >= got[1]["valid_to"].(string) {
		t.Errorf("tokens are not oldest first: %s", body)
	}
	if got[0]["revoke_failed"] != true || got[1]["revoke_failed"] != nil {
		t.Errorf("revoke_failed = %v, %v; want true on the token whose revoke was abandoned only", got[0]["revoke_failed"], got[1]["revoke_failed"])
	}
}

// A run that holds no token answers [], not null.
func TestRunADOTokens_NoneIsAnEmptyList(t *testing.T) {
	fx := newADOPATLane(t)
	if w := fx.runTokens(t); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("GET ado-tokens on a run with none = %d %s, want 200 []", w.Code, w.Body.String())
	}
}

// D-6: a reader who is not the owner or an admin gets GET /runs/{id}'s own
// 404, byte for byte, and the owner reads the list.
func TestRunADOTokens_ForeignReaderGetsTheRunsOwn404(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	foreign := pvMember(t, "sub-pv-foreign")
	wTokens := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/ado-tokens", foreign.cookie, "")
	wRun := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), foreign.cookie, "")
	if wTokens.Code != http.StatusNotFound || wRun.Code != http.StatusNotFound {
		t.Fatalf("foreign reader: ado-tokens=%d run=%d, want 404 both", wTokens.Code, wRun.Code)
	}
	if !bytes.Equal(wTokens.Body.Bytes(), wRun.Body.Bytes()) {
		t.Errorf("the 404 differs from GET /runs/{id}'s:\n tokens: %s\n run:    %s", wTokens.Body, wRun.Body)
	}
	owner := pvMember(t, pvOwner)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/ado-tokens", owner.cookie, ""); w.Code != http.StatusOK {
		t.Errorf("owner read = %d %s, want 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/ado-tokens", pvSecurity(t).cookie, ""); w.Code != http.StatusOK {
		t.Errorf("security admin read = %d %s, want 200", w.Code, w.Body.String())
	}
}
