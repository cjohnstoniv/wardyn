// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The captured-AWS-SSO injection resolve, from the sidecar's side: the whole
// security contract of Phase B is what this endpoint refuses.

const (
	reauthRegion = "eu-west-2"
	reauthPortal = "portal.sso.eu-west-2.amazonaws.com"
	reauthToken  = "live-sso-access-token-9f2c"
)

// reauthStore is the minimal store the resolve path reads: the run (for its
// agent and creator), the roster, the run's grants (for the snapshot) and the
// transactional resolve seam.
type reauthStore struct {
	store.Store
	mu        sync.Mutex
	run       types.AgentRun
	loginRun  types.AgentRun
	site      types.SiteConfig
	grants    []types.CredentialGrant
	approvals *fakeApprovals
	// audit is where the TRANSACTION writes credential.reauth.resolved. The
	// real store inserts it into audit_events inside the same tx, never through
	// Server.recordAudit, so a fake that dropped it would make the row invisible
	// to exactly the assertion that matters.
	audit     *reauthRecorder
	resolveNs int // ResolveReauthApproval calls
	resolveNo bool
	auditFail bool
}

func (s *reauthStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id == s.loginRun.ID && s.loginRun.ID != uuid.Nil {
		return s.loginRun, nil
	}
	if id == s.run.ID {
		return s.run, nil
	}
	return types.AgentRun{}, errStoreNotFound
}
func (s *reauthStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }
func (s *reauthStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}
func (s *reauthStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

// ResolveReauthApproval mirrors store.PG's contract: a PENDING-only CAS, and
// the audit row and the state change stand or fall together.
func (s *reauthStore) ResolveReauthApproval(ctx context.Context, id uuid.UUID, d types.ApprovalDecision, ev types.AuditEvent) (types.ApprovalRequest, error) {
	if s.resolveNo {
		return types.ApprovalRequest{}, fmt.Errorf("this store has no transactional resolve")
	}
	s.mu.Lock()
	s.resolveNs++
	s.mu.Unlock()
	if s.auditFail {
		// The whole point of the one transaction: a failed audit write leaves
		// the row PENDING.
		return types.ApprovalRequest{}, fmt.Errorf("audit sink refused %s", ev.Action)
	}
	cur, err := s.approvals.Get(ctx, id)
	if err != nil {
		return types.ApprovalRequest{}, err
	}
	if cur.State != types.ApprovalPending {
		return types.ApprovalRequest{}, store.ErrAlreadyDecided
	}
	out, derr := s.approvals.Decide(ctx, id, types.ActorHuman, d)
	if derr != nil {
		return types.ApprovalRequest{}, derr
	}
	if s.audit != nil {
		_ = s.audit.Record(ctx, ev)
	}
	return out, nil
}

func reauthRosterRow(perUser bool) types.SiteConfig {
	src := types.CredentialSourceShared
	if perUser {
		src = types.CredentialSourcePerUser
	}
	return agentRoster(types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: src, SSOStartURL: "https://acme.awsapps.com/start",
	})
}

func reauthSnapshotScope(t *testing.T, owner, region string, perUser bool, account, role string) json.RawMessage {
	t.Helper()
	src := string(types.CredentialSourceShared)
	if perUser {
		src = string(types.CredentialSourcePerUser)
	}
	raw, err := json.Marshal(map[string]any{
		"host": reauthPortal, "header": awsSSOInjectHeader, "format": "%s",
		"secret_name": types.AWSSSOAccessTokenSecret,
		"snapshot": awsSSOScopeSnapshot{
			OwnerSubject: owner, CredentialSource: src,
			Mechanism:    string(types.AgentMechanismBedrockSSO),
			SSOAccountID: account, SSORoleName: role, Region: region,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// reauthRecorder is recRecorder with a lock and two questions. The lock is not
// decoration: one case drives sixteen concurrent resolvers, each of which
// records, and the shared fake would race.
type reauthRecorder struct {
	mu  sync.Mutex
	evs []types.AuditEvent
}

func (r *reauthRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
	return nil
}

func (r *reauthRecorder) events() []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.AuditEvent(nil), r.evs...)
}

func (r *reauthRecorder) has(action, outcome string) bool {
	for _, ev := range r.events() {
		if ev.Action == action && ev.Outcome == outcome {
			return true
		}
	}
	return false
}

// hasReason matches a FAILURE row of action whose data names reason.
func (r *reauthRecorder) hasReason(action, reason string) bool {
	for _, ev := range r.events() {
		if ev.Action != action || ev.Outcome != "failure" {
			continue
		}
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) == nil && d["reason"] == reason {
			return true
		}
	}
	return false
}

// reauthBroker is a MintBroker with no shared mutable state. The package's own
// fakeBroker records its last caller on every mint, which sixteen concurrent
// resolvers race on — and the race is the fake's, not the code's.
type reauthBroker struct {
	mu     sync.Mutex
	minted broker.Minted
}

func (b *reauthBroker) MintForGrant(context.Context, *identity.Claims, uuid.UUID) (broker.Minted, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.minted
	rule := *m.Injection
	m.Injection = &rule
	return m, nil
}

func (b *reauthBroker) RevokeRun(context.Context, uuid.UUID) error { return nil }

func (b *reauthBroker) setHost(host string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.minted.Injection.Host = host
}

type reauthFixture struct {
	srv     *Server
	brk     *reauthBroker
	st      *reauthStore
	audit   *reauthRecorder
	secrets *memSecrets
	token   string
	runID   uuid.UUID
	grantID uuid.UUID
}

// newReauthFixture wires a per_user captured-SSO run whose grant carries a
// truthful snapshot and whose owner has a live stored session.
func newReauthFixture(t *testing.T, opts func(*reauthFixture)) *reauthFixture {
	t.Helper()
	h := newHarness(t)
	runID, grantID := uuid.New(), uuid.New()
	f := &reauthFixture{audit: &reauthRecorder{}, runID: runID, grantID: grantID}
	f.st = &reauthStore{
		run:       types.AgentRun{ID: runID, Agent: "claude-code", CreatedBy: "alice@example.com"},
		site:      reauthRosterRow(true),
		approvals: h.approvals,
		audit:     f.audit,
		grants: []types.CredentialGrant{{
			ID: grantID, RunID: runID,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: reauthSnapshotScope(t, "alice@example.com", reauthRegion, true, "111122223333", "WardynAgent")},
		}},
	}
	// BOTH maps, initialised. memSecrets.For hands back a COPY that shares these
	// maps, so a nil one means the copy allocates its own and the write is lost —
	// silently, and only for the OPERATOR namespace (owner ""), which is the one
	// the shared/legacy lane reads. A putBlob(t, "", …) then looks like a stored
	// session and resolves as "no session at all".
	f.secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	cfg := baseTestConfig(h, f.st)
	// An OIDC authenticator so the member tier is reachable in this fixture:
	// the kind rule must answer the same 409 to a member as to a security
	// operator (security NIT-5), and without a session decoder a member request
	// never reaches the rule at all.
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = f.audit
	cfg.Approvals = h.approvals
	cfg.Secrets = f.secrets
	f.brk = &reauthBroker{minted: broker.Minted{
		Kind: types.GrantAPIKey, JTI: "jti-1", GrantID: grantID,
		Injection: &egress.InjectionRule{
			Host: reauthPortal, Header: awsSSOInjectHeader, Format: "%s",
			SecretName: types.AWSSSOAccessTokenSecret,
		},
	}}
	cfg.Broker = f.brk
	f.srv = New(cfg)
	h.srv = f.srv
	f.token = h.mintRunToken(t, runID)
	if opts != nil {
		opts(f)
	}
	return f
}

// putBlob stores an SSO session in a namespace.
func (f *reauthFixture) putBlob(t *testing.T, owner string, b awsSSOBlob) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.secrets.For(owner).Put(context.Background(), harnessCredSecretName(awsSSOProvider), raw); err != nil {
		t.Fatal(err)
	}
}

func liveSSOBlob() awsSSOBlob {
	return awsSSOBlob{
		AccessToken: reauthToken, StartURL: "https://acme.awsapps.com/start", Region: reauthRegion,
		AccountID: "111122223333", RoleName: "WardynAgent",
		ExpiresAt: time.Now().Add(2 * time.Hour).UTC(), CapturedAt: time.Now().UTC(),
	}
}

func deadSSOBlob() awsSSOBlob {
	b := liveSSOBlob()
	b.ExpiresAt = time.Now().Add(-time.Hour).UTC()
	return b // no refresh token: nothing to renew, and nothing can heal it
}

func (f *reauthFixture) resolve(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String(), f.token, "")
}

// A LIVE session resolves to the forced header and the blob's own expiry, and
// the token is masked for THIS run.
func TestResolveAWSSSOInjection_LiveSessionInjectsAndMasksPerRun(t *testing.T) {
	f := newReauthFixture(t, nil)
	blob := liveSSOBlob()
	f.putBlob(t, "alice@example.com", blob)

	w := f.resolve(t)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Header != awsSSOInjectHeader {
		t.Errorf("header = %q, want %q", resp.Header, awsSSOInjectHeader)
	}
	if resp.Value != reauthToken {
		t.Errorf("value = %q, want the bare token (format %%s, no Bearer prefix)", resp.Value)
	}
	if resp.ExpiresAt != blob.ExpiresAt.UnixMilli() {
		t.Errorf("expires_at = %d, want the blob's own expiry %d — the proxy re-resolves inside its refresh margin, so a 0 here would make it treat a rotating session as static", resp.ExpiresAt, blob.ExpiresAt.UnixMilli())
	}
	if !f.audit.has("secret.read", "success") {
		t.Error("no secret.read success row for a resolve that handed back a live session")
	}
}

// I4 — the host pin. A grant naming any other host is refused, not injected.
func TestResolveAWSSSOInjection_HostPinRefusesAnotherHost(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", liveSSOBlob())
	f.brk.setHost("evil.example.com")

	w := f.resolve(t)
	if w.Code != http.StatusForbidden {
		t.Fatalf("resolve: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), reauthToken) {
		t.Fatal("the refusal body echoed the session token")
	}
	if !f.audit.hasReason("secret.read", "sso_host_not_portal") {
		t.Error("no secret.read failure naming the host pin")
	}
}

// I3 — the substitution hole. An admin flips the roster row per_user -> shared
// while the run is working: a resolve-time roster read would hand this run the
// OPERATOR's blob. It must be a 403, and no blob may be read at all.
func TestResolveAWSSSOInjection_RosterDriftIsRefusedNotSubstituted(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", liveSSOBlob())
	operatorBlob := liveSSOBlob()
	operatorBlob.AccessToken = "the-operators-own-session-token"
	f.putBlob(t, "", operatorBlob)

	f.st.site = reauthRosterRow(false) // the admin flips it to shared

	w := f.resolve(t)
	if w.Code != http.StatusForbidden {
		t.Fatalf("resolve: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), operatorBlob.AccessToken) || strings.Contains(w.Body.String(), reauthToken) {
		t.Fatal("a token reached the drift refusal body")
	}
	if !f.audit.hasReason("secret.read", "scope_changed") {
		t.Error("no secret.read failure naming scope_changed")
	}
}

// I3 on the declared lane: a per_user row flipped from bedrock_sso to
// bedrock_bearer mid-run no longer names the session this run was dispatched
// with, so the resolve refuses — the same rule the bearer sink applies when a
// per_user row stops declaring the bearer.
func TestResolveAWSSSOInjection_RowFlippedToTheBearerIsRefused(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", liveSSOBlob())
	f.st.site = agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer,
		CredentialSource: types.CredentialSourcePerUser})

	w := f.resolve(t)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), reauthToken) {
		t.Fatalf("resolve: code = %d, want 403 with no token; body=%s", w.Code, w.Body.String())
	}
	if !f.audit.hasReason("secret.read", "scope_changed") {
		t.Error("no secret.read failure naming scope_changed")
	}
}

// I3, the other direction: the roster row's account/role PIN is admin-asserted
// identity. Re-pinning it mid-run must not silently re-point a held run.
func TestResolveAWSSSOInjection_AccountPinDriftIsRefused(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", liveSSOBlob())
	row := f.st.site.AgentProviders.Agents[0]
	row.SSOAccountID = "999988887777"
	f.st.site.AgentProviders.Agents[0] = row

	if w := f.resolve(t); w.Code != http.StatusForbidden {
		t.Fatalf("resolve: code = %d, want 403 on an account-pin change; body=%s", w.Code, w.Body.String())
	}
}

// I2 — a grant naming an owner who is not the run token's own subject cannot
// pass on the per_user lane, however truthfully it names the roster.
func TestResolveAWSSSOInjection_GrantNamingAnotherOwnerIsRefused(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "bob@corp.example", liveSSOBlob())
	f.st.grants[0].Spec.Scope = reauthSnapshotScope(t, "bob@corp.example", reauthRegion, true, "111122223333", "WardynAgent")

	w := f.resolve(t)
	if w.Code != http.StatusForbidden {
		t.Fatalf("resolve: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !f.audit.hasReason("secret.read", "scope_changed") {
		t.Error("no scope_changed failure for a grant naming another member")
	}
	// #656: the audited reason must also reach the wire body — the proxy has
	// no audit access and used to see only the human sentence.
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason != reasonScopeChanged {
		t.Errorf("wire reason = %q, want %q", body.Reason, reasonScopeChanged)
	}
}

// A DEAD session raises exactly ONE visible request and answers 423 with the id
// the sidecar polls. A second resolve while it is open answers 423 again with
// the SAME id and raises nothing.
func TestResolveAWSSSOInjection_DeadSessionRaisesOneRequestAndHolds(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())

	w := f.resolve(t)
	if w.Code != http.StatusLocked {
		t.Fatalf("resolve: code = %d, want 423; body=%s", w.Code, w.Body.String())
	}
	var body reauthPendingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 423: %v", err)
	}
	if body.State != reauthPendingState || body.ApprovalID == uuid.Nil {
		t.Fatalf("423 body = %+v, want a state and an approval id the sidecar can poll", body)
	}
	rows, _ := f.srv.cfg.Approvals.List(context.Background(), types.ApprovalPending)
	n := 0
	for _, ap := range rows {
		if ap.Kind == types.ApprovalCredentialReauth {
			n++
			var sc map[string]string
			_ = json.Unmarshal(ap.RequestedScope, &sc)
			if sc["owner"] != "alice@example.com" {
				t.Errorf("scope owner = %q, want the run's own subject", sc["owner"])
			}
			if _, carriesReason := sc["reason"]; carriesReason {
				t.Error("the raise reason is in requested_scope — the scope is the DEDUP key, so a spent->unavailable flip would raise a second row for one lapse")
			}
		}
	}
	if n != 1 {
		t.Fatalf("pending credential_reauth rows = %d, want exactly 1", n)
	}
	if !f.audit.has("credential.reauth.requested", "success") {
		t.Fatal("no credential.reauth.requested audit row")
	}

	// A second resolve joins the SAME request.
	w2 := f.resolve(t)
	if w2.Code != http.StatusLocked {
		t.Fatalf("second resolve: code = %d, want 423", w2.Code)
	}
	var body2 reauthPendingResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &body2)
	if body2.ApprovalID != body.ApprovalID {
		t.Errorf("second resolve raised a NEW request %s (first %s) — N concurrent calls for one run must share ONE request", body2.ApprovalID, body.ApprovalID)
	}
}

// A terminal row answers 403, never 423: a sidecar must not hold its whole
// budget against a question nobody can answer any more.
func TestResolveAWSSSOInjection_CancelledRequestIsTerminalNotHeld(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	if _, err := f.srv.cfg.Approvals.CancelForRun(context.Background(), f.runID, "run_killed"); err != nil {
		t.Fatal(err)
	}
	w := f.resolve(t)
	if w.Code != http.StatusForbidden {
		t.Fatalf("resolve after the run was killed: code = %d, want 403 terminal; body=%s", w.Code, w.Body.String())
	}
	// #656: a terminal hold now carries a machine reason on the wire too.
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason != reasonSigninClosed {
		t.Errorf("wire reason = %q, want %q", body.Reason, reasonSigninClosed)
	}
}

// The capture resolves the request, and the NEXT resolve answers 200 on the new
// token — the whole point of the lane, end to end.
func TestResolveAWSSSOInjection_CaptureResolvesAndTheRetrySucceeds(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)

	// The sign-in lands: a login run created AFTER the raise (I6), owned by the
	// same principal (I2).
	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: time.Now().Add(time.Second)}
	f.st.loginRun = loginRun
	fresh := liveSSOBlob()
	fresh.AccessToken = "the-freshly-captured-token"
	f.putBlob(t, "alice@example.com", fresh)
	f.srv.resolvePendingReauth(context.Background(), awsSSOScope{perUser: true, owner: "alice@example.com"}, "alice@example.com", loginRun)

	got, err := f.srv.cfg.Approvals.Get(context.Background(), ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.ApprovalApproved {
		t.Fatalf("after the capture the request is %s, want APPROVED", got.State)
	}
	if !f.audit.has("credential.reauth.resolved", "success") {
		t.Error("no credential.reauth.resolved audit row")
	}
	if f.audit.has("approval.decide", "success") {
		t.Error("approval.decide was written for a decision nobody made")
	}

	w := f.resolve(t)
	if w.Code != http.StatusOK {
		t.Fatalf("retry after the sign-in: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Value != fresh.AccessToken {
		t.Errorf("retry value = %q, want the NEW token", resp.Value)
	}
}

// I6 — a sign-in that predates the request cannot answer it.
func TestResolveAWSSSOInjection_StaleCaptureDoesNotResolve(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)

	old := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: ap.RequestedAt.Add(-time.Minute)}
	f.srv.resolvePendingReauth(context.Background(), awsSSOScope{perUser: true, owner: "alice@example.com"}, "alice@example.com", old)

	got, _ := f.srv.cfg.Approvals.Get(context.Background(), ap.ID)
	if got.State != types.ApprovalPending {
		t.Fatalf("an older sign-in resolved a newer request: state = %s", got.State)
	}
}

// I2 on the resolution side — a capture by another principal resolves nothing.
func TestResolveAWSSSOInjection_WrongOwnerCaptureDoesNotResolve(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)

	other := types.AgentRun{ID: uuid.New(), CreatedBy: "bob@corp.example", CreatedAt: time.Now().Add(time.Minute)}
	f.srv.resolvePendingReauth(context.Background(), awsSSOScope{perUser: true, owner: "bob@corp.example"}, "bob@corp.example", other)

	got, _ := f.srv.cfg.Approvals.Get(context.Background(), ap.ID)
	if got.State != types.ApprovalPending {
		t.Fatalf("another member's sign-in resolved this request: state = %s", got.State)
	}
}

// I7 — no APPROVED without its audit row. A store whose transaction fails
// leaves the request PENDING, for the reconcile-on-read to repair.
func TestResolveAWSSSOInjection_AuditSinkFailureLeavesItPending(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)
	f.st.auditFail = true

	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: time.Now().Add(time.Second)}
	f.srv.resolvePendingReauth(context.Background(), awsSSOScope{perUser: true, owner: "alice@example.com"}, "alice@example.com", loginRun)

	got, _ := f.srv.cfg.Approvals.Get(context.Background(), ap.ID)
	if got.State != types.ApprovalPending {
		t.Fatalf("the request moved to %s although its audit row could not be written — I7 says the two commit together or neither does", got.State)
	}
}

// Codex #1 — the reconcile-on-read: a crash between the capture and the
// resolution must not strand a valid credential behind a PENDING row, and the
// repair must need no second human action.
func TestReconcileReauthOnRead_ResolvesAStrandedRequest(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)

	// The capture landed; the resolution did not (a crash in the gap).
	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: time.Now().Add(time.Second)}
	f.st.loginRun = loginRun
	fresh := liveSSOBlob()
	fresh.SourceRunID = loginRun.ID.String()
	f.putBlob(t, "alice@example.com", fresh)

	// The sidecar's ordinary poll of the approval repairs it.
	w := do(t, f.srv, http.MethodGet, "/api/v1/internal/approvals/"+ap.ID.String(), f.token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("poll: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.ApprovalRequest
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != types.ApprovalApproved {
		t.Fatalf("the poll read %s, want APPROVED — a stranded request must be derivable from the capture's own provenance", got.State)
	}
	if !f.audit.has("credential.reauth.resolved", "success") {
		t.Error("the reconcile resolved the row without its audit row")
	}

	// IDEMPOTENT: a second poll resolves nothing a second time.
	before := f.st.resolveNs
	_ = do(t, f.srv, http.MethodGet, "/api/v1/internal/approvals/"+ap.ID.String(), f.token, "")
	if f.st.resolveNs != before {
		t.Errorf("a second poll called the resolve transaction again (%d -> %d)", before, f.st.resolveNs)
	}
}

// …and it is not a SECOND, weaker door: a pre-request capture is refused on
// read exactly as it is on the eager path.
func TestReconcileReauthOnRead_RefusesAPreRequestCapture(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)

	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: ap.RequestedAt.Add(-time.Hour)}
	f.st.loginRun = loginRun
	fresh := liveSSOBlob()
	fresh.SourceRunID = loginRun.ID.String()
	f.putBlob(t, "alice@example.com", fresh)

	w := do(t, f.srv, http.MethodGet, "/api/v1/internal/approvals/"+ap.ID.String(), f.token, "")
	var got types.ApprovalRequest
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != types.ApprovalPending {
		t.Fatalf("a capture from BEFORE the request resolved it on read: %s", got.State)
	}
}

// S5/concurrency — many resolvers, ONE row. The dedup is what makes N waiting
// calls one human question.
func TestResolveAWSSSOInjection_ConcurrentResolversRaiseOneRequest(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())

	var wg sync.WaitGroup
	codes := make([]int, 16)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = do(t, f.srv, http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String(), f.token, "").Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusLocked {
			t.Fatalf("resolver %d: code = %d, want 423", i, c)
		}
	}
	rows, _ := f.srv.cfg.Approvals.List(context.Background(), types.ApprovalPending)
	n := 0
	for _, ap := range rows {
		if ap.Kind == types.ApprovalCredentialReauth {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("pending credential_reauth rows = %d, want exactly 1 for 16 concurrent resolvers", n)
	}
	// …AND ONE TRAIL ENTRY, AND ONE COUNT (W6-S F4). RequestApproval's dedup
	// answers the loser with the WINNER'S row, silently, so a caller that cannot
	// tell them apart audits and counts a request it did not raise — 16 rows
	// naming one approval id on a hash-chained log, and a `requested` counter
	// that no longer means "requests raised".
	raised := 0
	for _, ev := range f.audit.events() {
		if ev.Action == "credential.reauth.requested" {
			raised++
		}
	}
	if raised != 1 {
		t.Errorf("credential.reauth.requested rows = %d, want exactly 1 — the losers of the raise must "+
			"not audit a request somebody else raised", raised)
	}
	if got := reauthCount(t, f.srv, "requested"); got != "1" {
		t.Errorf("wardyn_credential_reauth_total{outcome=\"requested\"} = %s, want 1", got)
	}
}

// The workflow cap: the ninth is refused, and nothing is substituted.
func TestResolveAWSSSOInjection_WorkflowCapRefusesTheNinth(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	for i := 0; i < maxReauthHolds; i++ {
		if w := f.resolve(t); w.Code != http.StatusLocked {
			t.Fatalf("workflow %d: code = %d, want 423", i, w.Code)
		}
		// Resolve it so the NEXT resolve opens a new workflow rather than
		// joining this one.
		ap := onlyPendingReauthRow(t, f.srv)
		if _, err := f.srv.cfg.Approvals.Decide(context.Background(), ap.ID, types.ActorHuman,
			types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "alice@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	w := f.resolve(t)
	if w.Code != http.StatusForbidden {
		t.Fatalf("the ninth workflow: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too many times") {
		t.Errorf("cap refusal body = %s, want the capped sentence", w.Body.String())
	}
	// #656: the hold chain now carries a machine reason on the wire too.
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason != reasonSigninHoldsExhausted {
		t.Errorf("wire reason = %q, want %q", body.Reason, reasonSigninHoldsExhausted)
	}
}

// I8 — the token appears in no sink this endpoint writes to.
func TestResolveAWSSSOInjection_TokenIsInNoAuditSink(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", liveSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusOK {
		t.Fatalf("resolve: code = %d, want 200", w.Code)
	}
	for _, ev := range f.audit.events() {
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), reauthToken) {
			t.Fatalf("the session token is in an audit row: %s", raw)
		}
	}
}

// decide() refuses this kind on every tier: a Deny would read as governance and
// change nothing, because the next resolve raises a fresh row.
func TestDecide_RefusesACredentialReauthRow(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)
	// BOTH TIERS (security NIT-5). The admin token is the security tier; the
	// run's own member token is the other. The plan's promise is 409 on EVERY
	// tier, and behind the member gate a member used to get that gate's refusal
	// instead — refused either way, but for the wrong reason: "you may not use
	// this verb" rather than "this verb does not exist for this kind".
	for _, verb := range []string{"approve", "deny"} {
		w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+ap.ID.String()+"/"+verb, adminToken, "")
		if w.Code != http.StatusConflict {
			t.Errorf("security operator %s: code = %d, want 409; body=%s", verb, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "signing in") {
			t.Errorf("security operator %s: body = %s, want the not-decidable sentence", verb, w.Body.String())
		}
	}

	// THE MEMBER TIER, through a real OIDC session — and the two halves of it
	// (security round-2 SHOULD-1). The run's OWNER is told the same thing the
	// security operator is: the verb does not exist for this kind. A member who
	// does NOT own the run is told nothing at all, because a 409 there would
	// answer "is this UUID a credential_reauth approval of somebody's run?" for
	// any authenticated caller — the existence oracle routes.go, this file's own
	// member gate and approvals_decide_test.go all exist to deny. The first fix
	// for NIT-5 put the kind test ahead of ownership and opened exactly that.
	for _, tc := range []struct {
		name  string
		sess  *http.Cookie
		want  int
		wantB string
	}{
		{"the run's owner", ssoSession(t, "alice@example.com", "alice@example.com", oidc.RoleMember), http.StatusConflict, "signing in"},
		{"a foreign member", ssoSession(t, "member-sub", "member@corp.example", oidc.RoleMember), http.StatusNotFound, "approval not found"},
	} {
		for _, verb := range []string{"approve", "deny"} {
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+ap.ID.String()+"/"+verb, tc.sess, "")
			if w.Code != tc.want {
				t.Errorf("%s %s: code = %d, want %d; body=%s", tc.name, verb, w.Code, tc.want, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantB) {
				t.Errorf("%s %s: body = %s, want %q", tc.name, verb, w.Body.String(), tc.wantB)
			}
		}
	}
	// …and the foreign member's refusal is BYTE-IDENTICAL to the one a UUID that
	// does not exist at all gets, so the two cannot be told apart.
	foreignSess := func() *http.Cookie { return ssoSession(t, "member-sub", "member@corp.example", oidc.RoleMember) }
	fresh := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+uuid.New().String()+"/approve", foreignSess(), "")
	foreign := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+ap.ID.String()+"/approve", foreignSess(), "")
	if fresh.Code != foreign.Code || fresh.Body.String() != foreign.Body.String() {
		t.Errorf("a foreign member can tell a real credential_reauth row (%d %s) from a nonexistent one (%d %s)",
			foreign.Code, foreign.Body.String(), fresh.Code, fresh.Body.String())
	}

	got, _ := f.srv.cfg.Approvals.Get(context.Background(), ap.ID)
	if got.State != types.ApprovalPending {
		t.Fatalf("a refused decision still moved the row to %s", got.State)
	}
}

func onlyReauthRow(t *testing.T, srv *Server) types.ApprovalRequest {
	t.Helper()
	rows, err := srv.cfg.Approvals.List(context.Background(), types.ApprovalPending)
	if err != nil {
		t.Fatal(err)
	}
	for _, ap := range rows {
		if ap.Kind == types.ApprovalCredentialReauth {
			return ap
		}
	}
	t.Fatal("no credential_reauth row")
	return types.ApprovalRequest{}
}

func onlyPendingReauthRow(t *testing.T, srv *Server) types.ApprovalRequest {
	t.Helper()
	rows, err := srv.cfg.Approvals.List(context.Background(), types.ApprovalPending)
	if err != nil {
		t.Fatal(err)
	}
	for _, ap := range rows {
		if ap.Kind == types.ApprovalCredentialReauth && ap.State == types.ApprovalPending {
			return ap
		}
	}
	t.Fatal("no PENDING credential_reauth row")
	return types.ApprovalRequest{}
}

// THE RAISE REASON IS THE SPENT ONE when the session's refresh token is gone —
// and this case exists because a MERGE broke it silently.
//
// The run-credential-door lane turned awsSSORefreshSpentSentence into a FORMAT
// string whose %s is the audience's own remedy, and refreshAWSSSOBlob returns
// the composed awsSSORefreshSpentRefusal. Compared against the bare constant
// the classification arm is always false, it still compiles, and every spent
// session is audited `unavailable` — a reason that names the wrong fix ("the
// renewal failed, try again" rather than "this session is gone, sign in").
//
// So the assertion is on the AUDIT ROW's reason, which is the thing an operator
// reads, and not on the comparison.
func TestResolveAWSSSOInjection_SpentSessionIsAuditedSpent(t *testing.T) {
	f := newReauthFixture(t, nil)
	// A blob with a refresh token, whose redemption AWS refuses: the exact
	// shape awsSSOErrorIsSpent classifies as a retired session.
	spent := liveSSOBlob()
	spent.RefreshToken = "refresh-token-aws-has-retired"
	spent.ExpiresAt = time.Now().Add(-time.Hour).UTC()
	spent.ClientID, spent.ClientSecret = "cid", "csec"
	spent.RegistrationExpiresAt = time.Now().Add(30 * 24 * time.Hour).UTC()
	f.putBlob(t, "alice@example.com", spent)

	oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"retired"}`))
	}))
	defer oidc.Close()
	f.srv.cfg.AWSSSOEndpointOverride = oidc.URL
	// The host pin follows the override, so the grant must name that host too.
	f.st.grants[0].Spec.Scope = reauthSnapshotScope(t, "alice@example.com", reauthRegion, true, "111122223333", "WardynAgent")
	f.brk.setHost(ssoPortalHost(reauthRegion, oidc.URL))

	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve: code = %d, want 423; body=%s", w.Code, w.Body.String())
	}
	for _, ev := range f.audit.events() {
		if ev.Action != "credential.reauth.requested" {
			continue
		}
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) != nil {
			t.Fatalf("audit data is not JSON: %s", ev.Data)
		}
		if d["reason"] != awsSSOReauthReasonSpent {
			t.Fatalf("the raise reason is %q, want %q — a retired session and a failed renewal have different fixes, "+
				"and only the first is answered by signing in", d["reason"], awsSSOReauthReasonSpent)
		}
		return
	}
	t.Fatal("no credential.reauth.requested row")
}

// ─── legacy open mode: no roster (W6-S F1) ───────────────────────────────────

// THE TWO HALVES, JOINED. Dispatch and resolve each had thorough tests and they
// disagreed about the same deployment, because no test ever ran both: every
// resolver case seeds a roster row (reauthRosterRow) and every no-roster case
// stops at dispatch.
//
// The shape is an upgraded 0.7.5 install that never wrote a roster — legacy open
// mode, which CHANGELOG and docs/MEMBERS.md both name as supported, and which
// awsSSOScopeFor answers with the operator namespace. Dispatch authors Phase B
// for it (TestDispatchWiring_SwitchOnAuthorsThePhaseBLane dispatches with
// SiteConfig{} and asserts the grant, the MITM entry and the CA). Then every
// resolve answered 403 scope_changed, because driftFrom read a MISSING roster as
// a WITHDRAWN row — including the proxy sidecar's BOOT mint, which fails closed.
// So the run could not launch at all with the switch at its default, and the
// refusal said "the roster changed" about a deployment where nothing had ever
// been there to change.
func TestResolveAWSSSOInjection_LegacyNoRosterResolvesOnTheSharedLane(t *testing.T) {
	for _, tc := range []struct {
		name string
		site types.SiteConfig
	}{
		{"no roster at all — legacy open mode", types.SiteConfig{}},
		{"a shared roster row — the control", reauthRosterRow(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReauthFixture(t, func(f *reauthFixture) {
				f.st.site = tc.site
				// The SHARED snapshot dispatch authors for this shape: owner "",
				// credential_source "shared" (awsSSOScope{} → the operator namespace).
				f.st.grants[0].Spec.Scope = reauthSnapshotScope(t, "", reauthRegion, false, "111122223333", "WardynAgent")
			})
			// The OPERATOR namespace holds the session, which is what the shared
			// lane reads.
			f.putBlob(t, "", liveSSOBlob())

			w := f.resolve(t)
			if w.Code != http.StatusOK {
				t.Fatalf("resolve = %d, want 200 — a live operator session on the shared lane. "+
					"body=%s", w.Code, w.Body.String())
			}
			if f.audit.hasReason("secret.read", "scope_changed") {
				t.Error("audited as scope_changed: a deployment with no roster has nothing to have drifted from")
			}
			var got types.ResolvedInjection
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not a ResolvedInjection: %v", err)
			}
			if got.Value != reauthToken {
				t.Errorf("resolved value = %q, want the stored session token", got.Value)
			}
		})
	}
}

// …and legacy open mode does NOT disarm the checks that do not need a roster.
// The owner/credential-source arms are the substitution hole itself, and they
// still run: a snapshot claiming the PER-USER lane cannot resolve against a
// deployment whose scope is shared, roster or no roster.
func TestResolveAWSSSOInjection_LegacyNoRosterStillRefusesAScopeMismatch(t *testing.T) {
	f := newReauthFixture(t, func(f *reauthFixture) {
		f.st.site = types.SiteConfig{}
		f.st.grants[0].Spec.Scope = reauthSnapshotScope(t, "alice@example.com", reauthRegion, true, "111122223333", "WardynAgent")
	})
	f.putBlob(t, "", liveSSOBlob())
	f.putBlob(t, "alice@example.com", liveSSOBlob())

	if w := f.resolve(t); w.Code != http.StatusForbidden {
		t.Fatalf("a per_user snapshot on a no-roster (shared) deployment resolved %d, want 403 — "+
			"skipping the ROSTER arms must not skip the scope ones. body=%s", w.Code, w.Body.String())
	}
}

// dedupApprovals is the LOSER'S view of a concurrent raise: Request answers with
// an already-existing row whose id is not the one this caller minted. That is
// exactly what approval.RequestApproval does — for the pre-insert scan and for
// the partial unique index's loser alike — and it does it SILENTLY, which is the
// whole difficulty: without the minted id there is nothing in the answer to tell
// "I raised this" from "somebody else did".
type dedupApprovals struct {
	ApprovalService
	winner types.ApprovalRequest
	calls  int
}

func (d *dedupApprovals) Request(_ context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	d.calls++
	w := d.winner
	w.RunID, w.Kind, w.RequestedScope = req.RunID, req.Kind, req.RequestedScope
	return w, nil
}

func (d *dedupApprovals) Get(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	if id == d.winner.ID {
		return d.winner, nil
	}
	return types.ApprovalRequest{}, errStoreNotFound
}

// THE LOSER AUDITS NOTHING AND COUNTS NOTHING (W6-S F4).
//
// It still gets its 423 naming the winner's approval id — the row is real, it is
// PENDING, and the sidecar's hold joins the same workflow by that id — but
// `credential.reauth.requested` and outcome=requested belong to whoever raised
// it. With N resolvers for one lapse the alternative is N rows on a hash-chained
// log all naming one approval, and a counter that no longer means "requests
// raised".
//
// The concurrency test above cannot see this: its later resolvers meet the
// PENDING row at the check ABOVE the raise and never reach it. This drives the
// raise with a service that answers the way the real dedup answers.
func TestResolveAWSSSOInjection_ARaiseThatLostTheRaceAuditsNothing(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())

	winner := types.ApprovalRequest{
		ID: uuid.New(), RunID: f.runID, Kind: types.ApprovalCredentialReauth,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	dedup := &dedupApprovals{ApprovalService: f.srv.cfg.Approvals, winner: winner}
	f.srv.cfg.Approvals = dedup

	before := reauthCount(t, f.srv, "requested")
	w := f.resolve(t)
	if w.Code != http.StatusLocked {
		t.Fatalf("resolve = %d, want 423 — the loser still waits on the winner's row. body=%s", w.Code, w.Body.String())
	}
	if dedup.calls != 1 {
		t.Fatalf("Request called %d times, want 1", dedup.calls)
	}
	var body reauthPendingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.ApprovalID != winner.ID {
		t.Errorf("the 423 names %s, want the WINNER's row %s — the hold polls whatever this says",
			body.ApprovalID, winner.ID)
	}
	for _, ev := range f.audit.events() {
		if ev.Action == "credential.reauth.requested" {
			t.Errorf("the loser audited credential.reauth.requested for %s, a request it did not raise", ev.Target)
		}
	}
	if after := reauthCount(t, f.srv, "requested"); after != before {
		t.Errorf("outcome=requested moved %s -> %s for a request this caller did not raise", before, after)
	}
}

// #656, following #204's ADO-lane precedent: a raise that cannot even ask (the
// approval store errors) now carries reason "raise_failed" on the wire, not
// just the human sentence — the AWS SSO hold shares the ADO sign-in hold's
// refusal vocabulary because it is the same shape (an approval-backed hold
// whose own raise can fail).
func TestResolveAWSSSOInjection_RaiseFailureCarriesReasonOnWire(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	f.st.approvals.requestErr = errors.New("approvals store unavailable")

	w := f.resolve(t)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("resolve: code = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason != reasonRaiseFailed {
		t.Errorf("wire reason = %q, want %q", body.Reason, reasonRaiseFailed)
	}
}

// #656: the re-auth hold's approvals_unreadable refusal answers outside fail(),
// so it gets its own wire assertion like its three siblings: a dead session
// whose run approvals cannot be listed is a 503 carrying that reason.
func TestResolveAWSSSOInjection_ApprovalsUnreadableCarriesReasonOnWire(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	f.st.approvals.listErr = errors.New("approvals store unavailable")

	w := f.resolve(t)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("resolve: code = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason != "approvals_unreadable" {
		t.Errorf("wire reason = %q, want %q", body.Reason, "approvals_unreadable")
	}
}
