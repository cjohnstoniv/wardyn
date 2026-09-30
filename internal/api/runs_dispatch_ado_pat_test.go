// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeADOPATs is adoPATClient over a map of what Azure DevOps would hold.
type fakeADOPATs struct {
	mu        sync.Mutex
	creates   []adoPATRequest
	live      map[string]adoPAT
	revoked   []string
	tokens    []string
	createErr error
	revokeErr error
}

func (f *fakeADOPATs) Create(_ context.Context, org, accessToken string, req adoPATRequest) (adoPAT, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if org != "contoso" || accessToken == "" {
		return adoPAT{}, &adoPATError{Status: http.StatusBadRequest}
	}
	if f.createErr != nil {
		return adoPAT{}, f.createErr
	}
	f.creates = append(f.creates, req)
	p := adoPAT{AuthorizationID: uuid.NewString(), Scope: req.Scope, ValidTo: req.ValidTo,
		Token: fmt.Sprintf("fakepat%02d%s", len(f.creates), strings.Repeat("q", 44))}
	if f.live == nil {
		f.live = map[string]adoPAT{}
	}
	f.live[p.AuthorizationID] = p
	f.tokens = append(f.tokens, p.Token)
	return p, nil
}

func (f *fakeADOPATs) Revoke(_ context.Context, org, accessToken, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if org != "contoso" || accessToken == "" {
		return &adoPATError{Status: http.StatusBadRequest}
	}
	if f.revokeErr != nil {
		return f.revokeErr
	}
	if _, ok := f.live[id]; !ok {
		return &adoPATError{Status: http.StatusNotFound}
	}
	delete(f.live, id)
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeADOPATs) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *fakeADOPATs) revokedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.revoked)
}

// adoPATStore is the capability fixture's store plus the run-token record, the
// run rows and the pause seam.
type adoPATStore struct {
	*adoCapStore
	*store.MemRunPATs
	mu   sync.Mutex
	runs map[uuid.UUID]types.AgentRun
	hint string
}

func (s *adoPATStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return r, nil
}

func (s *adoPATStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.AgentRun{}
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out, nil
}

func (s *adoPATStore) edit(id uuid.UUID, f func(*types.AgentRun)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	f(&r)
	s.runs[id] = r
}

func (s *adoPATStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r.State != from {
		return false, nil
	}
	r.State = to
	s.runs[id] = r
	return true, nil
}

func (s *adoPATStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hint = hint
	return nil
}

func (s *adoPATStore) SetSandboxRef(context.Context, uuid.UUID, string) error { return nil }

func (s *adoPATStore) ListAPITokens(context.Context) ([]types.APIToken, error) { return nil, nil }

func (s *adoPATStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }

func (s *adoPATStore) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	return nil, time.Time{}, nil
}

func (s *adoPATStore) StampRunActive(context.Context, uuid.UUID) (bool, error) { return false, nil }

func (s *adoPATStore) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) { return false, nil }

func (s *adoPATStore) MarkRunPaused(_ context.Context, id uuid.UUID, reason types.PauseReason, _ *time.Time) (bool, error) {
	now := time.Now().UTC()
	s.edit(id, func(r *types.AgentRun) { r.PausedAt, r.PausedReason = &now, reason })
	return true, nil
}

func (s *adoPATStore) ClearRunPaused(_ context.Context, id uuid.UUID) (bool, error) {
	s.edit(id, func(r *types.AgentRun) { r.PausedAt, r.PausedReason = nil, "" })
	return true, nil
}

func (s *adoPATStore) unrevoked(t *testing.T) []store.RunPAT {
	t.Helper()
	rows, err := s.ListUnrevokedRunPATs(context.Background(), store.RunPATFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// adoPATFixture is a person connected for minting, on a minted_pat row, with
// one RUNNING run whose lane resolves.
type adoPATFixture struct {
	*adoFixture
	st      *adoPATStore
	pats    *fakeADOPATs
	fa      *fakeApprovals
	run     types.AgentRun
	ado     adoEntraRun
	grants  map[string]uuid.UUID
	subject string
	mu      sync.Mutex
	now     time.Time
}

func (fx *adoPATFixture) clock() time.Time {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.now
}

func (fx *adoPATFixture) advance(d time.Duration) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.now = fx.now.Add(d)
}

func (fx *adoPATFixture) setClock(at time.Time) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.now = at
}

// newADOPATLane is everything up to dispatch.
func newADOPATLane(t *testing.T) *adoPATFixture {
	t.Helper()
	f := newADOFixture(t)
	// The sign-in a minted_pat row captures: the two token permissions only.
	f.fake.SetConsentedScopes(adoscope.MintScopes()...)
	f.cfg.Scopes = adoscope.MintScopes()
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	row := adoEntraTestRow()
	row.ID = f.cfg.RowID
	row.Entra.TenantID, row.Entra.ClientID = f.cfg.TenantID, f.cfg.ClientID
	row.Entra.TokenMode = types.ADOTokenModeMintedPAT
	fa := newFakeApprovals()
	st := &adoPATStore{
		adoCapStore: &adoCapStore{adoTestStore: &adoTestStore{site: adoSite(row)}, approvals: fa},
		MemRunPATs:  store.NewMemRunPATs(), runs: map[uuid.UUID]types.AgentRun{},
	}
	fx := &adoPATFixture{adoFixture: f, st: st, pats: &fakeADOPATs{}, fa: fa, subject: subject, now: adoTestNow,
		grants: map[string]uuid.UUID{}}
	f.srv.cfg.Store, f.srv.cfg.Approvals = st, fa
	f.srv.cfg.Now = fx.clock
	f.srv.adoPATs = fx.pats
	end := adoTestNow.Add(24 * time.Hour)
	fx.run = types.AgentRun{ID: uuid.New(), State: types.RunRunning, CreatedBy: subject, EndsAt: &end, SandboxRef: "ref-1"}
	st.runs[fx.run.ID] = fx.run
	ado, ok := resolveADOEntraRun(st.site, []string{adoTestRepo}, subject)
	if !ok || ado.tokenMode != types.ADOTokenModeMintedPAT || ado.patHours != types.ADOPATMaxHoursDefault {
		t.Fatalf("lane = %+v ok=%v, want a minted_pat lane with the default token life", ado, ok)
	}
	fx.ado = ado
	return fx
}

func (fx *adoPATFixture) dispatch(t *testing.T) bool {
	t.Helper()
	_, _, ok := fx.srv.authorADOEntraInjection(context.Background(), fx.run, fx.ado, "CERT", "KEY",
		&types.RunPolicySpec{}, map[string]string{}, nil)
	for _, g := range fx.st.grants {
		var sc struct{ Host string }
		_ = json.Unmarshal(g.Spec.Scope, &sc)
		fx.grants[sc.Host] = g.ID
	}
	return ok
}

// newADOPATFixture is the lane, dispatched.
func newADOPATFixture(t *testing.T) *adoPATFixture {
	t.Helper()
	fx := newADOPATLane(t)
	if !fx.dispatch(t) {
		t.Fatalf("dispatch refused: %q", fx.st.hint)
	}
	return fx
}

// resolve is one proxy resolve for host, with the extra query q.
func (fx *adoPATFixture) resolve(t *testing.T, host string, q url.Values) (*httptest.ResponseRecorder, types.ResolvedInjection) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+fx.grants[host].String()+"?"+q.Encode(), nil)
	fx.srv.resolveADOInjection(w, r,
		&identity.Claims{RunID: fx.run.ID, Sub: fx.subject, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + host, Injection: &egress.InjectionRule{Host: host, SecretName: types.ADOEntraAccessTokenSecret}},
		fx.grants[host])
	var resp types.ResolvedInjection
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, resp
}

// ok is resolve that must answer 200.
func (fx *adoPATFixture) ok(t *testing.T, host string, q url.Values) types.ResolvedInjection {
	t.Helper()
	w, resp := fx.resolve(t, host, q)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve %s: status %d body %q", host, w.Code, w.Body.String())
	}
	return resp
}

// mintReasons is the reason of every ado_pat.mint row, in order.
func (fx *adoPATFixture) auditReasons(action string) []string {
	var out []string
	for _, ev := range fx.audit.find(action) {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		out = append(out, fmt.Sprint(d["reason"]))
	}
	return out
}

func stale(jti string) url.Values { return url.Values{"stale_jti": {jti}} }

// Dispatch creates ONE token scoped exactly to PATScope(caps), records it,
// authors Basic grants, and every host's resolve hands out that token as
// Basic ":"+PAT with its own validTo and authorization id.
func TestMintedPAT_DispatchCreatesOneTokenScopedToTheRun(t *testing.T) {
	fx := newADOPATFixture(t)
	wantScope, _ := adoscope.PATScope(fx.ado.caps)
	validTo := adoTestNow.Add(8 * time.Hour).UTC().Truncate(time.Second)
	if n := fx.pats.createCount(); n != 1 {
		t.Fatalf("creates at dispatch = %d, want 1", n)
	}
	req := fx.pats.creates[0]
	if req.Scope != wantScope || !req.ValidTo.Equal(validTo) || req.DisplayName != "Wardyn run "+fx.run.ID.String()[:8] {
		t.Fatalf("create = %+v, want scope %q, validTo %v, name 'Wardyn run %s'", req, wantScope, validTo, fx.run.ID.String()[:8])
	}
	rows := fx.st.unrevoked(t)
	if len(rows) != 1 || rows[0].Scope != wantScope || rows[0].Org != "contoso" || rows[0].Owner != fx.subject ||
		rows[0].ProviderRowID != fx.cfg.RowID {
		t.Fatalf("recorded rows = %+v, want one for this run's token", rows)
	}
	for host, id := range fx.grants {
		for _, g := range fx.st.grants {
			if g.ID == id && !strings.Contains(string(g.Spec.Scope), `"format":"Basic %s"`) {
				t.Errorf("grant for %s = %s, want format Basic %%s", host, g.Spec.Scope)
			}
		}
	}
	if got := fx.auditReasons(adoPATAuditMint); !slices.Equal(got, []string{adoPATMintDispatch}) {
		t.Fatalf("ado_pat.mint reasons = %v, want [dispatch]", got)
	}
	token := fx.pats.tokens[0]
	for _, host := range []string{"dev.azure.com", "vssps.dev.azure.com", "contoso.visualstudio.com"} {
		resp := fx.ok(t, host, nil)
		if resp.Header != "Authorization" || resp.Value != "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+token)) ||
			resp.JTI != rows[0].AuthorizationID.String() || resp.ExpiresAt != validTo.UnixMilli() {
			t.Fatalf("%s: response = %+v, want Basic of the token, JTI %s, ExpiresAt %d", host, resp, rows[0].AuthorizationID, validTo.UnixMilli())
		}
	}
	if n := fx.pats.createCount(); n != 1 {
		t.Fatalf("creates after three resolves = %d, want still 1", n)
	}
}

// A launch that cannot get a token is refused with the reason, and authors
// nothing.
func TestMintedPAT_DispatchRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*adoPATFixture)
		hint  string
	}{
		"not connected": {func(fx *adoPATFixture) {
			fx.ado.owner = "someone-who-never-signed-in"
			fx.run.CreatedBy = fx.ado.owner
		}, adoRunPATNotConnected},
		"organisation blocks token creation": {func(fx *adoPATFixture) {
			fx.pats.createErr = &adoPATError{Status: http.StatusForbidden, PatTokenError: adoPATErrAccessDenied}
		}, adoRunPATPolicyBlocked},
		"no token client": {func(fx *adoPATFixture) { fx.srv.adoPATs = nil }, adoRunPATUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newADOPATLane(t)
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunStarting })
			fx.run.State = types.RunStarting
			tc.setup(fx)
			if fx.dispatch(t) {
				t.Fatal("dispatch went ahead")
			}
			if len(fx.st.grants) != 0 || len(fx.st.unrevoked(t)) != 0 {
				t.Fatalf("a refused launch authored grants=%d rows=%d", len(fx.st.grants), len(fx.st.unrevoked(t)))
			}
			if fx.st.hint != tc.hint {
				t.Fatalf("failure hint = %q, want %q", fx.st.hint, tc.hint)
			}
		})
	}
}

// Renewal is resolve-driven: outside the window the cached token, the first
// resolve inside it creates the next, a second host gets that one without a
// create, and the old token is not revoked.
func TestMintedPAT_RenewalIsResolveDriven(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	validTo := time.UnixMilli(first.ExpiresAt)

	fx.setClock(validTo.Add(-adoRunPATRenewWindow - time.Minute))
	if got := fx.ok(t, "dev.azure.com", nil); got.JTI != first.JTI || fx.pats.createCount() != 1 {
		t.Fatalf("outside the window: JTI %s creates %d, want the cached token and no create", got.JTI, fx.pats.createCount())
	}
	fx.setClock(validTo.Add(-adoRunPATRenewWindow + time.Minute))
	renewed := fx.ok(t, "vssps.dev.azure.com", nil)
	if renewed.JTI == first.JTI || fx.pats.createCount() != 2 || renewed.ExpiresAt <= first.ExpiresAt {
		t.Fatalf("inside the window: JTI %s creates %d expires %d, want a new, later token", renewed.JTI, fx.pats.createCount(), renewed.ExpiresAt)
	}
	if other := fx.ok(t, "dev.azure.com", stale(first.JTI)); other.JTI != renewed.JTI || fx.pats.createCount() != 2 {
		t.Fatalf("second host: JTI %s creates %d, want the renewed token from the cache", other.JTI, fx.pats.createCount())
	}
	if len(fx.pats.revokedIDs()) != 0 || len(fx.st.unrevoked(t)) != 2 {
		t.Fatalf("revoked %v, live rows %d: the old token must live to its own validTo", fx.pats.revokedIDs(), len(fx.st.unrevoked(t)))
	}
	if got := fx.auditReasons(adoPATAuditMint); !slices.Equal(got, []string{adoPATMintDispatch, adoPATMintRenewal}) {
		t.Fatalf("mint reasons = %v", got)
	}
}

// A renewal Azure DevOps refuses keeps the current token until its validTo.
func TestMintedPAT_RenewFailureKeepsTheCurrentToken(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.setClock(time.UnixMilli(first.ExpiresAt).Add(-time.Minute))
	fx.pats.createErr = &adoPATError{Status: http.StatusServiceUnavailable}
	if got := fx.ok(t, "dev.azure.com", nil); got.JTI != first.JTI {
		t.Fatalf("JTI %s, want the current token while it still works", got.JTI)
	}
	if got := fx.auditReasons(adoPATAuditMintDenied); !slices.Equal(got, []string{adoPATMintRenewal}) {
		t.Fatalf("mint.denied reasons = %v, want the renewal's", got)
	}
}

// An approval installs a union token; another host naming the old one as
// stale gets the union token without a create; the narrower one lives on.
func TestMintedPAT_WideningInstallsAUnionToken(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	ask := url.Values{"capability": {string(adoscope.CapPR)}, "first_use": {string(types.FirstUseWaitForReview)},
		"method": {"POST"}, "path": {"/contoso/proj/_apis/git/repositories/app/pullrequests"}}
	w, _ := fx.resolve(t, "dev.azure.com", ask)
	id := pendingID(t, w, adoCapabilityPendingState)
	if _, err := fx.fa.Decide(context.Background(), id, types.ActorHuman,
		types.ApprovalDecision{State: types.ApprovalApproved, Scope: types.ScopeRun}); err != nil {
		t.Fatal(err)
	}
	ask.Set("approval", id.String())
	union := fx.ok(t, "dev.azure.com", ask)
	wantScope, _ := adoscope.PATScope([]adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR})
	if union.JTI == first.JTI || fx.pats.createCount() != 2 || fx.pats.creates[1].Scope != wantScope ||
		!slices.Contains(union.Capabilities, string(adoscope.CapPR)) {
		t.Fatalf("union = %+v creates %d scope %q, want a new token scoped %q", union, fx.pats.createCount(),
			fx.pats.creates[len(fx.pats.creates)-1].Scope, wantScope)
	}
	if other := fx.ok(t, "vssps.dev.azure.com", stale(first.JTI)); other.JTI != union.JTI || fx.pats.createCount() != 2 {
		t.Fatalf("other host with the old JTI: got %s creates %d, want the union token and no create", other.JTI, fx.pats.createCount())
	}
	if len(fx.pats.revokedIDs()) != 0 || len(fx.st.unrevoked(t)) != 2 {
		t.Fatalf("revoked %v: the narrower token must not be revoked early", fx.pats.revokedIDs())
	}
	if got := fx.auditReasons(adoPATAuditMint); !slices.Equal(got, []string{adoPATMintDispatch, adoPATMintWiden}) {
		t.Fatalf("mint reasons = %v", got)
	}
}

// A stale_jti naming the CURRENT token creates a fresh one, at most once per
// run per minute, revokes the refused one, and is audited.
func TestMintedPAT_StaleCurrentForcesOneMintPerMinute(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fresh := fx.ok(t, "dev.azure.com", stale(first.JTI))
	if fresh.JTI == first.JTI || fx.pats.createCount() != 2 {
		t.Fatalf("stale current: JTI %s creates %d, want a fresh token", fresh.JTI, fx.pats.createCount())
	}
	if got := fx.pats.revokedIDs(); !slices.Equal(got, []string{first.JTI}) {
		t.Fatalf("revoked %v, want the refused token", got)
	}
	if got := fx.auditReasons(adoPATAuditRevoke); !slices.Equal(got, []string{adoPATRevokeUpstream401}) {
		t.Fatalf("revoke reasons = %v", got)
	}
	fx.advance(30 * time.Second)
	if again := fx.ok(t, "dev.azure.com", stale(fresh.JTI)); again.JTI != fresh.JTI || fx.pats.createCount() != 2 {
		t.Fatalf("inside the minute: JTI %s creates %d, want the current token", again.JTI, fx.pats.createCount())
	}
	fx.advance(31 * time.Second)
	if later := fx.ok(t, "dev.azure.com", stale(fresh.JTI)); later.JTI == fresh.JTI || fx.pats.createCount() != 3 {
		t.Fatalf("after the minute: JTI %s creates %d, want another fresh token", later.JTI, fx.pats.createCount())
	}
	if got := fx.auditReasons(adoPATAuditMint); !slices.Equal(got,
		[]string{adoPATMintDispatch, adoPATMintUpstream401, adoPATMintUpstream401}) {
		t.Fatalf("mint reasons = %v", got)
	}
}

// Pause revokes every token and empties the cache; a paused run's resolve is
// refused; after resume, the revoked JTI named as stale creates a fresh one.
func TestMintedPAT_PauseRevokesAndResumeRemints(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	second := fx.ok(t, "dev.azure.com", stale(first.JTI)) // two rows: one revoked, one live
	fx.setClock(time.UnixMilli(second.ExpiresAt).Add(-time.Minute))
	third := fx.ok(t, "dev.azure.com", nil) // a renewal: two live rows now
	if live := fx.st.unrevoked(t); len(live) != 2 {
		t.Fatalf("live rows before pause = %d, want 2", len(live))
	}
	rn := &pauseRunner{fakeRunner: &fakeRunner{}, freeze: map[types.ConfinementClass]bool{types.CC1: true}}
	fx.srv.cfg.Runner = rn
	run, _ := fx.st.GetRun(context.Background(), fx.run.ID)
	fx.srv.pauseRun(context.Background(), fx.st, run, types.PauseIdle, time.Hour)
	if live := fx.st.unrevoked(t); len(live) != 0 {
		t.Fatalf("live rows after pause = %+v, want none", live)
	}
	if got := fx.pats.revokedIDs(); !slices.Contains(got, second.JTI) || !slices.Contains(got, third.JTI) {
		t.Fatalf("revoked %v, want both live tokens", got)
	}
	creates := fx.pats.createCount()
	w, _ := fx.resolve(t, "dev.azure.com", stale(third.JTI))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonADOPATRunInactive) || fx.pats.createCount() != creates {
		t.Fatalf("paused resolve: status %d body %q creates %d, want a 409 and no create", w.Code, w.Body.String(), fx.pats.createCount())
	}
	run, _ = fx.st.GetRun(context.Background(), fx.run.ID)
	if err := fx.srv.resumeRun(context.Background(), fx.st, run, types.ActorSystem, "wardynd", "test"); err != nil {
		t.Fatal(err)
	}
	resumed := fx.ok(t, "dev.azure.com", stale(third.JTI))
	if resumed.JTI == third.JTI || fx.pats.createCount() != creates+1 {
		t.Fatalf("after resume: JTI %s creates %d, want a fresh token", resumed.JTI, fx.pats.createCount())
	}
	if got := fx.auditReasons(adoPATAuditMint); got[len(got)-1] != adoPATMintResume {
		t.Fatalf("mint reasons = %v, want the last to be resume", got)
	}
}

// Every end path revokes every live token of the run, with its reason.
func TestMintedPAT_EveryEndPathRevokes(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		end    func(*adoPATFixture)
		reason string
	}{
		// finalizeRunTail is the completion watcher, the lease's stop, the
		// reconciler and the probe reclaim.
		"complete, fail, lease end, reconcile, probe kill": {func(fx *adoPATFixture) {
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunCompleted })
			fx.srv.finalizeRunTail(ctx, fx.run.ID, "", "run.complete", "success", map[string]any{})
		}, adoPATRevokeRunEnd},
		"a failed dispatch or exec": {func(fx *adoPATFixture) {
			fx.srv.failAndRevoke(ctx, fx.run.ID, types.RunRunning, "exec failed")
		}, adoPATRevokeRunEnd},
		"kill": {func(fx *adoPATFixture) {
			run, _ := fx.st.GetRun(ctx, fx.run.ID)
			if applied, _, err := fx.srv.killRunCascade(ctx, run, types.ActorHuman, fx.subject, nil); !applied || err != nil {
				t.Fatalf("kill applied=%v err=%v", applied, err)
			}
		}, adoPATRevokeKill},
		"idle stop": {func(fx *adoPATFixture) {
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunStopped })
			fx.srv.CancelTerminalRunApprovals(ctx, fx.run.ID)
		}, adoPATRevokeRunEnd},
		"a kept run's end (lease end, lost sandbox)": {func(fx *adoPATFixture) {
			fx.srv.revokeRunBroker(ctx, fx.run.ID)
		}, adoPATRevokeRunEnd},
		"boot reconcile of a terminal run's sandbox": {func(fx *adoPATFixture) {
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunFailed })
			if err := fx.srv.reconcileOrphanedSandbox(ctx); err != nil {
				t.Fatal(err)
			}
		}, adoPATRevokeRunEnd},
		"sandbox sweep": {func(fx *adoPATFixture) {
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunFailed })
			fx.srv.cfg.Runner = &fakeRunner{}
			fx.srv.SweepTerminalSandboxes(ctx) //nolint:errcheck // asserted through the revoke
		}, adoPATRevokeRunEnd},
		"token sweep of a terminal run": {func(fx *adoPATFixture) {
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunFailed })
			if err := fx.srv.sweepRunPATs(ctx); err != nil {
				t.Fatal(err)
			}
		}, adoPATRevokeSweep},
		"drift": {func(fx *adoPATFixture) {
			fx.st.site.WorkspaceProviders.Git[0].Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapRead}
			if w, _ := fx.resolve(t, "dev.azure.com", nil); w.Code != http.StatusForbidden {
				t.Fatalf("drifted resolve: status %d, want 403", w.Code)
			}
		}, adoPATRevokeDrift},
		"offboarding": {func(fx *adoPATFixture) {
			erasePerson(t, fx.srv, fx.subject)
		}, adoPATRevokeOffboarding},
		"disconnect": {func(fx *adoPATFixture) {
			fx.srv.revokeOwnerRunPATs(ctx, fx.subject, adoPATRevokeDisconnect)
		}, adoPATRevokeDisconnect},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newADOPATFixture(t)
			first := fx.ok(t, "dev.azure.com", nil)
			fx.setClock(time.UnixMilli(first.ExpiresAt).Add(-time.Minute))
			second := fx.ok(t, "dev.azure.com", nil) // renewal: two live tokens
			fx.audit.rows = nil
			tc.end(fx)
			if live := fx.st.unrevoked(t); len(live) != 0 {
				t.Fatalf("live rows = %+v, want none", live)
			}
			if got := fx.pats.revokedIDs(); !slices.Contains(got, first.JTI) || !slices.Contains(got, second.JTI) {
				t.Fatalf("revoked %v, want both tokens", got)
			}
			if got := fx.auditReasons(adoPATAuditRevoke); len(got) != 2 || got[0] != tc.reason || got[1] != tc.reason {
				t.Fatalf("revoke reasons = %v, want two %q", got, tc.reason)
			}
		})
	}
}

// A restart loses the cache: the next resolve creates a token and revokes
// nothing the proxy may hold. The sweep closes expired rows of a live run and
// revokes the tokens of a run that ended.
func TestMintedPAT_RestartRemintsAndTheSweepCleansUp(t *testing.T) {
	ctx := context.Background()
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.srv.adoRunPATs = adoRunPATCache{} // the daemon restarted
	fx.advance(time.Hour)
	again := fx.ok(t, "dev.azure.com", nil)
	if again.JTI == first.JTI || len(fx.pats.revokedIDs()) != 0 || len(fx.st.unrevoked(t)) != 2 {
		t.Fatalf("restart: JTI %s revoked %v rows %d, want a new token and nothing revoked", again.JTI,
			fx.pats.revokedIDs(), len(fx.st.unrevoked(t)))
	}
	if got := fx.auditReasons(adoPATAuditMint); got[len(got)-1] != adoPATMintRestart {
		t.Fatalf("mint reasons = %v, want the last to be restart", got)
	}

	// Past the first token's validTo, the sweep closes its row as expired and
	// calls nothing.
	fx.setClock(time.UnixMilli(first.ExpiresAt).Add(time.Minute))
	if err := fx.srv.sweepRunPATs(ctx); err != nil {
		t.Fatal(err)
	}
	if live := fx.st.unrevoked(t); len(live) != 1 || live[0].AuthorizationID.String() != again.JTI || len(fx.pats.revokedIDs()) != 0 {
		t.Fatalf("after the sweep: live %+v revoked %v, want only the restart's token live", live, fx.pats.revokedIDs())
	}
	// Inside adoRunPATSweepEvery it does nothing; after it, a terminal run's
	// tokens are revoked.
	fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunKilled })
	fx.advance(time.Minute)
	if err := fx.srv.sweepRunPATs(ctx); err != nil || len(fx.st.unrevoked(t)) != 1 {
		t.Fatalf("a sweep inside the interval acted: err=%v", err)
	}
	fx.advance(adoRunPATSweepEvery)
	if err := fx.srv.sweepRunPATs(ctx); err != nil || len(fx.st.unrevoked(t)) != 0 {
		t.Fatalf("the sweep left %d live rows for a killed run: err=%v", len(fx.st.unrevoked(t)), err)
	}
}

// Concurrent resolves from every host at boot create one token.
func TestMintedPAT_ConcurrentResolvesCreateOneToken(t *testing.T) {
	fx := newADOPATFixture(t)
	fx.srv.adoRunPATs = adoRunPATCache{}
	var wg sync.WaitGroup
	for host := range fx.grants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/x", nil)
			fx.srv.resolveADOInjection(w, r,
				&identity.Claims{RunID: fx.run.ID, Sub: fx.subject, SPIFFEID: "spiffe://wardyn.local/run"},
				broker.Minted{JTI: "jti", Injection: &egress.InjectionRule{Host: host, SecretName: types.ADOEntraAccessTokenSecret}},
				fx.grants[host])
		}()
	}
	wg.Wait()
	if n := fx.pats.createCount(); n != 2 {
		t.Fatalf("creates = %d, want 2 (dispatch, then one after the restart)", n)
	}
}

// No token value reaches a row, an audit row or a log line.
func TestMintedPAT_NoTokenInRowsLogsOrAudit(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.ok(t, "dev.azure.com", stale(first.JTI))
	fx.pats.revokeErr = &adoPATError{Status: http.StatusInternalServerError}
	fx.srv.revokeRunPATs(context.Background(), fx.run.ID, adoPATRevokeRunEnd)
	rows := fmt.Sprintf("%+v", fx.st.unrevoked(t))
	for _, tok := range fx.pats.tokens {
		for _, form := range []string{tok, base64.StdEncoding.EncodeToString([]byte(":" + tok))} {
			if strings.Contains(rows, form) || strings.Contains(logs.String(), form) {
				t.Fatalf("a token form reached a row or a log line")
			}
			for _, ev := range fx.audit.rows {
				if strings.Contains(string(ev.Data), form) || strings.Contains(ev.Target, form) {
					t.Fatalf("a token form reached audit row %s", ev.Action)
				}
			}
		}
	}
	if len(fx.audit.find(adoPATAuditRevokeFailed)) == 0 || len(fx.st.unrevoked(t)) == 0 {
		t.Fatal("a revoke that did not complete must be audited and left for the sweep")
	}
}

func TestADORunPATValidTo(t *testing.T) {
	now := adoTestNow
	end := now.Add(2 * time.Hour)
	past := now.Add(-time.Hour)
	for name, tc := range map[string]struct {
		endsAt *time.Time
		want   time.Time
	}{
		"no end":           {nil, now.Add(8 * time.Hour)},
		"the run ends":     {&end, end.Add(adoRunPATEndGrace)},
		"the run is ended": {&past, now.Add(adoRunPATEndGrace)},
	} {
		if got := adoRunPATValidTo(now, 8, tc.endsAt); !got.Equal(tc.want.UTC().Truncate(time.Second)) {
			t.Errorf("%s: validTo = %v, want %v", name, got, tc.want)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the log handler's writers.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
