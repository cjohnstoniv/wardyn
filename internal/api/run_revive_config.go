// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reasserted is what reassertProxyCeiling changed.
type reasserted struct {
	added, droppedInjection, droppedLane []string
}

// reassertProxyCeiling is reassertCeilingDenies over a rendered proxy config:
// it unions the ceiling's denies into the frozen policy and drops every
// injection, MCP server, brokered PAT host and MITM host withheld from the run:
// the ceiling denies it, or a layer the revive's seal read (layers: the Wardyn
// boundary) denies it or does not allow it (withholding). It runs after that
// seal, never re-resolves the policy and never widens it.
//
// One lane cannot be dropped here. Dispatch withholds the git broker by
// emptying its grants AND unsetting WARDYN_GITHUB_GRANT_ID in the sandbox, and
// the proxy refuses the mint route for a brokered grant only while its grant
// map is non-empty (isBrokeredGitGrant). A running agent's env cannot change,
// so emptying the map would hand the sandbox a GitHub installation token on
// request. A revive that withholds a broker-managed host is refused, and so is
// one that withholds the host of the run's Azure CLI door.
func reassertProxyCeiling(run types.AgentRun, cfg *proxy.Config, c ownerCeiling, layers []sealLayer) (reasserted, *reviveError) {
	if cfg.RunID != run.ID {
		return reasserted{}, reviveRefused(http.StatusConflict, reasonReviveConfigRunMismatch, "the run's proxy config names another run")
	}
	var re reasserted
	w := withholding{deny: c.deny, layers: layers}
	if !w.binds() {
		return re, nil
	}
	if len(cfg.GitGrants) > 0 && w.withheldAny(gitBrokerManagedHosts) {
		return re, reviveRefused(http.StatusConflict, reasonReviveCeilingDeniesGitBroker,
			"the owner's governance profile or the Wardyn boundary now denies GitHub, which this run's git broker needs; start a new run")
	}
	if rerr := azureCLIReviveRefusal(cfg, w); rerr != nil {
		return re, rerr
	}
	re.added = unionCeilingDenies(&cfg.Policy, c.deny)
	if cfg.GitHubAPI != nil && githubDoorWithheld(w) { // the sandbox holds only the inert token, so the door can go
		cfg.GitHubAPI = nil
		closeGitHubDoor(&cfg.Policy)
		re.droppedLane = append(re.droppedLane, githubDoorLane)
	}
	cfg.Injection = slices.DeleteFunc(cfg.Injection, func(in proxy.InjectionConfig) bool {
		if w.withheld(in.Host) {
			re.droppedInjection = append(re.droppedInjection, in.Host)
			return true
		}
		return false
	})
	// An MCP server keeps its header credential on its broker entry, not in Injection, so the drop
	// above never sees it. A server on a withheld host goes, as dispatch drops one (dispatchMCP), and so
	// do the harness bindings that name it.
	gone := map[string]bool{}
	cfg.MCPServers = slices.DeleteFunc(cfg.MCPServers, func(sv proxy.MCPServerConfig) bool {
		d, err := types.MCPURLDestination(sv.URL)
		if err == nil && !w.withheld(d.Host) {
			return false
		}
		gone[sv.Key] = true
		if sv.GrantID != nil {
			re.droppedInjection = append(re.droppedInjection, d.Host)
		}
		return true
	})
	for i := range cfg.MCPBindings {
		cfg.MCPBindings[i].Servers = slices.DeleteFunc(cfg.MCPBindings[i].Servers, func(k string) bool { return gone[k] })
	}
	for host := range cfg.PATGrants {
		if w.withheld(host) {
			re.droppedLane = append(re.droppedLane, host)
			delete(cfg.PATGrants, host)
		}
	}
	slices.Sort(re.droppedLane)
	cfg.MITMHosts = slices.DeleteFunc(cfg.MITMHosts, func(h string) bool {
		return ceilingDenies(c.deny, h) || w.layerWithheld(interceptionEntryHost(h))
	})
	return re, nil
}

// pruneUnpairedInterception drops every interception entry of a revived
// config whose host no longer has an injection rule. Dispatch authors an
// entry only beside the rule it exists for, and the strip and the ceiling
// re-assertion above can take the rule away: left in place, the entry would
// have the proxy terminate that host's TLS and set nothing. A vendor's own
// model host is kept — the proxy decides those by the run's inspection
// intent, not by this list.
func (s *Server) pruneUnpairedInterception(cfg *proxy.Config) {
	cfg.MITMHosts = slices.DeleteFunc(cfg.MITMHosts, func(entry string) bool {
		host := interceptionEntryHost(entry)
		return !s.isModelProviderHost(host) && !slices.ContainsFunc(cfg.Injection, func(in proxy.InjectionConfig) bool {
			return hostEqual(in.Host, host)
		})
	})
}

// interceptionEntryHost is the bare host of one interception entry, read as
// the proxy reads it: an optional scheme, then host and optional port.
func interceptionEntryHost(entry string) string {
	entry = strings.TrimSpace(entry)
	for _, scheme := range []string{"http://", "https://"} {
		if len(entry) >= len(scheme) && strings.EqualFold(entry[:len(scheme)], scheme) {
			entry = entry[len(scheme):]
			break
		}
	}
	return componentMITMHost(entry)
}

// loadRenderedProxyConfig loads a run's rendered proxy config with its
// control-plane hop (URL and CA) set to this deployment's current pair BEFORE
// the strict loader validates it. A config rendered before the TLS hop names
// an http control plane the loader now refuses, and every reader here (revive,
// extend) hands the proxy the current pair anyway. Only those two fields change,
// on the raw JSON object: every other field, an unknown one included, still
// meets the loader as written, and the run id and the per-run MITM CA are read
// back exactly as rendered.
func (s *Server) loadRenderedProxyConfig(raw []byte) (*proxy.Config, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if fields == nil {
		return nil, errors.New("parse config: not a JSON object")
	}
	for k, v := range map[string]string{"control_plane_url": s.cfg.ControlPlaneURL, "control_plane_ca_pem": s.cfg.ControlPlaneCAPEM} {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		fields[k] = b
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return proxy.LoadConfigBytes(b)
}
