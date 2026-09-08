// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F153 residue. The first fix added recordCeilingLimits, which reads the two
// named limits — and it CANNOT FIRE on the route the finding names, so that
// route's behaviour was byte-for-byte unchanged:
//
//	POST /workspaces/{id}/record is mounted operatorOnly (routes.go);
//	requireOperator gates on s.isOperator; resolveEffectiveCeiling
//	short-circuits to a nil-Profile deployment ceiling on that SAME predicate;
//	and recordCeilingLimits returns nil at its first line for a nil Profile.
//
// So every principal who can reach the route arrives unwalled, and the guard's
// own comment rested on a false premise ("a SECURITY-tier route, so a security
// admin can open one" — a security admin gets 403 at the door).
//
// The residue is closed by binding the LANE rather than two of its limits: this
// lane skips the member clamp and opens an interactive, attachable, allow-all,
// operator-credentialed sandbox, so a principal with ANY assigned profile may
// not use it at all. That is the strongest form of the finding's `expected`,
// and it is what makes the route's tier and its governance story one fact
// instead of two that can drift.

// f153RecordSrv drives launchRecordRun over a store that hands `profile` to
// whoever asks, with `active` runs already going.
func f153RecordSrv(t *testing.T, profile *types.GovernanceProfile, active int) (*Server, *fakeRunner) {
	t.Helper()
	h := newHarness(t)
	ws := types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}
	fr := &fakeRunner{}
	cfg := baseTestConfig(h, quotaRecordStore{
		ceilingRecordStore: ceilingRecordStore{recordLLMModeStore: newRecordLLMModeStore(ws), profile: profile},
		active:             active,
	})
	cfg.Runner = fr
	cfg.Broker = h.broker
	return New(cfg), fr
}

// f153ScanStore is the shipped scan-lane double plus the quota read the Limits
// axis makes.
type f153ScanStore struct {
	*scanLaneStore
	active int
}

func (s *f153ScanStore) CountActiveRunsBy(context.Context, string) (int, error) { return s.active, nil }

// f153Scan drives the real launchSourceScanRun for a member with `active` runs
// already going, under `profile`.
func f153Scan(t *testing.T, profile *types.GovernanceProfile, active int) (*Server, error) {
	t.Helper()
	cs := &capStore{}
	if profile != nil {
		cs = assignedStore(profile)
	}
	srv, st, _ := govEscapeFixture(t, cs)
	srv.cfg.Runner = &fakeRunner{}
	srv.cfg.Store = &f153ScanStore{scanLaneStore: &scanLaneStore{govEscapeStore: st}, active: active}
	src := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: govWorkspaceRepo}
	_, err := srv.launchSourceScanRun(govMemberCtx([]string{"eng"}, false), "bob@corp.example", src)
	return srv, err
}

// TestF153_ScanLaneIsBoundByTheRunQuota is the reachable half of the residue.
//
// POST /workspaces/{id}/scan is mounted on the MEMBER group and handleScanWorkspace
// authorizes owner-or-admin, so a WALLED MEMBER genuinely reaches this lane —
// unlike the record route the finding named, which is operatorOnly and therefore
// answers a walled principal 403 at the door. The lane creates a run FOR that
// member and read no limit at all: someone sitting at their max_concurrent_runs
// cap could keep spawning scans, and each one carries a git-broker grant.
func TestF153_ScanLaneIsBoundByTheRunQuota(t *testing.T) {
	p := govProfile("two-at-a-time")
	p.Limits = types.GovernanceLimits{MaxConcurrentRuns: 2}

	_, err := f153Scan(t, p, 2)
	if !errors.Is(err, errRecordCeilingLimit) {
		t.Fatalf("a scan launched at the principal's run cap: err = %v, want the quota refusal — "+
			"max_concurrent_runs bound their POST /runs and not this lane, which creates a run for the same principal", err)
	}
	if !strings.Contains(err.Error(), "too many runs at once (max 2)") {
		t.Errorf("the refusal did not name the limit that stopped them: %v", err)
	}

	// UNDER the cap the same profile scans normally — the comparison is real,
	// not a blanket refusal of assigned principals.
	if _, err := f153Scan(t, p, 1); err != nil {
		t.Fatalf("under the cap: err = %v, want a normal scan launch", err)
	}
}

// TestF153_ScanLaneIgnoresDenyInteractive is the other half of the per-lane
// decision: whether a lane opens an attachable session is STRUCTURAL, and a scan
// run never is. Binding deny_interactive here would refuse a walled member's
// workspace scan for a terminal nobody can open.
func TestF153_ScanLaneIgnoresDenyInteractive(t *testing.T) {
	p := govProfile("no-terminals") // govProfile carries DenyInteractive:true
	p.Limits = types.GovernanceLimits{DenyInteractive: true}
	if _, err := f153Scan(t, p, 0); err != nil {
		t.Fatalf("deny_interactive refused a SCAN: %v — a scan run is server-authored and unattachable", err)
	}
}

// TestF153_UnwalledPrincipalsThreadNothing is the counterfactual the guard must
// not be satisfied by refusing everyone: no assignment ⇒ Record Mode unchanged.
func TestF153_UnwalledPrincipalsThreadNothing(t *testing.T) {
	srv, fr := f153RecordSrv(t, nil, 0)
	if _, _, err := srv.launchRecordRun(govMemberCtx(nil, false),
		"unwalled@corp.example", types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false); err != nil {
		t.Fatalf("launchRecordRun with no assigned profile = %v, want a launch — the moat workflow must be byte-for-byte unchanged", err)
	}
	if fr.createCalls != 1 {
		t.Errorf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
}

// TestF153_RecordRouteStaysOnTheOperatorTier pins the structural half the
// residue proved and the shipped comment denied. The finding's stated `actual`
// ("a security_admin gets the session") is not reachable BECAUSE of this
// mounting — if the route ever moves to securityOps, the guard above becomes
// the only thing standing between that tier and an allow-all credentialed
// sandbox, and this test is where that decision gets made rather than inherited.
func TestF153_RecordRouteStaysOnTheOperatorTier(t *testing.T) {
	srv, st := newTopologyWorkspaceServer(t, "")
	path := "/api/v1/workspaces/" + st.ws.ID.String() + "/record"
	for _, tc := range []struct {
		name    string
		session *http.Cookie
	}{
		{"security admin", ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)},
		{"plain member", ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodPost, path, tc.session, `{"name":"build & test"}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("POST .../record as a %s = %d, want the operator tier's constant 403: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}
