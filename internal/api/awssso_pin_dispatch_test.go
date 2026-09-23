// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// P4 — "setting a pin does not invalidate a stored capture that contradicts it".
//
// bindCaptureToPin binds a capture to the pin AS IT READ AT LAUNCH, at CAPTURE
// TIME and nowhere else. So the sequence that produces a wrong identity is:
// capture UNPINNED (nothing constrains the account/role), then set the pin.
// The stored blob is now the one thing the pin was supposed to govern and the
// only door that ever compared them has already closed. Worse, the console's
// `bedrock_provider` warning about an unpinned row DISAPPEARS the moment the
// pin is saved, so the estate reads MORE correct than it did; dispatch bakes
// the old account/role into the run's ~/.aws/config verbatim
// (awsSSOConfigFileContents) and botocore asks GetRoleCredentials for an
// account the person may not even reach — an IAM 403 ten retries deep inside
// somebody else's terminal, which is the failure awssso_pin.go's own header
// says the refuse-don't-rewrite decision exists to avoid.
//
// This file pins the four doors that close it: dispatch, create/preflight, the
// caller's own /setup/status grading, and the predicate all three share.

// pinTestMember is the per_user principal whose own namespace holds the blob.
const pinTestMember = "member-sub"

// pinDispatchModel is a BARE cross-region inference profile id: it names no
// account, so bedrockModelAccount() answers "" and nothing in these tests is
// decided by the model-account check that already exists. The subject here is
// the ROSTER PIN versus the STORED BLOB, and nothing else.
const pinDispatchModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"

// pinnedPerUserRoster is the roster shape this whole file is about: one
// enabled per_user bedrock_sso row for claude-code, pinned to account/role.
// An empty account/role pair is the UNPINNED row (the upgrade guard's shape).
func pinnedPerUserRoster(account, role string) types.SiteConfig {
	return agentRoster(types.AgentProvider{
		ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
		SSOStartURL:      "https://acme.awsapps.com/start",
		SSOAccountID:     account, SSORoleName: role,
	})
}

// putPinnedSSOBlob writes a structurally valid captured session naming a
// CHOSEN account/role into owner's namespace. putScopedSSOBlob (modelaccess_
// test.go) hardcodes one pair; the whole question here is which pair is stored,
// so this file needs its own.
func putPinnedSSOBlob(t *testing.T, sec *scopedSecrets, owner, account, role string) {
	t.Helper()
	raw, err := json.Marshal(awsSSOBlob{
		AccessToken:  "access-" + owner,
		RefreshToken: "refresh-" + owner,
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		StartURL:     "https://acme.awsapps.com/start",
		Region:       "us-east-1",
		AccountID:    account,
		RoleName:     role,
		// One hour out: renewable() is true and needsRefresh() is false, so the
		// dispatch pass (refresh=true) is a no-op on the wire and these tests
		// need no fake OIDC endpoint.
		ExpiresAt:  awsSSOTestFixedNow.Add(time.Hour),
		CapturedAt: awsSSOTestFixedNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	if perr := sec.For(owner).Put(context.Background(), harnessCredSecretName(awsSSOProvider), raw); perr != nil {
		t.Fatalf("put blob for %q: %v", owner, perr)
	}
}

// pinDispatchSrv is a Bedrock deployment with a secret store, a fixed clock and
// the refusal path's store (failAndRevoke CASes STARTING -> FAILED). It carries
// NO roster of its own: every test hands the gate the site config it means.
func pinDispatchSrv(t *testing.T) (*Server, *scopedSecrets, *mechanismGateStore, *harness) {
	t.Helper()
	h := newHarness(t)
	sec := newScopedSecrets()
	st := &mechanismGateStore{}
	srv := New(Config{
		Identity: h.idp, Audit: h.audit, Store: st,
		BedrockRegion: "us-east-1", BedrockModel: pinDispatchModel,
		Secrets:      sec,
		MaskRegistry: secretmask.NewRegistry(),
		Now:          func() time.Time { return awsSSOTestFixedNow },
	})
	h.srv = srv
	return srv, sec, st, h
}

// dispatchWithScope resolves the run's transport exactly as dispatch does
// (resolveLLMInjections -> resolveLLMTransport with refresh=true) and then asks
// the declared-mechanism gate, returning the gate's verdict plus the transport
// so a test can assert the credential really DID resolve — a refusal that comes
// from "nothing was selected" would prove nothing about the pin.
func dispatchWithScope(t *testing.T, srv *Server, sc types.SiteConfig, run types.AgentRun,
	owner string, interactive bool, taskMode string,
) (bool, llmTransport) {
	t.Helper()
	sso := awsSSOScopeFor(sc, run.Agent, owner)
	policy := &types.RunPolicySpec{AllowedDomains: []string{"git.example.com"}}
	llm := srv.resolveLLMTransport(context.Background(), run, policy, map[string]string{},
		nil, interactive, taskMode, "http://wardyn-proxy:3128", nil, sso)
	return srv.enforceConfiguredLLMMechanism(context.Background(), run, sc, llm, nil), llm
}

func pinTestRun() types.AgentRun {
	return types.AgentRun{ID: uuid.New(), Agent: modelAccessAgent, State: types.RunStarting}
}

// TestDispatch_StoredBlobContradictingThePinIsRefused is the defect itself.
// The capture landed while the row was UNPINNED (account 222222222222, the
// unrelated dev account the cloud team granted); the admin then pinned
// 111111111111. Today the run dispatches READY on the old pair and dies as an
// IAM 403 inside the member's terminal.
func TestDispatch_StoredBlobContradictingThePinIsRefused(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "222222222222", "DevPower")
	sc := pinnedPerUserRoster("111111111111", "BedrockRunner")

	admitted, llm := dispatchWithScope(t, srv, sc, pinTestRun(), pinTestMember, false, "")
	if !llm.bedrock.ready || !llm.bedrock.ssoInject {
		t.Fatalf("the stored session did not resolve at all (ready=%v ssoInject=%v) — "+
			"this test must refuse a credential that WOULD have carried the run", llm.bedrock.ready, llm.bedrock.ssoInject)
	}
	if admitted {
		t.Fatal("a run dispatched on a stored AWS session for an account the roster no longer allows")
	}
	if !st.failed {
		t.Error("the refused run was not marked FAILED — dispatch would carry on")
	}
}

// TestDispatch_TheRolePinIsRefused: the RIGHT account, the wrong role — the
// dispatch mirror of TestUploadSSOToken_RoleNameNotRowPinRejected. The
// finding's own repro had two roles and neither could invoke the model, so the
// role half is not decoration.
func TestDispatch_TheRolePinIsRefused(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "111111111111", "ReadOnly")
	sc := pinnedPerUserRoster("111111111111", "BedrockRunner")

	admitted, llm := dispatchWithScope(t, srv, sc, pinTestRun(), pinTestMember, false, "")
	if !llm.bedrock.ssoInject {
		t.Fatalf("the stored session did not resolve: %+v", llm.bedrock)
	}
	if admitted || !st.failed {
		t.Fatalf("a run dispatched on the pinned account's WRONG role (admitted=%v failed=%v)", admitted, st.failed)
	}
}

// TestDispatch_RefusalIsAuditedAndNamesBothIdentities is the dispatch mirror of
// TestUploadSSOToken_RefusalIsAudited: the refusal has to be readable by the
// person who has to fix it and by an incident review afterwards, so the run's
// failure reason names the pair that is STORED and the pair that is ALLOWED.
// Named "sign in again" too, because that — not an admin ticket — is the
// member's own recovery.
func TestDispatch_RefusalIsAuditedAndNamesBothIdentities(t *testing.T) {
	srv, sec, _, h := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "222222222222", "DevPower")
	sc := pinnedPerUserRoster("111111111111", "BedrockRunner")
	run := pinTestRun()

	if admitted, _ := dispatchWithScope(t, srv, sc, run, pinTestMember, false, ""); admitted {
		t.Fatal("the run was admitted; there is no refusal to audit")
	}
	var refusal string
	rows := 0
	for _, ev := range h.audit.events {
		if ev.Action != "run.create" || ev.Outcome != "failure" || ev.RunID == nil || *ev.RunID != run.ID {
			continue
		}
		rows++
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("run.create failure data is not an object: %v", err)
		}
		refusal, _ = data["error"].(string)
	}
	// EXACTLY ONE. The pin refusal REUSES the gate's existing emit rather than
	// adding a second one, so a duplicate row would mean two refusal paths ran
	// for one run — and an incident review counting refusals would double every
	// one of them.
	if rows != 1 {
		t.Fatalf("run.create failure rows for this run = %d, want exactly 1", rows)
	}
	if refusal == "" {
		t.Fatal("a refused dispatch left no run.create failure row carrying its reason")
	}
	for _, want := range []string{"222222222222", "DevPower", "111111111111", "BedrockRunner"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("refusal %q does not name %q — it must name BOTH the stored identity and the allowed one", refusal, want)
		}
	}
}

// TestCreateRun_StoredBlobContradictingThePinIs422 is the create/preflight
// twin: the same comparison, answered before a run row exists, because this
// file's own rule is that create refuses exactly the runs dispatch would (a
// 201 followed by an immediate FAILED is the boot-and-die the gate exists to
// prevent).
func TestCreateRun_StoredBlobContradictingThePinIs422(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "222222222222", "DevPower")
	st.sc = pinnedPerUserRoster("111111111111", "BedrockRunner")

	rec := httptest.NewRecorder()
	req := createRunRequest{Agent: modelAccessAgent, Task: "ship it"}
	ok := srv.enforceCreateLLMMechanism(context.Background(), rec, req, types.RunPolicySpec{}, nil, pinTestMember, nil, true)
	if ok {
		t.Fatal("create admitted a run whose stored AWS session contradicts the roster pin")
	}
	if rec.Code != 422 {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	for _, want := range []string{"222222222222", "111111111111"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("422 body = %q, want it to name %q", rec.Body.String(), want)
		}
	}
	// A new sign-in under the current pin is the repair, so this arm carries
	// the class the console's launch door acts on too.
	if !strings.Contains(rec.Body.String(), `"reason":"model_credential"`) {
		t.Errorf("422 body = %q, want reason model_credential", rec.Body.String())
	}
}

// TestSetupStatus_StoredBlobContradictingThePinGradesExpiredSignin is the
// AFFORDANCE half, and without it the two refusals above strand the member.
//
// MODEL_ACCESS_ACTIONABLE = {not_configured, expired_signin, expiring}
// (workspace-providers-copy.ts) is what decides whether the console offers
// "Sign in to AWS" at all. A contradicting-but-renewable blob grades `live` on
// expiry alone, so the button is HIDDEN — and a new sign-in is precisely the
// recovery (a new login run stamps the CURRENT pin and its capture OVERWRITES
// the old blob; see TestUploadSSOToken_NewLoginRunReplacesAContradictingCapture
// below). The state is an EXISTING one: nothing new on the wire, no TS mirror
// change, and the per-principal checklist row moves with it.
func TestSetupStatus_StoredBlobContradictingThePinGradesExpiredSignin(t *testing.T) {
	srv, sec, _, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "222222222222", "DevPower")
	putPinnedSSOBlob(t, sec, "admin-sub", "111111111111", "BedrockRunner")
	sc := pinnedPerUserRoster("111111111111", "BedrockRunner")
	ctx := context.Background()

	harnesses, _, ma := srv.setupHarnessCreds(ctx, sc, awsSSOScope{perUser: true, owner: pinTestMember})
	if ma.State != modelAccessExpiredSignin {
		t.Fatalf("model_access.state = %q, want %q — the member cannot ask for the sign-in that would repair this",
			ma.State, modelAccessExpiredSignin)
	}
	for _, want := range []string{"222222222222", "DevPower", "111111111111", "BedrockRunner"} {
		if !strings.Contains(ma.Action, want) {
			t.Errorf("action %q does not name %q", ma.Action, want)
		}
	}
	var awsRow SetupCheck
	for _, h := range harnesses {
		if chk, ok := harnessCredentialCheck(h, ma); ok && chk.ID == "harness_credential_aws" {
			awsRow = chk
		}
	}
	if awsRow.Status != "warn" {
		t.Errorf("harness_credential_aws status = %q, want warn", awsRow.Status)
	}
	// And it must SAY the true thing. `expired_signin` was already a warn row
	// before this lane, so status alone asserts nothing this fix added: without
	// the PinMismatch arm the row reads "Your captured AWS SSO session expired
	// at <ts> and cannot be renewed" about a session putPinnedSSOBlob made live
	// and renewable an hour out. The Fix is ma.Action verbatim, so the
	// operator's row and the member's action line cannot disagree about one
	// credential.
	if awsRow.Detail != harnessCredentialAWSPinMismatchDetail {
		t.Errorf("harness_credential_aws detail = %q, want the pin-mismatch sentence", awsRow.Detail)
	}
	if strings.Contains(awsRow.Detail, "expired at") {
		t.Errorf("harness_credential_aws detail = %q calls a LIVE, renewable session expired", awsRow.Detail)
	}
	if awsRow.Fix != ma.Action {
		t.Errorf("harness_credential_aws fix = %q, want the same action line the member reads (%q)", awsRow.Fix, ma.Action)
	}
	// 0.7.8: the grade stays warn, but the row must never confiscate the
	// console over ONE person's lapsed credential — Blocking is server-marked
	// now, and this is exactly the row the 0.7.6 field report was about.
	if awsRow.Blocking {
		t.Error("harness_credential_aws must never be Blocking — it is graded through the CALLER's own session, not the install")
	}

	// The ADMIN'S own status is untouched: their capture agrees with the pin, so
	// the grading is the one it always was.
	if _, _, adminMA := srv.setupHarnessCreds(ctx, sc, awsSSOScope{perUser: true, owner: "admin-sub"}); adminMA.State != modelAccessLive {
		t.Errorf("the admin's own model_access.state = %q, want %q — an agreeing capture must grade exactly as before",
			adminMA.State, modelAccessLive)
	}
}

// negative controls: every shipped behaviour this fix must leave alone

// TestDispatch_UnpinnedRowNeverRefusesAStoredBlob is THE UPGRADE GUARD, and it
// matters more than it looks: the pin is optional on purpose and most estates
// carry none. A comparison that fired on an unpinned row would take Bedrock
// away from every one of them on upgrade — the failure awssso_pin.go's own
// BedrockSSOPinUnenforced doc warns about, in its dispatch form. It is the
// dispatch twin of TestUploadSSOToken_NonARNModelSkipsAccountBinding.
func TestDispatch_UnpinnedRowNeverRefusesAStoredBlob(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "222222222222", "DevPower")

	admitted, llm := dispatchWithScope(t, srv, pinnedPerUserRoster("", ""), pinTestRun(), pinTestMember, false, "")
	if !llm.bedrock.ssoInject {
		t.Fatalf("the stored session did not resolve: %+v", llm.bedrock)
	}
	if !admitted || st.failed {
		t.Fatalf("an UNPINNED row refused a stored capture (admitted=%v failed=%v) — "+
			"every unpinned deployment would lose Bedrock on upgrade", admitted, st.failed)
	}
}

// TestDispatch_SharedRowNeverRefusesAStoredBlob: perUserLoginRow is false for a
// `shared` row (harnesscred.go), and a shared estate has no per-person identity
// to contradict — the pin fields are refused on such a row at the save door in
// the first place (TestAgentProviders_PinFieldsRefusedOnSharedRow).
func TestDispatch_SharedRowNeverRefusesAStoredBlob(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, "", "222222222222", "DevPower")
	// A hand-edited JSONB could still hold pin fields on a shared row; the gate
	// must not read them.
	sc := agentRoster(types.AgentProvider{
		ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
		SSOAccountID: "111111111111", SSORoleName: "BedrockRunner",
	})

	admitted, llm := dispatchWithScope(t, srv, sc, pinTestRun(), "", false, "")
	if !llm.bedrock.ssoInject {
		t.Fatalf("the stored session did not resolve: %+v", llm.bedrock)
	}
	if !admitted || st.failed {
		t.Fatalf("a `shared` row refused a stored capture (admitted=%v failed=%v)", admitted, st.failed)
	}
}

// TestDispatch_AgreeingBlobDispatchesUnchanged: the pin and the blob name the
// same pair, so the run is byte-identical to what it was — including the
// generated ~/.aws/config the sandbox actually reads.
func TestDispatch_AgreeingBlobDispatchesUnchanged(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "111111111111", "BedrockRunner")
	sc := pinnedPerUserRoster("111111111111", "BedrockRunner")

	admitted, llm := dispatchWithScope(t, srv, sc, pinTestRun(), pinTestMember, false, "")
	if !admitted || st.failed {
		t.Fatalf("an agreeing capture was refused (admitted=%v failed=%v)", admitted, st.failed)
	}
	cfg := decodeSSOFiles(t, llm.bedrock.env[awsSSOConfigEnvVar])[".aws/config"]
	for _, want := range []string{"sso_account_id = 111111111111", "sso_role_name = BedrockRunner"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("generated ~/.aws/config %q is missing %q", cfg, want)
		}
	}
}

// TestDispatch_BearerAndMountLanesAreUnaffected: the comparison is about the
// captured-SSO lane alone. A run carried by the operator's bearer key or the
// host ~/.aws mount has no stored account/role to compare, and reading the
// roster pin at it would refuse a run that never touched a capture.
func TestDispatch_BearerAndMountLanesAreUnaffected(t *testing.T) {
	h := newHarness(t)
	st := &mechanismGateStore{}
	cfg := bedrockBearerCfg()
	cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
	srv := New(cfg)
	// A `shared` bedrock_sso row: the coarse fold admits the whole Bedrock chain,
	// which is exactly today's behaviour (TestMechanismSatisfied_PerUserAdmits…).
	sc := agentRoster(types.AgentProvider{ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO})

	admitted, llm := dispatchWithScope(t, srv, sc, pinTestRun(), "", false, "")
	if !llm.bedrock.bearer {
		t.Fatalf("this fixture is meant to resolve the BEARER lane: %+v", llm.bedrock)
	}
	if !admitted || st.failed {
		t.Fatalf("a bearer-lane run was refused (admitted=%v failed=%v)", admitted, st.failed)
	}
}

// TestDispatch_LegacyOpenModeRefusesNothing: with no roster there is no pin and
// no declaration, and the gate's own contract is that legacy mode refuses
// nothing at all.
func TestDispatch_LegacyOpenModeRefusesNothing(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, "", "222222222222", "DevPower")

	for name, sc := range map[string]types.SiteConfig{
		"no block":       {},
		"rows elsewhere": agentRoster(types.AgentProvider{ID: "my-image", Mechanism: types.AgentMechanismNone}),
	} {
		admitted, _ := dispatchWithScope(t, srv, sc, pinTestRun(), "", false, "")
		if !admitted || st.failed {
			t.Fatalf("%s: legacy open mode refused a run (admitted=%v failed=%v)", name, admitted, st.failed)
		}
	}
}

// TestDispatch_NonModelRunAndLoginRunAreNotGated: gating the credential-CAPTURE
// box would DEADLOCK per_user — signing in again is how a member replaces the
// contradicting blob, and a login run that refuses to launch because the blob
// contradicts the pin can never be repaired. A run that makes no model call
// has no lane at all.
func TestDispatch_NonModelRunAndLoginRunAreNotGated(t *testing.T) {
	sc := pinnedPerUserRoster("111111111111", "BedrockRunner")
	for name, tc := range map[string]struct {
		task     string
		taskMode string
	}{
		"the login box that would replace the blob": {task: harnessLoginTask},
		"an exec run signs no model request":        {taskMode: "exec"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, sec, st, _ := pinDispatchSrv(t)
			putPinnedSSOBlob(t, sec, pinTestMember, "222222222222", "DevPower")
			run := pinTestRun()
			run.Task = tc.task
			admitted, _ := dispatchWithScope(t, srv, sc, run, pinTestMember, false, tc.taskMode)
			if !admitted || st.failed {
				t.Fatalf("an ungated run kind was refused (admitted=%v failed=%v)", admitted, st.failed)
			}
		})
	}
}

// TestDispatch_RefreshedBlobIsTheOneCompared: dispatch is the ONE pass allowed
// to redeem the rotating refresh token, and the comparison must be against the
// blob the run will actually carry — the POST-refresh one. Asserted through the
// generated ~/.aws/config, which is the artifact the sandbox reads.
func TestDispatch_RefreshedBlobIsTheOneCompared(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	putPinnedSSOBlob(t, sec, pinTestMember, "111111111111", "BedrockRunner")
	sc := pinnedPerUserRoster("111111111111", "BedrockRunner")

	admitted, llm := dispatchWithScope(t, srv, sc, pinTestRun(), pinTestMember, false, "")
	if !admitted || st.failed {
		t.Fatalf("the refreshed blob's own pair was refused (admitted=%v failed=%v)", admitted, st.failed)
	}
	cfg := decodeSSOFiles(t, llm.bedrock.env[awsSSOConfigEnvVar])[".aws/config"]
	if !strings.Contains(cfg, "sso_account_id = 111111111111") {
		t.Errorf("the run carries %q — the compared pair must be the one baked into the sandbox", cfg)
	}
}

// the recovery the refusals above depend on (EXISTING behaviour, pinned)

// twoLoginRunStore serves TWO aws-sso login runs, each with its own launch
// stamp — the shape the member's recovery actually has: the contradicting
// capture came from one login run, and the sign-in that repairs it is a NEW
// one, stamped with the CURRENT roster pin.
type twoLoginRunStore struct {
	store.Store
	runs   map[uuid.UUID]types.AgentRun
	events map[uuid.UUID][]types.AuditEvent
}

func (s twoLoginRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	return s.runs[id], nil
}

func (s twoLoginRunStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

func (s twoLoginRunStore) QueryAuditEvents(_ context.Context, id uuid.UUID, _ int) ([]types.AuditEvent, error) {
	return s.events[id], nil
}

// TestUploadSSOToken_NewLoginRunReplacesAContradictingCapture pins the recovery
// path the three refusals above hand the member — and it needs NO code change,
// which is the point of pinning it.
//
// `already_captured` (ssotoken.go) fires only when prev.SourceRunID == this
// run's id: the SAME login sandbox capturing twice. A member who clicks "Sign
// in to AWS" again gets a NEW run, stamped with the roster pin AS IT READS NOW
// (harnesscred.go), and its capture passes bindCaptureToPin and OVERWRITES the
// contradicting blob. Weakening `already_captured` to make "the pin changed"
// re-capturable would instead let a still-live login sandbox overwrite its own
// genuine capture, so it is deliberately untouched — and this test is what says
// so if somebody tries.
func TestUploadSSOToken_NewLoginRunReplacesAContradictingCapture(t *testing.T) {
	h := newHarness(t)
	unpinnedRun, pinnedRun := uuid.New(), uuid.New()
	st := twoLoginRunStore{
		runs: map[uuid.UUID]types.AgentRun{
			unpinnedRun: loginRunFor(unpinnedRun), pinnedRun: loginRunFor(pinnedRun),
		},
		events: map[uuid.UUID][]types.AuditEvent{
			// The launch that produced the contradicting blob: no pin existed yet.
			unpinnedRun: pinnedLoginStarted(unpinnedRun, "", ""),
			// The member's second sign-in, launched AFTER the admin set the pin.
			pinnedRun: pinnedLoginStarted(pinnedRun, "111111111111", "BedrockRunner"),
		},
	}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.BedrockRegion = operatorRegion
	srv := New(cfg)
	h.srv = srv

	stored := func() awsSSOBlob {
		t.Helper()
		var b awsSSOBlob
		raw, ok := sec.m[harnessCredSecretName(awsSSOProvider)]
		if !ok {
			t.Fatal("no aws sso credential is stored")
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("stored blob: %v", err)
		}
		return b
	}

	// 1. The capture that predates the pin lands, exactly as it did.
	if code, body := putSSOToken(t, srv, unpinnedRun, h.mintRunToken(t, unpinnedRun),
		ssoBlobFor("222222222222", "DevPower")); code != 204 {
		t.Fatalf("the unpinned capture = %d, want 204; body=%s", code, body)
	}
	if b := stored(); b.AccountID != "222222222222" {
		t.Fatalf("stored account = %q, want the unpinned capture's", b.AccountID)
	}

	// 2. The member signs in again. A NEW run, the CURRENT pin, and the store
	//    holds the new pair afterwards — recovery, with no server change.
	pinnedTok := h.mintRunToken(t, pinnedRun)
	if code, body := putSSOToken(t, srv, pinnedRun, pinnedTok,
		ssoBlobFor("111111111111", "BedrockRunner")); code != 204 {
		t.Fatalf("the re-sign-in = %d, want 204; body=%s", code, body)
	}
	b := stored()
	if b.AccountID != "111111111111" || b.RoleName != "BedrockRunner" {
		t.Fatalf("stored pair = %s/%s, want the pinned one — the sign-in did not replace the contradicting capture",
			b.AccountID, b.RoleName)
	}
	if b.SourceRunID != pinnedRun.String() {
		t.Errorf("stored source_run_id = %q, want the NEW login run's", b.SourceRunID)
	}

	// 3. The same run capturing twice is STILL refused: this guard is what stops
	//    a still-live login sandbox overwriting its own genuine capture.
	if code, body := putSSOToken(t, srv, pinnedRun, pinnedTok,
		ssoBlobFor("111111111111", "BedrockRunner")); code != 409 {
		t.Fatalf("a second capture from the SAME run = %d, want 409; body=%s", code, body)
	}
}

// TestBedrockBlobPinMismatch is the predicate itself, in the shape of its
// sibling TestBedrockSSOPinUnenforced: every row that must answer "no
// contradiction" is an existing deployment shape the fix must not break, and
// the two that answer "yes" are the defect.
func TestBedrockBlobPinMismatch(t *testing.T) {
	ssoAuth := func(account, role string) bedrockAuth {
		return bedrockAuth{ready: true, ssoInject: true, ssoAccountID: account, ssoRoleName: role}
	}
	pinned := pinnedPerUserRoster("111111111111", "BedrockRunner")
	for name, c := range map[string]struct {
		sc   types.SiteConfig
		auth bedrockAuth
		want bool
	}{
		"a stored account the pin does not allow": {sc: pinned, auth: ssoAuth("222222222222", "BedrockRunner"), want: true},
		"the pinned account's wrong role":         {sc: pinned, auth: ssoAuth("111111111111", "ReadOnly"), want: true},
		"the pinned pair itself":                  {sc: pinned, auth: ssoAuth("111111111111", "BedrockRunner")},
		// The upgrade guards. Each of these is a shipped deployment shape, and a
		// predicate that fired on any of them would take Bedrock away from it.
		"an unpinned per_user row":    {sc: pinnedPerUserRoster("", ""), auth: ssoAuth("222222222222", "DevPower")},
		"a blob carrying no pair yet": {sc: pinned, auth: ssoAuth("", "")},
		"a blob with only a role":     {sc: pinned, auth: ssoAuth("", "DevPower")},
		// R-03: the MIRROR of the row above, and of the two half-pinned rows —
		// both-halves-or-neither on the STORED side too. An account-only pair
		// differs from the pin, so without the guard it returns mismatch=true
		// with a role nobody can name, and every caller composes a sentence
		// that names both halves.
		"a blob with only an account": {sc: pinned, auth: ssoAuth("222222222222", "")},
		"a half-pinned row (account)": {sc: pinnedPerUserRoster("111111111111", ""), auth: ssoAuth("222222222222", "DevPower")},
		"a half-pinned row (role)":    {sc: pinnedPerUserRoster("", "BedrockRunner"), auth: ssoAuth("222222222222", "DevPower")},
		"no roster at all":            {auth: ssoAuth("222222222222", "DevPower")},
		"the bearer lane":             {sc: pinned, auth: bedrockAuth{ready: true, bearer: true}},
		"the host ~/.aws mount":       {sc: pinned, auth: bedrockAuth{ready: true, awsMount: true}},
		"the resident SigV4 fallback": {sc: pinned, auth: bedrockAuth{ready: true}},
		"no Bedrock lane at all":      {sc: pinned},
		"a shared row carrying a pin": {auth: ssoAuth("222222222222", "DevPower"), sc: agentRoster(types.AgentProvider{
			ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
			SSOAccountID: "111111111111", SSORoleName: "BedrockRunner",
		})},
		"a disabled per_user row": {auth: ssoAuth("222222222222", "DevPower"), sc: agentRoster(types.AgentProvider{
			ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, Disabled: true,
			SSOAccountID: "111111111111", SSORoleName: "BedrockRunner",
		})},
	} {
		t.Run(name, func(t *testing.T) {
			stored, pin, got := bedrockBlobPinMismatch(c.sc, c.auth)
			if got != c.want {
				t.Fatalf("bedrockBlobPinMismatch = %v, want %v", got, c.want)
			}
			if !c.want {
				if stored != (awsSSOPin{}) || pin != (awsSSOPin{}) {
					t.Errorf("no mismatch must return zero pairs, got %+v / %+v", stored, pin)
				}
				return
			}
			// A refusal has to be able to name BOTH pairs, so neither may come
			// back empty on the one path that composes a sentence from them.
			if !stored.set() || !pin.set() {
				t.Errorf("a mismatch returned an unnameable pair: stored=%+v pinned=%+v", stored, pin)
			}
		})
	}
}

// TestBedrockProviderCheck_StoredCaptureContradictingThePinWarns is D4: the
// THIRD roster posture on the bedrock_provider row, beside the two 0.7.3
// shipped.
//
// It is the arm docs/OPERATIONS.md now sells to operators ("the setup
// checklist's AWS Bedrock row warns naming both pairs"), and it is the one an
// admin reads at the exact moment the row got LESS informative on its own:
// saving the pin removes the "nothing constrains the account/role" warning
// above, so without this the estate reads more correct than it did while every
// run on the stored session is already refused.
//
// APPEND-NEVER-SUBSTITUTE is asserted, not described: the row is still the only
// place the console names the live region and model, and the pin-vs-model
// posture is a DIFFERENT half of the same question (what the roster pins vs the
// model's account) that must survive this one landing on top of it.
func TestBedrockProviderCheck_StoredCaptureContradictingThePinWarns(t *testing.T) {
	const thirdAccountARN = "arn:aws:bedrock:us-east-1:333333333333:inference-profile/us.anthropic.claude-v1:0"
	pinned := pinnedPerUserRoster("111111111111", "BedrockRunner")

	for name, c := range map[string]struct {
		model string
		// alsoWantModelPosture: the pin-vs-model sentence must ALSO be present,
		// because both postures are true of this deployment at once.
		alsoWantModelPosture bool
	}{
		// The bare cross-region id names no account, so the pin-vs-model posture
		// is silent and this one stands alone.
		"a bare model id": {model: pinDispatchModel},
		// A model ARN in a THIRD account: the roster pins 111111111111, the model
		// lives in 333333333333, and the stored capture is for 222222222222 —
		// three different accounts, two true postures, one row.
		"a model ARN in a third account": {model: thirdAccountARN, alsoWantModelPosture: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := New(Config{
				BedrockRegion: "us-east-1", BedrockModel: c.model,
				Secrets: &memSecrets{m: map[string][]byte{}},
			})
			bedrock := srv.setupBedrock(context.Background(), map[string]bool{}, awsSSOScope{})
			// This caller HAS captured — and the pair they captured is the one the
			// roster no longer allows. setupBedrock reads it off the blob in
			// production; there is no blob in this fixture, so it is set here.
			bedrock.SSOPresent, bedrock.Ready = true, true
			bedrock.SSOAccountID, bedrock.SSORoleName = "222222222222", "DevPower"

			chk, ok := bedrockProviderCheck(bedrock, pinned, true)
			if !ok {
				t.Fatal("a region+model-configured Bedrock row must always surface a check")
			}
			if chk.Status != "warn" {
				t.Errorf("status = %q, want warn — every run on the stored session is refused", chk.Status)
			}
			for _, want := range []string{"222222222222", "DevPower", "111111111111", "BedrockRunner"} {
				if !strings.Contains(chk.Detail, want) {
					t.Errorf("detail = %q, want it to name %q — both pairs or the admin cannot tell which is which", chk.Detail, want)
				}
			}
			if !strings.Contains(chk.Fix, bedrockPinContradictedFix) {
				t.Errorf("fix = %q, want the DRAFT remedy (sign in again; Wardyn never rewrites a stored session)", chk.Fix)
			}
			// APPENDED, never substituted: the plain row (no roster ⇒ no posture)
			// is still the head of this Detail, so the live region and model are
			// not lost.
			plain, _ := bedrockProviderCheck(bedrock, types.SiteConfig{}, true)
			if plain.Status != "ok" {
				t.Fatalf("the plain row = %+v, want ok — this fixture must be a READY deployment or the append proves nothing", plain)
			}
			if !strings.HasPrefix(chk.Detail, plain.Detail) {
				t.Errorf("detail = %q, want it to keep %q and append the posture", chk.Detail, plain.Detail)
			}
			if !strings.Contains(chk.Detail, "us-east-1") {
				t.Errorf("detail = %q, lost the configured region/model sentence", chk.Detail)
			}
			// BOTH POSTURES, when both are true. A fold that substituted instead
			// of appending would drop the sibling, and no single-posture test
			// would have caught it.
			hasModelPosture := strings.Contains(chk.Detail, "333333333333")
			if hasModelPosture != c.alsoWantModelPosture {
				t.Errorf("pin-vs-model posture present = %v, want %v; detail = %q",
					hasModelPosture, c.alsoWantModelPosture, chk.Detail)
			}
			if c.alsoWantModelPosture {
				if !strings.Contains(chk.Detail, fmt.Sprintf(bedrockPinModelAccountDetail, "111111111111", "333333333333")) {
					t.Errorf("detail = %q, want the pin-vs-model sentence verbatim", chk.Detail)
				}
				if !strings.Contains(chk.Detail, fmt.Sprintf(bedrockPinContradictedDetail,
					"222222222222", "DevPower", "111111111111", "BedrockRunner")) {
					t.Errorf("detail = %q, want the pin-vs-capture sentence verbatim", chk.Detail)
				}
				if !strings.Contains(chk.Fix, bedrockPinModelAccountFix) {
					t.Errorf("fix = %q, want the sibling posture's remedy kept too", chk.Fix)
				}
			}

			// An AGREEING capture says nothing: the posture is a contradiction, not
			// a report that a session exists.
			agreeing := bedrock
			agreeing.SSOAccountID, agreeing.SSORoleName = "111111111111", "BedrockRunner"
			quiet, _ := bedrockProviderCheck(agreeing, pinned, true)
			if strings.Contains(quiet.Fix, bedrockPinContradictedFix) ||
				strings.Contains(quiet.Detail, "no longer allows") {
				t.Errorf("an agreeing capture raised the posture: %q", quiet.Detail)
			}
			// And an UNREADABLE roster asserts no posture at all, like its siblings.
			if blind, _ := bedrockProviderCheck(bedrock, pinned, false); strings.Contains(blind.Detail, "no longer allows") {
				t.Errorf("an unreadable roster asserted a posture: %q", blind.Detail)
			}
		})
	}
}
