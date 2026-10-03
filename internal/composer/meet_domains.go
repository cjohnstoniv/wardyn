// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runtime defaults the egress rules normalise against. They mirror the
// proxy sidecar's own (approvals.go configureHold and its ceilings); the
// oracle tests in internal/egress/proxy pin each of them to the behaviour.
const (
	defaultFirstUseHoldSec = int(types.HoldWindowEgress / time.Second)
	maxHoldSec             = int(types.HoldWindowPush / time.Second)
	defaultMaxHolds        = 16
	maxHoldsCeiling        = 256
)

// normFirstUseHold is what first_use_hold_seconds means at runtime: <= 0 is the
// default, and a larger value is cut to the proxy's own ceiling.
func normFirstUseHold(x int) int {
	if x <= 0 {
		return defaultFirstUseHoldSec
	}
	return min(x, maxHoldSec)
}

// normMaxHolds is what max_holds means at runtime.
func normMaxHolds(x int) int {
	if x <= 0 {
		return defaultMaxHolds
	}
	return min(x, maxHoldsCeiling)
}

// meetEgress composes the network-reach fields of the ceiling.
func (m *meeter) meetEgress(o types.CeilingOverlay) {
	c := &m.out.Ceiling
	if o.AllowedDomains != nil {
		m.meetAllowedDomains(*o.AllowedDomains)
	}
	if o.DeniedDomains != nil {
		c.DeniedDomains = unionDenied(c.DeniedDomains, *o.DeniedDomains)
	}
	if o.AllowAllEgress != nil {
		if *o.AllowAllEgress && !c.AllowAllEgress {
			m.widen("allow_all_egress", "the base does not allow all egress, and an overlay can only turn it off")
		}
		c.AllowAllEgress = c.AllowAllEgress && *o.AllowAllEgress
	}
	if o.FirstUseApproval != nil {
		b, v := c.FirstUseApproval.Normalize(), o.FirstUseApproval.Normalize()
		if firstUseApprovalRank(v) < firstUseApprovalRank(b) {
			m.widen("first_use_approval", "%q is looser than the base's %q", v, b)
		}
		if firstUseApprovalRank(v) > firstUseApprovalRank(b) {
			b = v
		}
		c.FirstUseApproval = b
	}
	if o.FirstUseHoldSeconds != nil {
		b, v := normFirstUseHold(c.FirstUseHoldSeconds), normFirstUseHold(*o.FirstUseHoldSeconds)
		if v > b {
			m.widen("first_use_hold_seconds", "%ds is longer than the base's %ds", v, b)
		}
		c.FirstUseHoldSeconds = min(b, v)
	}
	if o.MaxHolds != nil {
		b, v := normMaxHolds(c.MaxHolds), normMaxHolds(*o.MaxHolds)
		if v > b {
			m.widen("max_holds", "%d is more than the base's %d", v, b)
		}
		c.MaxHolds = min(b, v)
	}
	if o.AllowedMethods != nil {
		m.meetMethods(normMethods(*o.AllowedMethods))
	}
}

// normMethods is the proxy's reading of allowed_methods: trimmed, upper-cased,
// blanks dropped. A list that normalises to nothing compiles to "all methods".
func normMethods(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// meetMethods intersects two method sets where empty means "all". A disjoint
// pair has no representable result: the empty list the intersection would be
// is the one value the proxy reads as "every method".
func (m *meeter) meetMethods(ov []string) {
	c := &m.out.Ceiling
	base := normMethods(c.AllowedMethods)
	if len(base) == 0 {
		c.AllowedMethods = ov
		return
	}
	var both []string
	for _, s := range ov {
		if slices.Contains(base, s) {
			both = append(both, s)
		}
	}
	switch {
	case len(both) == 0:
		m.fail(ReasonOverlayUnsatisfiable, "allowed_methods", "no method is in both the base (%s) and the overlay (%s)", trimJoin(base), trimJoin(ov))
	case len(both) < len(ov):
		m.widen("allowed_methods", "the base excludes %s", trimJoin(slicesDiff(ov, both)))
	}
	c.AllowedMethods = both
}

func slicesDiff(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// domEntry is one allowed_domains/denied_domains entry as the proxy reads it:
// an exact host or a wildcard suffix (".example.com"), and a port (0 = any).
type domEntry struct {
	wild bool
	host string
	port int
}

func parseDomEntry(s string) (domEntry, bool) {
	exact, wild, port := domainmatch.Classify(s)
	switch {
	case wild != "":
		return domEntry{wild: true, host: wild, port: port}, true
	case exact != "":
		return domEntry{host: exact, port: port}, true
	}
	return domEntry{}, false
}

// String is the canonical spelling Classify reads back to the same entry.
func (e domEntry) String() string {
	host := e.host
	if e.wild {
		host = "*" + host
	}
	switch {
	case e.port == 0:
		return host
	case !e.wild && strings.Contains(host, ":"):
		return net.JoinHostPort(host, strconv.Itoa(e.port))
	}
	return host + ":" + strconv.Itoa(e.port)
}

// literalIP reports an exact entry that names an address. Only an EXACT entry
// reaches a private literal IP (Policy.AllowsLiteralIP), so such an entry is
// covered by an equal exact entry and by nothing else: a wildcard suffix can
// match the dotted text of an address without ever granting that trust.
func (e domEntry) literalIP() bool { return !e.wild && net.ParseIP(e.host) != nil }

// covers reports whether every request f matches is also matched by e: the
// proxy's own matcher decides the host half, so "covered" is never wider than
// what the proxy enforces.
func (e domEntry) covers(f domEntry) bool {
	if e.port != 0 && e.port != f.port {
		return false
	}
	switch {
	case f.literalIP():
		return !e.wild && e.host == f.host
	case e.wild:
		return domainmatch.MatchWild(f.host, []string{e.host})
	}
	return !f.wild && e.host == f.host
}

// intersect is the entry that matches exactly what both a and b match, if one
// can be spelled: a request is in the result only if both entries match it.
func (e domEntry) intersect(b domEntry) (domEntry, bool) {
	a := e
	port := a.port
	switch {
	case a.port == 0:
		port = b.port
	case b.port != 0 && b.port != a.port:
		return domEntry{}, false
	}
	switch {
	case a.wild && b.wild:
		switch {
		case strings.HasSuffix(a.host, b.host):
			return domEntry{wild: true, host: a.host, port: port}, true
		case strings.HasSuffix(b.host, a.host):
			return domEntry{wild: true, host: b.host, port: port}, true
		}
		return domEntry{}, false
	case a.wild:
		a, b = b, a
	}
	// a is exact; b is exact or a wildcard.
	if a.literalIP() && (b.wild || b.host != a.host) {
		return domEntry{}, false
	}
	if (b.wild && !domainmatch.MatchWild(a.host, []string{b.host})) || (!b.wild && a.host != b.host) {
		return domEntry{}, false
	}
	return domEntry{host: a.host, port: port}, true
}

// minimise drops an entry another entry of the same kind covers (an exact entry
// and a wildcard are not the same kind: an exact entry also enrols credential
// injection and trust of a literal IP, which a wildcard never does), dedupes,
// and sorts, so equal meets spell equal lists. Never nil: an empty allowlist is
// the explicit default-deny value.
func minimise(es []domEntry) []string {
	uniq := map[string]domEntry{}
	for _, e := range es {
		uniq[e.String()] = e
	}
	out := []string{}
	for name, e := range uniq {
		// Distinct canonical spellings never cover each other both ways, so
		// "another entry covers e" is a strict domination.
		if !hasDominator(uniq, name, e) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func hasDominator(all map[string]domEntry, name string, e domEntry) bool {
	for other, f := range all {
		if other != name && f.wild == e.wild && f.covers(e) {
			return true
		}
	}
	return false
}

// meetAllowedDomains narrows the base's allowlist by the overlay's: the result
// is every request BOTH lists allow, spelled as entries. A base's
// allow_all_egress never covers an entry (an exact entry reaches private literal
// IPs and enrols credential injection, which allow-all does not), so the
// base's own list is the only thing an overlay entry is measured against.
func (m *meeter) meetAllowedDomains(ov []string) {
	c := &m.out.Ceiling
	var base, over []domEntry
	for _, s := range c.AllowedDomains {
		if e, ok := parseDomEntry(s); ok {
			base = append(base, e)
		}
	}
	var uncovered []string
	for _, s := range ov {
		e, ok := parseDomEntry(s)
		if ok && slices.ContainsFunc(base, func(b domEntry) bool { return b.covers(e) }) {
			over = append(over, e)
			continue
		}
		if ok {
			over = append(over, e)
		}
		uncovered = append(uncovered, s)
	}
	if len(uncovered) > 0 {
		m.widen("allowed_domains", "%d entr(y/ies) the base's own allowed_domains do not cover: %s", len(uncovered), trimJoin(uncovered))
	}
	var met []domEntry
	for _, b := range base {
		for _, o := range over {
			if e, ok := b.intersect(o); ok {
				met = append(met, e)
			}
		}
	}
	c.AllowedDomains = minimise(met)
}

// unionDenied is the deny-lists' union, spelled canonically; an entry the
// matcher cannot read is kept as typed rather than dropped.
func unionDenied(a, b []string) []string {
	var out []string
	for _, s := range slices.Concat(a, b) {
		if e, ok := parseDomEntry(s); ok {
			out = append(out, e.String())
		} else if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
