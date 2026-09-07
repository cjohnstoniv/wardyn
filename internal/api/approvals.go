// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decisionRequest is the approve/deny body.
type decisionRequest struct {
	Reason string `json:"reason"`
	// Scope/ExpiresAt are named decision_* ON THE WIRE deliberately, not for
	// symmetry with the stored column. A bare "scope" collides with the
	// approval's own RequestedScope — the host JSON that is part of the PENDING
	// dedup index, and the identifier the console already binds to on three
	// surfaces — so a client would POST one name and read back another. A bare
	// "expires_at" collides with this table's OTHER expiry concept: the EXPIRED
	// state and approval.ExpireStale, which age out stale PENDING requests
	// rather than bounding a grant somebody actually made.
	//
	// Both are omitempty, so every pre-scope client (SDK, console, the bodyless
	// shell callers) puts nothing on the wire and Normalize() keeps `run`.
	Scope     types.ApprovalScope `json:"decision_scope,omitempty"`
	ExpiresAt *time.Time          `json:"decision_expires_at,omitempty"`
}

// maxDecisionUntil bounds an `until`-scoped grant. A decision that outlives
// every plausible run is an `always` in disguise — without always's
// operator-only gate, its durable workspace row, or the revocation PUT that
// makes it undoable — so the write boundary refuses to record one.
const maxDecisionUntil = 30 * 24 * time.Hour

// handleListApprovals returns approvals filtered by ?state= (empty = all) and
// ?run_id= (empty = every run), paginated by ?limit=&offset= (see parseListPage).
//
// When the lister also implements the OPTIONAL approvalPageLister (the wardynd
// adapter does, promoting store.PG's ListApprovalsPage), the un-filtered list
// applies LIMIT/OFFSET at the DB — the console polls four single-state lists
// every 10s and decided rows are never deleted, so the unpaged read grew with
// deployment age. A lister without it (test fakes) keeps servePage's fetch-all
// fallback.
//
// ?run_id= stays on the fetch-all path and filters INSIDE the closure, i.e.
// before servePage windows the result. That ordering is the whole point: the
// list is requested_at DESC and capped at maxListLimit, so filtering after the
// window would drop a run older than the newest 1000 approvals from its own
// detail page — silently, with a PENDING badge of 0.
//
// Ownership scoping (item 2): a member's ?run_id= must name an owned run —
// checked via the SAME getRunAuthorized gate GET/kill/profile/grants use, so a
// foreign or unknown run_id answers with the byte-identical 404 (no existence
// oracle). A member's UNSCOPED list (no run_id) is narrowed to approvals on
// runs they created (store.ApprovalsByRunCreatorPager) — fail CLOSED, never an
// unscoped fallback, when the backend does not implement it.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	state := types.ApprovalState(r.URL.Query().Get("state"))
	switch state {
	case "", types.ApprovalPending, types.ApprovalApproved, types.ApprovalDenied, types.ApprovalExpired:
	default:
		writeError(w, http.StatusBadRequest, "invalid state filter")
		return
	}
	var runID uuid.UUID
	if raw := r.URL.Query().Get("run_id"); raw != "" {
		var err error
		if runID, err = uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_id")
			return
		}
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}

	if !s.isSecurityOperator(r.Context()) { // security tier sees the org-wide queue (http.go's isSecurityOperator)
		if runID == uuid.Nil {
			pager, capable := s.cfg.Approvals.(store.ApprovalsByRunCreatorPager)
			if !capable {
				writeError(w, http.StatusInternalServerError, "approval listing is not scoped for members on this backend")
				return
			}
			principal := principalFromRequest(r)
			servePage(w, page, func(p store.Page) ([]types.ApprovalRequest, error) {
				return pager.ListApprovalsPageByRunCreator(r.Context(), principal, state, p)
			}, nil)
			return
		}
		// ?run_id= given: prove ownership up front. Once proven, the fetch-all +
		// filter-by-runID path below is exactly as scoped as the admin path — it
		// can only ever surface THIS one, now-owned run's approvals.
		if _, ok := s.getRunAuthorized(w, r, runID); !ok {
			return
		}
	}

	var pageFn func(store.Page) ([]types.ApprovalRequest, error)
	if pl, ok := s.cfg.Approvals.(approvalPageLister); ok && runID == uuid.Nil {
		pageFn = func(p store.Page) ([]types.ApprovalRequest, error) {
			return pl.ListApprovalsPage(r.Context(), state, p)
		}
	}
	servePage(w, page, pageFn, func() ([]types.ApprovalRequest, error) {
		all, err := s.cfg.Approvals.List(r.Context(), state)
		if err != nil || runID == uuid.Nil {
			return all, err
		}
		out := make([]types.ApprovalRequest, 0, len(all))
		for _, ap := range all {
			if ap.RunID == runID {
				out = append(out, ap)
			}
		}
		return out, nil
	})
}

// approvalPageLister is the optional DB-paged read surface an ApprovalService
// may additionally implement (see handleListApprovals). Deliberately NOT part
// of ApprovalService: test doubles embedding the interface keep compiling, the
// exact rationale store.Pager documents.
type approvalPageLister interface {
	ListApprovalsPage(ctx context.Context, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error)
}

// handleApproveApproval transitions an approval to APPROVED. For credential
// approvals the broker mints inside the same transaction that observes the
// APPROVED state (handled by the broker on the next mint call); here we only
// record the human decision via the approval FSM. Owner-or-admin FOR
// egress_domain approvals ONLY (item 3 + HIGH-1 review fix): see decide().
func (s *Server) handleApproveApproval(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, true)
}

// handleDenyApproval transitions an approval to DENIED (fail closed).
// Owner-or-admin for egress_domain approvals only (item 3 + HIGH-1): see decide().
func (s *Server) handleDenyApproval(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, false)
}

// decide is the ONE chokepoint both verbs funnel through. The ORDER of the
// blocks below is the correctness argument, not a style choice:
//
//  1. parse the id and decode the body (pure input; depends on no row, so it
//     can discriminate nothing);
//  2. the MEMBER GATE, unchanged and unconditional;
//  3. the scope rules, which read the approval / run / workspace;
//  4. the optional second-human gate (rule 8, requireSecondHuman);
//  5. Decide(), then the durable `always` write-back.
//
// Step 2 must stay ahead of step 3. Run rule 4 (a scope on a non-egress_domain
// kind -> 400) before the ownership check and a member can distinguish "a
// credential approval exists on someone else's run" (400) from "no such
// approval" (404) — exactly the existence oracle authorizeMemberDecision's own
// comment goes out of its way to close, and that docs/OPERATIONS.md states as
// policy.
// Everything the scope rules add is therefore behind a 404 for a caller who has
// not already proven the approval exists, is egress_domain, and is theirs.
//
// Steps 1 and 2 are named helpers below, as are the two halves of step 3 that
// stand alone (rules 1–3, rules 5–7); decide itself keeps only the ORDER, rule
// 4 with the scope-gated load it depends on, and step 4, because the order is
// the part a reviewer has to be able to read without scrolling. Each helper
// writes its own 4xx and reports false: the reply strings are asserted verbatim
// by the API tests and belong next to the rule that refuses.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return
	}
	body, ok := decodeDecisionRequest(w, r)
	if !ok {
		return
	}

	// Loaded AT MOST ONCE each. The member gate needs both for its own checks
	// and the scope rules reuse whatever it loaded; an OPERATOR's decide read
	// nothing from the store before this change, and the scope-gated loads
	// further down keep it that way for the default `run` scope and for every
	// bodyless caller — the console's busiest action pays no new round trip.
	//
	// The gate loads BOTH rows or NEITHER, so its one flag seeds both here; they
	// diverge below, where rule 4 may load the approval alone and leave the run
	// unread for `always` to fetch.
	ap, run, loaded, ok := s.authorizeMemberDecision(w, r, id)
	if !ok {
		return
	}
	haveAP, haveRun := loaded, loaded

	// --- Scope rules. ALL of them run before Decide(), because PENDING ->
	// decided is one-way: a rule that answered 4xx afterwards would be
	// rejecting a decision the operator can no longer take back.
	scope := body.Scope

	// Rules 1–3, which need no row at all (see validateDecisionScope).
	if !validateDecisionScope(w, scope, body.ExpiresAt) {
		return
	}

	// H4 — the store loads, gated on SCOPE. Two conditions, not one: rule 4
	// needs ap.Kind, and rule 4 can ONLY ever fire on the operator path, since
	// the member gate above already forced egress_domain or 404. Gate the load
	// on `always` alone and an operator POSTing {"decision_scope":"once"} at a
	// CREDENTIAL approval gets a 200 and a persisted scope that means nothing —
	// precisely what rule 4 exists to refuse. Gate on ANY explicit scope, "run"
	// included: an explicit {"decision_scope":"run"} at a credential approval
	// was accepted and persisted before this, contradicting rule 4's own docs
	// and the CLI's --scope help. Only the truly bodyless/default "" path
	// skips the load.
	needAP := scope != ""
	if needAP && !haveAP {
		var err error
		if ap, err = s.cfg.Approvals.Get(r.Context(), id); err != nil {
			// The same answer the member gate gives for the same failed load,
			// and the same answer Decide() below gives for a row that vanished.
			writeError(w, http.StatusNotFound, "approval not found")
			return
		}
		haveAP = true
	}
	// Rule 4 — a scope must MEAN something on the kind it is written to, and the
	// two kinds differ in what "something" is.
	//
	// tool_call is bounded by the clamp, so no scope changes anything: refused.
	//
	// credential USED to be in the same bucket ("a credential mints exactly once
	// by construction"), and for once/until/always it still is. `run` is the one
	// exception, and it is the whole of B2's per-run credential lease
	// (docs/adoption/corp-network-onboarding-findings.md): a git_pat installs a
	// STANDING credential helper git invokes on every operation, so single-use
	// forced the operator to choose between a click per git op and standing
	// auto-issue of a real personal credential. A run-scoped decision is the
	// middle ground — one approval, re-mintable for this run's lifetime — and the
	// broker reads it RAW (leaseCoversRemint) so no legacy decision becomes one.
	//
	// Deliberately NOT narrowed to git_pat here: decide holds no grant, so
	// checking the kind would cost a load on the approval's grant_id for a rule
	// the broker already enforces at the only place a lease can be spent. A `run`
	// scope on another credential kind is recorded and simply leases nothing.
	if needAP && ap.Kind != types.ApprovalEgressDomain {
		if !(ap.Kind == types.ApprovalCredential && scope == types.ScopeRun) {
			writeError(w, http.StatusBadRequest,
				"decision_scope is only valid on an egress_domain approval (or \"run\" on a credential approval, for a per-run lease)")
			return
		}
	}

	// target is the workspace an `always` decision persists to — resolved by
	// rules 5–7 (resolveAlwaysTarget), consumed by the write-back after Decide().
	var target uuid.UUID
	if scope == types.ScopeAlways {
		if target, ok = s.resolveAlwaysTarget(w, r, ap, run, haveRun, approve); !ok {
			return
		}
	}

	// Rule 8 — the optional SECOND-HUMAN gate on egress decisions. Last of the
	// pre-Decide() rules for the same reason they are all pre-Decide(): PENDING
	// -> decided is one-way. It loads what it needs (and nothing when the switch
	// is off), so a deployment that has not opted in pays no round trip.
	//
	// It REPORTS the admin-token break-glass rather than recording it: the row
	// carries the OUTCOME of the decision the bypass was spent on, and no
	// decision has been made yet at this line. Both emits are below Decide() —
	// success on the decided path, failure on the error path — so every bypass
	// leaves a row (docs/ENV.md's promise) and no row claims a decision that
	// did not happen (the repair that moved the emit down here).
	bypassedSecondHuman, ok := s.requireSecondHuman(w, r, id, ap, run, haveAP, haveRun)
	if !ok {
		return
	}

	decidedByType, decidedBy := actorFromRequest(r)

	// approve -> State is the same translation approval.Decide used to do
	// internally; it now happens here because ApprovalDecision.State is the
	// single source of truth DecideApproval persists (no separate bool to
	// keep in sync).
	state := types.ApprovalDenied
	if approve {
		state = types.ApprovalApproved
	}
	result, err := s.cfg.Approvals.Decide(r.Context(), id, decidedByType, types.ApprovalDecision{
		State:     state,
		DecidedBy: decidedBy,
		Reason:    body.Reason,
		Scope:     scope,
		ExpiresAt: body.ExpiresAt,
	})
	if err != nil {
		// THE BREAK-GLASS LEAVES A ROW EVEN WHEN THE DECISION FAILS, because
		// what the row records is that a four-eyes rule was bypassed — and it
		// was, at the gate above, before this call. docs/ENV.md promises the
		// operator that EVERY admin-token bypass writes one; an emit that only
		// fires on success makes the count of break-glass uses depend on
		// whether the store happened to answer, so a probing caller who never
		// completes a decision leaves nothing behind at all.
		//
		// THE OUTCOME IS WHAT KEEPS THE EARLIER ROUND'S REPAIR: this row must
		// never claim a decision was made. "failure" plus the error class says
		// exactly what happened — the gate was passed, the decision was not —
		// and it is the absence of an approval.decide beside it that a reader
		// would otherwise have to notice for themselves. The scope checks that
		// moved ABOVE the break-glass branch still hold, so a failure row can
		// only ever name an egress_domain approval that exists.
		if bypassedSecondHuman {
			s.recordAudit(r.Context(), s.auditEvent(bypassRunID(ap, haveAP), decidedByType, decidedBy,
				"approval.second_human.bypass", id.String(), "failure", mustJSON(map[string]any{
					"reason": "admin_token_break_glass", "switch": envEgressSecondHuman,
					"error": decideErrorClass(err),
				})))
		}
		switch {
		// One sentinel: approval.ErrAlreadyDecided IS store.ErrAlreadyDecided.
		case errors.Is(err, approval.ErrAlreadyDecided):
			writeError(w, http.StatusConflict, "approval already decided")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "approval not found")
		default:
			writeError(w, http.StatusInternalServerError, "decide approval: "+err.Error())
		}
		return
	}
	// Counted HERE, at the one service call both handlers (and every non-human
	// decision path) funnel through — never in handleApprove/handleDeny, which
	// would each need their own increment. The counter cannot live in
	// approval.Decide itself: internal/api imports internal/approval, so the
	// dependency only runs this way.
	s.metrics.approvalDecided(approve)
	// The four-eyes break-glass on a decision that WAS made — the success half
	// of the pair, the failure half being the emit on Decide's error path
	// above. Recorded HERE and not inside the gate that detected it, so this
	// row can carry the outcome of the decision the bypass was spent on: only
	// this side of Decide() knows it happened, and only outcome="success"
	// honours the sentence docs/ENV.md, docs/OPERATIONS.md and the threat model
	// all use. Written from the gate's decision that the switch applied, so it
	// covers exactly the egress_domain approvals that exist and that the gate
	// would otherwise have blocked, and carrying the run id so the
	// actor_type=system approval.decide it sits beside is findable from it.
	if bypassedSecondHuman {
		s.recordAudit(r.Context(), s.auditEvent(&result.RunID, decidedByType, decidedBy,
			"approval.second_human.bypass", id.String(), "success", mustJSON(map[string]any{
				"reason": "admin_token_break_glass", "switch": envEgressSecondHuman,
			})))
	}
	if approve {
		s.learnVerifyEgress(r.Context(), result, decidedByType, decidedBy)
	}
	// OUTSIDE the `if approve` above, which IS the approve-only guard: placing
	// this call beside learnVerifyEgress makes deny·always a silent no-op behind
	// a green UI — an operator's permanent deny that never reaches the workspace.
	//
	// The guard reads result.DecisionScope, i.e. what DecideApproval's RETURNING
	// echoed back, not the scope we sent: if the store's SET clause ever stops
	// persisting the column, this write-back stops firing too, and one
	// deny·always test catches BOTH failures instead of neither.
	if result.DecisionScope == types.ScopeAlways {
		s.persistWorkspaceEgressDecision(r.Context(), result, target, approve, decidedByType, decidedBy)
	}
	writeJSON(w, http.StatusOK, result)
}

// decodeDecisionRequest is decide's step 1: the body, and NOTHING that depends
// on a row. It is its own function because its tolerances are the delicate part
// — three shipped clients depend on them — and they must not be re-litigated by
// somebody skimming decide for the authorization order.
//
// Decoded BY HAND on purpose — do NOT "unify" this with decodeStrict
// (helpers.go), even though its own comment calls itself the package's single
// JSON-body decode primitive. decodeStrict 400s EVERY decode error including
// io.EOF, and three shipped shell clients POST approve with no body at all and
// assert 200|202 (scripts/test-drive.sh:465 and :579, test/e2e/e2e.sh:420) —
// all three would break. It also sets DisallowUnknownFields, which would reject
// an older or newer client sending a field this build does not know.
//
// What DID tighten: a malformed or truncated body is a 400 instead of being
// swallowed. Swallowing was right while the only field was an optional reason;
// it is wrong now the body carries the decision's blast radius, because a
// truncated body leaves Scope == "", which Normalize() reads as `run` — a
// client that meant `once` would get a run-wide grant and a 200.
// io.ErrUnexpectedEOF still catches the truncation; only the genuinely empty
// body (io.EOF) stays tolerated.
func decodeDecisionRequest(w http.ResponseWriter, r *http.Request) (decisionRequest, bool) {
	var body decisionRequest
	if r.Body == nil {
		return body, true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return body, false
	}
	return body, true
}

// authorizeMemberDecision is decide's step 2, the MEMBER GATE — the whole of
// it, so that "everything after this point has proven ownership" is a single
// call a reviewer can check rather than a block they must read to the end of.
// An OPERATOR passes through having read nothing from the store, which is why
// the returned rows come with a `loaded` flag instead of being assumed present:
// it reports whether BOTH ap and run were read (member path) or NEITHER
// (operator path), and the scope-gated loads downstream pay for what they need.
//
// Member: may decide only an egress_domain approval raised by a run THEY own
// (item 3, narrowed by the HIGH-1 review fix). credential and tool_call
// approvals stay admin-only REGARDLESS of ownership: the shipped default policy
// requires approval on github_token, so letting a member self-approve their OWN
// run's credential request would self-mint a real token, and self-approving a
// tool_call re-opens exactly the allowance the clamp (item 5 / HIGH-2) is
// supposed to bound — both under the SAME authority the operator's ceiling
// exists to constrain. Kind is checked before ownership so a foreign non-egress
// approval and an OWNED non-egress approval read identically (both 404, no
// existence oracle either way) — not audited: this is a foreign-shaped 404, not
// a distinct reachable-surface denial (see THREAT-MODEL.md).
//
// Then, and only then, the 0.6 egress_host capability: which hosts a member may
// decide FOR THEMSELVES. Ordered last on purpose — see the block itself.
func (s *Server) authorizeMemberDecision(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.ApprovalRequest, types.AgentRun, bool, bool) {
	var (
		ap  types.ApprovalRequest
		run types.AgentRun
	)
	if s.isSecurityOperator(r.Context()) { // security tier decides any kind on any run; LOCKSTEP pair, see http.go
		return ap, run, false, true
	}
	var err error
	ap, err = s.cfg.Approvals.Get(r.Context(), id)
	if err != nil || ap.Kind != types.ApprovalEgressDomain {
		writeError(w, http.StatusNotFound, "approval not found")
		return ap, run, false, false
	}
	var rerr error
	run, rerr = s.cfg.Store.GetRun(r.Context(), ap.RunID)
	if rerr != nil || !s.ownsRunOrAdmin(r, run) {
		writeError(w, http.StatusNotFound, "approval not found")
		if rerr == nil {
			// M1: audited only once the approval is confirmed to genuinely exist
			// and be decidable in kind — a run lookup failure here would be a
			// data-integrity oddity, not a clean "not owned".
			s.recordAudit(r.Context(), s.auditEvent(&ap.RunID, actorTypeFromRequest(r), principalFromRequest(r),
				"authz.denied", id.String(), "denied", mustJSON(map[string]any{"reason": "not_owner"})))
		}
		return ap, run, false, false
	}

	// The egress_host capability, LAST — after kind and ownership are both
	// proven, so it can never become the existence oracle the two 404s above
	// exist to deny: a member who reaches this line already knows the approval
	// is theirs and which host it asks for. That is also why the refusal is a
	// 403 rather than another byte-identical 404, exactly as resolveAlwaysTarget
	// argues for its own operator-only refusal.
	//
	// The host is the approval's OWN RequestedScope — never anything the client
	// sent — so a member cannot pick which value gets checked. An absent or
	// malformed scope resolves to "", which no allow can cover unless the admin
	// wrote a `*` grant: fail closed on a shape nobody should be deciding.
	//
	// `always` is unaffected: it stays operator-only (rule 6), so a grant here
	// never promotes a member's decision into durable workspace config.
	host := approvalHost(ap)
	allowed, cerr := s.capSeamAllowed(r.Context(), capEgressHost, host)
	if cerr != nil {
		writeError(w, http.StatusInternalServerError, "resolve capability: "+cerr.Error())
		return ap, run, false, false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "you are not granted egress host "+host+
			" — an admin decides this one, or can grant it to you")
		s.recordAudit(r.Context(), s.auditEvent(&ap.RunID, actorTypeFromRequest(r), principalFromRequest(r),
			"authz.denied", id.String(), "denied", mustJSON(map[string]any{
				"reason": "capability_" + capEgressHost, "host": host,
			})))
		return ap, run, false, false
	}
	return ap, run, true, true
}

// envEgressSecondHuman opts a deployment IN to four-eyes on egress approvals:
// the human who DECIDES an egress_domain approval must not be the human who
// created the run. DEFAULT OFF — turning it on unprompted would deadlock every
// single-operator deployment, which is most of them.
const envEgressSecondHuman = "WARDYN_EGRESS_SECOND_HUMAN"

// EgressSecondHumanEnabled reports whether the four-eyes egress switch is on.
//
// Exported for exactly one caller — cmd/wardynd's boot-time local-mode check,
// which warns when the switch is combined with a mode that cannot enforce it.
// It is a function rather than a second os.Getenv at the boot site so the env
// NAME and the truthiness rule keep ONE definition: a boot guard that disagreed
// with the runtime gate about what "on" means would warn about a deployment that
// is fine, or stay silent for one that is not.
func EgressSecondHumanEnabled() bool { return envEnabled(os.Getenv(envEgressSecondHuman)) }

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

// requireSecondHuman is decide's rule 8: under envEgressSecondHuman, refuse an
// egress_domain decision whose decider IS the run's creator. It returns false
// having already written its own 4xx/5xx, exactly like the other rule helpers.
//
// THE ADMIN-TOKEN PRINCIPAL BYPASSES IT, and that is stated here, in
// docs/OPERATIONS.md and in the threat model's residual list rather than left
// for someone to discover. A bare WARDYN_ADMIN_TOKEN caller is attributed
// system/admin-token (actorFromRequest, FIX #10) precisely because a shared
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
// LOCALMODE REFUSES THE GATE OUTRIGHT — 503, not a comparison. The switch is
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
// It returns (bypassed, ok). `bypassed` marks the admin-token break-glass, and
// it is REPORTED rather than recorded here: decide() writes the
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
func (s *Server) requireSecondHuman(w http.ResponseWriter, r *http.Request, id uuid.UUID, ap types.ApprovalRequest, run types.AgentRun, haveAP, haveRun bool) (bool, bool) {
	if !envEnabled(os.Getenv(envEgressSecondHuman)) {
		return false, true
	}
	actorType, principal := actorFromRequest(r)
	// THE SCOPE CHECKS COME FIRST, for the admin-token caller too. They used to
	// sit below the break-glass branch, so an approval.second_human.bypass row
	// was written for any admin-token caller while the switch was on — before
	// the kind, before the row was known to exist, and before the decision.
	// That produced break-glass records for credential approvals this switch
	// never governs, for approval ids that do not exist, and for requests that
	// went on to 500 with no approval.decide beside them, while docs/ENV.md
	// ("Scoped to egress_domain only") and the threat model both describe the
	// row as the record of a four-eyes rule bypassed on a decision that
	// happened. The load is what makes "on an approval that exists" true, so it
	// is paid on the admin path as well — only when the switch is on.
	if !haveAP {
		var err error
		if ap, err = s.cfg.Approvals.Get(r.Context(), id); err != nil {
			writeError(w, http.StatusNotFound, "approval not found")
			return false, false
		}
	}
	if ap.Kind != types.ApprovalEgressDomain {
		return false, true // the switch is scoped to egress decisions
	}
	// The break-glass itself, now that both "does this gate apply" questions are
	// answered. Still ahead of the local-mode refusal below, so an admin token
	// remains the way past a switch that mode cannot enforce. Reported, not
	// recorded: a bypass spent on a decision that then FAILED is a real
	// break-glass use and must leave a row, but not one that says a decision
	// was made — only an emit after Decide() can tell those two apart.
	if actorType == types.ActorSystem && principal == adminTokenPrincipal {
		return true, true
	}
	// AFTER the kind check on purpose: the switch governs egress decisions only,
	// so refusing here must not reach a credential or tool_call decision, which
	// are already admin-only and which this switch never claimed to gate.
	if s.cfg.LocalMode {
		writeError(w, http.StatusServiceUnavailable, envEgressSecondHuman+
			" cannot be enforced in local mode: local mode authenticates nobody, so both the decider and"+
			" the run's creator are client-supplied and no request can prove a second human decided."+
			" Configure SSO to use this switch, or unset it")
		return false, false
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
			writeError(w, http.StatusServiceUnavailable,
				envEgressSecondHuman+" is set, but this approval's run could not be read to verify a second human decided it")
			return false, false
		}
	}
	if run.CreatedBy == "" || run.CreatedBy != principal {
		return false, true
	}
	writeError(w, http.StatusForbidden, envEgressSecondHuman+
		" is set: a second human must decide this — you created this run, so someone else approves or denies its egress")
	s.recordAudit(r.Context(), s.auditEvent(&ap.RunID, actorType, principal,
		"authz.denied", id.String(), "denied", mustJSON(map[string]any{
			"reason": "second_human_required", "host": approvalHost(ap),
		})))
	return false, false
}

// validateDecisionScope is scope rules 1–3 — the ones that read ONLY the body.
// Kept together and kept row-free on purpose: a 400 from here discriminates
// nothing, so these three are the only scope rules whose position is not itself
// a security argument. They still run AFTER the member gate, and rules 4–7 —
// which read the approval / run / workspace — must (see decide's own comment).
//
// Rule 1 — garbage at the write boundary, the same place FirstUseMode.Valid is
// enforced. Runtime reads still fail closed via Normalize().
//
// Rules 2 and 3 — `until` and decision_expires_at imply each other, BOTH
// directions. An expiry on a non-until scope is never silently ignored: a
// client that sent one believes the grant is time-boxed, and it would not be.
func validateDecisionScope(w http.ResponseWriter, scope types.ApprovalScope, expiresAt *time.Time) bool {
	if !scope.Valid() {
		writeError(w, http.StatusBadRequest, "invalid decision_scope (once|run|until|always)")
		return false
	}
	switch {
	case scope == types.ScopeUntil:
		now := time.Now()
		switch {
		case expiresAt == nil:
			writeError(w, http.StatusBadRequest, "decision_scope until requires decision_expires_at")
			return false
		case !expiresAt.After(now):
			writeError(w, http.StatusBadRequest, "decision_expires_at is in the past")
			return false
		case expiresAt.After(now.Add(maxDecisionUntil)):
			writeError(w, http.StatusBadRequest, "decision_expires_at is more than 30d out — use always for a permanent decision")
			return false
		}
	case expiresAt != nil:
		writeError(w, http.StatusBadRequest, "decision_expires_at is only valid with decision_scope until")
		return false
	}
	return true
}

// resolveAlwaysTarget is scope rules 5–7: everything `always` — and ONLY
// `always` — has to establish before Decide() makes the decision permanent. It
// returns the workspace the write-back will persist to, or false having already
// answered the request. It owns the whole `always` pre-flight so the two facts
// that make it correct stay adjacent: the caller's role is checked before any
// run state is read, and EVERY refusal in here is still a 4xx/5xx that leaves
// the approval PENDING and re-decidable.
//
// run/haveRun carry whatever the member gate already loaded — an operator
// arrives with haveRun false and pays for the GetRun here, on the one scope
// that genuinely needs it.
func (s *Server) resolveAlwaysTarget(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, run types.AgentRun, haveRun, approve bool) (uuid.UUID, bool) {
	// Rule 6 FIRST — `always` is operator-only. The approve/deny routes live on
	// the MEMBER group, so without this a member self-grants a permanent
	// workspace allowlist entry through the approval queue: a back door around
	// PUT /workspaces/{id}/approved-egress, which is operator-gated on this
	// exact predicate.
	//
	// AUTHORIZATION BEFORE VALIDATION, deliberately. Rule 5 below answers 400
	// with "this run references no onboarded workspace" — a fact about the run's
	// configuration. Running it first would hand that answer to a caller who is
	// not permitted to use this scope at all, and would make the reply depend on
	// run state rather than on the caller's role. Both orders are defensible for
	// THIS field (the member already owns the run, so nothing here is secret),
	// but ordering by "can you do this at all?" before "is your request
	// well-formed?" is the rule that keeps working when a later validation step
	// touches something the caller genuinely cannot see.
	//
	// 403, NOT the byte-identical 404 the member gate uses. Those 404s exist to
	// deny an existence oracle; by HERE the caller has already proven the
	// approval exists, is egress_domain, and is on a run they own, so a 403
	// discloses nothing they do not already know — while a 404 would read as
	// "your own approval vanished". Do not "fix" this back.
	if !s.isSecurityOperator(r.Context()) { // LOCKSTEP with authorizeMemberDecision; see http.go
		writeError(w, http.StatusForbidden, "decision_scope always is operator-only")
		return uuid.Nil, false
	}

	// Rule 5 — `always` writes to a WORKSPACE, so the run must resolve to one.
	// s.cfg.Store is nilable throughout this package (learnVerifyEgress guards
	// it for the same reason), and a backend that cannot resolve the linkage
	// cannot serve this scope at all.
	if s.cfg.Store == nil {
		writeError(w, http.StatusInternalServerError, "decision_scope always is unavailable on this backend")
		return uuid.Nil, false
	}
	if !haveRun {
		var err error
		if run, err = s.cfg.Store.GetRun(r.Context(), ap.RunID); err != nil {
			// REJECT, never fall through: falling through would persist `always`
			// on the approval row with no workspace resolved and no durable
			// write — a permanent grant that exists only in the UI.
			writeError(w, http.StatusInternalServerError, "resolve run for always: "+err.Error())
			return uuid.Nil, false
		}
	}
	// The tie-break is spelled out because WorkspaceIDs[0] is undefined on
	// exactly the path that matters most: a record/verify run launches with
	// first_use_approval=wait_for_review and therefore raises egress approvals BY
	// CONSTRUCTION, yet newWorkspaceStepRun leaves WorkspaceIDs empty and sets
	// only the TRUSTED WorkspaceID. WorkspaceIDs[0] is the documented PRIMARY
	// (referencedWorkspaces builds the slice in a stable order — mounts, then
	// repos, deduped) and the same entry that drives image selection, so "first"
	// is deterministic and explainable in copy. No workspace_id body field and
	// no picker: a documented default beats a new wire field plus a new control
	// for a case that is rare and reversible via the
	// denied-egress/approved-egress PUTs.
	target := primaryWorkspace(run)
	if target == uuid.Nil {
		// "no recorded workspace link", not "references no workspace": a run
		// created before migration 0041 has a NULL workspace_ids even when it
		// referenced one — the server only knows what was recorded.
		writeError(w, http.StatusBadRequest, "always needs a workspace: this run has no recorded workspace link")
		return uuid.Nil, false
	}

	// Rule 7 — host shape, then the reject set for THIS DIRECTION. Both are
	// checked here, before Decide(), because this is the last moment a 4xx is
	// still meaningful.
	host := approvalHost(ap)
	if !hostrules.ValidApprovedHost(host) {
		writeError(w, http.StatusBadRequest,
			"invalid domain (plain lowercase host, no scheme/port/wildcard): "+host)
		return uuid.Nil, false
	}
	ws, err := s.cfg.Store.GetWorkspace(r.Context(), target)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "always needs a workspace: this run's workspace no longer exists")
			return uuid.Nil, false
		}
		writeError(w, http.StatusInternalServerError, "resolve workspace for always: "+err.Error())
		return uuid.Nil, false
	}
	if approve {
		if _, dead := s.approveAlwaysRejects(r.Context(), ws)[host]; dead {
			// The reason list is deliberately hedged rather than enumerated.
			// promoteSkipHosts carries the model-provider and clone hosts VERBATIM
			// from the ceiling, so on the wildcard ceiling the product itself
			// recommends ("*.anthropic.com") a CONCRETE model-provider host is not
			// in this set at all — naming it unconditionally would tell the
			// operator something that is often false.
			writeError(w, http.StatusBadRequest, "host "+host+" is already routed or wired in by construction "+
				"(git broker, control plane, or this workspace's own contract) — a permanent approved-egress entry for it is never consulted")
			return uuid.Nil, false
		}
	} else if msg := s.denyAlwaysReject(r.Context(), ws, host); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return uuid.Nil, false
	}
	return target, true
}
