// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsGatesEpoch is the fixtures' fixed clock, an instant with no date in it.
var wallsGatesEpoch = time.Unix(0, 0).UTC().Add(72 * time.Hour)

// wallsGatesStore serves the site config and each run's first allow rows from memory. Methods the gate and exit
// builders never call are the nil embedded Store's.
type wallsGatesStore struct {
	store.Store
	site     types.SiteConfig
	siteErr  error
	firsts   map[uuid.UUID][]store.DecisionRow
	firstErr error
	calls    int
	perRun   []int
}

func (s *wallsGatesStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, s.siteErr
}

func (s *wallsGatesStore) AuditWallsFirstAllows(_ context.Context, runIDs []uuid.UUID, _ []time.Time, perRun int) ([]store.DecisionRow, error) {
	s.calls++
	s.perRun = append(s.perRun, perRun)
	if s.firstErr != nil {
		return nil, s.firstErr
	}
	var out []store.DecisionRow
	for _, id := range runIDs {
		rows := s.firsts[id]
		out = append(out, rows[:min(len(rows), perRun)]...)
	}
	return out, nil
}

// wallsGatesServer is a Server over st whose deployment trusts a corporate CA when ca is set.
func wallsGatesServer(st store.Store, ca bool) *Server {
	cfg := Config{Store: st}
	if ca {
		cfg.TrustedCAPEM = "planted corporate CA"
	}
	return &Server{cfg: cfg}
}

// wallsGatesRow is one decision row of run, its times seq seconds after the epoch.
func wallsGatesRow(run uuid.UUID, seq int64, action, source, host string, port int) store.DecisionRow {
	at := wallsGatesEpoch.Add(time.Duration(seq) * time.Second)
	return store.DecisionRow{ID: uuid.New(), RunID: run, Seq: seq, Action: action, RuleSource: source, Host: host,
		Port: port, Weight: 1, Time: at, DecidedAt: at, RecordedAt: at, Format: 1}
}

// wallsGatesStart is run's upstream start row, recording the upstream it chains through.
func wallsGatesStart(run uuid.UUID, seq int64, host string, port int) store.DecisionRow {
	return wallsGatesRow(run, seq, "egress.allow", egress.RuleSourceUpstreamProxy, host, port)
}

// wallsGatesRun is a run created at the epoch, on runner when it is set.
func wallsGatesRun(runner *uuid.UUID) types.AgentRun {
	return types.AgentRun{ID: uuid.New(), RunnerID: runner, CreatedAt: wallsGatesEpoch}
}

// wallsGatesFind is the gate with id, or nil.
func wallsGatesFind(gates []WallsGate, id string) *WallsGate {
	for i := range gates {
		if gates[i].ID == id {
			return &gates[i]
		}
	}
	return nil
}

// TestWallsGatesRule: the direct gate is always there; the site's URL gate carries addr only for a super admin and
// a secret-ref gate never does; two runners with distinct upstreams are two runner gates with runner_id and no
// ca_trusted; a runner with two upstreams over time is one gate whose addr is the newer; no id names a host or a
// port; gateOf gives the gate of a run's newest start row; and a run's walls name a gate by the topology's id.
func TestWallsGatesRule(t *testing.T) {
	one, two := uuid.New(), uuid.New()
	if one.String() > two.String() {
		one, two = two, one
	}
	siteRun, runA, runA2, runB, quiet := wallsGatesRun(nil), wallsGatesRun(&one), wallsGatesRun(&one), wallsGatesRun(&two), wallsGatesRun(&two)
	runnerOf := func(id uuid.UUID) *uuid.UUID {
		for _, r := range []types.AgentRun{siteRun, runA, runA2, runB, quiet} {
			if r.ID == id {
				return r.RunnerID
			}
		}
		return nil
	}
	starts := []store.DecisionRow{
		wallsGatesStart(siteRun.ID, 1, "proxy.corp.example", 3128),
		wallsGatesStart(runA.ID, 2, "up-one.corp.example", 8080),
		wallsGatesStart(runB.ID, 3, "up-two.corp.example", 3129),
		wallsGatesStart(runA2.ID, 10, "up-one-new.corp.example", 8081),
	}
	st := &wallsGatesStore{site: types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.example:3128"}}
	s := wallsGatesServer(st, true)
	ctx := context.Background()
	runnerOne, runnerTwo := wallsGatesRunnerPrefix+one.String(), wallsGatesRunnerPrefix+two.String()

	for _, addrs := range []bool{false, true} {
		gates, gateOf := s.wallsGates(ctx, starts, runnerOf, addrs)
		if len(gates) != 4 {
			t.Fatalf("addrs=%v: %d gates %+v, want direct, the site's and two runners'", addrs, len(gates), gates)
		}
		if d := wallsGatesFind(gates, "direct"); d == nil || d.Kind != "direct" || d.Ordinal != 1 || d.Source != "site" {
			t.Errorf("addrs=%v: direct gate %+v", addrs, d)
		}
		site := wallsGatesFind(gates, "upstream-site")
		if site == nil || site.Kind != "upstream" || site.Source != "site" || site.Ordinal != 1 || site.CATrusted == nil || !*site.CATrusted {
			t.Fatalf("addrs=%v: site gate %+v, want the first corporate proxy with ca_trusted", addrs, site)
		}
		if want := map[bool]string{true: "proxy.corp.example:3128"}[addrs]; site.Addr != want {
			t.Errorf("addrs=%v: site gate addr %q, want %q", addrs, site.Addr, want)
		}
		for i, id := range []string{runnerOne, runnerTwo} {
			g := wallsGatesFind(gates, id)
			if g == nil || g.Kind != "upstream" || g.Source != "runner" || g.RunnerID == nil || g.CATrusted != nil || g.Ordinal != i+2 {
				t.Fatalf("addrs=%v: runner gate %q = %+v, want upstream ordinal %d with runner_id and no ca_trusted", addrs, id, g, i+2)
			}
		}
		if g := wallsGatesFind(gates, runnerOne); *g.RunnerID != one || g.Addr != map[bool]string{true: "up-one-new.corp.example:8081"}[addrs] {
			t.Errorf("addrs=%v: the first runner's gate %+v, want its newer upstream's addr for a super admin only", addrs, g)
		}
		if g := wallsGatesFind(gates, runnerTwo); *g.RunnerID != two || g.Addr != map[bool]string{true: "up-two.corp.example:3129"}[addrs] {
			t.Errorf("addrs=%v: the second runner's gate %+v", addrs, g)
		}
		for _, g := range gates {
			if strings.Contains(g.ID, "corp.example") || strings.Contains(g.ID, "proxy") || strings.Contains(g.ID, ":") ||
				!slices.Contains([]string{"direct", "upstream-site", runnerOne, runnerTwo}, g.ID) {
				t.Errorf("addrs=%v: gate id %q names a host or a port, or is not a stable id", addrs, g.ID)
			}
		}
		for run, want := range map[uuid.UUID]string{siteRun.ID: "upstream-site", runA.ID: runnerOne, runA2.ID: runnerOne,
			runB.ID: runnerTwo, quiet.ID: ""} {
			if got := gateOf(run); got != want {
				t.Errorf("addrs=%v: gateOf(%s) = %q, want %q", addrs, run, got, want)
			}
		}
	}

	st.site = types.SiteConfig{UpstreamProxySecretRef: "corp-proxy"}
	gates, _ := wallsGatesServer(st, false).wallsGates(ctx, nil, runnerOf, true)
	if site := wallsGatesFind(gates, "upstream-site"); site == nil || site.Addr != "" || site.CATrusted == nil || *site.CATrusted {
		t.Errorf("a secret-ref site gate %+v, want one with no addr and ca_trusted false", site)
	}
	if len(gates) != 2 || gates[0].ID != "direct" {
		t.Errorf("with no start rows: %+v, want the direct gate then the site's", gates)
	}

	st.site, st.siteErr = types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.example:3128"}, errors.New("read refused")
	gates, _ = s.wallsGates(ctx, nil, runnerOf, true)
	if len(gates) != 1 || gates[0].ID != "direct" {
		t.Errorf("a failed site config read: %+v, want the direct gate alone", gates)
	}
	st.siteErr = nil

	// One fixture, two routes: a run's walls name its gate by the topology's id, numbered over its own gates.
	topo, _ := s.wallsGates(ctx, starts, runnerOf, false)
	st.firsts = map[uuid.UUID][]store.DecisionRow{runA2.ID: {starts[3]}, siteRun.ID: {starts[0]}}
	for run, want := range map[*types.AgentRun]string{&runA2: runnerOne, &siteRun: "upstream-site"} {
		_, runGates, _ := s.wallsRunExits(ctx, *run, types.RunPolicySpec{}, nil, 0, false, false)
		ids := make([]string, 0, len(runGates))
		for _, g := range runGates {
			ids = append(ids, g.ID)
		}
		if !slices.Equal(ids, []string{"direct", want}) || wallsGatesFind(topo, want) == nil {
			t.Errorf("run %s: gates %v, want direct and the topology's %q", run.ID, ids, want)
		}
		if g := wallsGatesFind(runGates, want); g == nil || g.Ordinal != 1 {
			t.Errorf("run %s: gate %+v, want ordinal 1 over the run's own gates", run.ID, g)
		}
	}
}
