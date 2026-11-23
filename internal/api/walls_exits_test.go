// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsExitsUpstreamURL is the site's corporate proxy in these fixtures.
const wallsExitsUpstreamURL = "http://proxy.corp.example:3128"

// wallsExitsSiteRun is a store over site, a Server on it, and a run on the organisation's executor whose first
// allow row is its upstream start row.
func wallsExitsSiteRun(site types.SiteConfig) (*wallsGatesStore, *Server, types.AgentRun) {
	run := wallsGatesRun(nil)
	st := &wallsGatesStore{site: site, firsts: map[uuid.UUID][]store.DecisionRow{
		run.ID: {wallsGatesStart(run.ID, 1, "proxy.corp.example", 3128)},
	}}
	return st, wallsGatesServer(st, false), run
}

// wallsExitsAllow is an allow row of run to host on 443, with via as its hop class.
func wallsExitsAllow(run uuid.UUID, seq int64, source, host, via string) store.DecisionRow {
	r := wallsGatesRow(run, seq, "egress.allow", source, host, 443)
	r.Via = via
	return r
}

// wallsExitsFind is the exit with path.
func wallsExitsFind(t *testing.T, exits []RunWallsExit, path string) RunWallsExit {
	t.Helper()
	for _, e := range exits {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no exit %q in %+v", path, exits)
	return RunWallsExit{}
}

// wallsExitsCheck compares one exit's route, gate, inner node and hairpin.
func wallsExitsCheck(t *testing.T, name string, e RunWallsExit, route, gate, inner string, hairpin bool) {
	t.Helper()
	if e.Route != route || e.Gate != gate || e.Inner != inner || e.Hairpin != hairpin {
		t.Errorf("%s: exit %+v, want route %q, gate %q, inner %q, hairpin %v", name, e, route, gate, inner, hairpin)
	}
}

// wallsExitsInnerIDs lists inner[]'s ids in order.
func wallsExitsInnerIDs(inner []RunWallsInner) []string {
	ids := make([]string, 0, len(inner))
	for _, n := range inner {
		ids = append(ids, n.ID)
	}
	return ids
}

// wallsExitsGateIDs lists gates[]' ids in order.
func wallsExitsGateIDs(gates []WallsGate) []string {
	ids := make([]string, 0, len(gates))
	for _, g := range gates {
		ids = append(ids, g.ID)
	}
	return ids
}

// TestRunWallsExitRoutes: the newest row naming a hop wins, over the bypass list too; with none, a run with no
// start row in a short first page dials direct; a run on the organisation's executor goes direct on the site's
// bypass list and upstream off it; a run on a registered runner with no hop row has no route and no gate, the
// exit the view labels "route not reported". Control, configuration and unclassified rows make no exit.
func TestRunWallsExitRoutes(t *testing.T) {
	ctx := context.Background()
	site := types.SiteConfig{UpstreamProxyURL: wallsExitsUpstreamURL, UpstreamProxyNoProxy: []string{"bypass.example"}}
	_, s, run := wallsExitsSiteRun(site)
	served := types.RunPolicySpec{AllowedDomains: []string{"bypass.example", "*.bypass.example", "pypi.org"}}
	rows := []store.DecisionRow{
		wallsExitsAllow(run.ID, 4, "policy:allowlist", "viahost.example", "upstream-proxy"),
		wallsExitsAllow(run.ID, 5, "policy:allowlist", "viahost.example", "direct"),
		wallsExitsAllow(run.ID, 6, "policy:allowlist", "forced.bypass.example", "upstream-proxy"),
		wallsExitsAllow(run.ID, 7, "brokered:mint", "cp.wardyn.example", ""),
		wallsGatesStart(run.ID, 8, "proxy.corp.example", 3128),
		wallsGatesRow(run.ID, 9, "egress.deny", "policy:no-such-rule", "odd.example", 443),
	}
	exits, gates, _ := s.wallsRunExits(ctx, run, served, rows, 0, false, false)
	wallsExitsCheck(t, "the newest hop row", wallsExitsFind(t, exits, "viahost.example"), "direct", "direct", "", false)
	wallsExitsCheck(t, "a hop row over the bypass list", wallsExitsFind(t, exits, "forced.bypass.example"), "upstream", "upstream-site", "", false)
	wallsExitsCheck(t, "a bypassed host", wallsExitsFind(t, exits, "bypass.example"), "direct", "direct", "", false)
	wallsExitsCheck(t, "a bypassed wildcard", wallsExitsFind(t, exits, "*.bypass.example"), "direct", "direct", "", false)
	wallsExitsCheck(t, "a host off the bypass list", wallsExitsFind(t, exits, "pypi.org"), "upstream", "upstream-site", "", false)
	if len(exits) != 5 {
		t.Errorf("%d exits %+v, want the three entries and the two hosts with requests", len(exits), exits)
	}
	if ids := wallsExitsGateIDs(gates); !slices.Equal(ids, []string{"direct", "upstream-site"}) {
		t.Errorf("gates %+v, want the direct gate and the site's", gates)
	}

	plain := wallsGatesRun(nil)
	st := &wallsGatesStore{site: site, firsts: map[uuid.UUID][]store.DecisionRow{
		plain.ID: {wallsGatesRow(plain.ID, 1, "egress.allow", "policy:allowlist", "pypi.org", 443)},
	}}
	exits, gates, _ = wallsGatesServer(st, false).wallsRunExits(ctx, plain, served, nil, 0, false, false)
	wallsExitsCheck(t, "no start row", wallsExitsFind(t, exits, "pypi.org"), "direct", "direct", "", false)
	if ids := wallsExitsGateIDs(gates); !slices.Equal(ids, []string{"direct"}) {
		t.Errorf("a run with no upstream: gates %+v, want the direct gate alone", gates)
	}

	runner := uuid.New()
	hosted := wallsGatesRun(&runner)
	st = &wallsGatesStore{site: site, firsts: map[uuid.UUID][]store.DecisionRow{
		hosted.ID: {wallsGatesStart(hosted.ID, 1, "up.runner.example", 8080)},
	}}
	rows = []store.DecisionRow{wallsExitsAllow(hosted.ID, 7, "policy:allowlist", "viarunner.example", "upstream-proxy")}
	exits, _, _ = wallsGatesServer(st, false).wallsRunExits(ctx, hosted, served, rows, 0, false, false)
	wallsExitsCheck(t, "a runner's run with no hop row", wallsExitsFind(t, exits, "pypi.org"), "", "", "", false)
	wallsExitsCheck(t, "a runner's run on the site's bypass list", wallsExitsFind(t, exits, "bypass.example"), "", "", "", false)
	wallsExitsCheck(t, "a runner's hop row", wallsExitsFind(t, exits, "viarunner.example"), "upstream", wallsGatesRunnerPrefix+runner.String(), "", false)
}

// TestRunWallsInternalAndHairpin: an internal host reached direct is an internal route with its inner node and no
// gate; the same suffix reached through the corporate proxy keeps the upstream route and gate, hairpins and lands
// on the same node; a redirect target is internal.
func TestRunWallsInternalAndHairpin(t *testing.T) {
	site := types.SiteConfig{
		UpstreamProxyURL: wallsExitsUpstreamURL,
		InternalHosts:    []types.InternalHost{{HostSuffix: "corp.internal"}},
		EgressRedirects:  []types.EgressRedirect{{From: "registry.npmjs.org", To: "https://npm.mirror.example/", Ecosystem: "npm"}},
	}
	_, s, run := wallsExitsSiteRun(site)
	rows := []store.DecisionRow{
		wallsExitsAllow(run.ID, 5, "site-config:internal-host", "git.corp.internal", "direct"),
		wallsExitsAllow(run.ID, 6, "site-config:internal-host", "wiki.corp.internal", "upstream-proxy"),
		wallsExitsAllow(run.ID, 7, "policy:allowlist", "npm.mirror.example", "direct"),
	}
	exits, _, inner := s.wallsRunExits(context.Background(), run, types.RunPolicySpec{}, rows, 0, false, false)
	wallsExitsCheck(t, "an internal host reached direct", wallsExitsFind(t, exits, "git.corp.internal"), "internal", "", "inner-1", false)
	wallsExitsCheck(t, "an internal host through the proxy", wallsExitsFind(t, exits, "wiki.corp.internal"), "upstream", "upstream-site", "inner-1", true)
	wallsExitsCheck(t, "a redirect target", wallsExitsFind(t, exits, "npm.mirror.example"), "internal", "", "inner-2", false)
	if ids := wallsExitsInnerIDs(inner); !slices.Equal(ids, []string{"control-plane", "inner-1", "inner-2"}) ||
		inner[1].Kind != "internal_host" || inner[2].Kind != "redirect" {
		t.Errorf("inner %+v, want the control plane, the internal host and the redirect", inner)
	}
}

// TestRunWallsStartRowUnknown: a failed start-row read, and a full first page with no start row, leave an exit with
// no hop row unrouted and ungated; a short page with none dials direct. The read asks for 64 rows a run.
func TestRunWallsStartRowUnknown(t *testing.T) {
	site := types.SiteConfig{UpstreamProxyURL: wallsExitsUpstreamURL}
	served := types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}}
	run := wallsGatesRun(nil)
	page := func(n int) map[uuid.UUID][]store.DecisionRow {
		rows := make([]store.DecisionRow, n)
		for i := range rows {
			rows[i] = wallsGatesRow(run.ID, int64(i+1), "egress.allow", "policy:allowlist", fmt.Sprintf("h%02d.example", i), 443)
		}
		return map[uuid.UUID][]store.DecisionRow{run.ID: rows}
	}
	hop := []store.DecisionRow{wallsExitsAllow(run.ID, 100, "policy:allowlist", "viahost.example", "upstream-proxy")}
	cases := []struct {
		name        string
		st          *wallsGatesStore
		route, gate string
	}{
		{"a failed read", &wallsGatesStore{site: site, firstErr: errors.New("read refused")}, "", ""},
		{"a full page with no start row", &wallsGatesStore{site: site, firsts: page(64)}, "", ""},
		{"a short page with no start row", &wallsGatesStore{site: site, firsts: page(63)}, "direct", "direct"},
	}
	for _, c := range cases {
		exits, gates, _ := wallsGatesServer(c.st, false).wallsRunExits(context.Background(), run, served, hop, 0, false, false)
		wallsExitsCheck(t, c.name, wallsExitsFind(t, exits, "pypi.org"), c.route, c.gate, "", false)
		// A hop row still routes, and names no gate the read could not find.
		wallsExitsCheck(t, c.name+", a hop row", wallsExitsFind(t, exits, "viahost.example"), "upstream", "", "", false)
		if ids := wallsExitsGateIDs(gates); !slices.Equal(ids, []string{"direct"}) {
			t.Errorf("%s: gates %+v, want the direct gate alone", c.name, gates)
		}
		if c.st.calls != 1 || !slices.Equal(c.st.perRun, []int{64}) {
			t.Errorf("%s: %d reads of %v rows a run, want one read of 64", c.name, c.st.calls, c.st.perRun)
		}
	}

	if _, known := wallsGatesServer(&wallsGatesStore{}, false).wallsStartRows(context.Background(), []types.AgentRun{run}); !known[run.ID] {
		t.Error("an empty first page: the run is not known, want known with no start row")
	}
	if _, known := (&Server{}).wallsStartRows(context.Background(), []types.AgentRun{run}); known[run.ID] {
		t.Error("a store with no start-row reader: the run is known")
	}
}

// TestRunWallsOwnerGetsNoInternalBit: the owner's exits take their route from a hop row alone, with no internal
// route, hairpin or inner node, and inner[] holds only the control plane and the secret store; a security admin
// gets the derived routes and the internal bit for the same run.
func TestRunWallsOwnerGetsNoInternalBit(t *testing.T) {
	site := types.SiteConfig{UpstreamProxyURL: wallsExitsUpstreamURL, InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}
	_, s, run := wallsExitsSiteRun(site)
	served := types.RunPolicySpec{AllowedDomains: []string{"pypi.org", "docs.corp.internal"}}
	rows := []store.DecisionRow{wallsExitsAllow(run.ID, 5, "site-config:internal-host", "git.corp.internal", "direct")}

	exits, _, inner := s.wallsRunExits(context.Background(), run, served, rows, 1, false, false)
	wallsExitsCheck(t, "security, an internal host reached direct", wallsExitsFind(t, exits, "git.corp.internal"), "internal", "", "inner-1", false)
	wallsExitsCheck(t, "security, an internal entry with no hop row", wallsExitsFind(t, exits, "docs.corp.internal"), "upstream", "upstream-site", "inner-1", true)
	wallsExitsCheck(t, "security, an entry behind the upstream", wallsExitsFind(t, exits, "pypi.org"), "upstream", "upstream-site", "", false)
	if ids := wallsExitsInnerIDs(inner); !slices.Equal(ids, []string{"control-plane", "secret-store", "inner-1"}) {
		t.Errorf("security inner ids %v", ids)
	}

	exits, _, inner = s.wallsRunExits(context.Background(), run, served, rows, 1, false, true)
	wallsExitsCheck(t, "owner, an internal host reached direct", wallsExitsFind(t, exits, "git.corp.internal"), "direct", "direct", "", false)
	wallsExitsCheck(t, "owner, an internal entry with no hop row", wallsExitsFind(t, exits, "docs.corp.internal"), "", "", "", false)
	wallsExitsCheck(t, "owner, an entry behind the upstream", wallsExitsFind(t, exits, "pypi.org"), "", "", "", false)
	if ids := wallsExitsInnerIDs(inner); !slices.Equal(ids, []string{"control-plane", "secret-store"}) {
		t.Errorf("owner inner ids %v, want the control plane and the secret store only", ids)
	}
}

// TestRunWallsRedirectLiteralHasNoNode: an egress-redirect literal-IP allow is internal with no inner node, and two
// exits under one internal host label its node with the lower host.
func TestRunWallsRedirectLiteralHasNoNode(t *testing.T) {
	site := types.SiteConfig{
		UpstreamProxyURL: wallsExitsUpstreamURL,
		InternalHosts:    []types.InternalHost{{HostSuffix: "corp.internal"}},
		EgressRedirects:  []types.EgressRedirect{{From: "registry.npmjs.org", To: "https://npm.mirror.example/", Ecosystem: "npm"}},
	}
	_, s, run := wallsExitsSiteRun(site)
	rows := []store.DecisionRow{
		wallsExitsAllow(run.ID, 5, "site-config:egress-redirect", "10.20.30.40", "direct"),
		wallsExitsAllow(run.ID, 6, "policy:allowlist", "b.corp.internal", "direct"),
		wallsExitsAllow(run.ID, 7, "policy:allowlist", "a.corp.internal", "direct"),
	}
	exits, _, inner := s.wallsRunExits(context.Background(), run, types.RunPolicySpec{}, rows, 0, false, false)
	wallsExitsCheck(t, "a redirect's literal address", wallsExitsFind(t, exits, "10.20.30.40"), "internal", "", "", false)
	wallsExitsCheck(t, "the lower host", wallsExitsFind(t, exits, "a.corp.internal"), "internal", "", "inner-1", false)
	wallsExitsCheck(t, "the higher host", wallsExitsFind(t, exits, "b.corp.internal"), "internal", "", "inner-1", false)
	if len(inner) != 2 || inner[1].ID != "inner-1" || inner[1].Label != "a.corp.internal" {
		t.Errorf("inner %+v, want the control plane and one internal host labelled a.corp.internal", inner)
	}
}

// TestRunWallsDiscovered: a host the allowlist does not list is tagged by how its allowed rows were admitted: an
// approval, a "*." entry, or allow-all; an allowlist entry, a refused-only host and a held-only host get no tag.
func TestRunWallsDiscovered(t *testing.T) {
	run := wallsGatesRun(nil)
	s := wallsGatesServer(&wallsGatesStore{}, false)
	served := types.RunPolicySpec{AllowedDomains: []string{"pypi.org", "*.wild.example"}, AllowAllEgress: true}
	rows := []store.DecisionRow{
		wallsExitsAllow(run.ID, 1, "approval:3f0c", "approved.example", "direct"),
		wallsExitsAllow(run.ID, 2, "policy:allowlist", "x.wild.example", "direct"),
		wallsExitsAllow(run.ID, 3, "approval:9a1d", "y.wild.example", "direct"),
		wallsExitsAllow(run.ID, 4, "policy:allowed", "open.example", "direct"),
		wallsExitsAllow(run.ID, 5, "policy:allowlist", "pypi.org", "direct"),
		wallsGatesRow(run.ID, 6, "egress.deny", "policy:denied", "denied.example", 443),
		wallsGatesRow(run.ID, 7, "egress.hold", "approval:pending", "waiting.example", 443),
	}
	want := map[string]string{
		"approved.example": "approval", "x.wild.example": "wildcard", "y.wild.example": "approval", "open.example": "allow_all",
		"pypi.org": "", "*.wild.example": "", "denied.example": "", "waiting.example": "",
	}
	exits, _, _ := s.wallsRunExits(context.Background(), run, served, rows, 0, false, false)
	for path, tag := range want {
		if got := wallsExitsFind(t, exits, path).Discovered; got != tag {
			t.Errorf("%s: discovered %q, want %q", path, got, tag)
		}
	}
	served.AllowAllEgress = false
	exits, _, _ = s.wallsRunExits(context.Background(), run, served, rows, 0, false, false)
	if got := wallsExitsFind(t, exits, "open.example").Discovered; got != "" {
		t.Errorf("open.example with no allow-all: discovered %q, want none", got)
	}
}

// TestRunWallsInnerNodes: the control plane is always there; the secret store only with grants; an internal host
// that is an SCM host reads "forge" and an ecosystem redirect its package mirror; nodes are numbered internal hosts
// first, then redirects by their to; and suffix goes to a super admin only.
func TestRunWallsInnerNodes(t *testing.T) {
	site := types.SiteConfig{
		InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}, {HostSuffix: "Unused.Internal."}},
		ScmHosts:      []string{"git.corp.internal"},
		EgressRedirects: []types.EgressRedirect{
			{From: "registry.npmjs.org", To: "https://npm.mirror.example/", Ecosystem: "npm"},
			{From: "files.example.com", To: "https://files.mirror.example/"},
		},
	}
	run := wallsGatesRun(nil)
	s := wallsGatesServer(&wallsGatesStore{site: site}, false)
	rows := []store.DecisionRow{
		wallsExitsAllow(run.ID, 1, "policy:allowlist", "npm.mirror.example", "direct"),
		wallsExitsAllow(run.ID, 2, "policy:allowlist", "git.corp.internal", "direct"),
		wallsExitsAllow(run.ID, 3, "policy:allowlist", "files.mirror.example", "direct"),
	}
	for _, c := range []struct {
		grants int
		addrs  bool
	}{{0, false}, {2, true}} {
		exits, _, inner := s.wallsRunExits(context.Background(), run, types.RunPolicySpec{}, rows, c.grants, c.addrs, false)
		want := []RunWallsInner{{ID: "control-plane", Kind: "control_plane", Label: "control plane"}}
		if c.grants > 0 {
			want = append(want, RunWallsInner{ID: "secret-store", Kind: "secret_store", Label: "secret store"})
		}
		nodes := []RunWallsInner{
			{ID: "inner-1", Kind: "internal_host", Label: "forge"},
			{ID: "inner-2", Kind: "redirect", Label: "files.mirror.example"},
			{ID: "inner-3", Kind: "redirect", Label: "package mirror · npm"},
		}
		if c.addrs {
			nodes[0].Suffix, nodes[1].Suffix, nodes[2].Suffix = "corp.internal", "https://files.mirror.example/", "https://npm.mirror.example/"
		}
		want = append(want, nodes...)
		if !slices.Equal(inner, want) {
			t.Errorf("grants %d, addrs %v: inner %+v, want %+v", c.grants, c.addrs, inner, want)
		}
		for path, id := range map[string]string{"git.corp.internal": "inner-1", "files.mirror.example": "inner-2", "npm.mirror.example": "inner-3"} {
			if got := wallsExitsFind(t, exits, path).Inner; got != id {
				t.Errorf("grants %d: %s lands on %q, want %q", c.grants, path, got, id)
			}
		}
	}
	if _, _, inner := s.wallsRunExits(context.Background(), run, types.RunPolicySpec{}, nil, 0, false, false); !slices.Equal(wallsExitsInnerIDs(inner), []string{"control-plane"}) {
		t.Errorf("no exits: inner %+v, want the control plane alone", inner)
	}
}

// TestRunWallsExitsCarryNoAddressBelowSuperAdmin: below a super admin, exits, gates and inner carry no addr, no
// suffix, no upstream host, no bypass entry, no redirect URL and no internal suffix the caller's rows do not name;
// a super admin gets the addresses.
func TestRunWallsExitsCarryNoAddressBelowSuperAdmin(t *testing.T) {
	site := types.SiteConfig{
		UpstreamProxyURL:     wallsExitsUpstreamURL,
		UpstreamProxyNoProxy: []string{"bypass-only.example"},
		InternalHosts:        []types.InternalHost{{HostSuffix: "hidden.internal"}, {HostSuffix: "unused-suffix.internal"}},
		EgressRedirects:      []types.EgressRedirect{{From: "registry.npmjs.org", To: "https://npm.mirror.example/npm-path/", Ecosystem: "npm"}},
	}
	_, s, run := wallsExitsSiteRun(site)
	served := types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}}
	rows := []store.DecisionRow{
		wallsExitsAllow(run.ID, 5, "site-config:internal-host", "app.hidden.internal", "upstream-proxy"),
		wallsExitsAllow(run.ID, 6, "policy:allowlist", "npm.mirror.example", "direct"),
	}
	payload := func(addrs, owner bool) string {
		exits, gates, inner := s.wallsRunExits(context.Background(), run, served, rows, 1, addrs, owner)
		b, err := json.Marshal(map[string]any{"exits": exits, "gates": gates, "inner": inner})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	for _, owner := range []bool{false, true} {
		js := payload(false, owner)
		for _, leak := range []string{`"addr"`, `"suffix"`, "proxy.corp.example", "3128", `"hidden.internal"`,
			"unused-suffix", "bypass-only", "npm-path", "https://"} {
			if strings.Contains(js, leak) {
				t.Errorf("owner=%v: %s carries %s", owner, js, leak)
			}
		}
	}
	js := payload(true, false)
	for _, want := range []string{`"addr":"proxy.corp.example:3128"`, `"suffix":"hidden.internal"`, `"suffix":"https://npm.mirror.example/npm-path/"`} {
		if !strings.Contains(js, want) {
			t.Errorf("a super admin: %s lacks %s", js, want)
		}
	}
}

// TestRunWallsExitCap: 300 destinations give 256 exits, the allowlist's entries first and then the hosts by row count
// and bytes.
func TestRunWallsExitCap(t *testing.T) {
	run := wallsGatesRun(nil)
	s := wallsGatesServer(&wallsGatesStore{}, false)
	var served types.RunPolicySpec
	for i := range 20 {
		served.AllowedDomains = append(served.AllowedDomains, fmt.Sprintf("declared-%02d.example", i))
	}
	var rows []store.DecisionRow
	for i := range 280 {
		rows = append(rows, wallsExitsAllow(run.ID, int64(i+1), "policy:allowlist", fmt.Sprintf("host-%03d.example", i), "direct"))
	}
	rows = append(rows, wallsGatesRow(run.ID, 1000, "egress.deny", "policy:denied", "host-279.example", 443))
	exits, _, _ := s.wallsRunExits(context.Background(), run, served, rows, 0, false, false)
	if len(exits) != 256 {
		t.Fatalf("%d exits, want 256", len(exits))
	}
	for i, entry := range served.AllowedDomains {
		if exits[i].Path != entry {
			t.Fatalf("exit %d is %q, want the entry %q", i, exits[i].Path, entry)
		}
	}
	if exits[20].Path != "host-279.example" || exits[21].Path != "host-000.example" || exits[255].Path != "host-234.example" {
		t.Errorf("the hosts after the entries run %q, %q … %q; want host-279 (two rows), then host-000 to host-234",
			exits[20].Path, exits[21].Path, exits[255].Path)
	}
}
