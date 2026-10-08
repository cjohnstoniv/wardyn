// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Dispatch of a run's components. The component gate (applyRunComponents)
// already turned each one into allowed domains and grants on the run's policy,
// so most of a component reaches the sandbox through the phases that carry any
// policy. Three things are left for dispatch, and they live here:
//
//   - the proxy must TERMINATE a header host's TLS to set the header at all, so
//     each one becomes an interception entry and the run gets its per-run CA;
//   - a component's plain config becomes sandbox environment;
//   - the gate's "one credential per host" rule is asked again over everything
//     dispatch authored, because dispatch reads a later site config than the
//     gate did and authors credentials the gate cannot see.

// componentHeaderPort is the one port a component's header is delivered on: the
// host's standard TLS port. The proxy keys a credential by bare host, so a
// header bound to another port could not be told from this one.
const componentHeaderPort = "443"

// componentDispatch is what dispatch needs of a run's components beyond the
// policy the gate already expanded them into. Authored from the gate's
// decision about the run OWNER's own selection — never from the sandbox, the
// agent, or a request body taken verbatim. The zero value is a run without
// components, and adds nothing anywhere.
type componentDispatch struct {
	// HeaderHosts is the bare host of every header delivery, over TLS or not:
	// the hosts dispatch's credential re-check answers for.
	HeaderHosts []string
	// MITMHosts is "host:443" for every header delivery that requires TLS.
	// Each is paired with the injection rule of the grant the gate authored for
	// the same host; an entry whose rule a later phase dropped is dropped too.
	MITMHosts []string
	// Config is each component's plain, non-secret environment, in the order
	// the components were attached. The gate refused a key two of them share.
	Config []componentConfig
}

// componentConfig is one component's plain config, and what a record about it
// may say: an organisation's keys are the admin's content, a person's are
// described by position and count alone.
type componentConfig struct {
	ordinal     int
	source      string
	selfDefined bool
	env         map[string]string
}

// dispatch is the part of the gate's decision that dispatch acts on. A header
// an organisation's component delivers over plain HTTP gets no interception
// entry: there is no TLS to terminate, and the proxy sets it on the plain lane.
func (c runComponents) dispatch() componentDispatch {
	var d componentDispatch
	for i, a := range c.attached {
		def := a.snapshot.Definition
		for _, sec := range def.Secrets {
			del := sec.Delivery
			if del.Mode != types.ComponentDeliveryHeader {
				continue
			}
			d.HeaderHosts = append(d.HeaderHosts, del.Host)
			if !del.PlainHTTP {
				d.MITMHosts = append(d.MITMHosts, net.JoinHostPort(del.Host, componentHeaderPort))
			}
		}
		if len(def.Config) > 0 {
			d.Config = append(d.Config, componentConfig{ordinal: i, source: a.source, selfDefined: a.snapshot.SelfDefined, env: maps.Clone(def.Config)})
		}
	}
	return d
}

// applyComponentConfigEnv writes the components' plain config into the sandbox
// environment. It runs after every platform writer and immediately before
// resolveEnvSecretGrants, and holds a key to that lane's rules: it never
// replaces a variable dispatch already set to a non-empty value (the proxy
// variables, the trust bundle, a toolchain's), and never sets a WARDYN_* or
// model-provider variable. The gate refused the names it could at all three
// doors; this is the same question asked where the value is written, where
// the platform's own variables are known.
//
// A key left out is audited, one row per component (run.component_config.drop):
// the person asked for a variable and is not getting it. The row names the
// keys of an organisation's component and only counts a person's.
func (s *Server) applyComponentConfigEnv(ctx context.Context, run types.AgentRun, config []componentConfig, sandboxEnv map[string]string) {
	for _, c := range config {
		var dropped []string
		for _, k := range slices.Sorted(maps.Keys(c.env)) {
			if !validEnvVarName(k) || strings.HasPrefix(k, "WARDYN_") || modelEnvNames[k] || sandboxEnv[k] != "" {
				dropped = append(dropped, k)
				continue
			}
			sandboxEnv[k] = c.env[k]
		}
		if len(dropped) == 0 {
			continue
		}
		data := map[string]any{
			"ordinal": c.ordinal, "source": c.source, "dropped": len(dropped),
			"note": "these variables are set by the platform, or are ones a component may not set; the component's value was left out",
		}
		if !c.selfDefined {
			data["keys"] = dropped
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.component_config.drop",
			run.ID.String(), "success", mustJSON(data)))
	}
}

// componentMITMHost is the bare host of an interception entry dispatch()
// authored.
func componentMITMHost(entry string) string {
	if host, _, err := net.SplitHostPort(entry); err == nil {
		return host
	}
	return entry
}

// pruneUnpaired drops every component interception entry whose host no longer
// has an injection rule. Interception exists only to set that header: a phase
// that withheld the credential (the model-credential strip, the ceiling
// re-assertion) must not leave the proxy reading a person-chosen host's
// traffic for nothing.
func (c *componentDispatch) pruneUnpaired(injections []runner.InjectionGrant) {
	c.MITMHosts = slices.DeleteFunc(c.MITMHosts, func(entry string) bool {
		return !slices.ContainsFunc(injections, func(in runner.InjectionGrant) bool {
			return hostEqual(in.Rule.Host, componentMITMHost(entry))
		})
	})
}

// credentialedHost is one host the proxy will put a credential on for this
// run, and the grant that credential comes from.
type credentialedHost struct {
	grant uuid.UUID
	host  string
}

// overlaps reports whether two credentials land on one host. The comparison is
// the gate's (types.Destination.OverlapsAtAnyPort): the proxy keys a credential
// by bare host, so a port never tells two apart. A host that does not parse is
// compared as written, so nothing escapes the comparison by being malformed.
func (c credentialedHost) overlaps(o credentialedHost) bool {
	cd, cerr := types.ParseDestination(c.host)
	od, oerr := types.ParseDestination(o.host)
	if cerr != nil || oerr != nil {
		return hostEqual(c.host, o.host)
	}
	return cd.OverlapsAtAnyPort(od)
}

// credentialHostCollision finds two credentials dispatch authored for one
// host, over the FINAL composition: every injection rule — a policy's api_key
// grants, a component's header deliveries, the model provider's, the Azure
// DevOps lane's, a redirect's token, the AWS access-portal host of a Bedrock
// sign-in — and every git_pat grant whose forge API the proxy credentials on
// the intercepted connection. These are exactly what the proxy is handed, so
// the answer here and the proxy's own refusal at boot cannot differ.
func credentialHostCollision(injections []runner.InjectionGrant, patGrants map[string]proxy.PATGrant) (grants []uuid.UUID, found bool) {
	hosts := make([]credentialedHost, 0, len(injections)+len(patGrants))
	for _, in := range injections {
		hosts = append(hosts, credentialedHost{grant: in.GrantID, host: in.Rule.Host})
	}
	for _, host := range slices.Sorted(maps.Keys(patGrants)) {
		if g := patGrants[host]; g.API {
			hosts = append(hosts, credentialedHost{grant: g.GrantID, host: host})
		}
	}
	if i, j, found := firstCredentialCollision(hosts); found {
		return []uuid.UUID{hosts[i].grant, hosts[j].grant}, true
	}
	return nil, false
}

// firstCredentialCollision is the one pairwise comparison, for the doors and
// for dispatch: the first two entries that land on one host.
func firstCredentialCollision(hosts []credentialedHost) (first, second int, found bool) {
	for i, h := range hosts {
		if j := slices.IndexFunc(hosts[:i], h.overlaps); j >= 0 {
			return j, i, true
		}
	}
	return 0, 0, false
}

// admissionCredentialHosts is the credentials a run's policy and the site
// config already bind to a host before any component is admitted — what
// dispatch would hand the proxy, read at the door:
//
//   - the policy's api_key grants (an approval-gated one is never injected);
//   - the policy's git_pat grants whose forge API the proxy credentials;
//   - the per-person Azure DevOps lane's hosts, when it resolves for subject;
//   - the target of each redirect that carries a token, once per host: one
//     mirror behind several ecosystems is one credential. A redirect with no
//     token authors no credential, so its target is not listed here (a
//     component's header there is the component gate's own refusal).
//
// A grant whose scope names no host binds nothing and is not listed.
func admissionCredentialHosts(spec types.RunPolicySpec, sc types.SiteConfig, subject string) []credentialedHost {
	var hosts []credentialedHost
	add := func(host string) {
		if strings.TrimSpace(host) != "" {
			hosts = append(hosts, credentialedHost{host: host})
		}
	}
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantAPIKey:
			if !g.RequiresApproval {
				add(apiKeyGrantScopeHost(g.Scope))
			}
		case types.GrantGitPAT:
			if pat, err := types.DecodeGitPATScope(g.Scope); err == nil && pat.API {
				add(pat.Host)
			}
		}
	}
	if ado, on := resolveADOEntraRun(sc, repoLocatorsOf(spec.WorkspaceRepos), subject); on {
		for _, h := range ado.laneHosts() {
			add(h)
		}
	}
	var mirrors []string
	for _, red := range sc.EgressRedirects {
		host := hostrules.HostOf(red.To)
		if (red.TokenSecretRef == "" && red.TokenIntegrationRef == "") || slices.ContainsFunc(mirrors, func(m string) bool { return hostEqual(m, host) }) {
			continue
		}
		mirrors = append(mirrors, host)
		add(host)
	}
	return hosts
}

// DRAFT (M2 canon pending) — the three doors' refusal of a run whose policy
// and deployment bind two credentials to one host. It names no host: either
// credential may be one an admin configured.
const credentialHostCollisionRefusal = "Two of this run's credentials are bound to the same host, and a host carries only one. " +
	"Remove one of them from the run's policy; if neither is yours, ask your admin — the second may come from a redirect or a Git provider your organisation set up."

// credentialHostRefusal is the one-credential-per-host rule at the three run
// doors, for every run: with or without components. Dispatch asks it again
// over what it authored (settleCredentialHosts), and the proxy refuses a
// config that breaks it; a run that would die there is refused here, where
// the person can still change it.
//
// It reads the site config for every run, a policy with no credential
// included: the Azure DevOps lane and a redirect's token are credentials no
// policy names, and they can land on one host between themselves (a package
// feed on an Azure DevOps host that is also a redirect's target).
func (s *Server) credentialHostRefusal(r *http.Request, spec types.RunPolicySpec) *runRefusal {
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		if sc, err = s.cfg.Store.GetSiteConfig(r.Context()); err != nil {
			return runServerError("get site config", err)
		}
	}
	hosts := admissionCredentialHosts(spec, sc, runIdentitySubject(r.Context(), principalFromRequest(r)))
	if _, _, found := firstCredentialCollision(hosts); !found {
		return nil
	}
	return runError(http.StatusUnprocessableEntity, reasonCredentialHostCollision, credentialHostCollisionRefusal)
}

// componentHostCollision is the gate's own collision question, asked again at
// dispatch: does a component's header host overlap a destination that carries
// a credential from another source — as credentialedDestinations lists them,
// from the site config dispatch read and for the run's owner. That set is
// wider than the injection rules: it holds a redirect's target whether or not
// it has a token, and the public hosts a redirect stands in for, which
// dispatch has already swapped out of the run's allowlist.
//
// The components' own grants are left out of the policy first, as the gate
// asked before it added them; two of THEM on one host are two injection rules,
// which credentialHostCollision finds.
func componentHostCollision(c componentDispatch, site types.SiteConfig, policy types.RunPolicySpec, subject string,
	injections []runner.InjectionGrant,
) (grants []uuid.UUID, found bool) {
	if len(c.HeaderHosts) == 0 {
		return nil, false
	}
	policy.EligibleGrants = slices.DeleteFunc(slices.Clone(policy.EligibleGrants), func(g types.GrantSpec) bool {
		return g.Kind == types.GrantAPIKey && slices.ContainsFunc(c.HeaderHosts, func(h string) bool { return hostEqual(h, apiKeyGrantScopeHost(g.Scope)) })
	})
	others := credentialedDestinations(site, policy, subject)
	for _, host := range c.HeaderHosts {
		d, err := types.ParseDestination(host)
		if err == nil && !slices.ContainsFunc(others, d.OverlapsAtAnyPort) {
			continue
		}
		// The component's own grant, when its rule is still on the run.
		grants = []uuid.UUID{}
		for _, in := range injections {
			if hostEqual(in.Rule.Host, host) {
				grants = append(grants, in.GrantID)
			}
		}
		return grants, true
	}
	return nil, false
}

// credentialHostCollisionHint is the failed run's own sentence. It names no
// host: the run row is not erased with a person's components, and the host may
// be one a person typed into theirs.
const credentialHostCollisionHint = "This run was not launched: two of its credentials are bound to the same host, and a host carries only one. " +
	"Remove one of them — a component's header, a policy's credential, or a redirect for that host — and start a new run."

// settleCredentialHosts closes dispatch's credential composition, for every
// run and not only one with components. It returns the proxy's git_pat
// allowlist (scopedPATGrants), drops the component interception entries left
// without a rule, and fails the run closed when two credentials are bound to
// one host. It runs below every phase that authors or withholds a credential
// and above the ProxyConfig snapshot. ok=false means the run is already FAILED.
//
// The gate refused every collision it could see. This is the same comparison
// over what dispatch actually authored, which the gate could not see: a
// redirect or a provider an admin added since, and credentials whose host only
// dispatch learns. Without it the proxy refuses the config at boot
// (buildInjector) and the run dies with a sidecar error instead of a reason.
// The audit row names the grants involved, never the host (the run's own
// grant rows carry it).
func (s *Server) settleCredentialHosts(ctx context.Context, run types.AgentRun, p *dispatchParams, policy types.RunPolicySpec,
	site types.SiteConfig, rows []types.CredentialGrant, injections []runner.InjectionGrant,
) (map[string]proxy.PATGrant, bool) {
	patGrants, ok := s.scopedPATGrants(ctx, run, *p, rows)
	if !ok {
		return nil, false
	}
	p.Components.pruneUnpaired(injections)
	grants, found := componentHostCollision(p.Components, site, policy, runIdentitySubject(ctx, run.CreatedBy), injections)
	if !found {
		grants, found = credentialHostCollision(injections, patGrants)
	}
	if !found {
		return patGrants, true
	}
	s.failAndRevoke(ctx, run.ID, types.RunStarting, credentialHostCollisionHint)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{
			"error":     "credential host collision: two credentials are bound to one host",
			"reason":    reasonCredentialHostCollision,
			"grant_ids": grants,
		})))
	return nil, false
}
