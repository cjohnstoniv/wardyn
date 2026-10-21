// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The private_ip_refusals row reads the last week of refusals, at most this many
// rows per rule source, and names at most privateIPRefusalHosts of them.
const (
	privateIPRefusalWindow = 7 * 24 * time.Hour
	privateIPRefusalScan   = 50
	privateIPRefusalHosts  = 3
	// privateIPRefusalTTL bounds the read: /setup/status is polled every 5s,
	// and each read is three bounded data @> scans of the audit table.
	privateIPRefusalTTL = 5 * time.Minute
	// privateIPLiftMarker is the part of the proxy's lift hint
	// (internalHostsLiftHint, internal/egress/proxy) before the block it names.
	privateIPLiftMarker = " with site_config.internal_hosts, scoped to "
)

// privateIPRefusalSources are the rule sources whose deny row can carry the lift
// hint: a CONNECT or plain-HTTP refusal, and the two brokered git lanes' vet.
var privateIPRefusalSources = []string{"builtin:private-ip", "brokered:git-pat:denied", "brokered:ado-git:denied"}

// privateIPRefusalsRow warns when a run's proxy (a runner's included) refused a
// host because it resolved into private space that internal_hosts can lift. The
// proxy, not this server, saw the address: a runner on a corporate network can
// resolve a forge into CGNAT or RFC1918 where the cluster resolves it publicly.
// Each named refusal quotes the row's cause, which names the host and the block
// and never the address. A host a current internal_hosts entry covers for that
// block is left out. No runner reports its resolution before a run, so this is
// after the fact. Never Blocking; a failed read leaves the row out.
func (s *Server) privateIPRefusalsRows(ctx context.Context, sc types.SiteConfig) []SetupCheck {
	if chk, ok := s.privateIPRefusalsRow(ctx, sc); ok {
		return []SetupCheck{chk}
	}
	return nil
}

// privateIPRefusalsRow is the row, and false when there is none.
func (s *Server) privateIPRefusalsRow(ctx context.Context, sc types.SiteConfig) (SetupCheck, bool) {
	recent, ok := s.recentPrivateIPRefusals(ctx)
	if !ok {
		return SetupCheck{}, false
	}
	var causes []string
	for _, c := range recent {
		if !liftDeclared(c, sc.InternalHosts) {
			causes = append(causes, c)
		}
	}
	if len(causes) == 0 {
		return SetupCheck{}, false
	}
	if len(causes) > privateIPRefusalHosts {
		causes = causes[:privateIPRefusalHosts]
	}
	return SetupCheck{
		ID: "private_ip_refusals", Label: "Private-address refusals", Status: "warn",
		Detail: "In the last 7 days a run's proxy refused a host because it resolved into private address space, " +
			"which the built-in guard denies even for a configured git provider: " + strings.Join(causes, "; ") + ".",
		Fix: "Add the host to site config internal_hosts with the range named, then start a new run. " +
			"The range is what the run's proxy resolved, which on a self-hosted runner can differ from what this server resolves.",
	}, true
}

// privateIPRefusalCache holds the last read of recent lift causes, newest
// first, deduplicated, before the site-config filter (so a new declaration
// clears the row on the next poll).
type privateIPRefusalCache struct {
	mu     sync.Mutex
	at     time.Time
	causes []string
}

// recentPrivateIPRefusals is the cached read; false when the store cannot page
// or the read failed.
func (s *Server) recentPrivateIPRefusals(ctx context.Context) ([]string, bool) {
	pager, ok := s.cfg.Store.(store.Pager)
	if !ok {
		return nil, false
	}
	c := &s.privIPRefusals
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.cfg.Now()
	if !c.at.IsZero() && now.Sub(c.at) < privateIPRefusalTTL {
		return c.causes, true
	}
	var causes []string
	for _, src := range privateIPRefusalSources {
		rows, err := pager.QueryAuditEventsFilteredPage(ctx, nil, store.AuditFilter{
			Action: "egress.deny",
			// A run proxy's own row, written through the internal decision
			// route; a laptop may forward any row, and must not raise this one.
			Origin:       store.AuditOriginOrganisation,
			ActorType:    types.ActorAgent,
			Since:        now.Add(-privateIPRefusalWindow),
			DataContains: string(mustJSON(map[string]any{"rule_source": src})),
		}, store.Page{Limit: privateIPRefusalScan})
		if err != nil {
			slog.WarnContext(ctx, "wardynd: setup status: read private-address refusals", slog.Any("err", err))
			return nil, false
		}
		for _, ev := range rows {
			var d struct {
				Cause string `json:"cause"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && strings.Contains(d.Cause, privateIPLiftMarker) && !slices.Contains(causes, d.Cause) {
				causes = append(causes, d.Cause)
			}
		}
	}
	c.at, c.causes = now, causes
	return causes, true
}

// liftDeclared reports whether a lift hint's host and block are already covered
// by a declared entry: the host matches its suffix and, when it lists cidrs, one
// contains the whole block.
func liftDeclared(cause string, declared []types.InternalHost) bool {
	i := strings.LastIndex(cause, privateIPLiftMarker)
	host := cause[strings.LastIndex(cause[:i], " ")+1 : i]
	block, err := netip.ParsePrefix(cause[i+len(privateIPLiftMarker):])
	if err != nil {
		return false
	}
	for _, h := range declared {
		suffix := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h.HostSuffix)), ".")
		if host != suffix && !strings.HasSuffix(host, "."+suffix) {
			continue
		}
		if len(h.CIDRs) == 0 {
			return true
		}
		for _, c := range h.CIDRs {
			if p, err := netip.ParsePrefix(c); err == nil && p.Bits() <= block.Bits() && p.Contains(block.Addr()) {
				return true
			}
		}
	}
	return false
}
