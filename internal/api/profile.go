// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/recordmode"
)

// profileResponse is the Recording Mode synthesis output for human review: the
// proposed least-privilege sandbox profile derived from what the run ACTUALLY
// did, Wardyn's DETERMINISTIC risk grade, and the raw observations the proposal
// was built from. It mirrors composeResponse so the UI can reuse the
// compose-review surface, plus an observations block.
type profileResponse struct {
	Kind           string                  `json:"kind"` // always "profile_proposal"
	Proposed       composeProposed         `json:"proposed"`
	RiskAssessment []composer.RiskItem     `json:"risk_assessment"`
	OverallRisk    composer.RiskLevel      `json:"overall_risk"`
	Observations   recordmode.Observations `json:"observations"`
	Warnings       []string                `json:"warnings,omitempty"`
}

// handleSynthesizeProfile is the Recording Mode endpoint: from a run's already-
// captured audit / egress-decision / eBPF-ground-truth events it synthesizes a
// tightened, reusable RunPolicy (a "sandbox profile"). It is ADVISORY and
// READ-ONLY — it mints nothing and creates no policy; a human reviews the
// proposal and saves it via the normal POST /api/v1/policies path. The
// synthesized spec flows through the SAME clamp+validate+grade pipeline as a
// composer proposal, so a recording can never produce a profile beyond operator
// policy, and the risk grade is computed from the spec fields — never from
// anything the (possibly prompt-injected) recorded session "said".
func (s *Server) handleSynthesizeProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// Owner-or-admin (getRunAuthorized): a member synthesizing a profile from a
	// run they did not create gets the same 404 a missing run would.
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}

	events, err := s.cfg.Store.QueryAuditEvents(ctx, id, maxCaptureAuditEvents)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query audit events: "+err.Error())
		return
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list grants: "+err.Error())
		return
	}

	// confined=false: this endpoint synthesizes a profile from ANY run's audit
	// trail (not only a "workspace record" confined-replay session — see
	// reconcileRecordRun in workspace_run.go for that lane, which passes the
	// session's actual Confined flag), and AgentRun carries no frozen
	// AllowAllEgress to derive it from here. Preserves today's behavior: a
	// deny is treated as an anomaly a synthesis must not silently bless,
	// which is the conservative/correct default for a run whose confinement
	// mode is not otherwise known at this call site.
	obs := recordmode.Capture(events, false)
	synth, synthWarns := recordmode.Synthesize(obs, grants, run)

	// The control plane itself shows up in every capture (the sandbox's
	// brokered result upload is a real, logged egress.allow) — same plumbing
	// the promote-egress path (record.go) excludes via selfHost. A synthesized
	// profile must not allowlist wardynd itself, even when the run's ceiling
	// was allow-all.
	if selfHost := controlPlaneHost(s.cfg.ControlPlaneURL); selfHost != "" {
		var kept []string
		for _, d := range synth.AllowedDomains {
			if strings.ToLower(strings.TrimSpace(d)) == selfHost {
				synthWarns = append(synthWarns, "host "+d+" is the Wardyn control plane itself; excluded from allowed_domains (plumbing, not a task need)")
				continue
			}
			kept = append(kept, d)
		}
		synth.AllowedDomains = kept
	}

	// Clamp to the operator ceiling, validate, and deterministically grade —
	// identical to the composer path (see compose.go) so a recording can never
	// mint a profile beyond operator policy and the grade is spec-derived.
	//
	// W23-S1-3: composer.Clamp now treats an EMPTY ceiling github_token repo
	// list as deny-all (the RBAC floor a hand-authored/member inline_policy
	// needs). synth's own github_token grant repos, if any, are already
	// provably real (recordmode.Synthesize derives them from grants the
	// SOURCE run actually held, themselves already clamped once at creation —
	// see synthGitHubRepos), so widen this call's own ceiling copy to that set
	// when the operator's ceiling itself sets none, or the new deny-all floor
	// would strip access this synthesis already proved legitimate.
	ceiling := widenCeilingRepoAllowlist(s.cfg.DefaultPolicy, synthGitHubRepos(synth))
	clamped, clampWarns := composer.Clamp(synth, ceiling)
	if verr := validatePolicySpec(clamped); verr != nil {
		writeError(w, http.StatusUnprocessableEntity, "synthesized profile invalid: "+verr.Error())
		return
	}
	runInput := composer.RunInput{
		Agent:            run.Agent,
		Repo:             run.Repo,
		Task:             run.Task,
		ConfinementClass: string(run.ConfinementClass),
		Interactive:      run.Interactive,
	}
	// Raise the run's confinement class to the clamped floor so the synthesized
	// profile is self-consistent (a run weaker than its policy floor is 422'd).
	var confWarn string
	runInput.ConfinementClass, confWarn = composer.ClampRunConfinement(runInput.ConfinementClass, clamped.MinConfinementClass)
	items := composer.Grade(runInput, clamped)
	overall := composer.OverallLevel(items)

	warnings := append(append([]string{}, synthWarns...), clampWarns...)
	if confWarn != "" {
		warnings = append(warnings, confWarn)
	}
	if len(events) >= maxCaptureAuditEvents {
		warnings = append(warnings, captureAuditTruncatedNote)
	}
	// W20-W20-groundtruth-mapper-4: same one-line eBPF sensor coverage state
	// reconcileRecordRun stamps onto RecordTaskResult.Caveats — a synthesized
	// profile is reviewed on this SAME evidence, so it carries the same honesty
	// note about how much of it is kernel-corroborated.
	if gt := s.ebpfGroundtruthCaveat(ctx); gt != "" {
		warnings = append(warnings, gt)
	}

	s.recordAudit(ctx, s.auditEvent(&id, actorTypeFromRequest(r), principalFromRequest(r), "run.record.synthesize",
		id.String(), "success", mustJSON(map[string]any{
			"allowed_domains": clamped.AllowedDomains,
			"eligible_grants": len(clamped.EligibleGrants),
			"anomalies":       len(obs.Anomalies),
		})))

	writeJSON(w, http.StatusOK, profileResponse{
		Kind:           "profile_proposal",
		Proposed:       composeProposed{Run: runInput, InlinePolicy: clamped},
		RiskAssessment: items,
		OverallRisk:    overall,
		Observations:   obs,
		Warnings:       warnings,
	})
}
