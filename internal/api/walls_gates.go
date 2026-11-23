// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// wallsGatesStartProbe is how many of a run's first allow rows a start-row read takes. The proxy writes its
	// upstream start row when it starts, before it serves the sandbox, so the row is among the run's first allows.
	// The read filters by action, run and time only: rule_source is a data key, which the accessor never reads.
	wallsGatesStartProbe = 64
	// wallsGatesStartFallbacks is how many older runs of one runner wallsRunnerStarts probes after a probe that
	// returned no allow row.
	wallsGatesStartFallbacks = 3

	// The gate ids are stable across the topology and a run's walls, and none carries an address.
	wallsGatesDirect       = "direct"
	wallsGatesSite         = "upstream-site"
	wallsGatesRunnerPrefix = "upstream-runner-"

	wallsGatesKindDirect   = "direct"
	wallsGatesKindUpstream = "upstream"

	wallsGatesSourceSite   = "site"
	wallsGatesSourceRunner = "runner"
)

// wallsStartRows probes runs for upstream start rows (rule_source builtin:upstream-proxy): each run's first 64
// egress.allow rows since its own created_at, in one AuditWallsFirstAllows read. known[id] is true only when that read
// succeeded and either found the run's start row or returned fewer than 64 rows for it, so a known run with no start
// row has none. A run's walls probe their one run.
func (s *Server) wallsStartRows(ctx context.Context, runs []types.AgentRun) (starts []store.DecisionRow, known map[uuid.UUID]bool) {
	starts, known, _, _ = s.wallsGatesProbe(ctx, runs)
	return starts, known
}

// wallsGatesProbe is wallsStartRows' one read, with what wallsRunnerStarts also needs: seen counts the allow rows
// the read returned for each run, and read is false when the store has no WallsStartReader or the read failed. A
// run that is not known may have its start row past the probe, or on a runner sink that writes it only once the
// sink grants credit, so it is never read as a run without an upstream.
func (s *Server) wallsGatesProbe(ctx context.Context, runs []types.AgentRun) (starts []store.DecisionRow, known map[uuid.UUID]bool, seen map[uuid.UUID]int, read bool) {
	known = make(map[uuid.UUID]bool, len(runs))
	reader, ok := s.cfg.Store.(store.WallsStartReader)
	if !ok {
		return nil, known, nil, false
	}
	if len(runs) == 0 {
		return nil, known, map[uuid.UUID]int{}, true
	}
	ids := make([]uuid.UUID, len(runs))
	since := make([]time.Time, len(runs))
	asked := make(map[uuid.UUID]bool, len(runs))
	for i, run := range runs {
		ids[i], since[i] = run.ID, run.CreatedAt
		asked[run.ID] = true
	}
	rows, err := reader.AuditWallsFirstAllows(ctx, ids, since, wallsGatesStartProbe)
	if err != nil {
		slog.WarnContext(ctx, "wardyn: the Walls view could not read the runs' upstream start rows; their routes are not reported",
			slog.Int("runs", len(ids)), slog.Any("err", err))
		return nil, known, nil, false
	}
	seen = make(map[uuid.UUID]int, len(runs))
	found := make(map[uuid.UUID]bool, len(runs))
	for _, r := range rows {
		if !asked[r.RunID] {
			continue
		}
		seen[r.RunID]++
		if r.RuleSource == egress.RuleSourceUpstreamProxy {
			starts = append(starts, r)
			found[r.RunID] = true
		}
	}
	for _, id := range ids {
		known[id] = found[id] || seen[id] < wallsGatesStartProbe
	}
	return starts, known, seen, true
}

// wallsRunnerStarts is the topology's read: per distinct runner_id among runs, it probes the newest run, then, for a
// runner whose probe returned no allow row, the next newest, up to 3 fallbacks. One wallsStartRows read per pass.
// A failed pass ends the read with the rows of the passes before it. A runner whose probe returned allow rows but
// no start row draws no gate: its gate is never guessed.
func (s *Server) wallsRunnerStarts(ctx context.Context, runs []types.AgentRun) []store.DecisionRow {
	byRunner := map[uuid.UUID][]types.AgentRun{}
	var runners []uuid.UUID
	listed := make(map[uuid.UUID]bool, len(runs))
	for _, run := range runs {
		if run.RunnerID == nil || listed[run.ID] {
			continue
		}
		listed[run.ID] = true
		id := *run.RunnerID
		if _, ok := byRunner[id]; !ok {
			runners = append(runners, id)
		}
		byRunner[id] = append(byRunner[id], run)
	}
	for _, id := range runners {
		slices.SortStableFunc(byRunner[id], wallsGatesNewestFirst)
	}
	var starts []store.DecisionRow
	for pass := 0; pass <= wallsGatesStartFallbacks && len(runners) > 0; pass++ {
		var probe []types.AgentRun
		var of []uuid.UUID
		for _, id := range runners {
			if pass < len(byRunner[id]) {
				probe = append(probe, byRunner[id][pass])
				of = append(of, id)
			}
		}
		if len(probe) == 0 {
			break
		}
		got, _, seen, read := s.wallsGatesProbe(ctx, probe)
		if !read {
			break
		}
		starts = append(starts, got...)
		var next []uuid.UUID
		for i, run := range probe {
			if seen[run.ID] == 0 {
				next = append(next, of[i])
			}
		}
		runners = next
	}
	return starts
}

// wallsGatesNewestFirst orders runs by created_at, newest first, then by id bytes.
func wallsGatesNewestFirst(a, b types.AgentRun) int {
	if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
		return c
	}
	return bytes.Compare(a.ID[:], b.ID[:])
}

// wallsGates builds gates[] from the site config and the given upstream start rows (rule_source builtin:upstream-proxy),
// by wallsGatesBuild's rule. runnerOf gives a run's runner_id, nil for a run on the organisation's executor. addrs is true
// only for a super admin. gateOf names the upstream gate a run's newest start row lands on, "" when the run has none.
//
// A failed site config read sends no site gate and never fails the request.
func (s *Server) wallsGates(ctx context.Context, starts []store.DecisionRow, runnerOf func(uuid.UUID) *uuid.UUID, addrs bool) (gates []WallsGate, gateOf func(runID uuid.UUID) string) {
	site, _ := s.siteConfigSnapshot(ctx)
	return s.wallsGatesBuild(site, starts, runnerOf, addrs)
}

// wallsGatesBuild is wallsGates over a site config already read, so a run's walls read the config once for their
// gates and their exits.
//
//   - The direct gate is always there.
//   - The site's upstream is a gate when the site config sets upstream_proxy_url or upstream_proxy_secret_ref. Its
//     ca_trusted is whether the deployment trusts a corporate CA. Its addr comes from the URL, never from a secret.
//   - A run on a registered runner is never sent the site's upstream, so the start row of a run with a runner_id is
//     that runner's own upstream: one gate per runner, whose addr is its newest start row's. Its CA cannot be known
//     here, so it has no ca_trusted. A start row of a run with no runner_id is the site's gate and adds none.
//   - Upstream gates are numbered from 1, the site's first and then the runners' by runner_id; the number is the
//     "corporate proxy N" label only.
func (s *Server) wallsGatesBuild(site types.SiteConfig, starts []store.DecisionRow, runnerOf func(uuid.UUID) *uuid.UUID, addrs bool) ([]WallsGate, func(uuid.UUID) string) {
	gates := []WallsGate{{ID: wallsGatesDirect, Kind: wallsGatesKindDirect, Ordinal: 1, Source: wallsGatesSourceSite}}
	if site.UpstreamProxyURL != "" || site.UpstreamProxySecretRef != "" {
		ca := s.cfg.TrustedCAPEM != ""
		g := WallsGate{ID: wallsGatesSite, Kind: wallsGatesKindUpstream, Source: wallsGatesSourceSite, CATrusted: &ca}
		if addrs {
			g.Addr = wallsGatesURLAddr(site.UpstreamProxyURL)
		}
		gates = append(gates, g)
	}
	newest := map[uuid.UUID]store.DecisionRow{}
	byRun := map[uuid.UUID]string{}
	for _, r := range starts {
		if r.RuleSource != egress.RuleSourceUpstreamProxy {
			continue
		}
		runner := runnerOf(r.RunID)
		if runner == nil {
			byRun[r.RunID] = wallsGatesSite
			continue
		}
		byRun[r.RunID] = wallsGatesRunnerPrefix + runner.String()
		if cur, ok := newest[*runner]; !ok || r.Seq > cur.Seq {
			newest[*runner] = r
		}
	}
	runners := make([]uuid.UUID, 0, len(newest))
	for id := range newest {
		runners = append(runners, id)
	}
	slices.SortFunc(runners, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, id := range runners {
		runner := id
		g := WallsGate{ID: wallsGatesRunnerPrefix + id.String(), Kind: wallsGatesKindUpstream, Source: wallsGatesSourceRunner, RunnerID: &runner}
		if addrs {
			g.Addr = wallsGatesRowAddr(newest[id])
		}
		gates = append(gates, g)
	}
	wallsGatesNumber(gates)
	return gates, func(runID uuid.UUID) string { return byRun[runID] }
}

// wallsGatesNumber sets each gate's ordinal: the direct gate is 1, and upstream gates count from 1 in list order.
func wallsGatesNumber(gates []WallsGate) {
	n := 0
	for i := range gates {
		if gates[i].Kind == wallsGatesKindDirect {
			gates[i].Ordinal = 1
			continue
		}
		n++
		gates[i].Ordinal = n
	}
}

// wallsGatesURLAddr is the host:port of the site's upstream proxy URL, read as the proxy reads it (port 80 when the
// URL names none), or "" when the URL holds no host. It never reads userinfo.
func wallsGatesURLAddr(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// wallsGatesRowAddr is the host:port a start row records, the upstream the proxy chained through.
func wallsGatesRowAddr(r store.DecisionRow) string {
	if r.Host == "" {
		return ""
	}
	if r.Port == 0 {
		return r.Host
	}
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}
