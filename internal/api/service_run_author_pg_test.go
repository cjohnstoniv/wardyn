// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestServiceRunAuthor_SecondHuman: a service identity's run records the sponsor at launch as its human author,
// unchanged by later sponsor changes. Under each second-human switch, the gate and its read-only mirror refuse
// that author and the current sponsor, accept an independent person, and refuse everyone on a service run with
// no recorded author.
func TestServiceRunAuthor_SecondHuman(t *testing.T) {
	const independent = "independent-sub"
	for _, sw := range []string{envEgressSecondHuman, envCapabilitySecondHuman} {
		t.Run(sw, func(t *testing.T) {
			e := newSvcEnv(t)
			e.mustCreate(t, svcName, svcSponsor)
			ageSponsorships(t, e.pool) // else a stall under load makes the mint's authority time precede sponsor_since
			t.Setenv(sw, "1")
			ap := types.ApprovalRequest{Kind: types.ApprovalEgressDomain}
			if sw == envCapabilitySecondHuman {
				grant := uuid.New()
				scope, _ := json.Marshal(adoCapabilityScope{Lane: adoApprovalLane, ProviderID: "ado-row-1", Org: "contoso",
					GrantID: grant, Capability: "pr", Tool: "Azure DevOps", Cmd: "x"})
				ap = types.ApprovalRequest{Kind: types.ApprovalToolCall, GrantID: &grant, RequestedScope: scope}
			}
			if name, on := secondHumanGate(ap); name != sw || !on {
				t.Fatalf("fixture approval is governed by %q (on=%v), want %q", name, on, sw)
			}
			run, _ := e.serviceRunRunning(t, e.mustMint(t, e.sponsor, svcName))
			if run.HumanAuthor != svcSponsor {
				t.Fatalf("service run's recorded author %q, want the sponsor at launch %q", run.HumanAuthor, svcSponsor)
			}
			transfer := func(to string) {
				t.Helper()
				if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+to+`"}`); w.Code != http.StatusOK {
					t.Fatalf("sponsor change to %s: %d %s", to, w.Code, w.Body.String())
				}
			}
			reread := func() types.AgentRun {
				t.Helper()
				r, err := e.st.GetRun(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			// allowed asks the gate and the read-only mirror the same question and requires they agree.
			allowed := func(t *testing.T, sub string, r types.AgentRun) bool {
				t.Helper()
				ctx := withHumanIdentity(context.Background(), sub, "", oidc.RoleAdmin, types.UserTypeStandard, []string{}, false)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(ctx)
				w := httptest.NewRecorder()
				_, gate := e.h.srv.requireSecondHuman(w, req, uuid.New(), ap, r, true, true)
				mirror := e.h.srv.secondHumanAllows(req, ap, r)
				if gate != mirror {
					t.Errorf("%s: gate allows=%v (%d %s), mirror allows=%v; they must agree", sub, gate, w.Code, w.Body.String(), mirror)
				}
				if !gate && w.Code != http.StatusForbidden {
					t.Errorf("%s: gate refused with %d %s, want 403", sub, w.Code, w.Body.String())
				}
				return gate && mirror
			}
			check := func(t *testing.T, r types.AgentRun, want map[string]bool) {
				t.Helper()
				for _, sub := range []string{svcSponsor, svcSponsor2, independent} {
					if got := allowed(t, sub, r); got != want[sub] {
						t.Errorf("%s deciding: allowed=%v, want %v", sub, got, want[sub])
					}
				}
			}

			// Characterization: before any transfer the launching sponsor is refused and anyone else accepted.
			t.Run("launching_sponsor_unchanged", func(t *testing.T) {
				check(t, run, map[string]bool{svcSponsor: false, svcSponsor2: true, independent: true})
			})
			transfer(svcSponsor2)
			// Defect: the launching person stays the author after handing sponsorship to another person.
			// Characterization beside it: the current sponsor is refused and an independent person accepted.
			t.Run("after_transfer", func(t *testing.T) {
				r := reread()
				if r.HumanAuthor != svcSponsor {
					t.Fatalf("recorded author after a sponsor change %q, want %q", r.HumanAuthor, svcSponsor)
				}
				check(t, r, map[string]bool{svcSponsor: false, svcSponsor2: false, independent: true})
			})
			transfer(svcSponsor)
			// Characterization: sponsorship handed back, the launching person is refused again (author and current
			// sponsor), the intermediate sponsor is now a second human.
			t.Run("after_transfer_back", func(t *testing.T) {
				check(t, reread(), map[string]bool{svcSponsor: false, svcSponsor2: true, independent: true})
			})
			// Defect: a service run with no recorded author (created before the column) has no verifiable author,
			// so nobody passes, the independent person included.
			t.Run("no_recorded_author", func(t *testing.T) {
				if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET human_author = '' WHERE id = $1`, run.ID); err != nil {
					t.Fatal(err)
				}
				check(t, reread(), map[string]bool{svcSponsor: false, svcSponsor2: false, independent: false})
			})
		})
	}
}

// TestServiceRunAuthor_PersonRunRecordsCreator: a person's run records its creator as the author, and the
// second-human rule on it is unchanged: the creator is refused, another person accepted, with or without the
// recorded author.
func TestServiceRunAuthor_PersonRunRecordsCreator(t *testing.T) {
	e := newSvcEnv(t)
	t.Setenv(envEgressSecondHuman, "1")
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", e.sponsor, `{"agent":"claude-code","task":"t","policy_id":"`+e.plainPolicy(t, "p-"+uuid.NewString())+`"}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("person run create: %d %s", w.Code, w.Body.String())
	}
	run, err := e.st.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HumanAuthor != svcSponsor {
		t.Fatalf("person run's recorded author %q, want its creator %q", run.HumanAuthor, svcSponsor)
	}
	ap := types.ApprovalRequest{Kind: types.ApprovalEgressDomain}
	for _, r := range []types.AgentRun{run, {ID: run.ID, CreatedBy: run.CreatedBy, CreatedAt: time.Now()}} {
		for sub, want := range map[string]bool{svcSponsor: false, "independent-sub": true} {
			ctx := withHumanIdentity(context.Background(), sub, "", oidc.RoleAdmin, types.UserTypeStandard, []string{}, false)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			_, gate := e.h.srv.requireSecondHuman(rec, req, uuid.New(), ap, r, true, true)
			if mirror := e.h.srv.secondHumanAllows(req, ap, r); gate != want || mirror != want {
				t.Errorf("%s on a person run (author %q): gate=%v mirror=%v, want %v", sub, r.HumanAuthor, gate, mirror, want)
			}
		}
	}
}
