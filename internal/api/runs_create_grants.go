// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantWiring is what persistRunGrants derives from the policy's eligible
// grants: the non-secret sandbox wiring (grant ids, never credential values)
// plus the extra egress each SCM lane needs.
type grantWiring struct {
	// firstGitHubGrantID is surfaced in the sandbox env as
	// WARDYN_GITHUB_GRANT_ID (non-secret: the grant is an eligibility record,
	// not a token). The run token never appears in env.
	firstGitHubGrantID *uuid.UUID
	// gitGrants is the git-broker per-repo allowlist: canonical "<org>/<repo>" ->
	// grant id, from each github_token grant's scope.repos. Delivered proxy-side
	// only (never the sandbox) so the /wardyn/gh/ route serves exactly these repos.
	gitGrants map[string]uuid.UUID
	// githubAPI is the GitHub CLI door's grant (persistGitHubCLIGrants); nil without one.
	githubAPI *proxy.GitHubAPIGrant
	// injections are the proxy injection configs for auto-mintable api_key
	// grants: the proxy resolves their secret VALUES at startup via the internal
	// injection endpoint (values live only in proxy memory, never in the sandbox).
	injections []runner.InjectionGrant
	// gitPATGrants: host -> grant id, surfaced as WARDYN_GIT_PAT_GRANTS so the
	// git-credential helper can mint the stored PAT for a matched non-GitHub
	// host (non-secret: an eligibility record, not the PAT itself).
	gitPATGrants map[string]string
	// gitPATEgress collects the extra hosts a git_pat grant's host needs
	// reachable beyond the grant's own host (currently just ADO's dev.azure.com
	// / *.visualstudio.com bundle — see adoEgressDomains).
	gitPATEgress []string
	// sshGrants: host -> grant id, surfaced as WARDYN_SSH_GRANTS so agent-run
	// can mint the resident private key at clone time (the key material is
	// returned only through the brokered mint path and wiped after the clone).
	sshGrants map[string]string
	// sshEgress collects the SSH-over-443 endpoints these grants need reachable.
	sshEgress []string
	// warnings are the sentences the 201 has to carry about wiring this function
	// DECLINED to build — today only the provider lane vetoes. Collected
	// on the struct rather than returned separately for applySSHLaneWarnings'
	// reason: a lane dropped silently fails mid-clone, inside the sandbox, where
	// nobody is reading.
	warnings []string
	// delivery is a local run's per-grant delivery, for run.create (design 0.9 §7.5).
	delivery []grantDeliveryRecord
}

// grantDeliveryRecord is one run.create delivery entry: the grant, its OD-12 class and its mode.
type grantDeliveryRecord struct {
	GrantID uuid.UUID      `json:"grant_id"`
	Class   string         `json:"class"`
	Mode    placement.Mode `json:"mode"`
}

// persistRunGrants persists each eligible grant as an eligibility record (NOT
// issuance) and derives the sandbox wiring above. Approval-gated api_key grants
// are deliberately excluded from injections: an unmet approval would fail the
// proxy's startup mint and brick the sandbox's egress (fail closed, but a
// footgun as a default). A grant write failure is fatal (the run would be
// ungovernable): the HTTP error is written here and ok=false returned — through
// writeServerError, so the driver text behind it reaches the LOG and not the
// member who called POST /runs. That chokepoint is why the request is a
// parameter beside the writer: it is what names the method and path in the log
// line an operator is already reading. delivery is the credential delivery
// policy a local run's plan was judged on; a remote run never reads it.
// Extracted verbatim from handleCreateRun.
func (s *Server) persistRunGrants(ctx context.Context, w http.ResponseWriter, r *http.Request, runID uuid.UUID, now time.Time, spec types.RunPolicySpec, runPlacement types.Placement, delivery localJudgement) (grantWiring, bool) {
	if !remotePlacement(runPlacement) && runPlacement != types.PlacementLocal {
		writeErrorReason(w, placement.ReasonPlacementUnavailable.Status(), string(placement.ReasonPlacementUnavailable), "unclassified stored run placement")
		return grantWiring{}, false
	}
	var deliveries []localGrantDelivery
	if runPlacement == types.PlacementLocal {
		var refusal *runRefusal
		spec, deliveries, refusal = s.ownLocalGrantSpecs(r, spec, delivery)
		if refusal.write(s, w, r) {
			return grantWiring{}, false
		}
		// The class the plan was admitted at, before any grant: a revive holds the runner to it.
		if err := s.recordRunTrust(ctx, runID, delivery.trust); err != nil {
			writeServerError(w, r, "record run trust class", err)
			return grantWiring{}, false
		}
	}
	gw := grantWiring{
		gitPATGrants: map[string]string{},
		sshGrants:    map[string]string{},
		gitGrants:    map[string]uuid.UUID{},
	}
	// The provider policy the three git arms below ask about their lane, read ONCE
	// for the whole grant set: the answer cannot be allowed to change between two
	// grants of one run. Zero value in legacy open mode, where laneVetoed is a
	// no-op before it reads anything.
	sc, scErr := s.siteConfigForLaneVeto(ctx, spec)
	if scErr != nil {
		writeServerError(w, r, "get site config", scErr)
		return gw, false
	}
	// This run's own repositories, so a grant whose scope names only a HOST is
	// still decided by the row that ADMITTED the repository the run will clone
	// there — two rows of one kind on one host are a supported configuration, and
	// whichever of them is listed first is not an answer.
	specRepos := repoLocatorsOf(spec.WorkspaceRepos)
	for i, g := range spec.EligibleGrants {
		grantID := uuid.New()
		// A stored git token for an Azure DevOps host is read from the run
		// owner's OWN row only (#1429): the shared, operator-namespace token is
		// retired, and the fallback to it is what would serve it.
		if g.Kind == types.GrantGitPAT {
			if pat, derr := types.DecodeGitPATScope(g.Scope); derr == nil && adoGrantHost(sc, pat.Host) {
				g.OwnerOnly = true
			}
		}
		// A local run's grant is its owner's own or delivered via_org
		// (ownLocalGrantSpecs); the relay serves an injection or mint only for a
		// grant recorded own, and /internal/via-org/ only for one recorded via_org.
		delivery := types.GrantDeliveryNone
		if runPlacement == types.PlacementLocal {
			delivery = types.GrantDelivery(deliveries[i].mode)
			gw.delivery = append(gw.delivery, grantDeliveryRecord{GrantID: grantID, Class: deliveries[i].class, Mode: deliveries[i].mode})
		}
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID:        grantID,
			RunID:     runID,
			CreatedAt: now,
			Spec:      g,
			Delivery:  delivery,
		}); gerr != nil {
			// A grant write failure is fatal: the run would be ungovernable.
			writeServerError(w, r, "create grant", gerr)
			return gw, false
		}
		if g.Kind == types.GrantGitHubToken {
			// The `app` lane, vetoed: no broker entry, no WARDYN_GITHUB_GRANT_ID,
			// no /wardyn/gh/ route. The grant ROW stays (an eligibility record
			// nothing will mint), which is applySSHLaneWarnings' own rule.
			// Decided by the row that ADMITTED the grant's own repository — the scope
			// names it, so there is no need to guess from the host, and a GHES row and
			// a cloud row are both `kind: github`. A scope whose repo list is the empty
			// TEMPLATE (the common case for `--repo` and the example policies) falls
			// back to this run's own repositories on github.com, and only then to the
			// union of the rows claiming it. The broker mints per <org>/<repo> on
			// github.com alone (gitBrokerKey answers for no other host), which is what
			// GitLaneApp's own doc says.
			if drop := gitGrantLaneDrop(sc, specRepos, g); drop.vetoedBy != nil {
				gw.warnings = append(gw.warnings, s.recordLaneVeto(ctx, runID, drop.lane, g.Kind, *drop.vetoedBy))
				continue
			}
			// A built-in's grant is scoped to its own repositories: the run's
			// other GitHub repositories are never mapped to it (augmentGitBrokerGrants).
			if gw.firstGitHubGrantID == nil && !isBuiltinGitHubGrant(g) {
				id := grantID // copy loop var
				gw.firstGitHubGrantID = &id
			}
			// Populate the git-broker allowlist: each granted repo -> THIS grant.
			// The proxy serves /wardyn/gh/<org>/<repo> only for these keys and mints
			// the scoped installation token server-side (never into the sandbox).
			for _, repo := range githubScopeRepos(g.Scope) {
				gw.gitGrants[strings.ToLower(repo)] = grantID
			}
		}
		if g.Kind == types.GrantGitPAT {
			if pat, derr := types.DecodeGitPATScope(g.Scope); derr == nil {
				host := pat.Host
				// The `pat` lane, vetoed: no WARDYN_GIT_PAT_GRANTS entry — AND no ADO
				// egress bundle, which is the half a veto written anywhere else would
				// have left behind. Nothing but this arm adds those domains, so
				// dropping the lane has to drop them in the same breath or the run
				// carries reachability for a forge it can no longer authenticate to.
				if drop := gitGrantLaneDrop(sc, specRepos, g); drop.vetoedBy != nil {
					gw.warnings = append(gw.warnings, s.recordLaneVeto(ctx, runID, drop.lane, g.Kind, *drop.vetoedBy))
					continue
				}
				gw.gitPATGrants[host] = grantID.String()
				gw.gitPATEgress = append(gw.gitPATEgress, grantLaneEgress(g)...)
			}
		}
		if g.Kind == types.GrantSSHKey {
			// validatePolicySpec already vetted the host is a supported SSH-over-443
			// provider, so sshOver443Endpoint is expected to resolve here.
			if host, _, _, _, derr := sshKeyScopeFields(g.Scope); derr == nil {
				// Azure DevOps has no SSH lane (#1429): the grant row stays as an
				// eligibility record, and nothing is wired, so no key reaches the sandbox.
				// The `ssh` lane, vetoed: no WARDYN_SSH_GRANTS entry and no :443
				// endpoint added — so no private key is ever written into the sandbox
				// for a clone the admin said must not use one.
				switch drop := gitGrantLaneDrop(sc, specRepos, g); {
				case drop.adoSSH:
					gw.warnings = append(gw.warnings, adoSSHGrantDropped)
					continue
				case drop.vetoedBy != nil:
					gw.warnings = append(gw.warnings, s.recordLaneVeto(ctx, runID, drop.lane, g.Kind, *drop.vetoedBy))
					continue
				}
				gw.sshGrants[host] = grantID.String()
				gw.sshEgress = append(gw.sshEgress, grantLaneEgress(g)...)
			}
		}
		// Approval-gated api_key grants are deliberately excluded: an unmet
		// approval would fail the proxy's startup mint and brick the sandbox's
		// egress (fail closed, but a footgun as a default).
		if g.Kind == types.GrantAPIKey && !g.RequiresApproval {
			if rule, derr := injectionRuleFromScope(g.Scope); derr == nil {
				gw.injections = append(gw.injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
			} else {
				s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.create",
					grantID.String(), "failure", mustJSON(map[string]any{
						"error": "api_key grant scope invalid, injection skipped: " + derr.Error(),
					})))
			}
		}
	}
	return gw, true
}

// grantLaneEgress is the egress one grant's SCM lane needs beyond its own
// host: a git_pat to an Azure DevOps host needs the dev.azure.com /
// *.visualstudio.com bundle (adoEgressDomains), an ssh_key its PORT-QUALIFIED
// SSH-over-443 endpoint (sshOver443Endpoint). Every other kind, and a scope
// that does not parse, needs nothing.
//
// A function of the grant alone, BEFORE any veto, because two callers need
// the same answer at different times: persistRunGrants builds the lanes from
// it at launch, and the autonomy posture grades them on both doors before any
// lane exists (autonomyPostureSpec).
func grantLaneEgress(g types.GrantSpec) []string {
	switch g.Kind {
	case types.GrantGitPAT:
		if sc, err := types.DecodeGitPATScope(g.Scope); err == nil {
			return adoEgressDomains(sc.Host)
		}
	case types.GrantSSHKey:
		if host, _, _, _, err := sshKeyScopeFields(g.Scope); err == nil {
			if ep, ok := sshOver443Endpoint(host); ok {
				return []string{ep}
			}
		}
	}
	return nil
}

// gitLaneDrop is why create does not wire one git grant's lane: the provider
// row that turned the lane off, or an ssh_key for Azure DevOps, which has no
// SSH lane (#1429). Zero when the lane is wired. The agent's own missing SSH
// lane is agentHasSSHLane's, applied after it.
type gitLaneDrop struct {
	lane     types.GitLane
	vetoedBy *types.GitProvider
	adoSSH   bool
}

func (d gitLaneDrop) dropped() bool { return d.vetoedBy != nil || d.adoSSH }

// gitGrantLaneDrop decides a git grant's lane from the site config, the grant
// and the run's repositories alone, so persistRunGrants wires by it and the dry
// doors derive egress from it (lanesWiredFor) without either restating a rule.
//
// A github_token's app lane is decided by the row that admitted the grant's own
// repositories: its scope names them, and a scope whose list is the empty
// TEMPLATE (the common case for `--repo` and the example policies) falls back
// to this run's repositories on github.com, then to the rows claiming it. The
// broker mints per <org>/<repo> on github.com alone (gitBrokerKey).
func gitGrantLaneDrop(sc types.SiteConfig, specRepos []string, g types.GrantSpec) gitLaneDrop {
	d := gitLaneDrop{}
	veto := func(lane types.GitLane, host string, repos []string) gitLaneDrop {
		d.lane = lane
		if row, vetoed := grantHostVetoRow(sc, lane, host, repos); vetoed {
			d.vetoedBy = &row
		}
		return d
	}
	switch g.Kind {
	case types.GrantGitHubToken:
		return veto(types.GitLaneApp, "github.com", append(githubScopeRepos(g.Scope), specRepos...))
	case types.GrantGitPAT:
		if pat, err := types.DecodeGitPATScope(g.Scope); err == nil {
			return veto(types.GitLanePAT, pat.Host, specRepos)
		}
	case types.GrantSSHKey:
		if host, _, _, _, err := sshKeyScopeFields(g.Scope); err == nil {
			if adoGrantHost(sc, host) {
				return gitLaneDrop{lane: types.GitLaneSSH, adoSSH: true}
			}
			return veto(types.GitLaneSSH, host, specRepos)
		}
	}
	return d
}

// agentHasSSHLane reports whether an agent's image can clone over SSH:
// codex-cli has no openssh or corkscrew and its agent-run never reads
// WARDYN_SSH_GRANTS.
func agentHasSSHLane(agent string) bool { return agent != "codex-cli" }

// lanesWiredFor is grants without the git grants whose lane create would not
// wire for agent (gitGrantLaneDrop, then agentHasSSHLane): the dry doors'
// answer to which lanes add hosts. A placement's local grant narrowing
// (ownLocalGrantSpecs) is create's alone.
func lanesWiredFor(sc types.SiteConfig, agent string, specRepos []string, grants []types.GrantSpec) []types.GrantSpec {
	return slices.DeleteFunc(slices.Clone(grants), func(g types.GrantSpec) bool {
		return gitGrantLaneDrop(sc, specRepos, g).dropped() || (g.Kind == types.GrantSSHKey && !agentHasSSHLane(agent))
	})
}

// augmentGitBrokerGrants maps the run's DECLARED GitHub clone set (legacy run.Repo +
// WorkspaceRepos) to the run's github grant, so the git-broker serves those repos
// even when the github_token grant's scope.repos is an empty template (the common
// case for example policies + direct `--repo`). Entries already keyed from a grant's
// explicit scope.repos (persistRunGrants) win and are left as-is. No-op without a
// github grant — an un-granted github repo stays uncovered and is denied (repo is
// the unit of trust).
func (gw *grantWiring) augmentGitBrokerGrants(legacyRepo string, wsRepos []types.WorkspaceRepo) {
	if gw.firstGitHubGrantID == nil {
		return
	}
	add := func(slug string) {
		if key := gitBrokerKeyFromSlug(slug); key != "" {
			if _, ok := gw.gitGrants[key]; !ok {
				gw.gitGrants[key] = *gw.firstGitHubGrantID
			}
		}
	}
	add(legacyRepo)
	for _, wr := range wsRepos {
		add(wr.Repo)
	}
}

// applySSHLaneWarnings handles the agents/images whose SSH clone lane is absent
// or unverifiable, mutating gw and returning the warnings to surface:
//
// codex-cli has no SSH clone lane (no openssh/corkscrew in the image; its
// agent-run never reads WARDYN_SSH_GRANTS), so an ssh_key grant would sit
// unconsumed and the clone would fail SILENTLY mid-run. Fail loud at create
// instead: drop the wiring and tell the operator on the response + audit log.
// The persisted grant rows stay — they are eligibility records nothing will
// mint, not issued credentials.
//
// BYOI images get claude-code's agent-run, whose SSH lane needs openssh +
// corkscrew in the BASE image — which Wardyn cannot inspect from the control
// plane. Advise softly; the runtime guard in agent-run still fails loud
// in-sandbox if the tools are missing. Extracted verbatim from handleCreateRun.
func (s *Server) applySSHLaneWarnings(ctx context.Context, req createRunRequest, runID uuid.UUID, gw *grantWiring) []string {
	var warnings []string
	if !agentHasSSHLane(req.Agent) && len(gw.sshGrants) > 0 {
		hosts := make([]string, 0, len(gw.sshGrants))
		for h := range gw.sshGrants {
			hosts = append(hosts, h)
		}
		slices.Sort(hosts)
		warnings = append(warnings, fmt.Sprintf(
			"codex-cli has no SSH clone lane — dropping ssh_key grant(s) for %s; use an HTTPS/PAT source for this repo, or run it under claude-code",
			strings.Join(hosts, ", ")))
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.ssh.drop",
			req.Agent, "failure", mustJSON(map[string]any{"reason": "unsupported_agent", "dropped_hosts": hosts})))
		gw.sshGrants = map[string]string{}
		gw.sshEgress = nil
	}
	if req.Image != "" && len(gw.sshGrants) > 0 {
		warnings = append(warnings,
			"this run clones over SSH: your custom image must carry openssh-client + corkscrew, or the clone is skipped (agent-run warns in the run log)")
	}
	return warnings
}
