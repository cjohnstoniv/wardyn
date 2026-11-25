// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// wallsExitsCap bounds exits[]: the allowlist's entries first, then the hosts the run's rows name.
	wallsExitsCap = 256

	wallsExitsRouteUpstream = "upstream"
	wallsExitsRouteDirect   = "direct"
	wallsExitsRouteInternal = "internal"

	// The hop classes a decision row's via names (egress.DecisionLog.Via).
	wallsExitsViaUpstream = "upstream-proxy"
	wallsExitsViaDirect   = "direct"

	// The rule sources of an allow the site config's internal hosts or egress redirects granted.
	wallsExitsSourceInternalHost   = "site-config:internal-host"
	wallsExitsSourceEgressRedirect = "site-config:egress-redirect"

	wallsExitsDiscoveredApproval = "approval"
	wallsExitsDiscoveredWildcard = "wildcard"
	wallsExitsDiscoveredAllowAll = "allow_all"

	wallsExitsKindInternalHost = "internal_host"
	wallsExitsKindRedirect     = "redirect"
)

// wallsExitsDest is one destination of exits[] while it is built.
type wallsExitsDest struct {
	path     string // as served: an allowed_domains entry as written, or a row's host, with :port off 80 and 443
	key      string // the canonical path rows are matched on
	host     string // what the route and landing rules read: the host, or the suffix after "*.", with no port
	declared bool   // an allowed_domains entry
	count    int    // rows of class allowed, refused, held or fault
}

// wallsExitsNode is an inner node an exit lands on: a declared internal host or an egress redirect.
type wallsExitsNode struct {
	kind      string
	suffix    string // the declared host_suffix, or the redirect's to as configured
	toHost    string // a redirect's to host
	ecosystem string // a redirect's ecosystem
	hosts     []string
	id        string
}

// wallsExitsBuild holds what one wallsRunExits call reads while it decides each exit.
type wallsExitsBuild struct {
	run        types.AgentRun
	served     types.RunPolicySpec
	site       types.SiteConfig
	siteOK     bool
	owner      bool
	addrs      bool
	gate       string // the run's upstream gate (gateOf), "" when it has no start row
	gateListed bool   // gate is one of the run's gates[]
	known      bool   // the start-row read can say whether the run has an upstream
	byKey      map[string][]store.DecisionRow
	allowed    map[string][]store.DecisionRow
	wilds      []domainmatch.WildPort
	nodes      map[string]*wallsExitsNode
}

// wallsRunExits builds the exits[], gates[] and inner[] of one run's walls. owner is the owner projection, which marks
// no exit internal.
//
// rows are the run's recent decision rows. served is the policy the run is served, read for its allowlist and
// allow_all_egress. grants counts the run's grants. addrs is true only for a super admin: only then do a gate's addr
// and an inner node's suffix go out. The site config is read once, for the gates and the exits.
//
// An exit's route comes from the newest row for its host that names a hop. Without one, the proxy's own rule
// decides, and only where Wardyn holds its inputs: a run with no upstream start row dials direct when the read can
// say so; a run on the organisation's executor goes upstream unless the site's bypass list covers the host; a run on
// a registered runner follows that runner's own bypass list, which the server never holds, so its route is not
// reported. The owner gets the route a row names and nothing derived.
func (s *Server) wallsRunExits(ctx context.Context, run types.AgentRun, served types.RunPolicySpec, rows []store.DecisionRow, grants int, addrs, owner bool) (exits []RunWallsExit, gates []WallsGate, inner []RunWallsInner) {
	site, siteOK := s.siteConfigSnapshot(ctx)
	starts, known := s.wallsStartRows(ctx, []types.AgentRun{run})
	all, gateOf := s.wallsGatesBuild(site, starts, func(id uuid.UUID) *uuid.UUID {
		if id == run.ID {
			return run.RunnerID
		}
		return nil
	}, addrs)
	b := &wallsExitsBuild{run: run, served: served, site: site, siteOK: siteOK, owner: owner, addrs: addrs,
		gate: gateOf(run.ID), known: known[run.ID], nodes: map[string]*wallsExitsNode{}}
	gates = wallsExitsRunGates(all, b.gate)
	b.gateListed = b.gate != "" && slices.ContainsFunc(gates, func(g WallsGate) bool { return g.ID == b.gate })

	dests := b.destinations(rows)
	exits = make([]RunWallsExit, len(dests))
	landed := make([]*wallsExitsNode, len(dests))
	for i, d := range dests {
		exits[i], landed[i] = b.exit(d)
	}
	return exits, gates, b.inner(grants, dests, exits, landed)
}

// wallsExitsRunGates keeps the gates one run can use, the direct gate and its upstream gate, numbered over those.
func wallsExitsRunGates(all []WallsGate, gate string) []WallsGate {
	var out []WallsGate
	for _, g := range all {
		if g.ID == wallsGatesDirect || (gate != "" && g.ID == gate) {
			out = append(out, g)
		}
	}
	wallsGatesNumber(out)
	return out
}

// wallsExitsKey is a row's destination: its canonical host, with :port when the port is not 80 or 443.
func wallsExitsKey(host string, port int) string {
	h := domainmatch.CanonHost(strings.TrimSpace(host))
	if port == 0 || port == 80 || port == 443 {
		return h
	}
	return net.JoinHostPort(h, strconv.Itoa(port))
}

// destinations lists exits[]' destinations, at most wallsExitsCap: every allowed_domains entry as written, then every
// other host the rows of class allowed, refused, held or fault name, by row count descending and then by bytes.
// Control, configuration, tool-rule and unclassified rows make no destination.
func (b *wallsExitsBuild) destinations(rows []store.DecisionRow) []wallsExitsDest {
	b.byKey = map[string][]store.DecisionRow{}
	b.allowed = map[string][]store.DecisionRow{}
	counts := map[string]int{}
	hosts := map[string]string{}
	for _, r := range rows {
		if strings.TrimSpace(r.Host) == "" {
			continue
		}
		k := wallsExitsKey(r.Host, r.Port)
		b.byKey[k] = append(b.byKey[k], r)
		hosts[k] = domainmatch.CanonHost(strings.TrimSpace(r.Host))
		switch wallsClassify(r).Class {
		case wallsAllowed:
			b.allowed[k] = append(b.allowed[k], r)
			counts[k]++
		case wallsRefused, wallsHeld, wallsFault:
			counts[k]++
		}
	}
	var dests []wallsExitsDest
	listed := map[string]bool{}
	for _, entry := range b.served.AllowedDomains {
		exact, wild, port := domainmatch.Classify(entry)
		d := wallsExitsDest{path: entry, declared: true}
		switch {
		case wild != "":
			b.wilds = append(b.wilds, domainmatch.WildPort{Suffix: wild, Port: port})
			d.key, d.host = "*"+wild, strings.TrimPrefix(wild, ".")
			if port != 0 {
				d.key += ":" + strconv.Itoa(port)
			}
		case exact != "":
			d.key, d.host = wallsExitsKey(exact, port), exact
		default:
			continue
		}
		if !listed[d.key] {
			listed[d.key] = true
			dests = append(dests, d)
		}
	}
	var found []wallsExitsDest
	for k, n := range counts {
		if !listed[k] && n > 0 {
			found = append(found, wallsExitsDest{path: k, key: k, host: hosts[k], count: n})
		}
	}
	slices.SortFunc(found, func(x, y wallsExitsDest) int {
		if x.count != y.count {
			return y.count - x.count
		}
		return strings.Compare(x.path, y.path)
	})
	dests = append(dests, found...)
	return dests[:min(len(dests), wallsExitsCap)]
}

// exit decides one destination's route, landing, gate and discovered tag.
func (b *wallsExitsBuild) exit(d wallsExitsDest) (RunWallsExit, *wallsExitsNode) {
	e := RunWallsExit{Path: d.path, Route: b.route(d)}
	var node *wallsExitsNode
	if !b.owner {
		if internal, n := b.landing(d); internal {
			node = n
			if e.Route == wallsExitsRouteUpstream && n != nil {
				// Out through the corporate proxy and back in to the inner node.
				e.Hairpin = true
			} else {
				e.Route = wallsExitsRouteInternal
			}
		}
	}
	switch e.Route {
	case wallsExitsRouteUpstream:
		if b.gateListed {
			e.Gate = b.gate
		}
	case wallsExitsRouteDirect:
		e.Gate = wallsGatesDirect
	}
	if !d.declared {
		e.Discovered = b.discovered(d)
	}
	return e, node
}

// route is "upstream", "direct", or "" when it is not reported. The newest row for the destination that names a hop
// decides. Otherwise the proxy's rule decides where Wardyn holds its inputs, and never for the owner.
func (b *wallsExitsBuild) route(d wallsExitsDest) string {
	var newest *store.DecisionRow
	for i, r := range b.byKey[d.key] {
		if (r.Via == wallsExitsViaUpstream || r.Via == wallsExitsViaDirect) && (newest == nil || r.Seq > newest.Seq) {
			newest = &b.byKey[d.key][i]
		}
	}
	switch {
	case newest != nil && newest.Via == wallsExitsViaUpstream:
		return wallsExitsRouteUpstream
	case newest != nil:
		return wallsExitsRouteDirect
	case b.owner:
		return ""
	case b.gate == "" && b.known:
		// No upstream start row: the proxy had no upstream and dialled direct.
		return wallsExitsRouteDirect
	case b.gate == "":
		return ""
	case b.run.RunnerID != nil, !b.siteOK:
		// A runner's bypass list is its own config; an unread site config holds no bypass list to apply.
		return ""
	case proxy.NoProxyCovers(b.site.UpstreamProxyNoProxy, d.host):
		return wallsExitsRouteDirect
	}
	return wallsExitsRouteUpstream
}

// landing reports whether a destination is internal and the inner node it lands on, nil when there is none: an
// egress redirect whose to host it is, else the most specific declared internal host it falls under, else no node
// when only a row's internal-host or egress-redirect rule source marks it.
func (b *wallsExitsBuild) landing(d wallsExitsDest) (bool, *wallsExitsNode) {
	if n := b.redirectNode(d.host); n != nil {
		return true, n
	}
	if n := b.internalHostNode(d.host); n != nil {
		return true, n
	}
	for _, r := range b.byKey[d.key] {
		if r.RuleSource == wallsExitsSourceInternalHost || r.RuleSource == wallsExitsSourceEgressRedirect {
			return true, nil
		}
	}
	return false, nil
}

// redirectNode is the egress redirect whose to host is host, the lowest by its to when several are.
func (b *wallsExitsBuild) redirectNode(host string) *wallsExitsNode {
	var pick *types.EgressRedirect
	for i, red := range b.site.EgressRedirects {
		if to := strings.ToLower(hostrules.HostOf(red.To)); to != "" && to == host && (pick == nil || red.To < pick.To) {
			pick = &b.site.EgressRedirects[i]
		}
	}
	if pick == nil {
		return nil
	}
	return b.node(wallsExitsKindRedirect, pick.To, func(n *wallsExitsNode) { n.toHost, n.ecosystem = host, pick.Ecosystem })
}

// internalHostNode is the declared internal host host falls under by label suffix, as the proxy's internal-host lift
// matches, the longest suffix when several do.
func (b *wallsExitsBuild) internalHostNode(host string) *wallsExitsNode {
	pick := ""
	for _, ih := range b.site.InternalHosts {
		suffix := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(ih.HostSuffix)), ".")
		if suffix != "" && (host == suffix || strings.HasSuffix(host, "."+suffix)) && len(suffix) > len(pick) {
			pick = suffix
		}
	}
	if pick == "" {
		return nil
	}
	return b.node(wallsExitsKindInternalHost, pick, nil)
}

// node is the one node of kind and suffix, made on first use.
func (b *wallsExitsBuild) node(kind, suffix string, init func(*wallsExitsNode)) *wallsExitsNode {
	k := kind + "\x00" + suffix
	if n, ok := b.nodes[k]; ok {
		return n
	}
	n := &wallsExitsNode{kind: kind, suffix: suffix}
	if init != nil {
		init(n)
	}
	b.nodes[k] = n
	return n
}

// discovered names how a host the allowlist does not list was admitted, from its allowed rows: by an approval, under
// a "*." entry, or by allow-all. A host with no allowed row has no tag.
func (b *wallsExitsBuild) discovered(d wallsExitsDest) string {
	allowed := b.allowed[d.key]
	if len(allowed) == 0 {
		return ""
	}
	if slices.ContainsFunc(allowed, func(r store.DecisionRow) bool { return strings.HasPrefix(r.RuleSource, "approval:") }) {
		return wallsExitsDiscoveredApproval
	}
	for _, r := range allowed {
		h := domainmatch.CanonHost(strings.TrimSpace(r.Host))
		for _, w := range b.wilds {
			if strings.HasSuffix(h, w.Suffix) && (w.Port == 0 || w.Port == r.Port) {
				return wallsExitsDiscoveredWildcard
			}
		}
	}
	if b.served.AllowAllEgress {
		return wallsExitsDiscoveredAllowAll
	}
	return ""
}

// inner is inner[]: the control plane, the secret store when the run holds a grant (every grant's credential comes
// from the store through the control plane), and each node an exit lands on, numbered internal hosts first and then
// redirects, each by its suffix or to. A node's label is a host the caller's own rows or policy name, or what the
// site config says the host is: "forge" for an SCM host, "package mirror · <ecosystem>" for an ecosystem redirect.
// Its suffix goes to a super admin only.
func (b *wallsExitsBuild) inner(grants int, dests []wallsExitsDest, exits []RunWallsExit, landed []*wallsExitsNode) []RunWallsInner {
	inner := []RunWallsInner{{ID: "control-plane", Kind: "control_plane", Label: "control plane"}}
	if grants > 0 {
		inner = append(inner, RunWallsInner{ID: "secret-store", Kind: "secret_store", Label: "secret store"})
	}
	var nodes []*wallsExitsNode
	for i, n := range landed {
		if n == nil {
			continue
		}
		if len(n.hosts) == 0 {
			nodes = append(nodes, n)
		}
		n.hosts = append(n.hosts, dests[i].host)
	}
	slices.SortFunc(nodes, func(x, y *wallsExitsNode) int {
		if x.kind != y.kind {
			if x.kind == wallsExitsKindInternalHost {
				return -1
			}
			return 1
		}
		return strings.Compare(x.suffix, y.suffix)
	})
	for i, n := range nodes {
		n.id = "inner-" + strconv.Itoa(i+1)
		node := RunWallsInner{ID: n.id, Kind: n.kind, Label: b.label(n)}
		if b.addrs {
			node.Suffix = n.suffix
		}
		inner = append(inner, node)
	}
	for i, n := range landed {
		if n != nil {
			exits[i].Inner = n.id
		}
	}
	return inner
}

// label is a node's label. An internal host reads as the lowest of the hosts that land on it, or "forge" when that
// host is one of the site's SCM hosts. A redirect reads as its ecosystem's package mirror, else its to host.
func (b *wallsExitsBuild) label(n *wallsExitsNode) string {
	if n.kind == wallsExitsKindRedirect {
		if n.ecosystem != "" {
			return "package mirror · " + n.ecosystem
		}
		return n.toHost
	}
	lowest := slices.Min(n.hosts)
	for _, scm := range b.site.ScmHosts {
		if strings.TrimSuffix(strings.ToLower(strings.TrimSpace(scm)), ".") == lowest {
			return "forge"
		}
	}
	return lowest
}
