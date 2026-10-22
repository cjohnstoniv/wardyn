// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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

func (fx inlineCarrierFixture) grant(t *testing.T, ws types.Workspace, effect types.CapabilityEffect) {
	t.Helper()
	if _, err := fx.h.srv.cfg.Store.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{ID: uuid.New(),
		SubjectType: types.CapabilitySubjectUser, Subject: "alice", Capability: capWorkspace, Value: ws.ID.String(), Effect: effect}); err != nil {
		t.Fatal(err)
	}
}

// launch posts alice's run naming govWorkspaceRepo inline (attaching attach
// when set) and answers the response, the run row once dispatch has settled,
// and the allowlist dispatch handed the proxy.
func (fx inlineCarrierFixture) launch(t *testing.T, attach *uuid.UUID) (int, string, types.AgentRun, []string) {
	t.Helper()
	body := `{"agent":"claude-code","task":"t",`
	if attach != nil {
		body += `"workspace_id":"` + attach.String() + `",`
	}
	body += `"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
		`"workspace_repos":[{"repo":"` + govWorkspaceRepo + `"}]}}`
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
// deny on B. The run uses A, never the carrier alice is denied.
func TestPG_InlineRepoCarrier_AllowADenyB(t *testing.T) {
	fx := newInlineCarrierFixture(t, true, "")
	fx.grant(t, fx.a, types.CapabilityAllow)
	fx.grant(t, fx.b, types.CapabilityDeny)
	code, body, run, allowed := fx.launch(t, nil)
	fx.usesA(t, code, body, run, allowed)
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
