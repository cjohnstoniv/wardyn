// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sign-in doors keyed by model provider (MP-13): every capture lands in
// the signer's own namespace under the provider's UID-keyed name, is bound to
// the provider as it read at launch, and answers only that provider's holds.

// signInStore is integStore plus the one read the capture paths make: the
// login run's own harness.login.start stamp, served from the audit sink. Its
// site config can change under a live sign-in (setSite) and blip.
type signInStore struct {
	*integStore
	audit  *memAudit
	siteMu sync.Mutex
	blips  int // site-config reads that fail before one succeeds
}

func (s *signInStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	if s.blips > 0 {
		s.blips--
		return types.SiteConfig{}, errors.New("site config read blipped")
	}
	return s.site, nil
}

// setSite replaces the live block, as an admin's save would.
func (s *signInStore) setSite(sc types.SiteConfig) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.site = sc
}

func (s *signInStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	var out []types.AuditEvent
	for _, ev := range s.audit.find("harness.login.start") {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// signInFixture serves the doors over site's providers, with the boot Bedrock
// and SSO region set to one no provider names, so a launch that reads them is
// visible.
func signInFixture(t *testing.T, cs *capStore, site types.SiteConfig) (*Server, *signInStore, *memAudit, *memSecrets) {
	t.Helper()
	if cs == nil {
		cs = &capStore{}
	}
	h := newHarness(t)
	audit := &memAudit{}
	st := &signInStore{integStore: &integStore{govEscapeStore: newGovEscapeStore(cs), site: site}, audit: audit}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &fakeRunner{}
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	cfg.Secrets = sec
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion, cfg.BedrockAWSSSORegion = "eu-central-1", "eu-central-1"
	cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), st, audit, sec
}

// signIn POSTs one sign-in and returns the answer.
func signIn(t *testing.T, srv *Server, sess *http.Cookie, id string) (int, string) {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/model-providers/"+id+"/sign-in", sess,
		`{"sso_start_url":"https://someone-elses.awsapps.com/start"}`)
	return w.Code, w.Body.String()
}

func signInRunID(t *testing.T, body string) uuid.UUID {
	t.Helper()
	var resp harnessLoginResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode sign-in answer: %v (%s)", err, body)
	}
	id, err := uuid.Parse(resp.RunID)
	if err != nil {
		t.Fatalf("sign-in answered no run id: %s", body)
	}
	return id
}

// loginStamp is the one harness.login.start row, decoded.
func loginStamp(t *testing.T, audit *memAudit) loginRunStamp {
	t.Helper()
	rows := audit.find("harness.login.start")
	if len(rows) != 1 {
		t.Fatalf("harness.login.start rows = %d, want 1", len(rows))
	}
	var st loginRunStamp
	if err := json.Unmarshal(rows[0].Data, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestProviderSignInDoorsNeverBothAnswer(t *testing.T) {
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	t.Run("the legacy door refuses once a provider block exists", func(t *testing.T) {
		srv, _, audit, _ := signInFixture(t, nil, credentialSite(ssoProvider()))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", admin, `{"provider":"aws","sso_start_url":"https://acme.awsapps.com/start"}`)
		var eb errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &eb)
		if w.Code != http.StatusConflict || eb.Error != mpsLegacyDoor {
			t.Fatalf("legacy door = %d %s, want 409 %q", w.Code, w.Body.String(), mpsLegacyDoor)
		}
		if n := len(audit.find("harness.login.start")); n != 0 {
			t.Fatalf("the legacy door launched %d sign-ins beside a provider block", n)
		}
	})
	// One read decides both: a blip on it is a 503, never a read that says "no
	// block" beside another that authorizes.
	t.Run("a blipped read never lets the legacy door launch beside a block", func(t *testing.T) {
		srv, st, audit, _ := signInFixture(t, nil, credentialSite(ssoProvider()))
		st.blips = 1
		w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", admin, `{"provider":"aws","sso_start_url":"https://acme.awsapps.com/start"}`)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("legacy door on a blip = %d %s, want 503", w.Code, w.Body.String())
		}
		if n := len(audit.find("harness.login.start")); n != 0 {
			t.Fatalf("the legacy door launched %d sign-ins beside a provider block", n)
		}
	})
	t.Run("the provider door refuses while there is no block", func(t *testing.T) {
		srv, _, _, _ := signInFixture(t, nil, types.SiteConfig{})
		code, body := signIn(t, srv, admin, "bedrock-prod")
		if code != http.StatusConflict || !strings.Contains(body, mpsNoBlock) {
			t.Fatalf("provider door = %d %s, want 409 %q", code, body, mpsNoBlock)
		}
	})
}

// An AWS sign-in is seeded and stamped from the provider record alone: its
// portal, region and pin, never the request's or the boot config's, for the
// caller's own namespace under the provider's UID.
func TestProviderSignInAWSLaunch(t *testing.T) {
	site := credentialSite(ssoProvider())
	p := site.ModelProviders.Providers[0]
	srv, _, audit, _ := signInFixture(t, nil, site)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	code, body := signIn(t, srv, member, p.ID)
	if code != http.StatusOK {
		t.Fatalf("sign-in = %d %s, want 200", code, body)
	}
	signInRunID(t, body)
	st := loginStamp(t, audit)
	want := loginRunStamp{
		SSOStartURL: p.Bedrock.SSOStartURL, CredentialSource: string(types.CredentialSourcePerUser), Owner: st.Owner,
		SSOAccountID: p.Bedrock.SSOAccountID, SSORoleName: p.Bedrock.SSORoleName,
		ModelProvider: p.ID, ModelProviderUID: p.UID, ModelProviderAddress: providerAddressDigest(p),
		SSORegion: p.Bedrock.Region, Model: p.Harnesses[0].Model,
	}
	if st != want || st.Owner == "" {
		t.Fatalf("stamp = %+v\nwant  %+v with the member as owner", st, want)
	}
}

// An unpinned AWS sign-in is bound to the account of the model the caller may
// run on the provider, whichever harness serves it; models in two accounts
// are refused before a sandbox opens, unless a pin decides the account.
func TestProviderSignInAWSModel(t *testing.T) {
	const (
		modelA = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/claude"
		modelB = "arn:aws:bedrock:us-east-1:444455556666:application-inference-profile/codex"
	)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	provider := func(pinned bool, hs ...types.ProviderHarness) types.ModelProvider {
		p := ssoProvider()
		if !pinned {
			p.Bedrock.SSOAccountID, p.Bedrock.SSORoleName = "", ""
		}
		p.Harnesses = hs
		return p
	}
	both := []types.ProviderHarness{{Harness: "claude-code", Model: modelA}, {Harness: "codex-cli", Model: modelB}}
	for _, tc := range []struct {
		name  string
		p     types.ModelProvider
		code  int
		model string
	}{
		{"a provider serving only codex-cli binds to its model", provider(false, types.ProviderHarness{Harness: "codex-cli", Model: modelB}), http.StatusOK, modelB},
		{"unpinned, models in two accounts are refused", provider(false, both...), http.StatusUnprocessableEntity, ""},
		{"a pin outranks the models' accounts", provider(true, both...), http.StatusOK, modelA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, audit, _ := signInFixture(t, nil, credentialSite(tc.p))
			code, body := signIn(t, srv, member, tc.p.ID)
			if code != tc.code {
				t.Fatalf("sign-in = %d %s, want %d", code, body, tc.code)
			}
			if code != http.StatusOK {
				var eb errorBody
				_ = json.Unmarshal([]byte(body), &eb)
				if n := len(audit.find("harness.login.start")); n != 0 || eb.Error != fmt.Sprintf(mpsAccounts, tc.p.ID) {
					t.Fatalf("refusal %s launched %d sandboxes, want %q and none", body, n, fmt.Sprintf(mpsAccounts, tc.p.ID))
				}
				return
			}
			if got := loginStamp(t, audit).Model; got != tc.model {
				t.Fatalf("stamped model = %q, want %q", got, tc.model)
			}
		})
	}
}

func TestProviderSignInRefusals(t *testing.T) {
	noPortal := ssoProvider()
	noPortal.ID, noPortal.Bedrock = "bedrock-bare", &types.BedrockSettings{Region: "us-east-1"}
	off := subProvider("claude-off")
	off.Disabled = true
	site := credentialSite(ssoProvider(), subProvider("claude"), keyProvider("anthropic", "claude-code"), noPortal, off)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)

	for _, tc := range []struct {
		name string
		cs   *capStore
		prep func(*Server)
		id   string
		code int
		want string
	}{
		{"a provider nobody configured", nil, nil, "nope", http.StatusNotFound, fmt.Sprintf(mpcNotFound, "nope")},
		{"a key kind is not signed in to", nil, nil, "anthropic", http.StatusUnprocessableEntity, fmt.Sprintf(mpsTyped, "anthropic")},
		{"a provider that is turned off", nil, nil, "claude-off", http.StatusUnprocessableEntity, fmt.Sprintf(mpsOff, "claude-off")},
		{"a provider with no portal set", nil, nil, "bedrock-bare", http.StatusUnprocessableEntity, fmt.Sprintf(mpsNoPortal, "bedrock-bare")},
		{"no Claude sign-in image", nil, func(s *Server) { s.cfg.AgentImages = nil }, "claude", http.StatusUnprocessableEntity, mpsNoImage},
		{"a provider the member is not granted", &capStore{enf: map[string]bool{capModelProvider: true}}, nil,
			"bedrock-prod", http.StatusForbidden, fmt.Sprintf(mpsNotGranted, "bedrock-prod")},
		{"a provider serving no agent the member may launch", &capStore{grants: []types.CapabilityGrant{{
			SubjectType: types.CapabilitySubjectAll, Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny,
		}}}, nil, "bedrock-prod", http.StatusNotFound, fmt.Sprintf(mpcNotFound, "bedrock-prod")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, audit, _ := signInFixture(t, tc.cs, site)
			if tc.prep != nil {
				tc.prep(srv)
			}
			code, body := signIn(t, srv, member, tc.id)
			var eb errorBody
			_ = json.Unmarshal([]byte(body), &eb)
			if code != tc.code || eb.Error != tc.want {
				t.Fatalf("sign-in = %d %s\nwant %d %q", code, body, tc.code, tc.want)
			}
			if n := len(audit.find("harness.login.start")); n != 0 {
				t.Fatalf("a refused sign-in launched %d sandboxes", n)
			}
		})
	}

	// Rule 5: the admin token under OIDC is a mechanism, not a person — it
	// has no credential and cannot capture one.
	t.Run("the admin token cannot sign in", func(t *testing.T) {
		srv, _, audit, _ := signInFixture(t, nil, site)
		for _, m := range []string{http.MethodPost, http.MethodPut} {
			w := do(t, srv, m, "/api/v1/model-providers/claude/sign-in", adminToken, `{"run_id":"`+uuid.NewString()+`","token":"sk-ant-oat01-x"}`)
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "admin token") {
				t.Fatalf("%s as the admin token = %d %s, want 422", m, w.Code, w.Body.String())
			}
		}
		if n := len(audit.find("harness.login.start")); n != 0 {
			t.Fatal("the admin token launched a sign-in")
		}
	})
}

// providerSSOUpload wires the helper's upload route over a login run stamped
// for p by alice (mintRunToken's subject), with site as the live block.
func providerSSOUpload(t *testing.T, p types.ModelProvider, site types.SiteConfig, owner string) (*Server, *memSecrets, string, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	events := []types.AuditEvent{{
		ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "harness.login.start", Target: runID.String(), Outcome: "success",
		Data: mustJSON(map[string]any{
			"provider": awsSSOProvider, "sso_start_url": p.Bedrock.SSOStartURL,
			"credential_source": string(types.CredentialSourcePerUser), "owner": owner,
			"sso_account_id": p.Bedrock.SSOAccountID, "sso_role_name": p.Bedrock.SSORoleName,
			"model_provider": p.ID, "model_provider_uid": p.UID, "sso_region": p.Bedrock.Region,
			"model": p.Harnesses[0].Model, "model_provider_address": providerAddressDigest(p),
		}),
	}}
	srv, sec, tok := newSSOUploadSrvWith(t, events, site, runID)
	sec.owned = map[string]map[string][]byte{}
	// Boot config no provider names: a binding that read it would show.
	srv.cfg.BedrockRegion = "eu-central-1"
	return srv, sec, tok, runID
}

func providerSSOBody(p types.ModelProvider, region string) string {
	b, _ := json.Marshal(map[string]any{
		"access_token": "aws-sso-provider-access-token", "refresh_token": "aws-sso-provider-refresh",
		"start_url": p.Bedrock.SSOStartURL, "region": region,
		"account_id": p.Bedrock.SSOAccountID, "role_name": p.Bedrock.SSORoleName, "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
	return string(b)
}

// An AWS provider sign-in lands under the provider's own name in the
// signer's own namespace — never the roster's harness name, never the
// operator's — and only while the provider is still the one it was for.
func TestProviderSignInAWSCapture(t *testing.T) {
	site := credentialSite(ssoProvider())
	p := site.ModelProviders.Providers[0]

	t.Run("lands under the provider's name in the signer's own namespace", func(t *testing.T) {
		srv, sec, tok, runID := providerSSOUpload(t, p, site, subOwner)
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, providerSSOBody(p, p.Bedrock.Region))
		if w.Code != http.StatusNoContent {
			t.Fatalf("upload = %d %s, want 204", w.Code, w.Body.String())
		}
		if got := slices.Collect(maps.Keys(sec.owned[subOwner])); len(got) != 1 || got[0] != providerSecretName(p.UID, providerSSOPart) {
			t.Fatalf("the signer's namespace holds %v, want only the provider's -sso name", got)
		}
		if len(sec.m) != 0 {
			t.Fatalf("the operator namespace holds %v, want nothing", slices.Collect(maps.Keys(sec.m)))
		}
		blob, found, err := srv.readAWSSSOBlob(context.Background(), chosenProvider{provider: p, owner: subOwner}.awsScope())
		if err != nil || !found || blob.SourceRunID != runID.String() {
			t.Fatalf("provider read = (%+v, %v, %v), want the capture", blob, found, err)
		}
	})

	for _, tc := range []struct {
		name   string
		owner  string
		region string
		alter  func(*types.SiteConfig)
		code   int
	}{
		{"a session for the boot region, not the provider's", subOwner, "eu-central-1", nil, http.StatusBadRequest},
		{"a sign-in someone else started", "bob@example.com", "", nil, http.StatusConflict},
		{"the provider was re-addressed while it was open", subOwner, "", func(sc *types.SiteConfig) {
			sc.ModelProviders.Providers[0].Bedrock.Region = "us-west-2"
		}, http.StatusConflict},
		// Rule 8 purges on any address change, the region's or not.
		{"the provider's Bedrock base URL moved while it was open", subOwner, "", func(sc *types.SiteConfig) {
			sc.ModelProviders.Providers[0].Bedrock.BaseURL = brBaseURL
		}, http.StatusConflict},
		{"the provider was deleted and re-added under its id", subOwner, "", func(sc *types.SiteConfig) {
			sc.ModelProviders.Providers[0].UID = uuid.NewString()
		}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := credentialSite(ssoProvider())
			live.ModelProviders.Providers[0].UID = p.UID
			if tc.alter != nil {
				tc.alter(&live)
			}
			srv, sec, tok, runID := providerSSOUpload(t, p, live, tc.owner)
			w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok,
				providerSSOBody(p, cmpOr(tc.region, p.Bedrock.Region)))
			if w.Code != tc.code {
				t.Fatalf("upload = %d %s, want %d", w.Code, w.Body.String(), tc.code)
			}
			if len(sec.m) != 0 || len(sec.owned) != 0 {
				t.Fatalf("a refused capture stored %v / %v", slices.Collect(maps.Keys(sec.m)), slices.Collect(maps.Keys(sec.owned)))
			}
		})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// A Claude sign-in is stored by PUT, bound to the sign-in run its own caller
// launched for that provider through this door.
func TestProviderSignInClaudeCapture(t *testing.T) {
	const token = "sk-ant-oat01-member-own-claude-sign-in"
	site := credentialSite(subProvider("claude"), subProvider("claude-team"), ssoProvider())
	p := site.ModelProviders.Providers[0]
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	capture := func(t *testing.T, srv *Server, sess *http.Cookie, id string, runID uuid.UUID, tok string) (int, string) {
		t.Helper()
		w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/"+id+"/sign-in", sess,
			fmt.Sprintf(`{"run_id":%q,"token":%q}`, runID, tok))
		return w.Code, w.Body.String()
	}
	launch := func(t *testing.T) (*Server, *signInStore, *memAudit, *memSecrets, uuid.UUID) {
		t.Helper()
		srv, st, audit, sec := signInFixture(t, nil, site)
		code, body := signIn(t, srv, member, p.ID)
		if code != http.StatusOK {
			t.Fatalf("sign-in = %d %s", code, body)
		}
		return srv, st, audit, sec, signInRunID(t, body)
	}

	t.Run("stored for the signer alone under the provider's name", func(t *testing.T) {
		srv, _, audit, sec, runID := launch(t)
		if code, body := capture(t, srv, member, p.ID, runID, token); code != http.StatusNoContent {
			t.Fatalf("capture = %d %s, want 204", code, body)
		}
		owner := loginStamp(t, audit).Owner
		raw := sec.owned[owner][providerSecretName(p.UID, providerOAuthPart)]
		var blob managedCredBlob
		if json.Unmarshal(raw, &blob) != nil || blob.Token != token || blob.SourceRunID != runID.String() {
			t.Fatalf("stored %s, want the token and its sign-in run", raw)
		}
		if len(sec.m) != 0 || len(sec.owned) != 1 {
			t.Fatalf("operator rows %v, namespaces %d — want none and one", slices.Collect(maps.Keys(sec.m)), len(sec.owned))
		}
		if d, err := srv.providerSubscriptionRefusal(context.Background(), p, owner); err != nil || d.msg != "" {
			t.Fatalf("after signing in, dispatch's check = (%+v, %v), want live", d, err)
		}
		audit.mu.Lock()
		defer audit.mu.Unlock()
		for _, ev := range audit.rows {
			if strings.Contains(string(ev.Data), token) {
				t.Fatalf("%s carries the token", ev.Action)
			}
		}
	})

	t.Run("refused", func(t *testing.T) {
		other := ssoSession(t, "sub-other", "other@corp.example", oidc.RoleUser)
		for _, tc := range []struct {
			name string
			do   func(*testing.T, *Server, *signInStore, uuid.UUID) (int, string)
			code int
		}{
			{"someone else's sign-in run", func(t *testing.T, srv *Server, _ *signInStore, runID uuid.UUID) (int, string) {
				return capture(t, srv, other, p.ID, runID, token)
			}, http.StatusConflict},
			{"a run started for another provider", func(t *testing.T, srv *Server, _ *signInStore, runID uuid.UUID) (int, string) {
				return capture(t, srv, member, "claude-team", runID, token)
			}, http.StatusConflict},
			{"a legacy sign-in run with no provider stamp", func(t *testing.T, srv *Server, st *signInStore, _ uuid.UUID) (int, string) {
				legacy := types.AgentRun{ID: uuid.New(), Task: harnessLoginTask, Agent: "claude-code", CreatedBy: "member@corp.example"}
				_, _ = st.CreateRun(context.Background(), legacy)
				return capture(t, srv, member, p.ID, legacy.ID, token)
			}, http.StatusConflict},
			{"a sign-in a newer one replaced", func(t *testing.T, srv *Server, st *signInStore, runID uuid.UUID) (int, string) {
				st.mu.Lock()
				st.states[runID] = types.RunKilled
				st.mu.Unlock()
				return capture(t, srv, member, p.ID, runID, token)
			}, http.StatusConflict},
			{"the provider was re-addressed while it was open", func(t *testing.T, srv *Server, st *signInStore, runID uuid.UUID) (int, string) {
				// The same provider (UIDs kept), routed somewhere else.
				moved := *site.ModelProviders
				moved.Providers = slices.Clone(moved.Providers)
				moved.Providers[0].BaseURL = "https://claude-route.corp.example"
				st.setSite(types.SiteConfig{ModelProviders: &moved})
				return capture(t, srv, member, p.ID, runID, token)
			}, http.StatusConflict},
			{"an AWS provider, whose helper stores it", func(t *testing.T, srv *Server, _ *signInStore, runID uuid.UUID) (int, string) {
				return capture(t, srv, member, "bedrock-prod", runID, token)
			}, http.StatusUnprocessableEntity},
			{"not a setup-token", func(t *testing.T, srv *Server, _ *signInStore, runID uuid.UUID) (int, string) {
				return capture(t, srv, member, p.ID, runID, "not-a-claude-token-at-all")
			}, http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, st, _, sec, runID := launch(t)
				// The detached launch may still be moving the run; let it settle.
				waitSignInRunning(t, st, runID)
				if code, body := tc.do(t, srv, st, runID); code != tc.code {
					t.Fatalf("capture = %d %s, want %d", code, body, tc.code)
				}
				if len(sec.m) != 0 || len(sec.owned) != 0 {
					t.Fatalf("a refused capture stored %v / %v", slices.Collect(maps.Keys(sec.m)), slices.Collect(maps.Keys(sec.owned)))
				}
			})
		}
	})
}

// waitSignInRunning waits for the detached launch to take the run past PENDING, so
// a test that sets its state does not race dispatch.
func waitSignInRunning(t *testing.T, st *signInStore, runID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := st.GetRun(context.Background(), runID); err == nil && r.State != types.RunPending && r.State != types.RunStarting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// providerReauthFixture is newReauthFixture on a run that chose a bedrock_sso
// provider: its grant records the provider, and alice's session lives under
// the provider's own name.
func providerReauthFixture(t *testing.T) (*reauthFixture, types.ModelProvider) {
	t.Helper()
	p := types.ModelProvider{ID: "bedrock-prod", UID: uuid.NewString(), Kind: types.ModelProviderBedrockSSO,
		Bedrock: &types.BedrockSettings{Region: reauthRegion, SSOStartURL: "https://acme.awsapps.com/start",
			SSOAccountID: "111122223333", SSORoleName: "WardynAgent"},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: brModel}}}
	f := newReauthFixture(t, nil)
	f.st.site = types.SiteConfig{ModelProviders: providerBlock(p)}
	f.st.run.ModelProviderID = p.ID
	var scope map[string]any
	_ = json.Unmarshal(f.st.grants[0].Spec.Scope, &scope)
	sn := scope["snapshot"].(map[string]any)
	sn["provider_uid"] = p.UID
	f.st.grants[0].Spec.Scope, _ = json.Marshal(scope)
	return f, p
}

func putProviderBlob(t *testing.T, f *reauthFixture, p types.ModelProvider, b awsSSOBlob) {
	t.Helper()
	raw, _ := json.Marshal(b)
	if err := f.secrets.For("alice@example.com").Put(context.Background(), providerSecretName(p.UID, providerSSOPart), raw); err != nil {
		t.Fatal(err)
	}
}

// A provider run whose owner's session lapsed is HELD (no longer failed
// outright), and the hold names its provider: only a sign-in for that
// provider answers it (rule 6).
func TestProviderSignInReauth(t *testing.T) {
	raise := func(t *testing.T) (*reauthFixture, types.ModelProvider, types.ApprovalRequest) {
		t.Helper()
		f, p := providerReauthFixture(t)
		putProviderBlob(t, f, p, deadSSOBlob())
		if w := f.resolve(t); w.Code != http.StatusLocked {
			t.Fatalf("resolve on a lapsed session = %d %s, want 423", w.Code, w.Body.String())
		}
		return f, p, onlyReauthRow(t, f.srv)
	}

	t.Run("the hold names its provider", func(t *testing.T) {
		_, p, ap := raise(t)
		var sc map[string]string
		_ = json.Unmarshal(ap.RequestedScope, &sc)
		if sc["provider"] != p.ID || sc["provider_uid"] != p.UID || sc["owner"] != "alice@example.com" {
			t.Fatalf("requested_scope = %v, want the provider's id and uid and alice", sc)
		}
	})

	later := func() types.AgentRun {
		return types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: time.Now().Add(time.Minute)}
	}
	for _, tc := range []struct {
		name     string
		scope    func(types.ModelProvider) awsSSOScope
		resolves bool
	}{
		{"a sign-in for another provider", func(types.ModelProvider) awsSSOScope {
			return awsSSOScope{perUser: true, owner: "alice@example.com", provider: uuid.NewString()}
		}, false},
		{"a roster sign-in", func(types.ModelProvider) awsSSOScope {
			return awsSSOScope{perUser: true, owner: "alice@example.com"}
		}, false},
		{"another person's sign-in for the same provider", func(p types.ModelProvider) awsSSOScope {
			return awsSSOScope{perUser: true, owner: "bob@example.com", provider: p.UID}
		}, false},
		{"the owner's sign-in for this provider", func(p types.ModelProvider) awsSSOScope {
			return chosenProvider{provider: p, owner: "alice@example.com"}.awsScope()
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, ap := raise(t)
			f.srv.resolvePendingReauth(context.Background(), tc.scope(p), "alice@example.com", later())
			got, _ := f.srv.cfg.Approvals.Get(context.Background(), ap.ID)
			if (got.State == types.ApprovalApproved) != tc.resolves {
				t.Fatalf("state = %s, resolves = %v", got.State, tc.resolves)
			}
		})
	}

	t.Run("a provider sign-in never answers a roster hold", func(t *testing.T) {
		f := newReauthFixture(t, nil)
		f.putBlob(t, "alice@example.com", deadSSOBlob())
		if w := f.resolve(t); w.Code != http.StatusLocked {
			t.Fatalf("roster resolve = %d, want 423", w.Code)
		}
		ap := onlyReauthRow(t, f.srv)
		f.srv.resolvePendingReauth(context.Background(),
			awsSSOScope{perUser: true, owner: "alice@example.com", provider: uuid.NewString()}, "alice@example.com", later())
		if got, _ := f.srv.cfg.Approvals.Get(context.Background(), ap.ID); got.State != types.ApprovalPending {
			t.Fatalf("a provider sign-in resolved a roster hold: %s", got.State)
		}
	})

	t.Run("the sign-in lands, the hold resolves and the retry is served", func(t *testing.T) {
		f, p, ap := raise(t)
		login := later()
		f.st.loginRun = login
		fresh := liveSSOBlob()
		fresh.AccessToken, fresh.SourceRunID = "the-provider-sign-in-token", login.ID.String()
		putProviderBlob(t, f, p, fresh)
		// The capture's own resolution did not land (a crash): the sidecar's
		// poll repairs it through the provider's own name.
		w := do(t, f.srv, http.MethodGet, "/api/v1/internal/approvals/"+ap.ID.String(), f.token, "")
		var got types.ApprovalRequest
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got.State != types.ApprovalApproved {
			t.Fatalf("poll = %d, state %s; want the stranded hold resolved", w.Code, got.State)
		}
		var resp types.ResolvedInjection
		if w := f.resolve(t); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.Value != fresh.AccessToken {
			t.Fatalf("retry = %d %s, want the new session", w.Code, w.Body.String())
		}
	})
}
