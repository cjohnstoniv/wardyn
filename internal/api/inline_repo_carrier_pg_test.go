// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const carrierAHost, carrierBHost = "carrier-a.example", "carrier-b.example"

// inlineCarrierFixture: two operator workspaces, A and B, both carry
// govWorkspaceRepo, and each requires its own host (operator_set). bOwner, when
// set, makes B that member's own workspace instead.
type inlineCarrierFixture struct {
	h    *harness
	a, b types.Workspace
}

func newInlineCarrierFixture(t *testing.T, aNewer bool, bOwner string) inlineCarrierFixture {
	t.Helper()
	h, _ := newRunOwnerPGHarness(t)
	h.srv.cfg.Runner = &fakeRunner{}
	h.srv.cfg.Broker = h.broker
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	put := func(name, owner, host string, at time.Time) types.Workspace {
		t.Helper()
		ws, err := h.srv.cfg.Store.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: name, OwnedBy: owner, Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}},
			Requirements: map[string]types.WorkspaceRequirement{
				"egress:" + host: {Level: "required", Provenance: "operator_set"},
			},
			CreatedAt: at, UpdatedAt: base,
		})
		if err != nil {
			t.Fatalf("CreateWorkspace %s: %v", name, err)
		}
		return ws
	}
	aAt, bAt := base.Add(time.Minute), base
	if !aNewer {
		aAt, bAt = base, base.Add(time.Minute)
	}
	return inlineCarrierFixture{h: h, a: put("carrier-a", "", carrierAHost, aAt), b: put("carrier-b", bOwner, carrierBHost, bAt)}
}

func (fx inlineCarrierFixture) enforce(t *testing.T) {
	t.Helper()
	if _, err := fx.h.srv.cfg.Store.PutCapabilityEnforcement(context.Background(), map[string]bool{capWorkspace: true}); err != nil {
		t.Fatal(err)
	}
}

func (fx inlineCarrierFixture) grant(t *testing.T, ws types.Workspace, effect types.CapabilityEffect) uuid.UUID {
	t.Helper()
	g, err := fx.h.srv.cfg.Store.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{ID: uuid.New(),
		SubjectType: types.CapabilitySubjectUser, Subject: "alice", Capability: capWorkspace, Value: ws.ID.String(), Effect: effect})
	if err != nil {
		t.Fatal(err)
	}
	return g.ID
}

// carrierRunBody is alice's run naming govWorkspaceRepo inline, attaching attach when set.
func carrierRunBody(attach *uuid.UUID) string {
	body := `{"agent":"claude-code","task":"t",`
	if attach != nil {
		body += `"workspace_id":"` + attach.String() + `",`
	}
	return body + `"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
		`"workspace_repos":[{"repo":"` + govWorkspaceRepo + `"}]}}`
}

// launch posts carrierRunBody(attach) and answers the response, the run row once
// dispatch has settled, and the allowlist dispatch handed the proxy.
func (fx inlineCarrierFixture) launch(t *testing.T, attach *uuid.UUID) (int, string, types.AgentRun, []string) {
	t.Helper()
	return fx.post(t, carrierRunBody(attach))
}

// post creates alice's run from body, as launch does.
func (fx inlineCarrierFixture) post(t *testing.T, body string) (int, string, types.AgentRun, []string) {
	t.Helper()
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	w := doSSO(t, fx.h.srv, http.MethodPost, "/api/v1/runs", alice, body)
	if w.Code != http.StatusCreated {
		return w.Code, w.Body.String(), types.AgentRun{}, nil
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ctx := context.Background()
	var run types.AgentRun
	waitFor(t, "run to settle", func() bool {
		got, err := fx.h.srv.cfg.Store.GetRun(ctx, created.ID)
		run = got
		return err == nil && got.State != types.RunPending && got.State != types.RunStarting
	})
	fr := fx.h.srv.cfg.Runner.(*fakeRunner)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return w.Code, w.Body.String(), run, slices.Clone(fr.lastSpec.ProxyConfig.Policy.AllowedDomains)
}

// usesA holds an admitted run to carrier A alone: its row names A only, A's
// requirement reached the proxy and B's operator_set requirement did not.
func (fx inlineCarrierFixture) usesA(t *testing.T, code int, body string, run types.AgentRun, allowed []string) {
	t.Helper()
	if code != http.StatusCreated {
		t.Fatalf("create run: %d %s, want 201", code, body)
	}
	if !slices.Equal(run.WorkspaceIDs, []uuid.UUID{fx.a.ID}) {
		t.Errorf("run workspace_ids = %v, want only carrier A %s (B is %s)", run.WorkspaceIDs, fx.a.ID, fx.b.ID)
	}
	if !slices.Contains(allowed, carrierAHost) {
		t.Errorf("dispatched allowlist %v lacks A's requirement %s", allowed, carrierAHost)
	}
	if slices.Contains(allowed, carrierBHost) {
		t.Errorf("dispatched allowlist %v holds B's operator_set requirement %s", allowed, carrierBHost)
	}
}

// TestPG_InlineRepoCarrier_BothCreationOrders: capWorkspace enforced, alice
// granted A and holding no row on B. Admission admits the inline repo by A in
// either creation order, and the run uses A in either.
func TestPG_InlineRepoCarrier_BothCreationOrders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		aNewer bool
	}{{"A newer", true}, {"B newer", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInlineCarrierFixture(t, tc.aNewer, "")
			fx.enforce(t)
			fx.grant(t, fx.a, types.CapabilityAllow)
			code, body, run, allowed := fx.launch(t, nil)
			fx.usesA(t, code, body, run, allowed)
		})
	}
}

// TestPG_InlineRepoCarrier_AllowADenyB: enforcement off, an allow on A and a
// deny on B. The run uses A in either creation order, never the carrier alice
// is denied.
func TestPG_InlineRepoCarrier_AllowADenyB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		aNewer bool
	}{{"A newer", true}, {"B newer", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInlineCarrierFixture(t, tc.aNewer, "")
			fx.grant(t, fx.a, types.CapabilityAllow)
			fx.grant(t, fx.b, types.CapabilityDeny)
			code, body, run, allowed := fx.launch(t, nil)
			fx.usesA(t, code, body, run, allowed)
		})
	}
}

// TestPG_InlineRepoCarrier_UnenforcedControl (compatibility control):
// enforcement off and no rows, so both carriers are granted. In either creation
// order the run uses the oldest usable carrier, the one the index held before
// admission's choice was carried: nothing changes where every carrier is
// granted.
func TestPG_InlineRepoCarrier_UnenforcedControl(t *testing.T) {
	for _, tc := range []struct {
		name   string
		aNewer bool
	}{{"A newer", true}, {"B newer", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInlineCarrierFixture(t, tc.aNewer, "")
			oldest, oldestHost, newestHost := fx.b, carrierBHost, carrierAHost
			if !tc.aNewer {
				oldest, oldestHost, newestHost = fx.a, carrierAHost, carrierBHost
			}
			code, body, run, allowed := fx.launch(t, nil)
			if code != http.StatusCreated {
				t.Fatalf("create run: %d %s, want 201", code, body)
			}
			if !slices.Equal(run.WorkspaceIDs, []uuid.UUID{oldest.ID}) {
				t.Errorf("run workspace_ids = %v, want the oldest carrier %s %s", run.WorkspaceIDs, oldest.Name, oldest.ID)
			}
			if !slices.Contains(allowed, oldestHost) || slices.Contains(allowed, newestHost) {
				t.Errorf("dispatched allowlist %v, want %s and not %s", allowed, oldestHost, newestHost)
			}
		})
	}
}

// TestPG_InlineRepoCarrier_GrantRemovedAfterAdmission: A newer, enforced, the
// only grant on A. The grant is deleted right after the policy step admitted
// the repo by A and before the workspaces step runs. The run uses A, the
// carrier admission recorded, and never the ungranted B.
func TestPG_InlineRepoCarrier_GrantRemovedAfterAdmission(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "")
	fx.enforce(t)
	grantA := fx.grant(t, fx.a, types.CapabilityAllow)
	i := slices.IndexFunc(runFoldSteps, func(st runFoldStep) bool {
		return strings.HasSuffix(runtime.FuncForPC(reflect.ValueOf(st.run).Pointer()).Name(), ".stepPolicy")
	})
	if i < 0 {
		t.Fatal("stepPolicy is not a fold step")
	}
	steps := runFoldSteps
	revoke := runFoldStep{modes: foldAll, run: func(s *Server, f *runFold, rec *foldRecorder) bool {
		if _, err := s.cfg.Store.DeleteCapabilityGrant(context.Background(), grantA); err != nil {
			t.Errorf("delete A's grant: %v", err)
		}
		return true
	}}
	runFoldSteps = slices.Concat(steps[:i+1], []runFoldStep{revoke}, steps[i+1:])
	t.Cleanup(func() { runFoldSteps = steps })
	code, body, run, allowed := fx.launch(t, nil)
	runFoldSteps = steps
	if code == http.StatusCreated {
		fx.usesA(t, code, body, run, allowed)
	} else if strings.Contains(body, fx.b.ID.String()) {
		t.Errorf("refused naming B: %d %s", code, body)
	}
}

// TestPG_InlineRepoCarrier_DefaultPolicyKeepsOperatorCarrier (compatibility
// control): the default policy names the repo, the member edits nothing, B is
// the older carrier and only A is granted. No member narrowing judged the
// repo, so the run keeps the carrier the index holds, B, with B's requirement,
// exactly as before.
func TestPG_InlineRepoCarrier_DefaultPolicyKeepsOperatorCarrier(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "")
	fx.h.srv.cfg.DefaultPolicy.WorkspaceRepos = []types.WorkspaceRepo{{Repo: govWorkspaceRepo}}
	// The harness ceiling's bare api_key entry bounds members' proposed grants;
	// as the run's own spec it is an unscoped grant, so this default drops it.
	fx.h.srv.cfg.DefaultPolicy.EligibleGrants = nil
	fx.enforce(t)
	fx.grant(t, fx.a, types.CapabilityAllow)
	code, body, run, allowed := fx.post(t, `{"agent":"claude-code","task":"t"}`)
	if code != http.StatusCreated {
		t.Fatalf("create run: %d %s, want 201", code, body)
	}
	if !slices.Equal(run.WorkspaceIDs, []uuid.UUID{fx.b.ID}) {
		t.Errorf("run workspace_ids = %v, want the operator carrier B %s (A is %s)", run.WorkspaceIDs, fx.b.ID, fx.a.ID)
	}
	if !slices.Contains(allowed, carrierBHost) || slices.Contains(allowed, carrierAHost) {
		t.Errorf("dispatched allowlist %v, want B's requirement %s and not A's", allowed, carrierBHost)
	}
}

// workspaceRows is the workspace ids a setup checklist names.
func workspaceRows(items []SetupItem) []uuid.UUID {
	var out []uuid.UUID
	for _, it := range items {
		if id, ok := strings.CutPrefix(it.ID, "workspace:"); ok && it.Kind == "workspace" {
			out = append(out, uuid.MustParse(id))
		}
	}
	return out
}

// TestPG_InlineRepoCarrier_PreflightMatchesRun: allow on A, deny on B, A newer.
// Preflight's checklist names the workspace the run then records, A, and none
// of B's requirement hosts.
func TestPG_InlineRepoCarrier_PreflightMatchesRun(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "")
	fx.grant(t, fx.a, types.CapabilityAllow)
	fx.grant(t, fx.b, types.CapabilityDeny)
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	w := doSSO(t, fx.h.srv, http.MethodPost, "/api/v1/runs/preflight", alice, carrierRunBody(nil))
	if w.Code != http.StatusOK {
		t.Fatalf("preflight: %d %s, want 200", w.Code, w.Body.String())
	}
	var pre preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pre); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	code, body, run, _ := fx.launch(t, nil)
	if code != http.StatusCreated {
		t.Fatalf("create run: %d %s, want 201", code, body)
	}
	if got := workspaceRows(pre.SetupItems); !slices.Equal(got, run.WorkspaceIDs) || !slices.Equal(got, []uuid.UUID{fx.a.ID}) {
		t.Errorf("preflight workspace rows = %v, run workspace_ids = %v, want both only A %s (B is %s)", got, run.WorkspaceIDs, fx.a.ID, fx.b.ID)
	}
	if strings.Contains(w.Body.String(), carrierBHost) {
		t.Errorf("preflight names B's requirement host %s: %s", carrierBHost, w.Body.String())
	}
}

// TestPG_InlineRepoCarrier_SetupChecklist: the composer's setup checklist for
// alice (referencedWorkspaces) names the carrier her run uses, A, not the
// denied B.
func TestPG_InlineRepoCarrier_SetupChecklist(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "")
	fx.grant(t, fx.a, types.CapabilityAllow)
	fx.grant(t, fx.b, types.CapabilityDeny)
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceRepos: []types.WorkspaceRepo{{Repo: govWorkspaceRepo}}}
	items := fx.h.srv.deriveSetupItems(memberCtxFor("alice"), "alice", composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}, spec, nil, nil)
	if got := workspaceRows(items); !slices.Equal(got, []uuid.UUID{fx.a.ID}) {
		t.Errorf("checklist workspace rows = %v, want only A %s (B is %s)", got, fx.a.ID, fx.b.ID)
	}
	if raw := string(mustJSON(items)); strings.Contains(raw, carrierBHost) {
		t.Errorf("checklist names B's requirement host %s: %s", carrierBHost, raw)
	}
}

// TestPG_InlineRepoCarrier_EnforcedNoGrantOnB: enforced, and B is alice's own
// workspace with no grant row: admission's default deny leaves A, and the run
// uses A, not the ungranted carrier she owns.
func TestPG_InlineRepoCarrier_EnforcedNoGrantOnB(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "alice")
	fx.enforce(t)
	fx.grant(t, fx.a, types.CapabilityAllow)
	code, body, run, allowed := fx.launch(t, nil)
	fx.usesA(t, code, body, run, allowed)
}

// TestPG_InlineRepoCarrier_AttachedAParity: attaching A and naming the repo
// inline holds the run to A (the attached path), and the inline-only run
// matches it: the same workspace ids and the same allowlist.
func TestPG_InlineRepoCarrier_AttachedAParity(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "")
	fx.enforce(t)
	fx.grant(t, fx.a, types.CapabilityAllow)
	code, body, attached, attachedAllowed := fx.launch(t, &fx.a.ID)
	fx.usesA(t, code, body, attached, attachedAllowed)
	code, body, inline, inlineAllowed := fx.launch(t, nil)
	if code != http.StatusCreated {
		t.Fatalf("inline-only run: %d %s, want 201", code, body)
	}
	if !slices.Equal(inline.WorkspaceIDs, attached.WorkspaceIDs) {
		t.Errorf("inline-only workspace_ids = %v, attached-A = %v, want equal", inline.WorkspaceIDs, attached.WorkspaceIDs)
	}
	slices.Sort(inlineAllowed)
	slices.Sort(attachedAllowed)
	if !slices.Equal(inlineAllowed, attachedAllowed) {
		t.Errorf("inline-only allowlist = %v, attached-A = %v, want equal", inlineAllowed, attachedAllowed)
	}
}

// TestPG_InlineRepoCarrier_NoAuthorizedCarrier: enforced with no grant on
// either carrier refuses the run as before (403, the workspace capability);
// granting A then admits it by A and the run uses A.
func TestPG_InlineRepoCarrier_NoAuthorizedCarrier(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "")
	fx.enforce(t)
	if code, body, _, _ := fx.launch(t, nil); code != http.StatusForbidden || !strings.Contains(body, "workspace") {
		t.Fatalf("no carrier granted: %d %s, want the 403 workspace-capability refusal", code, body)
	}
	fx.grant(t, fx.a, types.CapabilityAllow)
	code, body, run, allowed := fx.launch(t, nil)
	fx.usesA(t, code, body, run, allowed)
}
