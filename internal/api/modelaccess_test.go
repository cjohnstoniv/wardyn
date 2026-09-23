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
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ── the per_user credential scope ────────────────────────────────────────────

// scopedSecrets is memSecrets plus a READ LOG KEYED BY OWNER, which is the only
// way to assert the property that matters here: a per_user resolve must make NO
// operator-namespace read at all. Asserting "the member did not get the admin's
// token" is weaker — it passes for an implementation that reads the operator's
// row and then discards it, which still leaks through every other lane.
//
// Its Get honours the REAL fallback contract (own row, then the operator's), so
// a test that passes here is not passing because the double is stricter than
// internal/secretstore/pg.
type scopedSecrets struct {
	owner string
	mu    *sync.Mutex
	reads *[]string // "<owner>|<name>", "" owner rendered as "(operator)"
	rows  map[string]map[string][]byte
}

func newScopedSecrets() *scopedSecrets {
	var reads []string
	return &scopedSecrets{mu: &sync.Mutex{}, reads: &reads, rows: map[string]map[string][]byte{}}
}

func (s *scopedSecrets) note(name string) {
	who := s.owner
	if who == "" {
		who = "(operator)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.reads = append(*s.reads, who+"|"+name)
}

// operatorReads returns every logged read made in the operator namespace.
func (s *scopedSecrets) operatorReads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range *s.reads {
		if strings.HasPrefix(r, "(operator)|") {
			out = append(out, r)
		}
	}
	return out
}

func (s *scopedSecrets) Name() string { return "scoped" }

func (s *scopedSecrets) Put(_ context.Context, name string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows[s.owner] == nil {
		s.rows[s.owner] = map[string][]byte{}
	}
	s.rows[s.owner][name] = v
	return nil
}

func (s *scopedSecrets) Get(_ context.Context, name string) ([]byte, error) {
	s.note(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.rows[s.owner][name]; ok {
		return v, nil
	}
	// The documented owner-view fallback to the operator's row.
	if v, ok := s.rows[""][name]; ok {
		return v, nil
	}
	return nil, secretstore.ErrNotFound
}

func (s *scopedSecrets) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows[s.owner], name)
	return nil
}

func (s *scopedSecrets) List(context.Context) ([]string, error) {
	s.note("*")
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for k := range s.rows[s.owner] {
		out = append(out, k)
	}
	return out, nil
}

func (s *scopedSecrets) For(owner string) secretstore.Store {
	return &scopedSecrets{owner: owner, mu: s.mu, reads: s.reads, rows: s.rows}
}

// perUserBedrockSrv is a Bedrock deployment with EVERY operator lane loaded —
// a bearer key, static keys, and the operator's own captured SSO session. Under
// `shared` any one of them carries a run; under `per_user` none of them may.
func perUserBedrockSrv(t *testing.T) (*Server, *scopedSecrets) {
	t.Helper()
	sec := newScopedSecrets()
	s := New(Config{
		BedrockRegion: "us-east-1",
		BedrockModel:  "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets:       sec,
		MaskRegistry:  secretmask.NewRegistry(),
		Now:           func() time.Time { return awsSSOTestFixedNow },
	})
	op := sec.For("").(*scopedSecrets)
	for name, v := range map[string]string{
		bedrockAPIKeySecret:          "operator-bedrock-bearer",
		bedrockAccessKeyIDSecret:     "AKIAOPERATOR",
		bedrockSecretAccessKeySecret: "operator-secret-key",
	} {
		if err := op.Put(context.Background(), name, []byte(v)); err != nil {
			t.Fatalf("seed operator secret %s: %v", name, err)
		}
	}
	putScopedSSOBlob(t, sec, "", awsSSOTestFixedNow.Add(time.Hour), "operator-access-token")
	return s, sec
}

// putScopedSSOBlob writes a structurally valid captured session into owner's
// namespace ("" = the operator's).
func putScopedSSOBlob(t *testing.T, sec *scopedSecrets, owner string, expiresAt time.Time, accessToken string) {
	t.Helper()
	raw, err := json.Marshal(awsSSOBlob{
		AccessToken:  accessToken,
		RefreshToken: "refresh-" + accessToken,
		ClientID:     "client-id",
		ClientSecret: "client-secret-" + accessToken,
		StartURL:     "https://example.awsapps.com/start",
		Region:       "us-east-1",
		AccountID:    "123456789012",
		RoleName:     "WardynBedrockRole",
		ExpiresAt:    expiresAt,
		CapturedAt:   awsSSOTestFixedNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	if perr := sec.For(owner).Put(context.Background(), harnessCredSecretName(awsSSOProvider), raw); perr != nil {
		t.Fatalf("put blob for %q: %v", owner, perr)
	}
}

// TestResolveBedrockAuth_PerUserNeverReadsTheOperatorRow is THE pin of this
// lane. A deployment that declared one credential per person has an operator
// bearer key, operator static keys AND the admin's own captured session sitting
// right there; a member with no session of their own must get NONE of them, and
// must not even have caused a read of them.
//
// Reading-and-discarding would not be good enough. Store.For(owner).Get falls
// back to the operator's row by contract, so any lane that reads by name
// resolves the admin's material for a member; the only safe shape is not
// reaching for it at all, which is what the read log proves.
func TestResolveBedrockAuth_PerUserNeverReadsTheOperatorRow(t *testing.T) {
	s, sec := perUserBedrockSrv(t)
	member := awsSSOScope{perUser: true, owner: "member-sub"}

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, false, nil, member)
	if ba.ready {
		t.Fatalf("a member with no captured session resolved READY (bearer=%v mount=%v ssoInject=%v) — "+
			"per_user was served an operator credential", ba.bearer, ba.awsMount, ba.ssoInject)
	}
	if got := sec.operatorReads(); len(got) != 0 {
		t.Errorf("per_user resolve made %d operator-namespace read(s): %v — the lanes must be skipped, not read and discarded", len(got), got)
	}

	// Under `shared` the SAME deployment resolves, so the refusal above is the
	// declaration doing its job rather than the fixture being broken.
	if shared := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, false, nil, awsSSOScope{}); !shared.ready {
		t.Error("the same deployment must still resolve under shared — otherwise this test proves nothing about per_user")
	}
}

// TestResolveBedrockAuth_PerUserServesTheOwnersOwnSession: with a session of
// their own, the member's run goes through — on THEIR token, never the
// operator's, and still without touching an operator lane.
func TestResolveBedrockAuth_PerUserServesTheOwnersOwnSession(t *testing.T) {
	s, sec := perUserBedrockSrv(t)
	putScopedSSOBlob(t, sec, "member-sub", awsSSOTestFixedNow.Add(time.Hour), "member-access-token")

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, false, nil,
		awsSSOScope{perUser: true, owner: "member-sub"})
	if !ba.ready || !ba.ssoInject {
		t.Fatalf("the owner's own session must carry the run: ready=%v ssoInject=%v", ba.ready, ba.ssoInject)
	}
	files := decodeSSOFiles(t, ba.env[awsSSOConfigEnvVar])
	cache := files[".aws/sso/cache/"+awsSSOCacheFileName(awsSSOProfileName)+".json"]
	if !strings.Contains(cache, "member-access-token") {
		t.Errorf("the sandbox cache does not carry the member's own token: %s", cache)
	}
	if strings.Contains(cache, "operator-access-token") {
		t.Error("the sandbox cache carries the OPERATOR's token — the owner-view fallback was not closed")
	}
	if got := sec.operatorReads(); len(got) != 0 {
		t.Errorf("per_user resolve made %d operator-namespace read(s): %v", len(got), got)
	}
	// The bearer key would have WON the precedence chain under shared; it must
	// not have been selected here.
	if ba.bearer {
		t.Error("the operator's bearer key won a per_user resolve")
	}
}

// TestAWSSSOScopeFor_OnlyAnEnabledPerUserRowNamespaces: every other roster
// shape resolves to the operator namespace, which is what keeps an upgraded
// install byte-for-byte what it was.
func TestAWSSSOScopeFor_OnlyAnEnabledPerUserRowNamespaces(t *testing.T) {
	perUser := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
	}
	disabled := perUser
	disabled.Disabled = true
	cases := map[string]struct {
		sc      types.SiteConfig
		want    awsSSOScope
		wantErr string
	}{
		"no roster at all": {sc: types.SiteConfig{}},
		"shared row": {sc: agentRoster(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		})},
		"a DISABLED per_user row": {sc: agentRoster(disabled)},
		"another agent's per_user row": {sc: agentRoster(types.AgentProvider{
			ID: "codex-cli", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser,
		})},
		"an enabled per_user row": {sc: agentRoster(perUser), want: awsSSOScope{perUser: true, owner: "sub-1"}},
	}
	for name, c := range cases {
		if got := awsSSOScopeFor(c.sc, "claude-code", "sub-1"); got != c.want {
			t.Errorf("%s: scope = %+v, want %+v", name, got, c.want)
		}
	}
	// A per_user row with no subject to scope to is a credential nobody owns: it
	// must read as not-configured, never fall back to the operator's.
	empty := awsSSOScopeFor(agentRoster(perUser), "claude-code", "")
	if empty.namespaced() {
		t.Error("an empty subject must not name a usable namespace")
	}
	s, sec := perUserBedrockSrv(t)
	if _, found, err := s.readAWSSSOBlob(context.Background(), empty); found || err != nil {
		t.Errorf("read under an ownerless per_user scope = (found %v, err %v), want (false, nil)", found, err)
	}
	if got := sec.operatorReads(); len(got) != 0 {
		t.Errorf("an ownerless per_user read touched the operator namespace: %v", got)
	}
}

// TestResolveRunLLMAccess_AdminsOwnPerUserCaptureResolvesAtCreate is the
// adjacent-helper trap, pinned. secretOwnerFromRequest answers "" for EVERY
// operator, so using it as the subject would scope an admin's own per_user
// capture to the operator namespace at create and 422 them ("sign in again")
// for a credential dispatch — which reads run.CreatedBy — resolves fine.
func TestResolveRunLLMAccess_AdminsOwnPerUserCaptureResolvesAtCreate(t *testing.T) {
	sec := newScopedSecrets()
	const admin = "admin@corp.example"
	putScopedSSOBlob(t, sec, admin, awsSSOTestFixedNow.Add(time.Hour), "admin-access-token")
	h := newHarness(t)
	st := &integStore{
		govEscapeStore: newGovEscapeStore(&capStore{}),
		site: agentRoster(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
		}),
	}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion, cfg.BedrockModel = "us-east-1", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	s := New(cfg)

	req := createRunRequest{Agent: "claude-code", Task: "ship it"}
	la := s.resolveRunLLMAccess(context.Background(), req, types.RunPolicySpec{}, map[string]bool{}, nil, admin)
	if la == nil || !la.Provisioned {
		t.Fatalf("the admin's OWN per_user capture must resolve at create, got %+v", la)
	}
	// …and the same call for somebody with no capture must not borrow it.
	if other := s.resolveRunLLMAccess(context.Background(), req, types.RunPolicySpec{}, map[string]bool{}, nil, "member@corp.example"); other != nil && other.Provisioned {
		t.Errorf("a member with no capture resolved provisioned: %+v", other)
	}
}

// ── the five lifecycle states ────────────────────────────────────────────────

// TestAWSSSOCredentialState_TheFiveStates walks the vocabulary, REGISTRATION
// first. The ordering is the point: the access token lives an hour on the
// reporting estate, so a probe keyed on it would make `expiring` permanent and
// `live` unreachable.
func TestAWSSSOCredentialState_TheFiveStates(t *testing.T) {
	now := awsSSOTestFixedNow
	live := func(mut func(*awsSSOBlob)) awsSSOBlob {
		b := awsSSOBlob{
			AccessToken: "a", RefreshToken: "r", StartURL: "https://x.awsapps.com/start",
			Region: "us-east-1", AccountID: "1", RoleName: "R",
			ExpiresAt:             now.Add(time.Hour),
			RegistrationExpiresAt: now.Add(90 * 24 * time.Hour),
		}
		if mut != nil {
			mut(&b)
		}
		return b
	}
	cases := []struct {
		name    string
		blob    awsSSOBlob
		found   bool
		perUser bool
		spent   bool // every row here is !spent — see TestAWSSSOCredentialState_SpentBoundary
		want    string
	}{
		{"renewable with a long registration", live(nil), true, true, false, modelAccessLive},
		{"expired ACCESS token but renewable folds into live",
			live(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(-time.Minute) }), true, true, false, modelAccessLive},
		{"a zero registration timestamp is live (the helper saw none)",
			live(func(b *awsSSOBlob) { b.RegistrationExpiresAt = time.Time{} }), true, true, false, modelAccessLive},
		{"the registration lapses within a day",
			live(func(b *awsSSOBlob) { b.RegistrationExpiresAt = now.Add(6 * time.Hour) }), true, true, false, modelAccessExpiring},
		{"no refresh token, access token within a day",
			live(func(b *awsSSOBlob) { b.RefreshToken = ""; b.ExpiresAt = now.Add(2 * time.Hour) }), true, true, false, modelAccessExpiring},
		{"no refresh token, access token expired",
			live(func(b *awsSSOBlob) { b.RefreshToken = ""; b.ExpiresAt = now.Add(-time.Minute) }), true, true, false, modelAccessExpiredSignin},
		{"a lapsed registration cannot be renewed",
			live(func(b *awsSSOBlob) {
				b.RegistrationExpiresAt = now.Add(-time.Hour)
				b.ExpiresAt = now.Add(-time.Minute)
			}), true, true, false, modelAccessExpiredSignin},
		{"per_user with nothing captured", awsSSOBlob{}, false, true, false, modelAccessNotConfigured},
		{"shared with nothing captured", awsSSOBlob{}, false, false, false, modelAccessSharedExpired},
		{"shared and dead is the ADMIN's problem, not the member's",
			live(func(b *awsSSOBlob) { b.RefreshToken = ""; b.ExpiresAt = now.Add(-time.Minute) }), true, false, false, modelAccessSharedExpired},
	}
	for _, c := range cases {
		if got := awsSSOCredentialState(c.blob, c.found, c.perUser, c.spent, now); got != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}

	// Every state that asks for something must SAY what — an unactionable
	// warning chip is the thing this replaced.
	for _, st := range []string{modelAccessExpiring, modelAccessExpiredSignin, modelAccessNotConfigured, modelAccessSharedExpired} {
		if modelAccessAction(st, "2026-01-01T00:00:00Z") == "" {
			t.Errorf("state %q carries no action line", st)
		}
	}
	if modelAccessAction(modelAccessLive, "") != "" {
		t.Error("live must ask for nothing — that is what folding expired_renewable into it is for")
	}
	if modelAccessAction(modelAccessNotApplicable, "") != "" {
		t.Error("not_applicable must ask for nothing — the caller is a mechanism, not a person")
	}
	// The deadline the expiring line names is the REGISTRATION's while the blob
	// can be renewed: the access token's own expiry is not what runs out.
	reg := live(func(b *awsSSOBlob) { b.RegistrationExpiresAt = now.Add(6 * time.Hour) })
	if got := modelAccessDeadline(reg, true, false, now); got != reg.RegistrationExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("deadline = %q, want the registration's lapse", got)
	}
	// The third Cause: no working refresh token, the access token itself is
	// what is running out.
	noRefresh := live(func(b *awsSSOBlob) { b.RefreshToken = ""; b.ExpiresAt = now.Add(2 * time.Hour) })
	if got := awsSSOCredentialCause(noRefresh, false, now); got != causeTokenExpiring {
		t.Errorf("cause = %q, want %q", got, causeTokenExpiring)
	}
}

// TestAWSSSOCredentialState_SpentBoundary is Finding 5's regression: a SPENT
// refresh token must grade DEAD once the access token is inside the refresh
// skew (dispatch would already refuse this run), and `expiring` — never
// `live` — while it is still comfortably outside it. Before this a spent
// token graded `live` until the CLIENT REGISTRATION lapsed, days later, while
// every dispatch refused the person's runs. Round-1 general S4 / Codex #6:
// grading `expiring` INSIDE the skew would promise a launch dispatch does not
// honour, so the boundary is needsRefresh(now), not the registration.
func TestAWSSSOCredentialState_SpentBoundary(t *testing.T) {
	now := awsSSOTestFixedNow
	base := func(mut func(*awsSSOBlob)) awsSSOBlob {
		b := awsSSOBlob{
			AccessToken: "a", RefreshToken: "r", StartURL: "https://x.awsapps.com/start",
			Region: "us-east-1", AccountID: "1", RoleName: "R",
			ExpiresAt:             now.Add(time.Hour),
			RegistrationExpiresAt: now.Add(90 * 24 * time.Hour),
		}
		if mut != nil {
			mut(&b)
		}
		return b
	}
	cases := []struct {
		name    string
		blob    awsSSOBlob
		perUser bool
		want    string
	}{
		// The boundary table: just before / exactly at / just after ExpiresAt-skew.
		{"just before the skew boundary: still outside it, expiring",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(awsSSORefreshSkew + time.Second) }), true, modelAccessExpiring},
		{"exactly at the skew boundary: needsRefresh fires, dead",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(awsSSORefreshSkew) }), true, modelAccessExpiredSignin},
		{"just after (already inside the skew): dead",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(awsSSORefreshSkew - time.Second) }), true, modelAccessExpiredSignin},
		// Nominal expiry: the access token has already lapsed outright.
		{"nominal expiry: already lapsed, dead",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(-time.Minute) }), true, modelAccessExpiredSignin},
		// Per-user vs shared-member projection of the dead state.
		{"spent + inside the skew, shared: shared_expired, not expired_signin",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(time.Minute) }), false, modelAccessSharedExpired},
	}
	for _, c := range cases {
		if got := awsSSOCredentialState(c.blob, true, c.perUser, true, now); got != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}

	// spent SKIPS the renewable arm entirely: a lapsed registration changes
	// nothing for a spent credential — only needsRefresh decides — which a
	// !spent blob with the SAME shape would grade differently (renewable's own
	// registrationLapsed check never fires because renewable() is false, so it
	// falls through to the plain-expiry arms and reads live, since the access
	// token has 48h of its own headroom left).
	lapsedRegBlob := base(func(b *awsSSOBlob) {
		b.ExpiresAt = now.Add(48 * time.Hour)
		b.RegistrationExpiresAt = now.Add(-time.Hour)
	})
	if got := awsSSOCredentialState(lapsedRegBlob, true, true, true, now); got != modelAccessExpiring {
		t.Errorf("spent + lapsed registration + 48h access-token headroom = %q, want expiring (needsRefresh alone decides)", got)
	}
	if got := awsSSOCredentialState(lapsedRegBlob, true, true, false, now); got != modelAccessLive {
		t.Errorf("sanity: the SAME blob !spent should read live (unchanged behaviour), got %q", got)
	}

	// The transient-refresh-with-a-usable-token path is UNCHANGED: !spent with a
	// token near expiry but still renewable and far from its registration lapse
	// folds into `live` exactly as before — dispatch renews it, so a near
	// expiry here is not yet the person's problem. This is the same fixture the
	// spent boundary cases above grade `expiring`/`dead`, which is the whole
	// point of the boundary: `spent` is what turns "renewable" off.
	transient := base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(time.Minute) })
	if got := awsSSOCredentialState(transient, true, true, false, now); got != modelAccessLive {
		t.Errorf("transient (!spent) near-expiry state = %q, want live (renewable, registration far out — unchanged)", got)
	}

	// modelAccessDeadline: the spent arm returns ExpiresAt-skew explicitly,
	// never the registration's lapse a spent credential can no longer redeem.
	spentBlob := base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(20 * time.Minute) })
	wantDeadline := spentBlob.ExpiresAt.Add(-awsSSORefreshSkew).UTC().Format(time.RFC3339)
	if got := modelAccessDeadline(spentBlob, true, true, now); got != wantDeadline {
		t.Errorf("spent deadline = %q, want ExpiresAt-skew %q", got, wantDeadline)
	}
	if got := modelAccessDeadline(spentBlob, true, false, now); got != spentBlob.RegistrationExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("non-spent deadline changed: got %q, want the registration's own lapse (unaffected by this lane)", got)
	}
}

// TestSetupModelAccess_SpentGradesExpiringWithCause is the SetupModelAccess
// assembly for Finding 5: a spent credential still inside the skew grades
// `expiring` with the ExpiresAt-skew deadline AND Cause == "renewal_spent", so
// the admin checklist row (awsSSOCredentialRow) can say WHICH fact this is —
// AWS retiring the grant, not a registration lapsing on its own schedule.
func TestSetupModelAccess_SpentGradesExpiringWithCause(t *testing.T) {
	now := awsSSOTestFixedNow
	blob := awsSSOBlob{
		AccessToken: "a", RefreshToken: "r", StartURL: "https://x.awsapps.com/start",
		Region: "us-east-1", AccountID: "1", RoleName: "R",
		ExpiresAt:             now.Add(20 * time.Minute),
		RegistrationExpiresAt: now.Add(90 * 24 * time.Hour),
	}
	scope := awsSSOScope{perUser: true, owner: "m"}
	got := setupModelAccess(agentRoster(mechanismRow()), blob, true, true, scope, true, now)
	if got.State != modelAccessExpiring {
		t.Fatalf("state = %q, want expiring", got.State)
	}
	wantDeadline := blob.ExpiresAt.Add(-awsSSORefreshSkew).UTC().Format(time.RFC3339)
	if got.Deadline != wantDeadline {
		t.Errorf("deadline = %q, want ExpiresAt-skew %q", got.Deadline, wantDeadline)
	}
	if got.Cause != causeRenewalSpent {
		t.Errorf("cause = %q, want %q", got.Cause, causeRenewalSpent)
	}
	if got.Action != fmt.Sprintf(modelAccessExpiringAction, wantDeadline) {
		t.Errorf("action = %q, does not name the graded deadline", got.Action)
	}

	// The admin checklist row must use the spent-specific sentence, not the
	// registration one — it would say something false about why this credential
	// stops working.
	h := SetupHarness{Provider: awsSSOProvider, Captured: true, ExpiresAt: blob.ExpiresAt.Format(time.RFC3339)}
	row, shown := harnessCredentialCheck(h, got)
	if !shown {
		t.Fatal("the AWS row must be shown for a captured session")
	}
	if want := fmt.Sprintf(harnessCredentialAWSRenewalSpentDetail, wantDeadline); row.Detail != want {
		t.Errorf("admin row detail = %q, want the renewal-spent sentence through the constant: %q", row.Detail, want)
	}
	if !strings.Contains(row.Detail, wantDeadline) {
		t.Errorf("admin row detail = %q, does not name the graded deadline", row.Detail)
	}
}

// TestSetupModelAccess_SilentWithNothingToSay: an install that never touched
// this feature ships no ModelAccess at all, so the console keeps rendering the
// chip it always did rather than claiming a state nobody configured.
func TestSetupModelAccess_SilentWithNothingToSay(t *testing.T) {
	now := awsSSOTestFixedNow
	if got := setupModelAccess(types.SiteConfig{}, awsSSOBlob{}, false, false, awsSSOScope{}, true, now); got.State != "" {
		t.Errorf("legacy open mode with no capture must say nothing, got %+v", got)
	}
	// A declared bedrock_sso lane speaks even with nothing captured — that IS
	// the state worth reporting.
	row := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
	}
	got := setupModelAccess(agentRoster(row), awsSSOBlob{}, false, false, awsSSOScope{perUser: true, owner: "m"}, true, now)
	if got.State != modelAccessNotConfigured || got.Mechanism != string(types.AgentMechanismBedrockSSO) || got.Action == "" {
		t.Errorf("a declared per_user lane with no capture = %+v, want not_configured with a mechanism and an action", got)
	}
}

// mechanismRow is the per_user claude-code/bedrock_sso roster row every
// setupModelAccess mechanism test below grades against.
func mechanismRow() types.AgentProvider {
	return types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
	}
}

// TestSetupModelAccess_AdminTokenUnderPerUserIsNotApplicable is finding 5,
// narrowed by S-01/S-02: an OIDC-configured deployment (a real console
// sign-in / wdn_ token exists as the remedy) reading the admin bearer token's
// state with NOTHING captured must report the TOKEN's own state
// (not_applicable), never "not_configured" + a "Sign in to AWS" action the
// caller cannot take.
func TestSetupModelAccess_AdminTokenUnderPerUserIsNotApplicable(t *testing.T) {
	scope := awsSSOScope{perUser: true, owner: adminTokenPrincipal}
	got := setupModelAccess(agentRoster(mechanismRow()), awsSSOBlob{}, false, false, scope, true, awsSSOTestFixedNow)
	if got.State != modelAccessNotApplicable {
		t.Fatalf("state = %q, want not_applicable", got.State)
	}
	if got.Action != "" || got.Deadline != "" {
		t.Errorf("the mechanism principal has no sign-in to complete, so Action/Deadline must be empty; got %+v", got)
	}
}

// TestSetupModelAccess_AdminTokenWithACapturedSessionGradesItNormally is S-02:
// "admin-token" is a non-empty owner, read and written like any other —
// readAWSSSOBlob only refuses an EMPTY one. A session already captured there
// is real and dispatch still serves it to admin-token-created runs, so it
// must be graded like any other blob, not hidden behind not_applicable.
func TestSetupModelAccess_AdminTokenWithACapturedSessionGradesItNormally(t *testing.T) {
	now := awsSSOTestFixedNow
	scope := awsSSOScope{perUser: true, owner: adminTokenPrincipal}
	blob := sharedExpiringBlob(now)
	blob.RegistrationExpiresAt = now.Add(30 * 24 * time.Hour) // far outside modelAccessExpiringWindow
	got := setupModelAccess(agentRoster(mechanismRow()), blob, true, false, scope, true, now)
	if got.State == modelAccessNotApplicable {
		t.Fatalf("a REAL captured session must be graded, not hidden behind not_applicable: %+v", got)
	}
	if got.State != modelAccessLive {
		t.Errorf("state = %q, want live (a renewable session with nothing near lapsing)", got.State)
	}
}

// TestSetupModelAccess_AdminTokenNoOIDCKeepsTodaysStates is S-01/S-02: with no
// OIDC configured, the admin token IS the only working per_user capture path
// (there is no console sign-in or wdn_ token to redirect to instead), so a
// no-OIDC deployment must keep grading it exactly like any other per_user
// principal — not_configured with a real sign-in action, never not_applicable.
func TestSetupModelAccess_AdminTokenNoOIDCKeepsTodaysStates(t *testing.T) {
	scope := awsSSOScope{perUser: true, owner: adminTokenPrincipal}
	got := setupModelAccess(agentRoster(mechanismRow()), awsSSOBlob{}, false, false, scope, false, awsSSOTestFixedNow)
	if got.State == modelAccessNotApplicable {
		t.Fatalf("no OIDC ⇒ the admin token is the only capture path here, got %+v", got)
	}
	if got.State != modelAccessNotConfigured || got.Action != modelAccessSignInAction {
		t.Errorf("state = %+v, want not_configured + %q", got, modelAccessSignInAction)
	}
}

// TestSetupModelAccess_LocalOperatorIsAPerson: LocalMode's own seat resolves
// to a real person-shaped namespace, never adminTokenPrincipal — it must keep
// reading not_configured (a real sign-in it can complete), not not_applicable.
func TestSetupModelAccess_LocalOperatorIsAPerson(t *testing.T) {
	scope := awsSSOScope{perUser: true, owner: "local:operator"}
	got := setupModelAccess(agentRoster(mechanismRow()), awsSSOBlob{}, false, false, scope, true, awsSSOTestFixedNow)
	if got.State == modelAccessNotApplicable {
		t.Fatalf("local mode's own seat is a real person, not the mechanism principal, got %+v", got)
	}
	if got.State != modelAccessNotConfigured || got.Action == "" {
		t.Errorf("state = %+v, want not_configured with a real sign-in action", got)
	}
}

// TestMemberModelAccess_NotApplicablePassesThrough: not_applicable must survive
// memberModelAccess unchanged. Fail-safe — the mechanism principal is never a
// real human member today — but without an explicit early return the default
// arm below rewrites any unrecognized state to `live`, which is a worse lie.
func TestMemberModelAccess_NotApplicablePassesThrough(t *testing.T) {
	in := SetupModelAccess{State: modelAccessNotApplicable, Mechanism: string(types.AgentMechanismBedrockSSO)}
	if got := memberModelAccess(in); got != in {
		t.Errorf("memberModelAccess(%+v) = %+v, want it passed through unchanged", in, got)
	}
}

// TestHandleHarnessLogin_AdminTokenUnderPerUserRefused: under a per_user row,
// with OIDC configured (a real console sign-in / wdn_ token exists as the
// remedy the refusal names), the shared admin bearer token must be refused
// (422) rather than admitted as an operator — every capture made with it
// would land in the SAME namespace (owner == "admin-token") and overwrite the
// last person's session. A shared row is unaffected: the admin token still
// may connect it. S-07: the refusal is an authz.denied row — the sibling
// refusals in authorizeHarnessLogin (denyMemberField/denyMemberCapability)
// both audit, and this is the one refusal on the credential-capture route an
// operator's own CI job hits with no error budget to notice it by otherwise.
func TestHandleHarnessLogin_AdminTokenUnderPerUserRefused(t *testing.T) {
	const path = "/api/v1/setup/harness-login"
	srv, audit := perUserLoginSrv(t) // default: claude-code/bedrock_sso/per_user row, OIDC configured
	w := do(t, srv, http.MethodPost, path, adminToken, `{"provider":"aws"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("admin-token under per_user (OIDC configured): code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "shared credential") {
		t.Errorf("body = %s, want the mechanism-principal refusal sentence", w.Body.String())
	}
	if len(audit.find("harness.login.started")) != 0 {
		t.Error("a refused sign-in launched a sandbox anyway")
	}
	rows := audit.find("authz.denied")
	if len(rows) != 1 {
		t.Fatalf("authz.denied rows = %d, want 1 — this refusal must audit like its siblings in the same function", len(rows))
	}
	var data struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode authz.denied data: %v", err)
	}
	if data.Reason != "harness_login_mechanism_principal" {
		t.Errorf("authz.denied reason = %q, want harness_login_mechanism_principal", data.Reason)
	}
	if rows[0].Target != "setup.harness_login" {
		t.Errorf("authz.denied target = %q, want setup.harness_login", rows[0].Target)
	}

	// Shared row: the admin token still may connect it.
	shared := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO}
	srv2, _ := perUserLoginSrv(t, shared)
	if w2 := do(t, srv2, http.MethodPost, path, adminToken, `{"provider":"anthropic"}`); w2.Code == http.StatusUnprocessableEntity {
		t.Fatalf("admin-token under a shared row must still be admitted, got 422: %s", w2.Body.String())
	}
}

// TestHandleHarnessLogin_NoOIDCAdminTokenStillAdmitted is S-01 (HIGH), red on
// 6a33a36e: with NO OIDC configured, neither remedy the refusal sentence
// names ("Sign in to the console" — there is no console session without
// OIDC; "use your own wdn_ API token" — handleCreateAPIToken refuses to mint
// one without a verified human) can exist, and the admin-token lane is the
// deployment's ONLY working per_user capture path. It must keep working
// exactly as it did before this lane's first commit — sshkeys.go's
// handleAddSSHKey guards its own identical admin-token refusal on
// `s.cfg.OIDC != nil` for this exact reason.
func TestHandleHarnessLogin_NoOIDCAdminTokenStillAdmitted(t *testing.T) {
	srv, _ := perUserLoginSrv(t)
	cfg := srv.cfg
	cfg.OIDC = nil
	noOIDC := New(cfg)
	w := do(t, noOIDC, http.MethodPost, "/api/v1/setup/harness-login", adminToken, `{"provider":"aws"}`)
	if w.Code == http.StatusUnprocessableEntity {
		t.Fatalf("no OIDC ⇒ the admin token is the ONLY capture path here — must not be refused as the mechanism, got 422: %s", w.Body.String())
	}
}

// TestHandleHarnessLogin_LocalModeStillMayCapture: LocalMode's own seat is a
// real person, not the mechanism principal — it must still pass under a
// per_user row exactly as it does today.
func TestHandleHarnessLogin_LocalModeStillMayCapture(t *testing.T) {
	srv, _ := perUserLoginSrv(t)
	cfg := srv.cfg
	cfg.LocalMode = true
	cfg.LocalOperator = "local:test"
	cfg.LocalLoopback = true
	local := New(cfg)
	w := do(t, local, http.MethodPost, "/api/v1/setup/harness-login", "", `{"provider":"aws"}`)
	if w.Code == http.StatusUnprocessableEntity {
		t.Fatalf("local mode is a person, not the mechanism principal — must not be refused as one, got 422: %s", w.Body.String())
	}
}

// TestHandleHarnessLogin_LocalDevHeaderDoesNotTripTheRefusal is S-05: the
// refusal predicate reads runIdentitySubject (S-01's guard), the SAME
// function every other per_user decision uses to pick a namespace — not bare
// principalFromRequest, which honours the DEV-ONLY X-Wardyn-Principal header
// in LocalMode. A client sending that header set to "admin-token" must NOT be
// refused by a predicate the namespace selector would never have reached.
func TestHandleHarnessLogin_LocalDevHeaderDoesNotTripTheRefusal(t *testing.T) {
	srv, _ := perUserLoginSrv(t)
	cfg := srv.cfg
	cfg.LocalMode = true
	cfg.LocalOperator = "local:test"
	cfg.LocalLoopback = true
	local := New(cfg)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/harness-login", strings.NewReader(`{"provider":"aws"}`))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Wardyn-Principal", adminTokenPrincipal)
	w := httptest.NewRecorder()
	panicFails(t, local.Handler()).ServeHTTP(w, req)
	if w.Code == http.StatusUnprocessableEntity {
		t.Fatalf("the dev header must not trip the refusal — runIdentitySubject ignores it in LocalMode, got 422: %s", w.Body.String())
	}
}

// TestRedactSetupStatusForMember_KeepsModelAccess: the member reduction drops
// the operator's diagnostic detail and KEEPS this — dropping it would leave a
// member reading llm_ready, the deployment fact that painted a green chip over
// their own lapsed session, which is the whole reason the field exists.
//
// PerUser, because `expired_signin` + "Sign in to AWS" is an answer only a
// principal who OWNS the credential may be given: under `shared` it is the
// operator's lifecycle and an instruction the server then refuses, and
// memberModelAccess collapses it. See modelaccess_member_redaction_test.go.
func TestRedactSetupStatusForMember_KeepsModelAccess(t *testing.T) {
	in := SetupStatus{
		ModelAccess: SetupModelAccess{
			State: modelAccessExpiredSignin, Mechanism: string(types.AgentMechanismBedrockSSO),
			Action: modelAccessSignInAction, PerUser: true,
		},
		Checks:  []SetupCheck{{ID: "runner", Detail: "operator detail"}},
		Secrets: SetupSecrets{Present: []string{"bedrock-api-key"}},
	}
	out := redactSetupStatusForMember(in, false, false)
	if out.ModelAccess != in.ModelAccess {
		t.Fatalf("ModelAccess = %+v, want it kept verbatim (%+v)", out.ModelAccess, in.ModelAccess)
	}
	// And it must stay redaction-SAFE: nothing in it may name a secret, a host
	// or the org's access portal.
	raw, err := json.Marshal(out.ModelAccess)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"bedrock-api-key", "awsapps.com", "amazonaws.com", "secret"} {
		if strings.Contains(strings.ToLower(string(raw)), leak) {
			t.Errorf("ModelAccess leaks %q: %s", leak, raw)
		}
	}
}

// ── the member's sign-in door ────────────────────────────────────────────────

const perUserPortal = "https://org-portal.awsapps.com/start"

// perUserLoginSrv is a deployment whose roster declares one AWS SSO sign-in per
// person, with a member session ready to drive it.
func perUserLoginSrv(t *testing.T, rows ...types.AgentProvider) (*Server, *memAudit) {
	t.Helper()
	return perUserLoginSrvUnder(t, &capStore{}, nil, rows...)
}

// perUserLoginSrvWithRunner is perUserLoginSrv with a runner the caller keeps a
// handle on, so a test can read the SandboxSpec the launch actually composed.
func perUserLoginSrvWithRunner(t *testing.T, runner runner.Runner, rows ...types.AgentProvider) (*Server, *memAudit) {
	t.Helper()
	return perUserLoginSrvUnder(t, &capStore{}, runner, rows...)
}

// perUserLoginSrvUnder is perUserLoginSrv with an explicit capability/governance
// store, for the arm that assigns a profile to the member driving the door.
func perUserLoginSrvUnder(t *testing.T, cs *capStore, rnr runner.Runner, rows ...types.AgentProvider) (*Server, *memAudit) {
	t.Helper()
	if rnr == nil {
		rnr = &fakeRunner{}
	}
	if len(rows) == 0 {
		rows = []types.AgentProvider{{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
		}}
	}
	h := newHarness(t)
	audit := &memAudit{}
	st := &integStore{govEscapeStore: newGovEscapeStore(cs), site: agentRoster(rows...)}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = rnr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), audit
}

func loginStartURL(t *testing.T, audit *memAudit) string {
	t.Helper()
	rows := audit.find("harness.login.started")
	if len(rows) != 1 {
		t.Fatalf("harness.login.started rows = %d, want 1", len(rows))
	}
	var data struct {
		SSOStartURL string `json:"sso_start_url"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode login audit data: %v", err)
	}
	return data.SSOStartURL
}

// TestHandleHarnessLogin_PerUserUsesTheRowsStartURL: the launch IGNORES the
// request's start URL under a per_user row and uses the admin's.
//
// This is a security property, not tidiness. The capture is bound to whatever
// portal the launch was seeded with (ssotoken.go's binding check compares the
// uploaded blob to THIS run's own audit record), so honouring a caller-supplied
// URL would let anyone bind their capture to an IdP and account of their
// choosing and have Wardyn bake it into every later Bedrock run's ~/.aws/config.
func TestHandleHarnessLogin_PerUserUsesTheRowsStartURL(t *testing.T) {
	for who, sess := range map[string]*http.Cookie{
		"member": ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember),
		"admin":  ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin),
	} {
		t.Run(who, func(t *testing.T) {
			srv, audit := perUserLoginSrv(t)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", sess,
				`{"provider":"aws","sso_start_url":"https://attacker.example.com/start"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if got := loginStartURL(t, audit); got != perUserPortal {
				t.Errorf("login seeded with %q, want the ROW's %q — a sign-in must never choose its own portal", got, perUserPortal)
			}
		})
	}
}

// TestHandleHarnessLogin_MemberRefusedWithoutPerUserRow is the other half of the
// widening: the route admits any signed-in human ONLY where the org said each
// person signs in themselves. A shared deployment — and one with no roster at
// all — still answers a member 403, and the operator still passes.
func TestHandleHarnessLogin_MemberRefusedWithoutPerUserRow(t *testing.T) {
	shared := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO}
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
	for name, rows := range map[string][]types.AgentProvider{
		"a shared row":   {shared},
		"a disabled row": {{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO, CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal, Disabled: true}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, audit := perUserLoginSrv(t, rows...)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", member, `{"provider":"aws"}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("member status = %d, want 403; body=%s", w.Code, w.Body.String())
			}
			if len(audit.find("authz.denied")) != 1 {
				t.Error("a refused sign-in must leave an authz.denied row")
			}
			if len(audit.find("harness.login.started")) != 0 {
				t.Error("a refused sign-in launched a sandbox anyway")
			}
			// The admin still reaches it — this is the SHARED credential's own
			// connect path, which never moved.
			adminW := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login",
				ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin),
				`{"provider":"aws","sso_start_url":"`+perUserPortal+`"}`)
			if adminW.Code != http.StatusOK {
				t.Fatalf("admin status = %d, want 200; body=%s", adminW.Code, adminW.Body.String())
			}
		})
	}
}

// TestHarnessLoginGovernance_ExemptsDenyInteractiveOnly: the acting principal's
// ceiling binds the login lane, EXCEPT deny_interactive.
//
// The exemption is narrow and it is the difference between a member having a
// route to model access and not: deny_interactive is about a person getting a
// shell for their own workload, and this box carries none — no workspace, no
// repo, no mounts, a device code on the terminal and nothing else. The quota
// still binds, because a login sandbox IS a run holding that person's slot.
func TestHarnessLoginGovernance_ExemptsDenyInteractiveOnly(t *testing.T) {
	gov := harnessLoginGovernance(governanceCeiling{})
	if gov.interactive {
		t.Error("the login lane must not declare itself interactive — deny_interactive would then wall a member out of signing in")
	}
	if !gov.counted {
		t.Error("a login sandbox is a real run and must count against max_concurrent_runs")
	}

	// A member whose assigned profile denies interactive runs outright.
	srv, audit := perUserLoginSrvUnder(t, assignedStore(limitsProfile("no-interactive",
		types.GovernanceLimits{DenyInteractive: true})), nil)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login",
		govSession(t, "sub-walled", []string{"eng"}, false), `{"provider":"aws"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — deny_interactive must not refuse the credential-capture box; body=%s", w.Code, w.Body.String())
	}
	if got := loginStartURL(t, audit); got != perUserPortal {
		t.Errorf("login seeded with %q, want %q", got, perUserPortal)
	}
}

// TestUploadSSOToken_PerUserCaptureIsOwnerScopedAndOnceOnly is the capture half
// of the lane, and the once-only guard is the part that had to move with it.
//
// The guard used to read the OPERATOR-wide blob. Under a per_user roster that is
// not the blob the run is about: `prev` would be the ADMIN's capture, its
// SourceRunID would never equal this run's, and the member's own login sandbox
// could PUT over its own genuine capture as often as it liked — reopening
// exactly the overwrite the guard exists to refuse (same start_url/region, the
// attacker's access_token/account/role), by reading the wrong namespace.
func TestUploadSSOToken_PerUserCaptureIsOwnerScopedAndOnceOnly(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	// mintRunToken mints the run identity with subject "alice@example.com" — the
	// SUBJECT, not the attribution, and the namespace selector. It is also what
	// launchHarnessLoginRun stamps onto harness.login.started as the launch-time
	// owner, which is the value handleUploadSSOToken now reads back.
	const subject = "alice@example.com"
	st := ssoLoginRunStore{
		run:    types.AgentRun{ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent},
		events: ssoLoginStartedPerUser(runID, "https://my-sso.awsapps.com/start", subject),
		siteCfg: agentRoster(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://my-sso.awsapps.com/start",
		}),
	}
	sec := &memSecrets{m: map[string][]byte{}}
	audit := &memAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.Audit = audit
	cfg.BedrockRegion = "us-west-2"
	srv := New(cfg)
	h.srv = srv
	tok := h.mintRunToken(t, runID)

	path := "/api/v1/internal/sso-token/" + runID.String()
	if w := do(t, srv, http.MethodPut, path, tok, validSSOBody); w.Code != http.StatusNoContent {
		t.Fatalf("first upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// It landed in the PRINCIPAL's namespace, and nowhere else.
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("a per_user capture landed in the OPERATOR namespace")
	}
	if _, ok := sec.owned[subject][harnessCredSecretName(awsSSOProvider)]; !ok {
		t.Fatalf("no capture in %q's own namespace; owned=%v", subject, sec.owned)
	}

	// THE PIN: a second upload from the same login run is refused, which it can
	// only be if the guard read the blob back through the same owner.
	w := do(t, srv, http.MethodPut, path, tok, validSSOBody)
	if w.Code != http.StatusConflict {
		t.Fatalf("second upload: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}

	// The capture row says WHOSE it is.
	rows := audit.find("harness.credential.captured")
	if len(rows) != 1 {
		t.Fatalf("harness.credential.captured rows = %d, want 1", len(rows))
	}
	var data struct {
		Owner            string `json:"owner"`
		CredentialSource string `json:"credential_source"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode capture audit data: %v", err)
	}
	if data.Owner != subject || data.CredentialSource != string(types.CredentialSourcePerUser) {
		t.Errorf("capture audit = owner %q / source %q, want %q / per_user", data.Owner, data.CredentialSource, subject)
	}
}

// TestUploadSSOToken_SharedCaptureStaysOperatorWide: with no per_user row the
// capture is byte-for-byte where it always was, and says so.
func TestUploadSSOToken_SharedCaptureStaysOperatorWide(t *testing.T) {
	srv, sec, tok, runID := newSSOUploadSrv(t)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody); w.Code != http.StatusNoContent {
		t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; !ok {
		t.Error("legacy/shared capture must stay in the operator namespace")
	}
}

// wedgedSecrets is a store that is UP but cannot answer — a rotated age key, a
// Postgres blip. Every read errors; nothing is ErrNotFound.
type wedgedSecrets struct{ err error }

func (wedgedSecrets) Name() string                                { return "wedged" }
func (w wedgedSecrets) Put(context.Context, string, []byte) error { return w.err }
func (w wedgedSecrets) Get(context.Context, string) ([]byte, error) {
	return nil, w.err
}
func (w wedgedSecrets) Delete(context.Context, string) error   { return w.err }
func (w wedgedSecrets) List(context.Context) ([]string, error) { return nil, w.err }
func (w wedgedSecrets) For(string) secretstore.Store           { return w }

// TestSetupHarnessCreds_AWedgedStoreGradesNoState: an outage is not a credential
// fact, and the probe must not turn one into the other.
//
// readHarnessBlob propagates every non-ErrNotFound error precisely so a rotated
// age key or a wedged Postgres is never mistaken for "not connected". Grading
// that into a state would undo it at the last step: a `shared` deployment's
// member would be told to ask their admin to reconnect a credential that is
// sitting there intact, and a per_user one would be told to sign in again.
func TestSetupHarnessCreds_AWedgedStoreGradesNoState(t *testing.T) {
	roster := agentRoster(types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
	})
	scopes := map[string]awsSSOScope{
		"shared":   {},
		"per_user": {perUser: true, owner: "member-sub"},
	}
	for name, scope := range scopes {
		t.Run(name, func(t *testing.T) {
			wedged := New(Config{
				Secrets: wedgedSecrets{err: errors.New("age: no identity matched any of the recipients")},
				Now:     func() time.Time { return awsSSOTestFixedNow },
			})
			rows, _, ma := wedged.setupHarnessCreds(context.Background(), roster, scope)
			if ma.State != "" {
				t.Errorf("a wedged store graded state %q — an outage must say nothing, not name a credential state", ma.State)
			}
			if len(rows) != 0 {
				t.Errorf("a wedged store reported %d harness row(s); it cannot know", len(rows))
			}

			// The CONTRAST, or the assertion above passes for a probe that never
			// says anything: a store that genuinely holds nothing DOES grade.
			empty := New(Config{
				Secrets: &memSecrets{m: map[string][]byte{}},
				Now:     func() time.Time { return awsSSOTestFixedNow },
			})
			if _, _, got := empty.setupHarnessCreds(context.Background(), roster, scope); got.State == "" {
				t.Error("a readable store with no capture must still grade a state")
			}
		})
	}
}

// TestPerUserLoginRow_IsKeyedByAgentNotOnlyMechanism: the sign-in DOOR and the
// CAPTURE must agree about which row they are acting on.
//
// The door admits on a per_user row; the upload scopes the capture with
// awsSSOScopeFor, which reads the modelAccessAgent row. A door keyed on the
// MECHANISM alone would, for a roster whose per_user bedrock_sso row named some
// other agent, admit the member and then resolve the ZERO scope — writing their
// capture into the OPERATOR namespace, the one place per_user exists to keep it
// out of.
func TestPerUserLoginRow_IsKeyedByAgentNotOnlyMechanism(t *testing.T) {
	foreign := agentRoster(types.AgentProvider{
		ID: "some-other-agent", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
	})
	if _, ok := perUserLoginRow(foreign, awsSSOProvider); ok {
		t.Error("a per_user row on another agent opened the sign-in door")
	}
	// The invariant itself: door and capture agree for every roster shape.
	for name, sc := range map[string]types.SiteConfig{
		"another agent's per_user row": foreign,
		"the right agent's per_user row": agentRoster(types.AgentProvider{
			ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
		}),
		"a shared row":     agentRoster(types.AgentProvider{ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO}),
		"no roster at all": {},
	} {
		_, door := perUserLoginRow(sc, awsSSOProvider)
		capture := awsSSOScopeFor(sc, modelAccessAgent, "member-sub").perUser
		if door != capture {
			t.Errorf("%s: the sign-in door says per_user=%v but the capture scopes per_user=%v — they must key on the same row", name, door, capture)
		}
	}

	// End to end: a member is refused at the door under that roster.
	srv, audit := perUserLoginSrv(t, types.AgentProvider{
		ID: "some-other-agent", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
	})
	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login",
		ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember), `{"provider":"aws"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if len(audit.find("harness.login.started")) != 0 {
		t.Error("a refused sign-in launched a sandbox anyway")
	}
}

// TestAWSSSOCredentialRow_NamesTheGradedDeadline: the admin's checklist row and
// the member's action line must name the SAME instant for one credential.
//
// The access token lives about an hour and the control plane renews it
// unattended, so printing its expiry on the `expiring` row was both a
// disagreement with the member's chip and a false sentence — the session keeps
// renewing past it. What runs out is the client registration.
func TestAWSSSOCredentialRow_NamesTheGradedDeadline(t *testing.T) {
	now := awsSSOTestFixedNow
	blob := awsSSOBlob{
		AccessToken: "a", RefreshToken: "r", StartURL: "https://x.awsapps.com/start",
		Region: "us-east-1", AccountID: "1", RoleName: "R",
		ExpiresAt:             now.Add(30 * time.Minute), // the hourly token
		RegistrationExpiresAt: now.Add(6 * time.Hour),    // what actually runs out
	}
	ma := setupModelAccess(agentRoster(types.AgentProvider{
		ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
	}), blob, true, false, awsSSOScope{}, true, now)
	if ma.State != modelAccessExpiring {
		t.Fatalf("state = %q, want expiring", ma.State)
	}
	if ma.Cause != causeRegistrationLapsing {
		t.Errorf("cause = %q, want %q — this blob is renewable, not spent", ma.Cause, causeRegistrationLapsing)
	}
	h := SetupHarness{
		Provider: awsSSOProvider, Captured: true,
		ExpiresAt: blob.ExpiresAt.Format(time.RFC3339),
		Renewable: true,
	}
	row, shown := harnessCredentialCheck(h, ma)
	if !shown {
		t.Fatal("the AWS row must be shown for a captured session")
	}
	if renewalSpentPrefix := strings.SplitN(harnessCredentialAWSRenewalSpentDetail, "%s", 2)[0]; strings.HasPrefix(row.Detail, renewalSpentPrefix) {
		t.Errorf("admin row detail = %q, must NOT use the renewal-spent sentence for a registration lapsing on schedule", row.Detail)
	}
	wantDeadline := blob.RegistrationExpiresAt.UTC().Format(time.RFC3339)
	if !strings.Contains(row.Detail, wantDeadline) {
		t.Errorf("admin row detail %q does not name the graded deadline %q", row.Detail, wantDeadline)
	}
	if strings.Contains(row.Detail, h.ExpiresAt) {
		t.Errorf("admin row detail names the ACCESS token's expiry %q — the session renews past it", h.ExpiresAt)
	}
	if !strings.Contains(ma.Action, wantDeadline) {
		t.Errorf("member action %q does not name the same deadline", ma.Action)
	}
}
