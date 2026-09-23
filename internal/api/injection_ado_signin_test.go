// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoSignInStore adds the run read and the failure-hint write to the
// capability fixture's store.
type adoSignInStore struct {
	*adoCapStore
	mu   sync.Mutex
	run  types.AgentRun
	hint string
}

func (s *adoSignInStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run, nil
}

func (s *adoSignInStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hint = hint
	return nil
}

type adoSignInFixture struct {
	*adoCapFixture
	st *adoSignInStore
}

func newADOSignInFixture(t *testing.T) *adoSignInFixture {
	t.Helper()
	f := newADOCapFixture(t)
	st := &adoSignInStore{adoCapStore: f.srv.cfg.Store.(*adoCapStore),
		run: types.AgentRun{ID: f.runID, State: types.RunStarting}}
	f.srv.cfg.Store = st
	return &adoSignInFixture{adoCapFixture: f, st: st}
}

// resolveQ is one plain resolve with a query, as the proxy makes it.
func (f *adoSignInFixture) resolveQ(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String()+query, nil)
	f.srv.resolveADOInjection(w, r,
		&identity.Claims{RunID: f.runID, Sub: f.subject, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + uuid.NewString(),
			Injection: &egress.InjectionRule{Host: "dev.azure.com", SecretName: types.ADOEntraAccessTokenSecret}},
		f.grantID)
	return w
}

// at moves the control plane's clock (the access-token cache and the capture
// timestamps both read it).
func (f *adoSignInFixture) at(when time.Time) { f.srv.cfg.Now = func() time.Time { return when } }

// A refresh token that dies MID-RUN, or a Conditional Access policy that wants
// the person present, holds the request on a sign-in request for the run's
// owner; the person's next capture resolves that request eagerly, and the
// held request's re-resolve goes through.
func TestADOSignIn_MidRunHoldIsResolvedByACapture(t *testing.T) {
	for name, fault := range map[ADOEntraFailure]func(*adoSignInFixture, bool){
		ADOEntraFailureDeadCredential:      func(f *adoSignInFixture, on bool) { f.fake.SetInvalidGrant(on) },
		ADOEntraFailureInteractionRequired: func(f *adoSignInFixture, on bool) { f.fake.SetInteractionRequired(on) },
	} {
		t.Run(string(name), func(t *testing.T) {
			f := newADOSignInFixture(t)
			if w := f.resolveQ(t, "?phase=boot"); w.Code != http.StatusOK {
				t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
			}
			// The cached access token nears expiry and the renewal meets the fault.
			fault(f, true)
			f.at(time.Now().Add(time.Minute))
			id := pendingID(t, f.resolveQ(t, ""), reauthPendingState)
			row := f.row(id)
			sc, ok := adoSignInScope(row)
			if !ok || sc.Owner != f.subject || sc.ProviderID != f.cfg.RowID || sc.Reason != adoSignInReason {
				t.Fatalf("raised row = %+v, want an Azure DevOps sign-in request for the owner", row)
			}
			if again := pendingID(t, f.resolveQ(t, ""), reauthPendingState); again != id {
				t.Fatalf("a second resolve for the same lapse raised %s, want the same request %s", again, id)
			}
			if req := f.audit.find("credential.reauth.request"); len(req) != 1 ||
				!strings.Contains(string(req[0].Data), `"reason":"`+string(name)+`"`) {
				t.Fatalf("credential.reauth.request rows = %+v, want exactly one with reason %s", req, name)
			}
			if got := f.srv.reconcileADOReauthOnRead(context.Background(), f.row(id)); got.State != types.ApprovalPending {
				t.Fatalf("resolved before any sign-in: %s", got.State)
			}

			fault(f, false)
			f.at(time.Now().Add(2 * time.Minute))
			if w := f.capture(t, f.subject); w.Code != http.StatusFound {
				t.Fatalf("re-sign-in: %d %s", w.Code, w.Body.String())
			}
			if st := f.row(id).State; st != types.ApprovalApproved {
				t.Fatalf("after the capture the sign-in request is %s, want APPROVED by the capture itself", st)
			}
			if w := f.resolveQ(t, ""); w.Code != http.StatusOK {
				t.Fatalf("the held request's re-resolve: %d %s, want 200", w.Code, w.Body.String())
			}
		})
	}
}

// The per-run budget is maxReauthHolds, shared with every other
// credential_reauth workflow the run has opened.
func TestADOSignIn_CountsTowardMaxReauthHolds(t *testing.T) {
	f := newADOSignInFixture(t)
	for range maxReauthHolds {
		ap, err := f.approvals.Request(context.Background(), types.ApprovalRequest{
			RunID: f.runID, Kind: types.ApprovalCredentialReauth,
			RequestedScope: json.RawMessage(`{"mechanism":"x","owner":"` + uuid.NewString() + `"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		f.decide(t, ap.ID, types.ApprovalApproved, "")
	}
	f.fake.SetInvalidGrant(true)
	w := f.resolveQ(t, "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "too many times") {
		t.Fatalf("status %d body %s, want the per-run cap's 403", w.Code, w.Body.String())
	}
}

// Consent rows are credential_reauth too, but they have their own cap
// (maxADOCapabilityHoldsPerRun): a run that asked for consent maxReauthHolds
// times can still be asked to sign in.
func TestADOSignIn_ConsentRowsDoNotSpendTheSignInBudget(t *testing.T) {
	f := newADOSignInFixture(t)
	for i := range maxReauthHolds {
		raw, _ := json.Marshal(adoConsentScopeBody{Lane: adoApprovalLane, Mechanism: adoConsentMechanism,
			Owner: f.subject, ProviderID: f.cfg.RowID, Scopes: []string{"scope-" + strconv.Itoa(i)}})
		if _, err := f.approvals.Request(context.Background(), types.ApprovalRequest{
			RunID: f.runID, Kind: types.ApprovalCredentialReauth, RequestedScope: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.fake.SetInvalidGrant(true)
	f.at(time.Now().Add(time.Minute))
	id := pendingID(t, f.resolveQ(t, ""), reauthPendingState)
	if _, ok := adoSignInScope(f.row(id)); !ok {
		t.Fatalf("held on %+v, want a new sign-in request", f.row(id))
	}
}

// A sign-in row on this run that names another owner is not this lapse's
// request. A run's rows all carry its own owner in production, so this pins
// the owner key in holdForADOSignIn's match rather than a reachable path: a
// seeded foreign row must not be the one the run is held on.
func TestADOSignIn_AnotherOwnersRowIsNotThisHold(t *testing.T) {
	f := newADOSignInFixture(t)
	raw, _ := json.Marshal(adoSignInScopeBody{Lane: adoApprovalLane, Mechanism: adoSignInMechanism,
		Reason: adoSignInReason, Owner: "someone-else", ProviderID: f.cfg.RowID})
	foreign, err := f.approvals.Request(context.Background(), types.ApprovalRequest{
		RunID: f.runID, Kind: types.ApprovalCredentialReauth, RequestedScope: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.fake.SetInvalidGrant(true)
	f.at(time.Now().Add(time.Minute))
	id := pendingID(t, f.resolveQ(t, ""), reauthPendingState)
	if sc, _ := adoSignInScope(f.row(id)); id == foreign.ID || sc.Owner != f.subject {
		t.Fatalf("held on %s (owner %q), want a new request for the run's owner %q", id, sc.Owner, f.subject)
	}
}

// A capture resolves only its OWN person's requests. Owner A's sign-in row
// stays PENDING when B captures, even with a post-raise sign-in on record for
// both; A's own capture then resolves it.
func TestADOSignIn_AnotherPersonsCaptureResolvesNothing(t *testing.T) {
	f := newADOSignInFixture(t)
	ctx := context.Background()
	f.fake.SetInvalidGrant(true)
	f.at(time.Now().Add(time.Minute))
	id := pendingID(t, f.resolveQ(t, ""), reauthPendingState)

	// Post-raise, usable sign-ins for A and for B, written without the eager
	// resolve a capture door would run.
	blob, _ := f.stored(t, f.subject)
	blob.CapturedAt, blob.DeadAt = time.Now().Add(time.Hour), time.Time{}
	const other = "someone-else"
	for _, owner := range []string{f.subject, other} {
		if err := f.srv.storeADOEntraBlob(ctx, owner, f.cfg.RowID, blob); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.resolvePendingADOReauth(ctx, other, f.cfg.RowID)
	if st := f.row(id).State; st != types.ApprovalPending {
		t.Fatalf("after another person's capture the sign-in request is %s, want PENDING", st)
	}
	f.srv.resolvePendingADOReauth(ctx, f.subject, f.cfg.RowID)
	if st := f.row(id).State; st != types.ApprovalApproved {
		t.Fatalf("after the owner's capture the sign-in request is %s, want APPROVED", st)
	}
}

// At the sidecar's boot there is no request to hold: the run fails with a hint
// that says to sign in again, and the stored sign-in is recorded as ended — so
// /me/scm-access and the launch gate say expired_signin before the next launch.
func TestADOSignIn_BootFailsWithAHintAndRecordsTheEnd(t *testing.T) {
	f := newADOSignInFixture(t)
	f.st.site = adoSite(f.row0())
	f.fake.SetInvalidGrant(true)
	w := f.resolveQ(t, "?phase=boot")
	if w.Code != http.StatusForbidden {
		t.Fatalf("boot resolve: %d %s, want 403", w.Code, w.Body.String())
	}
	if f.st.hint != adoResolveDeadCredential || !strings.Contains(f.st.hint, "sign in to Azure DevOps again") {
		t.Fatalf("failure hint = %q, want %q", f.st.hint, adoResolveDeadCredential)
	}
	if n := len(f.approvals.byID); n != 0 {
		t.Fatalf("the boot resolve raised %d requests, want none", n)
	}
	blob, _ := f.stored(t, f.subject)
	if !blob.signInEnded() || blob.DeadReason != ADOEntraFailureDeadCredential {
		t.Fatalf("stored sign-in = %+v, want its end recorded", blob)
	}

	access := f.srv.computeSCMAccessRowsFor(context.Background(), f.st.site, f.subject)
	if len(access) != 1 || access[0].State != modelAccessExpiredSignin || access[0].Cause != scmAccessCauseEnded {
		t.Fatalf("scm-access = %+v, want expired_signin / ended", access)
	}
	var gc *gitCredentialRefusalError
	if err := f.srv.gitCredentialRefusalForLauncher(context.Background(), f.subject, adoTestRepo); !errors.As(err, &gc) ||
		gc.Sentence != gitCredentialEndedRefusal {
		t.Fatalf("launch gate = %v, want the connection-ended refusal", err)
	}

	// A fresh sign-in, consented for the row's ceiling, clears it.
	f.fake.SetInvalidGrant(false)
	f.cfg.Scopes, _ = adoscope.ScopesFor(f.row0().Entra.CapabilityCeiling)
	f.fake.SetConsentedScopes(f.cfg.Scopes...)
	f.at(time.Now().Add(time.Minute))
	if w := f.capture(t, f.subject); w.Code != http.StatusFound {
		t.Fatalf("re-sign-in: %d %s", w.Code, w.Body.String())
	}
	if access := f.srv.computeSCMAccessRowsFor(context.Background(), f.st.site, f.subject); access[0].State != modelAccessLive {
		t.Fatalf("after a fresh sign-in scm-access = %+v, want live", access)
	}
}

// row0 is the fixture's provider row, as the resolve reads it.
func (f *adoSignInFixture) row0() types.GitProvider { return f.st.site.WorkspaceProviders.Git[0] }

// An administrator widening the row's ceiling after people signed in must not
// fail every run at boot: the resolve asks only for what the sign-in covers, a
// capability that needs the new scope goes through consent, and scm-access and
// the launch gate report the re-consent state only when the run's BASELINE
// outgrows the sign-in.
func TestADOSignIn_WidenedCeiling(t *testing.T) {
	f := newADOSignInFixture(t)
	old := []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR}
	oldScopes, _ := adoscope.ScopesFor(old)
	f.fake.SetConsentedScopes(oldScopes...)
	f.cfg.Scopes = oldScopes
	f.at(time.Now())
	if w := f.capture(t, f.subject); w.Code != http.StatusFound {
		t.Fatalf("sign-in on the old ceiling: %d %s", w.Code, w.Body.String())
	}

	// The widening: build_execute joins the ceiling, not the sign-in.
	row := f.row0()
	row.Entra.CapabilityCeiling = append(old, adoscope.CapBuildExecute)
	f.st.site = adoSite(row)
	f.cfg.Scopes, _ = adoscope.ScopesFor(row.Entra.CapabilityCeiling)

	if w := f.resolveQ(t, "?phase=boot"); w.Code != http.StatusOK {
		t.Fatalf("a run on the old baseline no longer boots: %d %s", w.Code, w.Body.String())
	}
	if access := f.srv.computeSCMAccessRowsFor(context.Background(), f.st.site, f.subject); len(access) != 1 ||
		access[0].State != modelAccessLive {
		t.Fatalf("scm-access = %+v, want live while the baseline is covered", access)
	}

	// The new capability goes through the consent chain.
	path := "/contoso/proj/_apis/build/builds"
	a := pendingID(t, f.ask(t, adoscope.CapBuildExecute, types.FirstUseWaitForReview, uuid.Nil, path), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)
	b := pendingID(t, f.ask(t, adoscope.CapBuildExecute, types.FirstUseWaitForReview, a, path), reauthPendingState)
	if sc, ok := adoConsentScope(f.row(b)); !ok || !slices.Contains(sc.Scopes, adoscope.ResourceID+"/vso.build_execute") {
		t.Fatalf("row %+v, want a consent request naming the new scope", f.row(b))
	}

	// The baseline outgrows the sign-in: re-consent state, and the gate says so.
	row.Entra.DefaultProfile = []adoscope.Capability{adoscope.CapRead, adoscope.CapBuildExecute}
	f.st.site = adoSite(row)
	access := f.srv.computeSCMAccessRowsFor(context.Background(), f.st.site, f.subject)
	if len(access) != 1 || access[0].State != modelAccessExpiredSignin || access[0].Cause != scmAccessCauseConsentNeeded {
		t.Fatalf("scm-access = %+v, want expired_signin / consent_needed", access)
	}
	var gc *gitCredentialRefusalError
	if err := f.srv.gitCredentialRefusalForLauncher(context.Background(), f.subject, adoTestRepo); !errors.As(err, &gc) ||
		gc.Sentence != gitCredentialConsentRefusal {
		t.Fatalf("launch gate = %v, want the re-consent refusal", err)
	}
}

// A sign-in captured after the request was raised, but found ended since,
// answers nothing.
func TestADOSignIn_AnEndedCaptureResolvesNothing(t *testing.T) {
	f := newADOSignInFixture(t)
	f.fake.SetInvalidGrant(true)
	f.at(time.Now().Add(time.Minute))
	id := pendingID(t, f.resolveQ(t, ""), reauthPendingState)
	blob, _ := f.stored(t, f.subject)
	blob.CapturedAt = time.Now().Add(time.Hour)
	if err := f.srv.storeADOEntraBlob(context.Background(), f.subject, f.cfg.RowID, blob); err != nil {
		t.Fatal(err)
	}
	if !blob.signInEnded() {
		t.Fatal("precondition: the failed renewal recorded the end")
	}
	if got := f.srv.reconcileADOReauthOnRead(context.Background(), f.row(id)); got.State != types.ApprovalPending {
		t.Fatalf("an ended sign-in resolved the request: %s", got.State)
	}
}

// The two expired_signin refusals are §7.1's rows, byte for byte.
func TestGitCredentialExpiredRefusalsMatchCanon(t *testing.T) {
	raw, err := os.ReadFile("../../docs/design/ado-entra-prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	for label, want := range map[string]string{
		"Connection ended, at run create":                            gitCredentialEndedRefusal,
		"Connection doesn't cover the run's baseline, at run create": gitCredentialConsentRefusal,
	} {
		var canon string
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "| "+label) {
				cells := strings.Split(line, "|")
				canon = strings.TrimSpace(cells[len(cells)-2])
			}
		}
		if canon != want {
			t.Errorf("%s: canon %q, code %q", label, canon, want)
		}
	}
}
