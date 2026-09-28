// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	errReplaceFailed = errors.Join(runner.ErrProxyReplaceFailed, errors.New("docker: start proxy: boom"))
	errStartFailed   = errors.New("docker: start agent: boom")
)

// Extend + Revive during an ended run's files grace (long-holds design rev 4
// §0/§2.1/§2.3/§6; #1061). A run its own lease ended is kept for
// EndedRunGrace with its agent and proxy stopped. Inside that grace its owner
// (or a super admin, with the owner's authority) may extend it and then
// revive it; the grace itself stays counted from when it ended.

// newEndedFixture is newOwnerFixture's run ended by the lease for real: its
// end passed, a sweep marked it ended at the fixture's clock and stopped its
// agent and proxy. Its runner can start the agent again.
func newEndedFixture(t *testing.T) (*reviveFixture, *startingRunner) {
	t.Helper()
	f, _ := newOwnerFixture(t)
	end := f.now.Add(-time.Minute)
	f.st.run.EndsAt = &end
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	f.ls.lapsed = false
	sr := &startingRunner{reviveRunner: f.rr}
	f.srv.cfg.Runner = sr
	f.sweep(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || !lostAt.Equal(f.now) || reason != types.LostEnded {
		t.Fatalf("fixture: lost = %v %q, want ended at %v", lostAt, reason, f.now)
	}
	f.run = f.st.run
	return f, sr
}

func (f *reviveFixture) ownerCookie(t *testing.T) *http.Cookie {
	return ssoSession(t, f.run.CreatedBy, ownerEmail, oidc.RoleUser)
}

// extendAs PATCHes the run's end as its owner, or as the admin token.
func (f *reviveFixture) extendAs(t *testing.T, owner bool, to time.Time) (int, string) {
	t.Helper()
	path := "/api/v1/runs/" + f.run.ID.String()
	if !owner {
		w := do(t, f.srv, http.MethodPatch, path, adminToken, endsAtBody(to))
		return w.Code, w.Body.String()
	}
	w := doSSO(t, f.srv, http.MethodPatch, path, f.ownerCookie(t), endsAtBody(to))
	return w.Code, w.Body.String()
}

func (f *reviveFixture) storedEnd() *time.Time {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	return f.st.run.EndsAt
}

// endedAt is when the fixture's run ended: the anchor of its files grace.
func (f *reviveFixture) endedAt() time.Time { return *f.run.LostAt }

// assertStillEnded: the run is kept exactly as it ended, with no new proxy
// and no agent started.
func (f *reviveFixture) assertStillEnded(t *testing.T, sr *startingRunner) {
	t.Helper()
	lostAt, reason := f.st.lost()
	if f.st.State() != types.RunRunning || lostAt == nil || !lostAt.Equal(f.endedAt()) || reason != types.LostEnded {
		t.Errorf("state %s, lost %v %q; want still RUNNING and ended at %v", f.st.State(), lostAt, reason, f.endedAt())
	}
	if len(f.rr.replaced) != 0 || len(sr.starts()) != 0 {
		t.Errorf("proxies replaced %d, agent starts %v; want none", len(f.rr.replaced), sr.starts())
	}
}

// TestEndedRun_ExtendThenReviveWithinFilesGrace: inside the grace, a revive
// first asks for an extension; the owner extends, the run stays ended and
// stopped (a sweep only re-asserts that), and the revive then brings it back
// under a new proxy with its agent started, audited as from "ended".
func TestEndedRun_ExtendThenReviveWithinFilesGrace(t *testing.T) {
	f, sr := newEndedFixture(t)
	f.now = f.now.Add(3 * 24 * time.Hour)

	if code, body := f.reviveAs(t, true); code != http.StatusConflict || !strings.Contains(body, "extend it first") {
		t.Fatalf("revive before extending = %d %s, want 409 asking for an extension", code, body)
	}
	later := f.now.Add(48 * time.Hour)
	if code, body := f.extendAs(t, true, later); code != http.StatusOK {
		t.Fatalf("extend inside the grace = %d %s, want 200", code, body)
	}
	if end := f.storedEnd(); end == nil || !end.Equal(later) {
		t.Fatalf("stored end = %v, want %v", end, later)
	}
	f.sweep(t)
	f.assertStillEnded(t, sr)
	if ev := f.audit.eventsFor(f.run.ID, "run.end.set"); len(ev) != 1 || ev[0].Outcome != "success" {
		t.Fatalf("run.end.set events = %+v, want one success", ev)
	}

	f.run = f.st.run
	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive after extending = %d %s, want 200", code, body)
	}
	if got := sr.starts(); !slices.Equal(got, []int{1}) {
		t.Fatalf("agent starts, by proxies replaced before each = %v; want one, after the new proxy", got)
	}
	cfg := f.newConfig(t)
	if cfg.RunToken == "old-token" || !slices.Contains(cfg.Policy.DeniedDomains, "api.openai.com") {
		t.Errorf("token %q, denied %v; want a fresh token and the owner's current denies", cfg.RunToken, cfg.Policy.DeniedDomains)
	}
	if lostAt, _ := f.st.lost(); lostAt != nil || f.st.State() != types.RunRunning {
		t.Errorf("lost %v, state %s; want the run live", lostAt, f.st.State())
	}
	ev := f.audit.eventsFor(f.run.ID, "run.revive")
	if len(ev) != 1 || ev[0].Outcome != "success" {
		t.Fatalf("run.revive events = %+v, want one success", ev)
	}
	if data := leaseAuditData(t, ev[0]); data["from"] != "ended" || data["agent_started"] != true {
		t.Errorf("run.revive data = %v; want from ended, agent_started", data)
	}
}

// TestEndedRun_OldTokenRefusedAndReviveMintsAFreshOne is #1176's revive half.
// The token the kept proxy holds is refused at the mint and injection doors the
// moment the run ends; a revive then gives the new proxy a FRESH token that the
// same doors admit, and retires the old one, which stays refused.
func TestEndedRun_OldTokenRefusedAndReviveMintsAFreshOne(t *testing.T) {
	f, _ := newEndedFixture(t)
	// The injection door is routed only with a secrets store.
	cfg := f.srv.cfg
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	f.srv = New(cfg)
	ctx := context.Background()
	old, err := f.srv.cfg.Identity.MintRunIdentity(ctx, f.run.ID, f.run.CreatedBy, f.run.CreatedBy, internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := proxy.LoadConfigBytes(f.rs.cfg)
	if err != nil {
		t.Fatal(err)
	}
	kept.RunToken = old.Token
	if f.rs.cfg, err = json.Marshal(kept); err != nil {
		t.Fatal(err)
	}
	mint := func(tok string) (int, string) {
		w := do(t, f.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, `{"grant_id":"`+uuid.NewString()+`"}`)
		return w.Code, w.Body.String()
	}
	inject := func(tok string) (int, string) {
		w := do(t, f.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), tok, "")
		return w.Code, w.Body.String()
	}
	for name, door := range map[string]func(string) (int, string){"mint": mint, "injection": inject} {
		if code, body := door(old.Token); code != http.StatusForbidden || !strings.Contains(body, "lost") {
			t.Fatalf("%s with the ended run's token = %d %s, want 403 run is lost", name, code, body)
		}
	}

	f.now = f.now.Add(time.Hour)
	if code, body := f.extendAs(t, true, f.now.Add(48*time.Hour)); code != http.StatusOK {
		t.Fatalf("extend = %d %s, want 200", code, body)
	}
	f.run = f.st.run
	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	fresh := f.newConfig(t).RunToken
	if fresh == "" || fresh == old.Token {
		t.Fatalf("revived proxy token = %q, want a fresh one", fresh)
	}
	for name, door := range map[string]func(string) (int, string){"mint": mint, "injection": inject} {
		if code, body := door(fresh); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s with the revived token = %d %s, want it past the liveness gate", name, code, body)
		}
		if code, body := door(old.Token); code != http.StatusUnauthorized {
			t.Errorf("%s with the retired token = %d %s, want 401 (revoked by its jti)", name, code, body)
		}
	}
}

// TestEndedRun_ExpiredFilesGraceRefusesExtendAndRevive: once the grace has
// run out, neither extension nor revive is taken, whether or not the sweep has
// torn the run down yet. An extension made inside the grace does not move it:
// the sweep still stops the run when the grace, counted from its end, runs
// out, and a revive then is refused.
func TestEndedRun_ExpiredFilesGraceRefusesExtendAndRevive(t *testing.T) {
	t.Run("not extended", func(t *testing.T) {
		f, sr := newEndedFixture(t)
		f.now = f.endedAt().Add(f.srv.cfg.EndedRunGrace)
		for _, owner := range []bool{true, false} {
			if code, body := f.extendAs(t, owner, f.now.Add(24*time.Hour)); code != http.StatusConflict || !strings.Contains(body, "no longer kept") {
				t.Errorf("extend (owner=%v) at the grace's end = %d %s, want 409", owner, code, body)
			}
			if code, body := f.reviveAs(t, owner); code != http.StatusConflict || !strings.Contains(body, "no longer kept") {
				t.Errorf("revive (owner=%v) at the grace's end = %d %s, want 409", owner, code, body)
			}
		}
		if end := f.storedEnd(); end == nil || !end.Equal(*f.run.EndsAt) {
			t.Errorf("stored end = %v, want it untouched", end)
		}
		f.assertStillEnded(t, sr)
		f.sweep(t)
		if f.st.State() != types.RunStopped {
			t.Errorf("state after the sweep = %s, want STOPPED", f.st.State())
		}
	})
	t.Run("extended inside the grace", func(t *testing.T) {
		f, sr := newEndedFixture(t)
		f.now = f.now.Add(24 * time.Hour)
		if code, body := f.extendAs(t, true, f.now.Add(30*24*time.Hour)); code != http.StatusOK {
			t.Fatalf("extend inside the grace = %d %s, want 200", code, body)
		}
		f.now = f.endedAt().Add(f.srv.cfg.EndedRunGrace)
		f.run = f.st.run
		if code, body := f.reviveAs(t, true); code != http.StatusConflict || !strings.Contains(body, "no longer kept") {
			t.Errorf("revive after the grace = %d %s, want 409", code, body)
		}
		f.sweep(t)
		if f.st.State() != types.RunStopped || len(sr.starts()) != 0 {
			t.Errorf("state %s, starts %v; want STOPPED at the grace's end despite the later end", f.st.State(), sr.starts())
		}
		if ev := f.audit.eventsFor(f.run.ID, "run.ended.expired"); len(ev) != 1 {
			t.Errorf("run.ended.expired events = %+v, want one", ev)
		}
	})
}

// TestEndedRun_ExtendAndReviveRequireOwnerAuthority: another member can
// neither extend nor revive the run, and the owner's own authority is
// re-checked for both, whoever asks: an agent denied to the owner since
// refuses each, audited.
func TestEndedRun_ExtendAndReviveRequireOwnerAuthority(t *testing.T) {
	t.Run("another member", func(t *testing.T) {
		f, sr := newEndedFixture(t)
		other := ssoSession(t, "sub-someone-else", "else@corp.example", oidc.RoleUser)
		path := "/api/v1/runs/" + f.run.ID.String()
		if w := doSSO(t, f.srv, http.MethodPatch, path, other, endsAtBody(f.now.Add(time.Hour))); w.Code < 400 || w.Code == http.StatusConflict {
			t.Errorf("extend by another member = %d %s, want refused as not theirs", w.Code, w.Body.String())
		}
		if w := doSSO(t, f.srv, http.MethodPost, path+"/revive", other, ""); w.Code < 400 || w.Code == http.StatusConflict {
			t.Errorf("revive by another member = %d %s, want refused as not theirs", w.Code, w.Body.String())
		}
		f.assertStillEnded(t, sr)
	})
	for _, owner := range []bool{true, false} {
		t.Run(fmt.Sprintf("the owner lost the agent/owner asks=%v", owner), func(t *testing.T) {
			f, sr := newEndedFixture(t)
			later := f.now.Add(24 * time.Hour)
			if code, body := f.extendAs(t, owner, later); code != http.StatusOK {
				t.Fatalf("extend with the agent granted = %d %s, want 200", code, body)
			}
			f.run = f.st.run
			f.st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, ownerEmail, capAgent, "claude-code", types.CapabilityDeny)}
			if code, _ := f.extendAs(t, owner, later.Add(time.Hour)); code != http.StatusForbidden {
				t.Errorf("extend with the agent denied = %d, want 403", code)
			}
			if code, _ := f.reviveAs(t, owner); code != http.StatusForbidden {
				t.Errorf("revive with the agent denied = %d, want 403", code)
			}
			f.assertStillEnded(t, sr)
			for _, action := range []string{"run.end.set", "run.revive"} {
				denied := slices.ContainsFunc(f.audit.eventsFor(f.run.ID, action), func(ev types.AuditEvent) bool {
					return ev.Outcome == "denied" && leaseAuditData(t, ev)["reason"] == "capability_"+capAgent
				})
				if !denied {
					t.Errorf("no %s denied row naming capability_%s", action, capAgent)
				}
			}
		})
	}
}

// TestEndedRun_ReviveStartsAgentAfterProxy: the agent starts only behind its
// new proxy, never without it. A revive that fails after its claim puts the
// run back to ended under its FIRST mark, agent and proxy stopped, so the
// files grace is not restarted and the next revive can still start it.
func TestEndedRun_ReviveStartsAgentAfterProxy(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange    func(f *reviveFixture, sr *startingRunner)
		code       int
		wantStarts []int
	}{
		"success":                   {func(*reviveFixture, *startingRunner) {}, http.StatusOK, []int{1}},
		"the proxy is not replaced": {func(f *reviveFixture, _ *startingRunner) { f.rr.replaceErr = errReplaceFailed }, http.StatusBadGateway, nil},
		"the agent does not start":  {func(_ *reviveFixture, sr *startingRunner) { sr.startErr = errStartFailed }, http.StatusBadGateway, []int{1}},
	} {
		t.Run(name, func(t *testing.T) {
			f, sr := newEndedFixture(t)
			f.now = f.now.Add(time.Hour)
			if code, body := f.extendAs(t, true, f.now.Add(24*time.Hour)); code != http.StatusOK {
				t.Fatalf("extend = %d %s", code, body)
			}
			f.run = f.st.run
			tc.arrange(f, sr)
			ends := f.rn.endCount()
			if code, body := f.reviveAs(t, true); code != tc.code {
				t.Fatalf("revive = %d %s, want %d", code, body, tc.code)
			}
			if got := sr.starts(); !slices.Equal(got, tc.wantStarts) {
				t.Errorf("agent starts, by proxies replaced before each = %v, want %v", got, tc.wantStarts)
			}
			if tc.code == http.StatusOK {
				return
			}
			lostAt, reason := f.st.lost()
			if f.st.State() != types.RunRunning || lostAt == nil || !lostAt.Equal(f.endedAt()) || reason != types.LostEnded {
				t.Errorf("state %s, lost %v %q; want kept, ended again at its first mark %v", f.st.State(), lostAt, reason, f.endedAt())
			}
			if f.rn.endCount() != ends+1 {
				t.Errorf("EndSandbox +%d, want +1: the agent and proxy must be stopped again", f.rn.endCount()-ends)
			}
		})
	}
}

// TestEndedRun_BulkRestartRefuses: the admin bulk restart never starts a
// stopped agent, so it refuses an ended run even once extended.
func TestEndedRun_BulkRestartRefuses(t *testing.T) {
	f, sr := newEndedFixture(t)
	if code, body := f.extendAs(t, false, f.now.Add(24*time.Hour)); code != http.StatusOK {
		t.Fatalf("extend = %d %s", code, body)
	}
	f.run = f.st.run
	w := do(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", adminToken, `{"run_ids":["`+f.run.ID.String()+`"]}`)
	var out struct {
		Results []adminRestartResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || len(out.Results) != 1 {
		t.Fatalf("restart = %d %s (%v), want 200 with one result", w.Code, w.Body.String(), err)
	}
	if r := out.Results[0]; r.OK || !strings.Contains(r.Error, "revive it from the run's page") {
		t.Fatalf("restart result = %+v; want refused, pointing at the run's page", r)
	}
	f.assertStillEnded(t, sr)
}

// TestEndedRun_ExtendAndExpiryRacePreservesWinner: the extension's write is
// conditional on the ended mark it decided on (store.EndedKept), so whatever
// lands between the decision and the write wins: the sweep tearing the run
// down as its grace runs out, or a kill. The write's own grace cutoff is
// pinned against Postgres (TestPG_EndedRunExtensionHonorsTheKeptMark).
func TestEndedRun_ExtendAndExpiryRacePreservesWinner(t *testing.T) {
	for name, tc := range map[string]struct {
		during    func(t *testing.T, f *reviveFixture)
		wantState types.RunState
	}{
		"the sweep tears the run down": {func(t *testing.T, f *reviveFixture) {
			f.now = f.endedAt().Add(f.srv.cfg.EndedRunGrace)
			f.sweep(t)
		}, types.RunStopped},
		"the run is killed": {func(t *testing.T, f *reviveFixture) {
			if w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/kill", adminToken, ""); w.Code >= 300 {
				t.Fatalf("kill = %d %s", w.Code, w.Body.String())
			}
		}, types.RunKilled},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newEndedFixture(t)
			f.now = f.endedAt().Add(f.srv.cfg.EndedRunGrace - time.Second)
			f.st.beforeSetEnd = func() {
				f.st.beforeSetEnd = nil
				tc.during(t, f)
			}
			if code, body := f.extendAs(t, true, f.now.Add(24*time.Hour)); code != http.StatusConflict {
				t.Fatalf("extend = %d %s, want 409", code, body)
			}
			if end := f.storedEnd(); end == nil || !end.Equal(*f.run.EndsAt) {
				t.Errorf("stored end = %v, want the ended end untouched", end)
			}
			if ev := f.audit.eventsFor(f.run.ID, "run.end.set"); len(ev) != 0 {
				t.Errorf("run.end.set events = %+v, want none", ev)
			}
			if f.st.State() != tc.wantState {
				t.Errorf("state = %s, want %s kept", f.st.State(), tc.wantState)
			}
		})
	}
}

// TestEndedRun_ReviveClaimRacesExpiryAndKill: what lands while a revive
// prepares its proxy image, after its admission and before its claim, wins.
// The claim is conditional on the ended mark, the grace and the end as of the
// claim itself, not as of the admission.
func TestEndedRun_ReviveClaimRacesExpiryAndKill(t *testing.T) {
	for name, tc := range map[string]struct {
		during    func(t *testing.T, f *reviveFixture)
		wantState types.RunState
	}{
		"the sweep tears the run down": {func(t *testing.T, f *reviveFixture) {
			f.now = f.endedAt().Add(f.srv.cfg.EndedRunGrace)
			f.sweep(t)
		}, types.RunStopped},
		"the grace runs out unswept": {func(_ *testing.T, f *reviveFixture) {
			f.now = f.endedAt().Add(f.srv.cfg.EndedRunGrace)
		}, types.RunRunning},
		"the extended end passes unswept": {func(_ *testing.T, f *reviveFixture) {
			f.now = f.storedEnd().Add(time.Second)
		}, types.RunRunning},
		"the run is killed": {func(t *testing.T, f *reviveFixture) {
			if w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/kill", adminToken, ""); w.Code >= 300 {
				t.Fatalf("kill = %d %s", w.Code, w.Body.String())
			}
		}, types.RunKilled},
	} {
		t.Run(name, func(t *testing.T) {
			f, sr := newEndedFixture(t)
			if code, body := f.extendAs(t, true, f.now.Add(time.Hour)); code != http.StatusOK {
				t.Fatalf("extend = %d %s", code, body)
			}
			f.run = f.st.run
			f.rr.onEnsure = func() {
				f.rr.onEnsure = nil
				tc.during(t, f)
			}
			if code, body := f.reviveAs(t, true); code != http.StatusConflict {
				t.Fatalf("revive = %d %s, want 409", code, body)
			}
			if len(f.rr.replaced) != 0 || len(sr.starts()) != 0 {
				t.Errorf("proxies replaced %d, agent starts %v; want none", len(f.rr.replaced), sr.starts())
			}
			if f.st.State() != tc.wantState {
				t.Errorf("state = %s, want %s", f.st.State(), tc.wantState)
			}
			if tc.wantState == types.RunRunning {
				if lostAt, reason := f.st.lost(); lostAt == nil || !lostAt.Equal(f.endedAt()) || reason != types.LostEnded {
					t.Errorf("lost %v %q; want still ended at %v", lostAt, reason, f.endedAt())
				}
			}
		})
	}
}

// TestEndedRun_TaskRunIsNotRevived: a task run's agent is its task, started
// once by dispatch, so starting its container again would bring back the
// files and no agent. Its revive is refused, extended or not.
func TestEndedRun_TaskRunIsNotRevived(t *testing.T) {
	f, sr := newEndedFixture(t)
	f.st.mu.Lock()
	f.st.run.Interactive = false
	f.st.mu.Unlock()
	if code, body := f.extendAs(t, true, f.now.Add(24*time.Hour)); code != http.StatusOK {
		t.Fatalf("extend = %d %s", code, body)
	}
	f.run = f.st.run
	if code, body := f.reviveAs(t, true); code != http.StatusConflict || !strings.Contains(body, "task run") {
		t.Fatalf("revive of an ended task run = %d %s, want 409 naming the task run", code, body)
	}
	f.assertStillEnded(t, sr)
}
