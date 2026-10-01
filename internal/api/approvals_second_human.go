// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envEgressSecondHuman opts a deployment IN to four-eyes on egress approvals:
// the human who DECIDES an egress_domain approval must not be the human who
// created the run. DEFAULT OFF — turning it on unprompted would deadlock every
// single-operator deployment, which is most of them.
const envEgressSecondHuman = "WARDYN_EGRESS_SECOND_HUMAN"

// EgressSecondHumanEnabled reports whether the four-eyes egress switch is on.
//
// Exported for exactly one caller — cmd/wardynd's boot-time local-mode check,
// which warns when the switch is combined with a mode that cannot enforce it.
// It is a function rather than a second cliutil.EnvBool at the boot site so
// the env NAME and the truthiness rule keep ONE definition: a boot guard that
// disagreed with the runtime gate about what "on" means would warn about a
// deployment that is fine, or stay silent for one that is not.
func EgressSecondHumanEnabled() bool { return envEnabled(envEgressSecondHuman) }

// envCapabilitySecondHuman is envEgressSecondHuman's twin for Azure DevOps
// capability escalations (a tool_call approval known by adoEscalationScope): the
// human who DECIDES one must not be the human who created the run. Without it,
// ownership is the whole member rule, so a run's creator can approve their own
// escalation, admin-class capabilities included. DEFAULT OFF, for the same
// single-operator-deadlock reason as the egress switch.
const envCapabilitySecondHuman = "WARDYN_CAPABILITY_SECOND_HUMAN"

// CapabilitySecondHumanEnabled is EgressSecondHumanEnabled for the capability
// switch, exported for the same single boot-time local-mode check.
func CapabilitySecondHumanEnabled() bool { return envEnabled(envCapabilitySecondHuman) }

// secondHumanGate maps an approval to the four-eyes switch that governs it and
// whether that switch is on. An empty name means no switch governs this kind
// (credential and the rest are admin-only already). It is the one place that
// pairs a kind with its switch: requireSecondHuman (the real gate) and
// secondHumanAllows (its read-only mirror) both ask it, so they cannot drift.
// Each branch names its const directly because envEnabled reads must be
// listed in envToggles (TestEnvTogglesListEveryEnvEnabledRead).
func secondHumanGate(ap types.ApprovalRequest) (name string, on bool) {
	if ap.Kind == types.ApprovalEgressDomain {
		return envEgressSecondHuman, envEnabled(envEgressSecondHuman)
	}
	if _, isADO := adoEscalationScope(ap); isADO {
		return envCapabilitySecondHuman, envEnabled(envCapabilitySecondHuman)
	}
	return "", false
}

// decideErrorClass names WHY a decision failed for the break-glass audit row,
// in the same three buckets the response switch below answers with — a class,
// never the raw error, since an audit row is read by people who did not make
// the request and a store error string can carry connection detail.
func decideErrorClass(err error) string {
	switch {
	case errors.Is(err, approval.ErrAlreadyDecided):
		return "already_decided"
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	default:
		return "error"
	}
}

// bypassRunID is the run a failed break-glass row points at, when this handler
// knows it. decide() holds the approval only on the paths that had a reason to
// load it, and the gate's own load is by value, so this is nil for a bodyless
// decision on an approval nothing else needed. That is honest rather than
// lossy: a FAILED decide writes no approval.decide row for the id to correlate
// with, and the row's target — the approval id — is the durable link either way.
func bypassRunID(ap types.ApprovalRequest, haveAP bool) *uuid.UUID {
	if !haveAP || ap.RunID == uuid.Nil {
		return nil
	}
	return &ap.RunID
}

// requireSecondHuman is decide's rule 8: under the switch that governs the
// approval's kind (secondHumanGate — envEgressSecondHuman for an egress_domain
// approval, envCapabilitySecondHuman for an Azure DevOps capability escalation),
// refuse a decision whose decider IS the run's creator. It returns ok=false
// having already written its own 4xx/5xx, exactly like the other rule helpers.
// Everything below says "egress" where it was written for the egress switch;
// the capability switch follows every one of those rules unchanged.
//
// The admin-token principal bypasses it, and that is stated here, in
// docs/OPERATIONS.md and in the threat model's residual list rather than left
// for someone to discover. A bare WARDYN_ADMIN_TOKEN caller is attributed
// system/admin-token (actorFromRequest) precisely because a shared
// token carries NO per-human identity — there is no second human to compare it
// against, and X-Wardyn-Principal is ignored off LocalMode specifically so a
// token bearer cannot forge one. Refusing the token instead would lock an
// operator out of their own approval queue the moment SSO breaks, which is when
// they need it most, so the bypass is the deliberate break-glass. It is NOT
// silent: each one writes approval.second_human.bypass, beside the
// actor_type=system approval.decide the decision itself emits. A deployment
// that wants the gate to actually bind must therefore treat the admin token as
// the break-glass credential it is — SSO configured, token held out of band.
// This function only REPORTS that bypass (its first return value); decide()
// writes the row once Decide() has succeeded, for the reason argued at the
// admin-token branch below.
//
// LocalMode refuses the gate outright — 503, not a comparison. The switch is
// UNENFORCEABLE there, and that is structural rather than a hole to patch:
// LocalMode is the no-auth bypass (humanOrAdminAuth injects Config.LocalOperator
// and authenticates nobody), so BOTH operands of "is the decider the creator"
// are client-supplied. actorFromRequest honors the DEV-ONLY X-Wardyn-Principal
// header in this mode by design (runs_policy.go), which makes the DECIDER
// forgeable; and run.CreatedBy comes from that same function at create
// (runs.go), which makes the CREATOR forgeable too. So comparing the INJECTED
// operator instead of the header — the obvious narrow fix — closes only the
// first half: a run created under `X-Wardyn-Principal: local:carol` is then
// decided by its real author with no header at all, because local:alice !=
// local:carol. Both arms are pinned in approvals_second_human_test.go.
//
// And the mode cannot be made to satisfy the gate honestly, because the only
// identity in it that is NOT client-supplied is a single deployment-wide
// constant — every request is the same principal, so a rule demanding a
// DIFFERENT human can never pass. Enforcing it correctly means refusing every
// egress decision forever. Refusing with a 503 that NAMES the incompatibility
// is the same outcome, arrived at honestly and once, instead of an operator
// discovering an unexplained deadlock or (worse) a gate they believe is binding
// that one curl defeats.
//
// This is NOT the single-dev machine only: Config.LocalTrustForwarder documents
// LocalMode as the compose/team topology too (server.go), so "nobody would turn
// it on there" was never a safe assumption.
//
// No audit event, matching the fail-closed 503 branch below rather than the
// bypass above: this refusal is a deployment-configuration answer that every
// caller gets identically and that the response itself states, not a decision
// about one principal. (A BOOT-time refusal would tell the operator earlier
// still; that belongs with cmd/wardynd's other boot-flag validation.)
//
// It returns (bypassSwitch, ok). `bypassSwitch` is the NAME of the switch the
// admin-token break-glass bypassed ("" when none was), and it is REPORTED
// rather than recorded here: decide() writes the
// approval.second_human.bypass row on BOTH of Decide()'s arms, so every bypass
// leaves a row and the row's OUTCOME says whether the decision it was spent on
// actually happened. Recording it here instead would lose that distinction —
// which is the whole reason the emit moved down.
//
// A run with an EMPTY created_by (system-created follow-on runs) has no human
// creator to be the same as, so the rule cannot apply and passes. Said out loud
// because a reader could reasonably expect empty to fail closed; here "closed"
// would mean refusing every decision on a run nobody authored, which no second
// human can ever unblock.
//
// That `run.CreatedBy == ""` test is REDUNDANT-BUT-DEFENSIVE today, and the note
// is here so nobody "simplifies" it away: it only changes the answer when the
// DECIDER's principal is also "", and every shape that yields an empty principal
// (no identity at all, an OIDC human with an empty sub) resolves to
// system/admin-token, which the bypass above returns on before reaching this
// line. So no behavioural test can distinguish it — which is exactly why it is
// worth keeping, since it is what holds this line correct if an empty principal
// ever becomes reachable.
func (s *Server) requireSecondHuman(w http.ResponseWriter, r *http.Request, id uuid.UUID, ap types.ApprovalRequest, run types.AgentRun, haveAP, haveRun bool) (string, bool) {
	if !envEnabled(envEgressSecondHuman) && !envEnabled(envCapabilitySecondHuman) {
		return "", true
	}
	actorType, principal := actorFromRequest(r)
	// The scope checks come first, for the admin-token caller too. Moving them
	// below the break-glass branch would write an approval.second_human.bypass row
	// for any admin-token caller while the switch is on — before
	// the kind, before the row is known to exist, and before the decision —
	// producing break-glass records for credential approvals this switch
	// never governs, for approval ids that do not exist, and for requests that
	// go on to 500 with no approval.decide beside them, while docs/ENV.md
	// ("Scoped to egress_domain only") and the threat model both describe the
	// row as the record of a four-eyes rule bypassed on a decision that
	// happened. The load is what makes "on an approval that exists" true, so it
	// is paid on the admin path as well — only when the switch is on.
	if !haveAP {
		var err error
		if ap, err = s.cfg.Approvals.Get(r.Context(), id); err != nil {
			writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
			return "", false
		}
	}
	sw, on := secondHumanGate(ap)
	if !on {
		return "", true // no switch governs this kind, or its switch is off
	}
	// The break-glass itself, now that both "does this gate apply" questions are
	// answered. Still ahead of the local-mode refusal below, so an admin token
	// remains the way past a switch that mode cannot enforce. Reported, not
	// recorded: a bypass spent on a decision that then FAILED is a real
	// break-glass use and must leave a row, but not one that says a decision
	// was made — only an emit after Decide() can tell those two apart.
	if actorType == types.ActorSystem && principal == adminTokenPrincipal {
		return sw, true
	}
	// AFTER the kind check on purpose: the switch governs egress decisions only,
	// so refusing here must not reach a credential or tool_call decision, which
	// are already admin-only and which this switch never claimed to gate.
	if s.cfg.LocalMode {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonEgressSecondHumanLocalMode, sw+
			" cannot be enforced in local mode: local mode authenticates nobody, so both the decider and"+
			" the run's creator are client-supplied and no request can prove a second human decided."+
			" Configure SSO to use this switch, or unset it")
		return "", false
	}
	if !haveRun {
		// Fail CLOSED on BOTH ways the run can be unavailable — a read error, and
		// a backend that has no run store at all (test wiring only; wardynd always
		// wires PG). Without the run we cannot prove the decider is not its
		// creator, and this gate exists for deployments that will not accept
		// "probably a different human". A nil Store must not read as a pass.
		var err error
		if s.cfg.Store == nil {
			err = errors.New("no run store configured")
		} else {
			run, err = s.cfg.Store.GetRun(r.Context(), ap.RunID)
		}
		if err != nil {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonRunUnreadable,
				sw+" is set, but this approval's run could not be read to verify a second human decided it")
			return "", false
		}
	}
	if run.CreatedBy == "" || run.CreatedBy != principal {
		return "", true
	}
	if sw == envCapabilitySecondHuman {
		sc, _ := adoEscalationScope(ap)
		s.refuse(w, r, authz.Deny(authz.ReasonSecondHumanRequired, id.String(), sw+
			" is set: a second human must decide this — you created this run, so someone else approves or denies its Azure DevOps access").
			OnRun(ap.RunID).With("capability", sc.Capability))
		return "", false
	}
	s.refuse(w, r, authz.Deny(authz.ReasonSecondHumanRequired, id.String(), sw+
		" is set: a second human must decide this — you created this run, so someone else approves or denies its egress").
		OnRun(ap.RunID).With("host", approvalHost(ap)))
	return "", false
}
