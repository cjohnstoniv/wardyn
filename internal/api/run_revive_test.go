// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveStore is lostStore plus store.RunReviver and the profile list, with
// MarkRunRevived's PG condition.
type reviveStore struct {
	*lostStore
	profiles []types.GovernanceProfile
	release  string
}

func (s *reviveStore) MarkRunRevived(_ context.Context, _ uuid.UUID, release string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != types.RunRunning || (s.run.LostAt != nil && s.run.LostReason != types.LostOutage) {
		return false, nil
	}
	s.run.LostAt, s.run.LostReason, s.release = nil, "", release
	s.lapsed = false
	return true, nil
}

func (s *reviveStore) ListRunProxyReleases(context.Context) ([]store.RunProxyRelease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []store.RunProxyRelease{{RunID: s.run.ID, CreatedBy: s.run.CreatedBy, State: s.state, Release: s.release}}, nil
}

func (s *reviveStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.lostStore.GetRun(ctx, id)
}

func (s *reviveStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, nil
}

// reviveRunner is lostRunner that holds one proxy config and can replace it.
type reviveRunner struct {
	*lostRunner
	cfg        []byte
	replaced   [][]byte
	replaceErr error
}

func (r *reviveRunner) ProxyConfig(context.Context, string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg, nil
}

func (r *reviveRunner) ReplaceProxy(_ context.Context, _ string, cfg []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replaceErr != nil {
		return r.replaceErr
	}
	r.replaced = append(r.replaced, cfg)
	r.cfg = cfg
	return nil
}

type reviveFixture struct {
	*lostFixture
	rs      *reviveStore
	rr      *reviveRunner
	profile types.GovernanceProfile
}

// newReviveFixture is a run lost to an outage: its owner's profile, as
// captured at create, now denies api.openai.com and pat.example, and its
// proxy config (stopped, kept) carries an injection and a brokered PAT for
// those hosts, plus the per-run MITM CA.
func newReviveFixture(t *testing.T) *reviveFixture {
	t.Helper()
	f := &reviveFixture{lostFixture: newLostFixture(t)}
	f.profile = types.GovernanceProfile{ID: uuid.New(), Name: "contractors",
		Ceiling: types.RunPolicySpec{DeniedDomains: []string{"api.openai.com", "pat.example"}}}
	f.st.run.GovernanceProfileID = &f.profile.ID
	f.st.run.SandboxRef = "wardyn-agent-" + f.st.run.ID.String()
	f.run = f.st.run
	f.rs = &reviveStore{lostStore: f.ls, profiles: []types.GovernanceProfile{f.profile}}
	cfg, err := runner.BuildProxyConfig(f.run.ID, runner.ProxyConfig{
		RunToken:        "old-token",
		ControlPlaneURL: "http://old-wardynd:8080",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"api.openai.com", "api.anthropic.com"}},
		Injection: []runner.InjectionGrant{
			{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: "api.openai.com", Header: "Authorization", Format: "Bearer %s"}},
			{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", Format: "%s"}},
		},
		PATGrants:     map[string]proxy.PATGrant{"pat.example": {GrantID: uuid.New()}, "git.example": {GrantID: uuid.New()}},
		MITMCACertPEM: "ca-cert",
		MITMCAKeyPEM:  "ca-key",
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	f.rr = &reviveRunner{lostRunner: f.lr, cfg: cfg}
	f.srv.cfg.Store = f.rs
	f.srv.cfg.Runner = f.rr
	f.srv.cfg.ControlPlaneURL = "http://wardynd:8080"
	f.ls.lapsed = true
	f.sweepTokens(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage {
		t.Fatalf("fixture: lost = %v %q, want lost (outage)", lostAt, reason)
	}
	f.run = f.st.run
	return f
}

// revive POSTs /runs/{id}/revive as the admin token: a caller whose OWN
// ceiling is empty, which is the point.
func (f *reviveFixture) revive(t *testing.T) int {
	t.Helper()
	return do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "").Code
}

func (f *reviveFixture) newConfig(t *testing.T) *proxy.Config {
	t.Helper()
	if len(f.rr.replaced) != 1 {
		t.Fatalf("ReplaceProxy calls = %d, want 1", len(f.rr.replaced))
	}
	cfg, err := proxy.LoadConfigBytes(f.rr.replaced[0])
	if err != nil {
		t.Fatalf("the revived proxy's config does not load: %v", err)
	}
	return cfg
}

// TestReviveRun_TheOwnersCeilingNotTheCallers is RL-10's security core. An
// admin revives a member's run lost to an outage. The new proxy gets a fresh
// token and the member's CURRENT profile denies over the frozen policy, with
// the credential lanes to those hosts dropped — never the admin's empty
// ceiling. Nothing else changes: the MITM CA and the allowlist carry over.
func TestReviveRun_TheOwnersCeilingNotTheCallers(t *testing.T) {
	f := newReviveFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	cfg := f.newConfig(t)

	for _, host := range f.profile.Ceiling.DeniedDomains {
		if !slices.Contains(cfg.Policy.DeniedDomains, host) {
			t.Errorf("denied_domains = %v; want the owner's profile deny %q re-asserted", cfg.Policy.DeniedDomains, host)
		}
	}
	for _, in := range cfg.Injection {
		if in.Host == "api.openai.com" {
			t.Error("the injection to a host the owner's profile denies survived the revive")
		}
	}
	if _, ok := cfg.PATGrants["pat.example"]; ok {
		t.Error("the brokered PAT for a host the owner's profile denies survived the revive")
	}
	if _, ok := cfg.PATGrants["git.example"]; !ok || len(cfg.Injection) != 1 {
		t.Errorf("PAT grants %v, injections %v; want the lanes to allowed hosts kept", cfg.PATGrants, cfg.Injection)
	}
	if cfg.RunToken == "old-token" || cfg.RunToken == "" || cfg.ControlPlaneURL != "http://wardynd:8080" {
		t.Errorf("run token %q, control plane %q; want a fresh token and the current control plane", cfg.RunToken, cfg.ControlPlaneURL)
	}
	claims, err := f.srv.cfg.Identity.Verify(context.Background(), cfg.RunToken, internalAudience)
	if err != nil || claims.RunID != f.run.ID || claims.Sub != f.run.CreatedBy {
		t.Errorf("fresh token claims %+v (err %v); want this run, minted for its owner %q", claims, err, f.run.CreatedBy)
	}
	if cfg.MITMCAKeyPEM != "ca-key" || cfg.MITMCACertPEM != "ca-cert" || !slices.Equal(cfg.Policy.AllowedDomains, []string{"api.openai.com", "api.anthropic.com"}) {
		t.Error("the revive changed the MITM CA or the allowlist; it may only add denies and a token")
	}

	if lostAt, _ := f.st.lost(); lostAt != nil || f.rs.release == "" {
		t.Errorf("lost_at %v, proxy release %q; want the run live on this release", lostAt, f.rs.release)
	}
	ev := f.audit.eventsFor(f.run.ID, "run.revive")
	if len(ev) != 1 || ev[0].Outcome != "success" {
		t.Fatalf("run.revive events = %+v, want one success", ev)
	}
	data := leaseAuditData(t, ev[0])
	if data["subject"] != f.run.CreatedBy || ev[0].Actor == f.run.CreatedBy || data["denies_added"] != float64(2) || data["from"] != "outage" {
		t.Errorf("run.revive actor %q data %v; want the admin as actor, the owner as subject, 2 denies added, from outage", ev[0].Actor, data)
	}
}

// TestReviveRun_Refusals: every refusal leaves the run lost with no proxy.
func TestReviveRun_Refusals(t *testing.T) {
	cases := map[string]struct {
		arrange func(f *reviveFixture)
		code    int
	}{
		"the captured profile is gone": {func(f *reviveFixture) { f.rs.profiles = nil }, http.StatusConflict},
		"the git broker would lose GitHub": {func(f *reviveFixture) {
			f.rs.profiles[0].Ceiling.DeniedDomains = append(f.rs.profiles[0].Ceiling.DeniedDomains, "github.com")
			var cfg map[string]any
			_ = json.Unmarshal(f.rr.cfg, &cfg)
			cfg["git_grants"] = map[string]string{"acme/app": uuid.NewString()}
			f.rr.cfg, _ = json.Marshal(cfg)
		}, http.StatusConflict},
		"a reboot stopped its agent": {func(f *reviveFixture) { f.st.run.LostReason = types.LostReboot }, http.StatusConflict},
		"it passed its end": {func(f *reviveFixture) {
			end := f.now.Add(-time.Minute)
			f.st.run.EndsAt = &end
		}, http.StatusConflict},
		"a config for another run": {func(f *reviveFixture) {
			f.rr.cfg, _ = runner.BuildProxyConfig(uuid.New(), runner.ProxyConfig{RunToken: "x", ControlPlaneURL: "http://x"}, 3128)
		}, http.StatusConflict},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReviveFixture(t)
			tc.arrange(f)
			if code := f.revive(t); code != tc.code {
				t.Fatalf("revive: code %d, want %d", code, tc.code)
			}
			if len(f.rr.replaced) != 0 {
				t.Error("a refused revive replaced the proxy")
			}
			if lostAt, _ := f.st.lost(); lostAt == nil {
				t.Error("a refused revive cleared the lost mark")
			}
		})
	}
}

// TestReviveRun_AFailedReplaceLosesTheRunAgain: once claimed, a proxy that
// cannot be replaced must not leave the run looking live. It is marked lost
// (outage) again with its proxy stopped, and the failure is audited.
func TestReviveRun_AFailedReplaceLosesTheRunAgain(t *testing.T) {
	f := newReviveFixture(t)
	stops := f.lr.proxyStopCount()
	f.rr.replaceErr = errors.Join(runner.ErrProxyReplaceFailed, errors.New("docker: start proxy: boom"))
	if code := f.revive(t); code != http.StatusBadGateway {
		t.Fatalf("revive: code %d, want 502", code)
	}
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage || f.st.State() != types.RunRunning {
		t.Fatalf("lost = %v %q, state %s; want the run lost (outage) again and kept", lostAt, reason, f.st.State())
	}
	if f.lr.proxyStopCount() != stops+1 {
		t.Errorf("StopProxy = %d, want one more: the run must have no proxy", f.lr.proxyStopCount()-stops)
	}
	if ev := f.audit.eventsFor(f.run.ID, "run.revive"); len(ev) != 1 || ev[0].Outcome != "failure" {
		t.Errorf("run.revive events = %+v, want one failure", ev)
	}
}

// TestReviveRun_RestartsALiveRun: the same path gives a live run a new proxy
// under its owner's current denies (the admin "Restart with current limits").
func TestReviveRun_RestartsALiveRun(t *testing.T) {
	f := newReviveFixture(t)
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	body := `{"run_ids":["` + f.run.ID.String() + `","` + uuid.NewString() + `"]}`
	w := do(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("restart: code %d (%s), want 200", w.Code, w.Body.String())
	}
	var out struct {
		Results []adminRestartResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || !out.Results[0].OK || out.Results[1].OK || out.Results[1].Error == "" {
		t.Fatalf("results = %+v; want the run restarted and the unknown id reported", out.Results)
	}
	if cfg := f.newConfig(t); !slices.Contains(cfg.Policy.DeniedDomains, "api.openai.com") {
		t.Errorf("denied_domains = %v; want the owner's current deny", cfg.Policy.DeniedDomains)
	}
	if ev := f.audit.eventsFor(f.run.ID, "run.revive"); len(ev) != 1 || leaseAuditData(t, ev[0])["from"] != "live" {
		t.Errorf("run.revive events = %+v, want one from live", ev)
	}
}

// TestAdminProxyWindow lists the runs whose proxy release is outside N and
// N-1, including one no release was recorded for.
func TestAdminProxyWindow(t *testing.T) {
	for _, tc := range []struct {
		current, release string
		in               bool
	}{
		{"0.8.0", "0.8.3", true}, {"0.8.0", "0.7.12", true}, {"0.8.0", "0.6.6", false},
		{"0.8.0", "0.9.0", false}, {"1.0.0", "0.9.0", false}, {"0.8.0", "", false},
	} {
		if got := inProxyWindow(tc.current, tc.release); got != tc.in {
			t.Errorf("inProxyWindow(%q, %q) = %v, want %v", tc.current, tc.release, got, tc.in)
		}
	}

	f := newReviveFixture(t)
	w := do(t, f.srv, http.MethodGet, "/api/v1/admin/runs/proxy-window", adminToken, "")
	var out struct {
		Outside []store.RunProxyRelease `json:"outside"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Outside) != 1 || out.Outside[0].RunID != f.run.ID {
		t.Fatalf("proxy-window: code %d body %s; want the run with no recorded release listed", w.Code, w.Body.String())
	}
	if f.revive(t) != http.StatusOK {
		t.Fatal("revive failed")
	}
	w = do(t, f.srv, http.MethodGet, "/api/v1/admin/runs/proxy-window", adminToken, "")
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Outside) != 0 {
		t.Errorf("proxy-window after a revive: %s; want nothing outside the window", w.Body.String())
	}
}
