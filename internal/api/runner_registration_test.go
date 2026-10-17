// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type runnerRegistrationFake struct {
	*authzStore
	store.RunnerStore
	configErr error
	lock      sync.Mutex
	tokens    map[string]types.RunnerRegistrationToken
	rows      map[uuid.UUID]types.Runner
	runs      map[uuid.UUID]int
	emails    map[string]string
	// selfHost is the self_host_runner grant state migration 0178 sets up: restricted, and allowed to
	// the subjects in selfHostAllow (empty: nobody).
	selfHostRestricted bool
	people             map[string][]string // PrincipalsNamed's directory: a name to the principals it resolves to
	selfHostAllow      []types.CapabilityGrant
}

func newRunnerRegistrationServer(t *testing.T) (*Server, *runnerRegistrationFake, *harness) {
	t.Helper()
	f := &runnerRegistrationFake{authzStore: newAuthzStore(), tokens: map[string]types.RunnerRegistrationToken{}, people: map[string][]string{"alice": {"alice"}, "bob": {"bob"}}, rows: map[uuid.UUID]types.Runner{}}
	f.siteCfg.Runners = &types.RunnerSettings{Enabled: true}
	h := newHarness(t)
	cfg := baseTestConfig(h, f)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.RunnerOrgURL = "https://org.example.com"
	return New(cfg), f, h
}

func (f *runnerRegistrationFake) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	if f.configErr != nil {
		return types.SiteConfig{}, f.configErr
	}
	return f.authzStore.GetSiteConfig(ctx)
}

func (f *runnerRegistrationFake) PrincipalsNamed(_ context.Context, name string) ([]string, error) {
	return f.people[name], nil
}

func (f *runnerRegistrationFake) ListCapabilityRestrictions(ctx context.Context) (map[string]map[string]bool, error) {
	if f.selfHostRestricted {
		return map[string]map[string]bool{capFeature: {featureSelfHostRunner: true}}, nil
	}
	return f.authzStore.ListCapabilityRestrictions(ctx)
}

func (f *runnerRegistrationFake) ListCapabilityGrantsFor(context.Context, []string, []string, string) ([]types.CapabilityGrant, error) {
	return f.selfHostAllow, nil
}

func (f *runnerRegistrationFake) MintRunnerRegistrationToken(_ context.Context, raw string, token types.RunnerRegistrationToken) (types.RunnerRegistrationToken, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.tokens[raw] = token
	return token, nil
}
func (f *runnerRegistrationFake) ConsumeRunnerRegistrationToken(_ context.Context, raw, hash string, now time.Time) (types.RunnerRegistrationToken, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	token, ok := f.tokens[raw]
	if !ok || token.ConsumedAt != nil || !token.ExpiresAt.After(now) || token.OrgURLSHA256 != hash {
		return types.RunnerRegistrationToken{}, false, nil
	}
	token.ConsumedAt = &now
	f.tokens[raw] = token
	return token, true, nil
}
func (f *runnerRegistrationFake) CreateRunner(_ context.Context, row types.Runner) (types.Runner, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for _, old := range f.rows {
		if old.KeyFingerprint == row.KeyFingerprint {
			return types.Runner{}, store.ErrConflict
		}
	}
	row.State = types.RunnerUnclaimed
	row.CreatedAt = time.Now().UTC()
	f.rows[row.ID] = row
	return row, nil
}
func (f *runnerRegistrationFake) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return row, store.ErrNotFound
	}
	return row, nil
}
func (f *runnerRegistrationFake) ClaimRunner(_ context.Context, id uuid.UUID, owner, fp string, now time.Time) (types.Runner, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return row, store.ErrNotFound
	}
	if row.State != types.RunnerUnclaimed {
		return row, store.ErrConflict
	}
	if row.Owner != owner || row.KeyFingerprint != fp || !row.CreatedAt.Add(24*time.Hour).After(now) {
		return row, store.ErrRunnerClaimMismatch
	}
	row.State = types.RunnerClaimed
	row.ClaimedAt = &now
	f.rows[id] = row
	return row, nil
}

func TestRunnerRegistrationTokenTTLAndAdminBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, sub, role string
		ttl                         time.Duration
		want                        int
	}{
		{"self", "/me/runners/tokens", "{}", "alice", oidc.RoleUser, time.Hour, 201},
		{"admin for person", "/runners/tokens", `{"owner":"alice"}`, "operator", oidc.RoleAdmin, 72 * time.Hour, 201},
		{"member cannot mint for another", "/runners/tokens", `{"owner":"bob"}`, "alice", oidc.RoleUser, 0, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, h := newRunnerRegistrationServer(t)
			cookie := memberModeSSOSession(t, tc.sub, tc.sub+"@example.com", tc.role, false)
			w := doSSO(t, srv, http.MethodPost, "/api/v1"+tc.path, cookie, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status%d: %s", w.Code, w.Body.String())
			}
			if tc.want != 201 {
				return
			}
			var token types.RunnerRegistrationToken
			if err := json.Unmarshal(w.Body.Bytes(), &token); err != nil {
				t.Fatal(err)
			}
			if len(token.Token) != 68 || !strings.HasPrefix(token.Token, "wdr_") || token.Owner != "alice" || token.MintedBy != tc.sub || token.ExpiresAt.Sub(token.CreatedAt) != tc.ttl {
				t.Fatalf("bad token metadata: %+v", token)
			}
			ev := lastAuditEvent(t, h.audit.snapshot(), "runner.token.create")
			if strings.Contains(string(ev.Data), token.Token) {
				t.Fatal("raw token in audit")
			}
		})
	}
	srv, _, _ := newRunnerRegistrationServer(t)
	if w := do(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", adminToken, "{}"); w.Code != 403 {
		t.Fatalf("admin bearer minted self token: %d", w.Code)
	}
}

// POST /me/runners/tokens is the self-host door: with the feature value restricted (migration 0178) a
// member is refused until an allow names it, `all` included; an operator is exempt; the admin mint
// (POST /runners/tokens) is never asked.
func TestRunnerSelfMintNeedsTheSelfHostGrant(t *testing.T) {
	allow := func(subject string) []types.CapabilityGrant {
		return []types.CapabilityGrant{{SubjectType: types.CapabilitySubjectAll, Subject: subject, Capability: capFeature, Value: featureSelfHostRunner, Effect: types.CapabilityAllow}}
	}
	wildcard := allow("")
	wildcard[0].Value = capWildcard
	for _, tc := range []struct {
		name, role string
		allow      []types.CapabilityGrant
		want       int
	}{
		{"member without a grant", oidc.RoleUser, nil, 403},
		{"member with a wildcard allow only", oidc.RoleUser, wildcard, 403},
		{"member with an all allow", oidc.RoleUser, allow(""), 201},
		{"operator is exempt", oidc.RoleAdmin, nil, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f, _ := newRunnerRegistrationServer(t)
			f.selfHostRestricted, f.selfHostAllow = true, tc.allow
			cookie := memberModeSSOSession(t, "alice", "alice@example.com", tc.role, false)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", cookie, "{}")
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.want == 403 && (!strings.Contains(w.Body.String(), "capability_feature") || !strings.Contains(w.Body.String(), "self_host_runner") || len(f.tokens) != 0) {
				t.Fatalf("refusal lacks its reason, or a token was minted: %s", w.Body.String())
			}
		})
	}
	srv, f, _ := newRunnerRegistrationServer(t)
	f.selfHostRestricted = true
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runners/tokens", admin, `{"owner":"alice"}`); w.Code != 201 {
		t.Fatalf("admin mint for a person was gated: %d %s", w.Code, w.Body.String())
	}
}

// An operator stays exempt from the self-host gate when governed admin runs are on: the admin mint lets
// them name themself, so the gate there would bound nothing.
func TestRunnerSelfMintOperatorExemptWhenAdminRunsAreGoverned(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	srv.cfg.GovernAdminRuns = true
	f.selfHostRestricted = true
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", admin, "{}"); w.Code != 201 {
		t.Fatalf("a governed-runs admin was asked for the grant: %d %s", w.Code, w.Body.String())
	}
	member := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", member, "{}"); w.Code != 403 {
		t.Fatalf("a member without the grant minted: %d %s", w.Code, w.Body.String())
	}
}

func TestRunnerRegistrationAnonymousUnclaimedAndSingleUse(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	raw := newBearer("wdr_")
	now := time.Now().UTC()
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "private-owner", MintedBy: "private-admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: testRunnerLimits()})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != 201 {
		t.Fatalf("register:%d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-owner", "private-admin", raw, "owner", "minted_by"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("runner response exposed %q", secret)
		}
	}
	var result types.RunnerRegistration
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != types.RunnerUnclaimed || result.Fingerprint != runnerwire.Fingerprint(pub) {
		t.Fatalf("bad registration:%+v", result)
	}
	row := f.rows[result.RunnerID]
	if row.Owner != "private-owner" || row.State != types.RunnerUnclaimed || row.OrgURLSHA256 != federation.OrgURLSHA256(srv.cfg.RunnerOrgURL) {
		t.Fatal("registration lost owner/binding or auto-claimed")
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body)); w.Code != 401 {
		t.Fatalf("token reused:%d", w.Code)
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runner.enrol")
	if ev.ActorType != types.ActorSystem || strings.Contains(string(ev.Data), raw) {
		t.Fatal("registration audit identity or secret leak")
	}
}

func TestRunnerClaimOnlyOwnerAndObservedFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name, sub, role, fp, bearer string
		state                       types.RunnerState
		want                        int
	}{
		{"owner", "alice", oidc.RoleUser, "fingerprint", "", types.RunnerUnclaimed, 200},
		{"admin personal own", "alice", oidc.RoleAdmin, "fingerprint", "", types.RunnerUnclaimed, 200},
		{"admin for other", "bob", oidc.RoleAdmin, "fingerprint", "", types.RunnerUnclaimed, 403},
		{"other owner", "bob", oidc.RoleUser, "fingerprint", "", types.RunnerUnclaimed, 403},
		{"wrong fingerprint", "alice", oidc.RoleUser, "wrong", "", types.RunnerUnclaimed, 403},
		{"revoked", "alice", oidc.RoleUser, "fingerprint", "", types.RunnerRevoked, 403},
		{"admin bearer", "", "", "fingerprint", adminToken, types.RunnerUnclaimed, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f, h := newRunnerRegistrationServer(t)
			id := uuid.New()
			f.rows[id] = types.Runner{ID: id, Owner: "alice", KeyFingerprint: "fingerprint", State: tc.state, CreatedAt: time.Now().UTC(), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
			body, _ := json.Marshal(types.RunnerClaimRequest{Fingerprint: tc.fp})
			path := "/api/v1/me/runners/" + id.String() + "/claim"
			cookie := memberModeSSOSession(t, tc.sub, tc.sub+"@example.com", tc.role, false)
			w := doSSO(t, srv, http.MethodPost, path, cookie, string(body))
			if tc.bearer != "" {
				w = do(t, srv, http.MethodPost, path, tc.bearer, string(body))
			}
			if w.Code != tc.want {
				t.Fatalf("claim:%d %s", w.Code, w.Body.String())
			}
			if tc.want == 200 {
				if f.rows[id].State != types.RunnerClaimed {
					t.Fatal("claim not stored")
				}
				lastAuditEvent(t, h.audit.snapshot(), "runner.claim")
			} else if f.rows[id].State != tc.state {
				t.Fatal("refused claim changed row")
			}
		})
	}
}

func TestRunnerRegistrationUnsetOrgURLFailsClosed(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	srv.cfg.RunnerOrgURL = ""
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", "{}")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "WARDYN_RUNNER_ORG_URL") {
		t.Fatalf("missing org configuration:%d %s", w.Code, w.Body.String())
	}
}

// The per-peer limiter runs before the site-config read: a caller past its burst is answered 429 without
// the store being asked, even while the store cannot answer.
func TestRunnerRegistrationLimiterRunsBeforeTheSiteConfigRead(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	f.configErr = errors.New("store down")
	for i := range int(enrolBurst) {
		if w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", "{}"); w.Code != 503 {
			t.Fatalf("attempt %d inside the burst = %d, want 503 (the config read)", i, w.Code)
		}
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", "{}"); w.Code != 429 {
		t.Fatalf("attempt past the burst = %d %s, want 429 before the config read", w.Code, w.Body.String())
	}
}

func TestRunnerRegistrationBoundTokenRefusalDoesNotConsume(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	raw := newBearer("wdr_")
	now := time.Now().UTC()
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256("https://other.example.com")}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: testRunnerLimits()})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != 401 || len(f.rows) != 0 || f.tokens[raw].ConsumedAt != nil {
		t.Fatalf("foreign-bound token spent or admitted:%d %s", w.Code, w.Body.String())
	}
}

func TestRunnerRegistrationCannotReactivateRevokedKey(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	raw := newBearer("wdr_")
	now := time.Now().UTC()
	orgHash := federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: orgHash}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldID := uuid.New()
	f.rows[oldID] = types.Runner{ID: oldID, Owner: "alice", State: types.RunnerRevoked, KeyFingerprint: runnerwire.Fingerprint(pub), PublicKey: pub, OrgURLSHA256: orgHash}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: testRunnerLimits()})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != 409 || len(f.rows) != 1 || f.rows[oldID].State != types.RunnerRevoked {
		t.Fatalf("revoked key reactivated:%d %s", w.Code, w.Body.String())
	}
}

type runnerRegistrationBody struct {
	io.Reader
	advance func()
}

func (b runnerRegistrationBody) Read(p []byte) (int, error) {
	b.advance()
	return b.Reader.Read(p)
}

func TestRunnerRegistrationChecksExpiryAfterReadingBody(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	start := time.Now().UTC()
	now := start
	srv.cfg.Now = func() time.Time { return now }
	raw := newBearer("wdr_")
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", CreatedAt: start, ExpiresAt: start.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: testRunnerLimits()})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", nil)
	req.Body = io.NopCloser(runnerRegistrationBody{Reader: strings.NewReader(string(body)), advance: func() { now = start.Add(2 * time.Hour) }})
	w := httptest.NewRecorder()
	srv.handleRunnerRegister(w, req)
	if w.Code != 401 || len(f.rows) != 0 || f.tokens[raw].ConsumedAt != nil {
		t.Fatalf("expired while reading body:%d %s", w.Code, w.Body.String())
	}
}

func TestRunnerTokenCannotNameReservedMechanismAsOwner(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	w := do(t, srv, http.MethodPost, "/api/v1/runners/tokens", adminToken, `{"owner":"admin-token"}`)
	if w.Code != 422 || len(f.tokens) != 0 {
		t.Fatalf("reserved owner accepted:%d %s", w.Code, w.Body.String())
	}
}

func TestRunnerTokenMintAnswersCarryTheOrgURLAndAListNever(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	srv.cfg.ProxyImage = "registry.example/wardyn-proxy:1"
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	for name, mint := range map[string]func() *httptest.ResponseRecorder{
		"admin": func() *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodPost, "/api/v1/runners/tokens", admin, `{"owner":"alice"}`)
		},
		"self": func() *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", alice, "{}")
		},
	} {
		w := mint()
		var token types.RunnerRegistrationToken
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &token) != nil || token.OrgURL != srv.cfg.RunnerOrgURL || token.ProxyImage != srv.cfg.ProxyImage {
			t.Fatalf("%s mint = %d %s: want org_url %q and proxy_image %q", name, w.Code, w.Body.String(), srv.cfg.RunnerOrgURL, srv.cfg.ProxyImage)
		}
	}
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/tokens", admin, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"owner"`) || strings.Contains(w.Body.String(), "org_url") || strings.Contains(w.Body.String(), "proxy_image") {
		t.Fatalf("a token list must name its tokens and never carry org_url: %d %s", w.Code, w.Body.String())
	}
}

// Runner registration keeps its own per-address bucket and charges failures only: a flood of failed
// device enrolments from one address leaves a valid runner registration from it working, and a valid
// registration is refunded, so registering more runners than the bucket holds from one address (many
// people behind one ingress) never runs dry while failed attempts still do.
func TestRunnerRegistrationRateBucketIsItsOwnAndChargesFailuresOnly(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	const peer = "203.0.113.50:4000"
	register := func(raw string) int {
		t.Helper()
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: testRunnerLimits()})
		return doPeer(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body), peer).Code
	}
	valid := func() string {
		raw := newBearer("wdr_")
		now := time.Now().UTC()
		f.lock.Lock()
		f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "owner", MintedBy: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
		f.lock.Unlock()
		return raw
	}
	for range int(enrolBurst) + 2 {
		doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"wde_not-a-token"}`, peer)
	}
	for i := range int(enrolBurst) + 2 {
		if code := register(valid()); code != http.StatusCreated {
			t.Fatalf("valid registration %d from an address that flooded device enrolment = %d, want 201", i+1, code)
		}
	}
	refused := 0
	for range int(enrolBurst) + 2 {
		if register(newBearer("wdr_")) == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Error("failed registrations from one address were never rate limited")
	}
}

// The admin mint resolves `owner` through the directory every admin-names-a-person route uses: an email
// reaches the principal the person signs in as, and what cannot be resolved is refused, never stored.
func TestRunnerTokenAdminResolvesTheOwner(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		people      map[string][]string
		want        int
		wantOwner   string
	}{
		{"exact principal", "sub-alice", map[string][]string{"sub-alice": {"sub-alice"}}, 201, "sub-alice"},
		{"email of a known person", "Alice@Corp.example", map[string][]string{"Alice@Corp.example": {"sub-alice"}}, 201, "sub-alice"},
		{"ambiguous", "alice", map[string][]string{"alice": {"sub-a", "sub-A"}}, 422, ""},
		{"email nobody is known by", "ghost@corp.example", nil, 422, ""},
		{"name nobody is known by", "ghost", nil, 422, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f, _ := newRunnerRegistrationServer(t)
			f.people = tc.people
			w := do(t, srv, http.MethodPost, "/api/v1/runners/tokens", adminToken, `{"owner":"`+tc.owner+`"}`)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.want == 422 {
				if !strings.Contains(w.Body.String(), reasonRunnerOwnerInvalid) || len(f.tokens) != 0 {
					t.Fatalf("refusal lacks its reason, or a token was stored: %s", w.Body.String())
				}
				return
			}
			var token types.RunnerRegistrationToken
			if err := json.Unmarshal(w.Body.Bytes(), &token); err != nil || token.Owner != tc.wantOwner || f.tokens[token.Token].Owner != tc.wantOwner {
				t.Fatalf("owner = %q (%v), want the resolved principal %q", token.Owner, err, tc.wantOwner)
			}
		})
	}
}

// With no proxy image configured the mint answer omits proxy_image rather than carrying an empty string.
func TestRunnerTokenMintOmitsAnUnsetProxyImage(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", alice, "{}")
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "proxy_image") {
		t.Fatalf("mint = %d %s: want 201 without proxy_image", w.Code, w.Body.String())
	}
}

// The register answer carries the proxy image chosen for the architecture the runner reported: its
// own variant, else the plain WARDYN_PROXY_IMAGE, else nothing. The claim answer never carries one.
func TestRunnerRegistrationAnswersTheProxyImageForItsArch(t *testing.T) {
	const plain, arm = "ghcr.io/example/proxy:plain", "mirror.example.com/proxy:arm"
	for _, tc := range []struct {
		name, arch, want string
		plain            string
		byArch           map[string]string
	}{
		{"arm64 variant", "arm64", arm, plain, map[string]string{"arm64": arm}},
		{"amd64 without a variant falls back to the plain image", "amd64", plain, plain, map[string]string{"arm64": arm}},
		{"unreported arch gets the plain image", "", plain, plain, map[string]string{"arm64": arm}},
		{"variant with no plain image", "arm64", arm, "", map[string]string{"arm64": arm}},
		{"nothing configured omits the field", "arm64", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f, _ := newRunnerRegistrationServer(t)
			srv.cfg.ProxyImage, srv.cfg.ProxyImageByArch = tc.plain, tc.byArch
			raw := newBearer("wdr_")
			now := time.Now().UTC()
			f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", MintedBy: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
			pub, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: testRunnerLimits(), Arch: tc.arch})
			w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
			var got types.RunnerRegistration
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ProxyImage != tc.want {
				t.Fatalf("register = %d %s, want proxy_image %q", w.Code, w.Body.String(), tc.want)
			}
			if tc.want == "" && strings.Contains(w.Body.String(), "proxy_image") {
				t.Fatalf("an unset proxy image was sent: %s", w.Body.String())
			}

			// The claim answer, which has the same type, leaves it empty.
			fingerprint := runnerwire.Fingerprint(pub)
			claim, _ := json.Marshal(types.RunnerClaimRequest{Fingerprint: fingerprint})
			cookie := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
			w = doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/"+got.RunnerID.String()+"/claim", cookie, string(claim))
			if w.Code != 200 || strings.Contains(w.Body.String(), "proxy_image") {
				t.Fatalf("claim = %d %s, want 200 without proxy_image", w.Code, w.Body.String())
			}
		})
	}
}

// The mint answer carries WARDYN_PROXY_IMAGE as it always has, unless per-architecture images are
// configured: the runner's architecture is unknown at mint, so it then leaves the choice to register.
func TestRunnerTokenMintProxyImageFollowsThePerArchConfig(t *testing.T) {
	for name, byArch := range map[string]map[string]string{"no per-arch images": nil, "per-arch images": {"arm64": "mirror.example.com/proxy:arm"}} {
		srv, _, _ := newRunnerRegistrationServer(t)
		srv.cfg.ProxyImage, srv.cfg.ProxyImageByArch = "ghcr.io/example/proxy:plain", byArch
		alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", alice, "{}")
		has := strings.Contains(w.Body.String(), "ghcr.io/example/proxy:plain")
		if w.Code != http.StatusCreated || has != (byArch == nil) || strings.Contains(w.Body.String(), "mirror.example.com") {
			t.Fatalf("%s: mint = %d %s", name, w.Code, w.Body.String())
		}
	}
}

// A person mints ten runner registration tokens for themself in an hour; the eleventh is 429 with the
// reason and a Retry-After, and nobody else's mints are counted against them.
func TestRunnerTokenSelf_TheEleventhInAnHourIs429(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleAdmin, false)
	for i := range 10 {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", alice, "{}"); w.Code != 201 {
			t.Fatalf("mint %d = %d %s, want 201", i+1, w.Code, w.Body.String())
		}
	}
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", alice, "{}")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "360" || !strings.Contains(w.Body.String(), "runner_token_rate_limited") {
		t.Fatalf("the eleventh mint = %d %q %s, want 429, Retry-After 360 and runner_token_rate_limited", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if len(f.tokens) != 10 {
		t.Errorf("%d tokens stored, want 10", len(f.tokens))
	}
	bob := memberModeSSOSession(t, "bob", "bob@example.com", oidc.RoleAdmin, false)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", bob, "{}"); w.Code != 201 {
		t.Errorf("another person's mint = %d %s, want 201", w.Code, w.Body.String())
	}
}
