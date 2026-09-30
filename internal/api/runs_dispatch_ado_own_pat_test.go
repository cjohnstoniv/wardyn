// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ownPATRun is a run on an own-token row dispatched for capSub, with the
// approval store and the run read the hold needs.
type ownPATRun struct {
	srv       *Server
	st        *adoSignInStore
	audit     *memAudit
	approvals *fakeApprovals
	runID     uuid.UUID
	grantID   uuid.UUID // the dev.azure.com grant
	env       map[string]string
	policy    types.RunPolicySpec
	now       time.Time
}

func newOwnPATRun(t *testing.T) *ownPATRun {
	t.Helper()
	return newOwnPATRunOn(t, ownPATTestRow(), adoTestRepo)
}

// newOwnPATRunOn is newOwnPATRun on row, for a run of repo.
func newOwnPATRunOn(t *testing.T, row types.GitProvider, repo string) *ownPATRun {
	t.Helper()
	base := &adoTestStore{site: adoSite(row)}
	fa := newFakeApprovals()
	f := &ownPATRun{audit: &memAudit{}, approvals: fa, runID: uuid.New(), env: map[string]string{}, now: time.Now().UTC()}
	f.st = &adoSignInStore{adoCapStore: &adoCapStore{adoTestStore: base, approvals: fa},
		run: types.AgentRun{ID: f.runID, State: types.RunRunning}}
	f.srv = &Server{cfg: Config{
		Store: f.st, Secrets: &memSecrets{m: map[string][]byte{}}, MaskRegistry: secretmask.NewRegistry(),
		Now: func() time.Time { return f.now }, Audit: f.audit, Approvals: fa,
	}}
	ado, ok := resolveADOEntraRun(base.site, []string{repo}, capSub)
	if !ok {
		t.Fatal("the own-token row resolved no lane")
	}
	if _, _, ok := f.srv.authorADOEntraInjection(context.Background(), types.AgentRun{ID: f.runID}, ado,
		"CERT", "KEY", &f.policy, f.env, nil); !ok {
		t.Fatalf("dispatch refused an own-token run: %v", base.failTo)
	}
	f.grantID = base.grants[0].ID
	return f
}

// token stores capSub's own token, expiring at exp, for org.
func (f *ownPATRun) token(t *testing.T, value, org string, exp time.Time) {
	t.Helper()
	raw, _ := json.Marshal(adoOwnPATBlob{Token: value, Org: org, ExpiresOn: exp, StoredAt: f.now})
	ctx := secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus)
	if err := f.srv.cfg.Secrets.For(capSub).Put(ctx, adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
}

func (f *ownPATRun) resolve(t *testing.T, sub, host, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String()+query, nil)
	if !f.srv.resolveADOOwnPATInjection(w, r, &identity.Claims{RunID: f.runID, Sub: sub, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + uuid.NewString(), Injection: &egress.InjectionRule{Host: host, SecretName: types.ADOEntraAccessTokenSecret}},
		f.grantID) {
		t.Fatal("an own-token grant was not handled by its own arm")
	}
	return w
}

// wantRefused asserts a refusal's status and wire reason.
func wantRefused(t *testing.T, w *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), `"reason":"`+reason+`"`) {
		t.Fatalf("resolve = %d %s, want %d %s", w.Code, w.Body.String(), status, reason)
	}
}

// Dispatch authors the own-token run exactly as the Entra lane does — the
// organisation's exact hosts, TLS only, the inert placeholder, git through the
// broker — with a snapshot that names own_pat and no tenant or client. The
// sandbox gets no credential.
func TestAuthorADOEntraInjection_OwnPATRow(t *testing.T) {
	f := newOwnPATRun(t)
	grants := f.st.grants
	if len(grants) != len(adoContosoHosts) {
		t.Fatalf("%d grants, want one per host (%d)", len(grants), len(adoContosoHosts))
	}
	var sc struct {
		RequireTLS bool                  `json:"require_tls"`
		Snapshot   adoEntraScopeSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(grants[0].Spec.Scope, &sc); err != nil {
		t.Fatal(err)
	}
	if !sc.RequireTLS || sc.Snapshot.TokenMode != "own_pat" || sc.Snapshot.TenantID != "" || sc.Snapshot.ClientID != "" ||
		sc.Snapshot.OwnerSubject != capSub || sc.Snapshot.Organisation != "contoso" {
		t.Errorf("grant scope = %+v", sc)
	}
	if f.env[adoEntraPlaceholderEnv] != adoEntraPlaceholderValue {
		t.Errorf("sandbox %s = %q, want the inert placeholder", adoEntraPlaceholderEnv, f.env[adoEntraPlaceholderEnv])
	}
	// minted_pat is still refused: only own_pat joined bearer.
	row := ownPATTestRow()
	row.Entra.TokenMode = types.ADOTokenModeMintedPAT
	st := &adoTestStore{site: adoSite(row)}
	srv := &Server{cfg: Config{Store: st, Audit: &memAudit{}, Now: time.Now}}
	ado, _ := resolveADOEntraRun(st.site, []string{adoTestRepo}, capSub)
	if _, _, ok := srv.authorADOEntraInjection(context.Background(), types.AgentRun{ID: uuid.New()}, ado,
		"CERT", "KEY", &types.RunPolicySpec{}, map[string]string{}, nil); ok || len(st.grants) != 0 {
		t.Fatal("a minted_pat row was dispatched")
	}
}

// The resolve injects the owner's own token, Basic with the token as the
// password, masked for the run, until the stored-key lease or the expiry the
// person entered, and audits it without the token.
func TestResolveADOOwnPAT_InjectsTheOwnersTokenAsBasic(t *testing.T) {
	f := newOwnPATRun(t)
	exp := f.now.Add(2 * time.Hour)
	f.token(t, ownPATToken, "contoso", exp)
	w := f.resolve(t, capSub, "dev.azure.com", "")
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	wantValue := "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+ownPATToken))
	if resp.Header != "Authorization" || resp.Value != wantValue || resp.Organisation != "contoso" ||
		strings.Join(resp.Capabilities, ",") != "project_read,code_read,code_write" {
		t.Errorf("response = %+v", resp)
	}
	if resp.ExpiresAt == 0 || resp.ExpiresAt > exp.UnixMilli() || resp.ExpiresAt > f.now.Add(storedKeyTTL).UnixMilli() {
		t.Errorf("expires_at = %d, want at most the token's expiry and the stored-key lease", resp.ExpiresAt)
	}
	masked := string(f.srv.cfg.MaskRegistry.Masker(f.runID).Mask([]byte("git said " + ownPATToken + " and " + wantValue)))
	if strings.Contains(masked, ownPATToken) || strings.Contains(masked, strings.TrimPrefix(wantValue, "Basic ")) {
		t.Errorf("the token survives the run's masker: %s", masked)
	}
	rows := f.audit.find("secret.read")
	if len(rows) != 1 || rows[0].Outcome != "success" || strings.Contains(string(rows[0].Data), ownPATToken) ||
		strings.Contains(string(rows[0].Data), strings.TrimPrefix(wantValue, "Basic ")) {
		t.Fatalf("secret.read rows = %+v, want one success carrying no token", rows)
	}
}

// SECURITY: the grant resolves only for the run token's own subject — the
// snapshot's owner — never for anyone else's token.
func TestResolveADOOwnPAT_OnlyForTheOwner(t *testing.T) {
	f := newOwnPATRun(t)
	f.token(t, ownPATToken, "contoso", f.now.Add(time.Hour))
	wantRefused(t, f.resolve(t, "someone-else", "dev.azure.com", ""), http.StatusForbidden, reasonOwnerNotCaller)
}

// SECURITY: a host outside the run's organisation is refused.
func TestResolveADOOwnPAT_PinsTheOrganisation(t *testing.T) {
	f := newOwnPATRun(t)
	f.token(t, ownPATToken, "contoso", f.now.Add(time.Hour))
	wantRefused(t, f.resolve(t, capSub, "fabrikam.visualstudio.com", ""), http.StatusForbidden, reasonHostNotOrganisation)
}

// SECURITY: a token its owner added for another organisation is not used.
func TestResolveADOOwnPAT_RefusesAnotherOrganisationsToken(t *testing.T) {
	f := newOwnPATRun(t)
	f.token(t, ownPATToken, "fabrikam", f.now.Add(time.Hour))
	wantRefused(t, f.resolve(t, capSub, "dev.azure.com", ""), http.StatusForbidden, reasonADOOwnPATOtherOrg)
}

// No token added: refused with its own reason.
func TestResolveADOOwnPAT_NotAdded(t *testing.T) {
	f := newOwnPATRun(t)
	wantRefused(t, f.resolve(t, capSub, "dev.azure.com", ""), http.StatusForbidden, reasonADOOwnPATNotAdded)
}

// SECURITY: an admin who switches the row away from own_pat mid-run stops the
// token being injected.
func TestResolveADOOwnPAT_RowDriftRefuses(t *testing.T) {
	f := newOwnPATRun(t)
	f.token(t, ownPATToken, "contoso", f.now.Add(time.Hour))
	row := ownPATTestRow()
	row.Entra.TokenMode = types.ADOTokenModeBearer
	row.Entra.TenantID, row.Entra.ClientID = adoTestTenant, adoTestClient
	f.st.site = adoSite(row)
	wantRefused(t, f.resolve(t, capSub, "dev.azure.com", ""), http.StatusForbidden, reasonScopeChanged)
}

// SECURITY: past the expiry its owner entered the token is never injected. At
// the sidecar's boot the run fails with the reason as its hint; mid-run it is
// held on a sign-in request, which the owner's next token answers, and the
// next resolve injects that new token.
func TestResolveADOOwnPAT_Expired(t *testing.T) {
	t.Run("at boot", func(t *testing.T) {
		f := newOwnPATRun(t)
		f.token(t, ownPATToken, "contoso", f.now.Add(-time.Minute))
		wantRefused(t, f.resolve(t, capSub, "dev.azure.com", "?phase=boot"), http.StatusForbidden, reasonADOOwnPATExpired)
		if f.st.hint != adoOwnPATExpiredRefusal {
			t.Errorf("failure hint = %q, want %q", f.st.hint, adoOwnPATExpiredRefusal)
		}
	})
	t.Run("mid-run: held, then answered by a new token", func(t *testing.T) {
		f := newOwnPATRun(t)
		f.token(t, ownPATToken, "contoso", f.now.Add(-time.Minute))
		id := pendingID(t, f.resolve(t, capSub, "dev.azure.com", ""), reauthPendingState)
		// The hold says what answers it — a new token, not a sign-in — and is
		// still the Azure DevOps sign-in request every matcher reads.
		ap, _ := f.approvals.Get(context.Background(), id)
		var body adoSignInScopeBody
		if err := json.Unmarshal(ap.RequestedScope, &body); err != nil || body.Reason != reasonADOOwnPATExpired ||
			body.Mechanism != adoSignInMechanism {
			t.Fatalf("hold scope = %s, want reason %s on the sign-in mechanism", ap.RequestedScope, reasonADOOwnPATExpired)
		}
		if _, ok := adoSignInScope(ap); !ok {
			t.Fatal("the own-token hold is not matched as an Azure DevOps sign-in request")
		}
		raised := f.audit.find("credential.reauth.request")
		if len(raised) != 1 || !strings.Contains(string(raised[0].Data), "adds a new token") {
			t.Fatalf("credential.reauth.request rows = %+v, want one whose detail says a new token answers it", raised)
		}
		// A second resolve waits on the same request.
		if again := pendingID(t, f.resolve(t, capSub, "dev.azure.com", ""), reauthPendingState); again != id {
			t.Fatalf("a second resolve raised %s, want the open %s", again, id)
		}
		// Stored with a clock BEHIND the hold's raise, as when the hold lands
		// while the identity check is still out: it is answered all the same.
		f.now = time.Now().UTC().Add(-time.Minute)
		f.token(t, "bobs-new-pat", "contoso", f.now.AddDate(0, 0, 10))
		f.srv.resolvePendingADOOwnPATHolds(context.Background(), capSub, ownPATRowID)
		if ap, _ := f.approvals.Get(context.Background(), id); ap.State != types.ApprovalApproved {
			t.Fatalf("the hold is %s after a new token, want APPROVED", ap.State)
		}
		w := f.resolve(t, capSub, "dev.azure.com", "")
		var resp types.ResolvedInjection
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != http.StatusOK || resp.Value != adoOwnPATHeaderValue("bobs-new-pat") {
			t.Fatalf("after the new token: %d %s", w.Code, w.Body)
		}
	})
}

// The capability ceiling and the run's capabilities hold exactly as on the
// Entra arm: a capability above the ceiling is refused, one inside it under
// review raises a request, and the token is not handed out for either.
func TestResolveADOOwnPAT_CapabilityArm(t *testing.T) {
	f := newOwnPATRun(t)
	f.token(t, ownPATToken, "contoso", f.now.Add(time.Hour))
	ask := func(c adoscope.Capability) *httptest.ResponseRecorder {
		q := url.Values{"capability": {string(c)}, "first_use": {string(types.FirstUseWaitForReview)},
			"method": {"POST"}, "path": {"/contoso/proj/_apis/git/repositories/app/pullrequests"}, "repo": {"app"}}
		return f.resolve(t, capSub, "dev.azure.com", "?"+q.Encode())
	}
	wantRefused(t, ask(adoscope.CapRepoAdmin), http.StatusForbidden, reasonCapabilityAboveCeiling)
	w := ask(adoscope.CapPR)
	pendingID(t, w, adoCapabilityPendingState)
	if strings.Contains(w.Body.String(), ownPATToken) {
		t.Fatal("a held capability ask carried the token")
	}
}

// The two Azure DevOps arms never serve each other's grants: the own-token arm
// declines an Entra grant, and the Entra arm refuses an own-token grant.
func TestADOResolveArms_DoNotCross(t *testing.T) {
	rf := newADOResolveFixture(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+rf.grantID.String(), nil)
	if rf.srv.resolveADOOwnPATInjection(w, r, &identity.Claims{RunID: rf.runID, Sub: rf.subject},
		broker.Minted{Injection: &egress.InjectionRule{Host: "dev.azure.com", SecretName: types.ADOEntraAccessTokenSecret}}, rf.grantID) {
		t.Fatal("the own-token arm handled an Entra grant")
	}

	f := newOwnPATRun(t)
	f.token(t, ownPATToken, "contoso", f.now.Add(time.Hour))
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String(), nil)
	f.srv.resolveADOInjection(w, r, &identity.Claims{RunID: f.runID, Sub: capSub, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{Injection: &egress.InjectionRule{Host: "dev.azure.com", SecretName: types.ADOEntraAccessTokenSecret}}, f.grantID)
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), ownPATToken) {
		t.Fatalf("the Entra arm served an own-token grant: %d %s", w.Code, w.Body)
	}
}

// Azure DevOps refusing a token before its expiry reaches the owner's
// /me/scm-access as refused_at while the state stays live: the proxy's
// re-resolve after the 401 (stale_jti) stamps the stored token once, never
// again, audits it on the own-token store action, and still injects.
func TestResolveADOOwnPAT_StampsARefusalBeforeExpiryOnce(t *testing.T) {
	f := newOwnPATRun(t)
	ownPATConfirm(t, http.StatusUnauthorized)
	f.token(t, ownPATToken, "contoso", f.now.Add(30*24*time.Hour))
	row := ownPATTestRow()
	if w := f.resolve(t, capSub, "dev.azure.com", ""); w.Code != http.StatusOK {
		t.Fatalf("plain resolve = %d %s", w.Code, w.Body)
	}
	if got := f.audit.find(adoPATAuditOwnStore); len(got) != 0 {
		t.Fatalf("a plain resolve stamped a refusal: %+v", got)
	}

	if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-old"); w.Code != http.StatusOK {
		t.Fatalf("resolve after a refusal = %d %s, want the token still injected", w.Code, w.Body)
	}
	blob, found, err := f.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID)
	if err != nil || !found || blob.RefusedAt == nil || !blob.RefusedAt.Equal(f.now) {
		t.Fatalf("stored = %+v (found %v, err %v), want refused_at = now", blob, found, err)
	}
	rows := f.audit.find(adoPATAuditOwnStore)
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"reason":"upstream_refused"`) ||
		strings.Contains(string(rows[0].Data), ownPATToken) {
		t.Fatalf("ado_pat.own.store rows = %+v, want one failure with reason upstream_refused and no token", rows)
	}
	access, err := f.srv.scmAccessForOwnPAT(context.Background(), row, capSub)
	if err != nil || access.State != modelAccessLive || access.RefusedAt != f.now.UTC().Format(time.RFC3339) {
		t.Fatalf("access = %+v (err %v), want live with refused_at", access, err)
	}

	// Refused again later: the first stamp stands and no second row is written.
	f.now = f.now.Add(time.Hour)
	if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-older"); w.Code != http.StatusOK {
		t.Fatalf("second resolve = %d %s", w.Code, w.Body)
	}
	if again, _, _ := f.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID); again.RefusedAt == nil || !again.RefusedAt.Equal(*blob.RefusedAt) {
		t.Errorf("the stamp moved: %v -> %v", blob.RefusedAt, again.RefusedAt)
	}
	if rows := f.audit.find(adoPATAuditOwnStore); len(rows) != 1 {
		t.Errorf("%d audit rows after the second refusal, want 1", len(rows))
	}
}

// A token past its expiry is not stamped: expiry, not refusal, is its state.
func TestResolveADOOwnPAT_NoStampOnAnExpiredToken(t *testing.T) {
	f := newOwnPATRun(t)
	f.token(t, ownPATToken, "contoso", f.now.Add(-time.Hour))
	f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-old")
	blob, _, _ := f.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID)
	if blob.RefusedAt != nil || len(f.audit.find(adoPATAuditOwnStore)) != 0 {
		t.Errorf("an expired token was stamped: %+v", blob)
	}
}

// ownPATConfirm points the stamp's connectionData confirm at a fake answering
// status, and counts the calls it gets.
func ownPATConfirm(t *testing.T, status int) *int {
	t.Helper()
	calls := new(int)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"authenticatedUser":{"properties":{"Account":{"$value":"bob@corp.example"}}}}`))
		}
	}))
	prev := adoOwnPATAPIBase
	adoOwnPATAPIBase = fake.URL
	t.Cleanup(func() { adoOwnPATAPIBase = prev; fake.Close() })
	return calls
}

// A 401 on one request can be a missing scope. The stamp is made only when
// connectionData also rejects the token; one that still authenticates, or a
// check that could not complete, is not stamped, and a stamped token is not
// asked about again.
func TestResolveADOOwnPAT_StampsOnlyWhenTheTokenItselfIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantStamp bool
	}{
		{"the token still authenticates", http.StatusOK, false},
		{"azure devops is unavailable", http.StatusServiceUnavailable, false},
		{"the token is refused", http.StatusUnauthorized, true},
		{"the answer is the sign-in page", http.StatusNonAuthoritativeInfo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOwnPATRun(t)
			calls := ownPATConfirm(t, tc.status)
			f.token(t, ownPATToken, "contoso", f.now.Add(30*24*time.Hour))
			if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-old"); w.Code != http.StatusOK {
				t.Fatalf("resolve = %d %s, want the token still injected", w.Code, w.Body)
			}
			blob, _, _ := f.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID)
			if stamped := blob.RefusedAt != nil; stamped != tc.wantStamp {
				t.Errorf("stamped = %v, want %v", stamped, tc.wantStamp)
			}
			if got := len(f.audit.find(adoPATAuditOwnStore)); (got == 1) != tc.wantStamp || got > 1 {
				t.Errorf("%d audit rows, want stamp = %v", got, tc.wantStamp)
			}
			if *calls != 1 {
				t.Errorf("%d confirm calls, want exactly 1", *calls)
			}
			if tc.wantStamp {
				f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-older")
				if *calls != 1 {
					t.Errorf("a stamped token was confirmed again (%d calls)", *calls)
				}
			}
		})
	}
}

// hookedSecrets runs afterGet after every Get that succeeds, so a test can
// interleave another writer between a read and the write that follows it.
type hookedSecrets struct {
	secretstore.Store
	afterGet *func(name string)
}

func (h hookedSecrets) For(owner string) secretstore.Store {
	return hookedSecrets{Store: h.Store.For(owner), afterGet: h.afterGet}
}

func (h hookedSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	v, err := h.Store.Get(ctx, name)
	if err == nil && *h.afterGet != nil {
		(*h.afterGet)(name)
	}
	return v, err
}

// The person removes their token (DELETE) while the refusal stamp is between
// its read and its write. The removal takes the same lock, so it lands after
// the stamp and the token stays removed; without it the stamp wrote the
// removed token back and it kept being injected.
func TestResolveADOOwnPAT_StampDoesNotResurrectARemovedToken(t *testing.T) {
	f := newOwnPATRun(t)
	inner := f.srv.cfg.Secrets
	var hook func(string)
	f.srv.cfg.Secrets = hookedSecrets{Store: inner, afterGet: &hook}
	f.token(t, ownPATToken, "contoso", f.now.Add(30*24*time.Hour))
	// The door the person removes the token through shares the run's store.
	door := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	door.srv.cfg.Secrets = inner
	ownPATConfirm(t, http.StatusUnauthorized) // after the door, which points the same base at its own fake

	var removed chan struct{}
	gets := 0
	hook = func(string) {
		if gets++; gets != 2 { // the resolve's own read is #1; the stamp's, under the lock, is #2
			return
		}
		removed = make(chan struct{})
		go func() {
			defer close(removed)
			if w := doSSO(t, door.srv, http.MethodDelete, "/api/v1/me/scm/azure-devops/token?org="+url.QueryEscape(ownPATOrgKey), memberCookie(t), ""); w.Code != http.StatusNoContent {
				t.Errorf("DELETE = %d %s", w.Code, w.Body)
			}
		}()
		select {
		case <-removed:
		case <-time.After(200 * time.Millisecond): // queued behind the stamp, as it must be
		}
	}
	if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-old"); w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	hook = nil
	if removed == nil {
		t.Fatal("the stamp never read the token")
	}
	<-removed
	if _, found, err := f.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID); err != nil || found {
		t.Fatalf("the token the person removed is in the store (found=%v, err=%v)", found, err)
	}
}
