// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// integration_probe.go is the Integrations surface's "Test" action: POST
// /integrations/{id}/test launches the SAME throwaway confined sandbox the
// site-config probes use (site_config_probe.go) and actually traverses the
// integration's own egress + credential injection — the path a run granted
// this row takes, not a "we wrote the credential down" tick. Wardyn's rule for
// test buttons (see site_config_probe.go's file comment) is that a green tick
// must mean something was really reached; this endpoint keeps it.
//
// The result is cached in memory only. A single replica is the deployment
// invariant this codebase already relies on for every in-process bound
// (siteConfigMu's comment), and "not tested" after a restart is the honest
// state — a probe result is a claim about the network a minute ago, not a
// stored fact.

// probeStatusTTL bounds how long a cached probe result is still worth
// reporting. Past it the row reads not_tested again: an hour-old "passed" is a
// claim about a network nobody has checked since, and the operator should
// re-test rather than trust it.
const probeStatusTTL = time.Hour

// probeStatus returns the cached probe result for id, or nil when the row has
// never been tested on this process (or the cached result has aged out) — the
// honest not_tested state the UI renders as its third chip.
func (s *Server) probeStatus(id string) *types.IntegrationProbeStatus {
	s.probeStatusMu.Lock()
	defer s.probeStatusMu.Unlock()
	st, ok := s.probeStatusCache[id]
	if !ok || s.cfg.Now().Sub(st.CheckedAt) > probeStatusTTL {
		return nil
	}
	out := st // copy: the caller marshals it onto a wire row
	return &out
}

// setProbeStatus records the result of a just-run probe.
func (s *Server) setProbeStatus(id string, st types.IntegrationProbeStatus) {
	s.probeStatusMu.Lock()
	defer s.probeStatusMu.Unlock()
	if s.probeStatusCache == nil {
		s.probeStatusCache = map[string]types.IntegrationProbeStatus{}
	}
	s.probeStatusCache[id] = st
}

// invalidateProbeStatus drops id's cached probe result. A cached "passed" is
// a claim about the credential/egress/config the row had when it was probed;
// a PUT or DELETE changes (or removes) that row, so the stale result must not
// keep reading as current for up to probeStatusTTL after the edit (W11-S1-2)
// — every write/delete path for a stored integration calls this.
func (s *Server) invalidateProbeStatus(id string) {
	s.probeStatusMu.Lock()
	defer s.probeStatusMu.Unlock()
	delete(s.probeStatusCache, id)
}

// integrationProbeScript curls the row's probe URL through the sandbox's normal
// egress path (HTTP_PROXY/HTTPS_PROXY already point at wardyn-proxy, which is
// also what injects the row's credential header). -f makes an HTTP error status
// a failure: with the integration's own credential presented, a 401/403 IS the
// answer the operator is asking for. The URL is operator-authored config that
// already passed validSiteURL at write time, and it is %q-quoted here — the
// same treatment handleTestSiteConfigProxy gives its custom target.
func integrationProbeScript(method, url string) string {
	head := ""
	if method == "HEAD" {
		head = "-I "
	}
	return fmt.Sprintf("curl -fsS %s-o /dev/null --connect-timeout 5 --max-time 15 %q\n", head, url)
}

// handleTestIntegration is POST /api/v1/integrations/{id}/test (operator-only,
// audited). It probes the row's own configured probe URL through the row's own
// egress allowlist and credential injection, caches what it observed, and
// returns it.
//
// Both refusals are 400s about the STORED row, never about the request body —
// there is no body: nothing here lets a caller pick the target (the SSRF rule
// handleTestSiteConfigRedirect follows). A probe URL outside the row's own
// egress is refused rather than quietly allowed for the probe: the point is to
// traverse what a RUN traverses, and a run granted this integration could not
// reach that host either.
//
//	POST /api/v1/integrations/{id}/test
func (s *Server) handleTestIntegration(w http.ResponseWriter, r *http.Request) {
	id := integrationIDParam(r)
	ctx := r.Context()
	integ, found := s.resolveIntegrationRef(ctx, id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no integration %q", id))
		return
	}
	if integ.Probe == nil || integ.Probe.URL == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"integration %q configures no probe — set probe.method/probe.url to make it testable", id))
		return
	}
	if integ.Disabled {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"integration %q is disabled, so a run granted it reaches nothing — there is no path to probe", id))
		return
	}
	host := workspacescan.HostOf(integ.Probe.URL)
	if !egressCovers(integ.Egress, host) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"probe url host %q is not in this integration's egress, so a run granted this integration could not reach "+
				"it either — add %s to the egress list or point the probe at a host that is on it", host, host))
		return
	}
	if s.cfg.Runner == nil {
		writeJSON(w, http.StatusOK, noRunnerResponse)
		return
	}

	// The SAME fold a run gets: egress union + the row's proxy-side credential
	// grants, authored by the one function the workspace-requirement path uses.
	// A disabled row folds to nothing and the probe just proves reachability.
	var spec types.RunPolicySpec
	rows := []integrationRow{{Integration: integ, Source: "stored"}}
	s.applyIntegrationRequirement(ctx, rows, &spec, id) // its audit entry belongs to a run; this endpoint audits itself below

	actor := principalFromRequest(r)
	runID, res, perr := s.runSiteConfigProbe(ctx, actor, integrationProbeScript(integ.Probe.Method, integ.Probe.URL),
		spec.AllowedDomains, spec.EligibleGrants, nil)
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "launch integration probe: "+perr.Error())
		return
	}
	resp := classifyProxyProbe(res, proxyProbeSubject{
		endpoints: stripURLScheme(integ.Probe.URL), hosts: host, custom: true,
	})
	st := types.IntegrationProbeStatus{
		State: probeStateFor(resp.State), CheckedAt: s.cfg.Now().UTC(), Detail: resp.Detail,
	}
	s.setProbeStatus(id, st)
	s.recordAudit(ctx, s.auditEvent(&runID, actorTypeFromRequest(r), actor, "integration.test",
		id, outcomeBool(st.State == "passed"), mustJSON(map[string]any{
			"state": st.State, "target_host": host, "elapsed_ms": resp.ElapsedMS,
			"injected_hosts": len(spec.EligibleGrants),
		})))
	writeJSON(w, http.StatusOK, st)
}

// egressCovers reports whether host would be reachable under an allowlist made
// of entries — an exact match or a leading-"*." wildcard, the two shapes
// proxy.ValidDomainEntry accepts (a ":port" qualifier is ignored: the probe
// dials whatever port its URL names). Deliberately looser than
// domainAllowedExact, which answers the INJECTOR's question ("may a credential
// be presented here"); this one answers reachability.
func egressCovers(entries []string, host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if i := strings.LastIndex(e, ":"); i > 0 {
			e = e[:i]
		}
		if e == h || (strings.HasPrefix(e, "*.") && strings.HasSuffix(h, e[1:])) {
			return true
		}
	}
	return false
}

// probeStateFor maps the shared probe vocabulary (classifyProxyProbe's
// reached/blocked) onto the integration row's own three-state chip. There is
// no third outcome to map: not_tested is the ABSENCE of a result, never
// something a completed probe reports.
func probeStateFor(state string) string {
	if strings.EqualFold(state, "reached") {
		return "passed"
	}
	return "failed"
}
