// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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

	if !s.isOperator(r.Context()) {
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
//  4. Decide(), then the durable `always` write-back.
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
	// Rule 4 — only egress_domain decisions carry a scope. A credential mints
	// exactly once by construction (minted_jti) and a tool_call is bounded by
	// the clamp, so a scope on either is a stored field that grants nothing.
	if needAP && ap.Kind != types.ApprovalEgressDomain {
		writeError(w, http.StatusBadRequest, "decision_scope is only valid on an egress_domain approval")
		return
	}

	// target is the workspace an `always` decision persists to — resolved by
	// rules 5–7 (resolveAlwaysTarget), consumed by the write-back after Decide().
	var target uuid.UUID
	if scope == types.ScopeAlways {
		if target, ok = s.resolveAlwaysTarget(w, r, ap, run, haveRun, approve); !ok {
			return
		}
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
	if s.isOperator(r.Context()) {
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
	if !s.isOperator(r.Context()) {
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

// primaryWorkspace is an `always` decision's target workspace: WorkspaceIDs[0]
// (the documented PRIMARY — referencedWorkspaces builds it in a stable order,
// mounts then repos, deduped, and it also drives image selection), else the
// trusted scan/verify/record WorkspaceID, else uuid.Nil when the run records no
// workspace link. Shared by resolveAlwaysTarget's live write-back and
// ReconcileWorkspaceEgressDecisions' boot heal so both resolve the same target.
func primaryWorkspace(run types.AgentRun) uuid.UUID {
	switch {
	case len(run.WorkspaceIDs) > 0:
		return run.WorkspaceIDs[0]
	case run.WorkspaceID != nil:
		return *run.WorkspaceID
	default:
		return uuid.Nil
	}
}

// ReconcileWorkspaceEgressDecisions re-applies every decided `always`-scoped
// egress decision to its run's primary workspace, healing D28: the post-Decide
// write-back (persistWorkspaceEgressDecision) is not atomic with Decide, so a PG
// blip there dropped a permanent allow/deny behind a 200 with only a failure
// audit row — future runs then never inherited the operator's decision.
// AddWorkspaceEgressDecision is idempotent (an upsert that also clears the mirror
// list), so re-applying an already-persisted decision is a no-op and a dropped one
// is recreated. Returns the count re-applied, for the boot log.
//
// This is the SMALLER of the two options the finding names (a boot/periodic
// reconcile vs threading one tx through Decide + the workspace write, which spans
// two service interfaces the api layer does not share a tx across). It runs once
// at boot (cmd/wardynd). ponytail: boot-only heals on the next restart; a periodic
// tick would heal sooner on a laptop that rarely reboots — add one if that window
// proves too wide. The scan reads all decided egress approvals; decided rows are
// never deleted, so on a very long-lived deployment cap this with a marker-keyed
// incremental reconcile.
func (s *Server) ReconcileWorkspaceEgressDecisions(ctx context.Context) (int, error) {
	if s.cfg.Store == nil || s.cfg.Approvals == nil {
		return 0, nil
	}
	reconciled := 0
	for _, state := range []types.ApprovalState{types.ApprovalApproved, types.ApprovalDenied} {
		aps, err := s.cfg.Approvals.List(ctx, state)
		if err != nil {
			return reconciled, err
		}
		allow := state == types.ApprovalApproved
		for _, ap := range aps {
			if ap.Kind != types.ApprovalEgressDomain || ap.DecisionScope.Normalize() != types.ScopeAlways {
				continue
			}
			host := approvalHost(ap)
			if host == "" || !hostrules.ValidApprovedHost(host) {
				continue
			}
			run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
			if err != nil {
				continue // run gone; nothing to persist onto
			}
			target := primaryWorkspace(run)
			if target == uuid.Nil {
				continue
			}
			// Best-effort, exactly like the live write-back: a deleted workspace or a
			// cap-reached list is skipped, not fatal to the rest of the reconcile.
			if _, err := s.cfg.Store.AddWorkspaceEgressDecision(ctx, target, host, allow, maxApprovedEgress); err != nil {
				continue
			}
			reconciled++
		}
	}
	return reconciled, nil
}

// approvalHost extracts an egress_domain approval's lowercased host from its
// RequestedScope JSON ("" when absent or malformed) — the same shape the proxy
// authors and approval.requestedScopeHost reads for the audit stream.
func approvalHost(ap types.ApprovalRequest) string {
	var scope struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(ap.RequestedScope, &scope) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(scope.Host))
}

// approveAlwaysRejects is the set of hosts an approve·always must refuse:
// entries a real run's proxy will never consult, so promoting one writes dead
// weight the operator believes is granting them something — the same honesty
// rule handleSetApprovedEgress and promoteSkipHosts already enforce at the
// other two write points.
//
// It is the UNION of those two sets, because they are NOT identical and neither
// is a superset: promoteSkipHosts (workspace-aware — model provider, the
// workspace's own bedrock transport, required-integration hosts, clone hosts,
// broker + brokered-SSH hosts) lacks only the control plane's OWN host, which
// handleSetApprovedEgress's inline static set carries. controlPlaneHost already
// lowercases, matching how that set inserts it raw.
//
// ALLOW-SHAPED ONLY — do not reuse this for the deny direction. A deny entry for
// a git-broker host IS consulted (runs_dispatch_gitbroker.go reads and extends
// policy.DeniedDomains), so the "never consulted" rationale does not transfer,
// and the deny direction's real hazard is the opposite one (see
// denyAlwaysReject).
func (s *Server) approveAlwaysRejects(ctx context.Context, ws types.Workspace) map[string]struct{} {
	skip := s.promoteSkipHosts(ctx, ws)
	if self := controlPlaneHost(s.cfg.ControlPlaneURL); self != "" {
		skip[self] = struct{}{}
	}
	return skip
}

// denyAlwaysReject reports why a deny·always on host must be refused, or "" to
// allow it. It is deliberately NOT approveAlwaysRejects' mirror: the hazard is
// not symmetric, and one shared set would be wrong in both directions.
// approve·always on api.anthropic.com is merely redundant, while deny·always on
// it BRICKS the workspace — deny beats everything the proxy evaluates, and
// Policy.AllowedExactHost (the gate for proxy-side credential injection)
// returns false on a denied host, so every future run of this workspace would
// launch with a model credential it can never use. An injected INTEGRATION host
// is worse still: buildInjector returns an error for a rule whose host is not
// exactly allowlisted, failing the sidecar outright rather than quietly
// disabling one credential.
//
// Best-effort BY DESIGN, and the message says so rather than implying the check
// is exhaustive. The hazard class is every host carrying a proxy-side injection
// rule (model providers, header-delivered integrations, artifact redirects);
// the two guarded here are the two that fail SILENTLY. The rest fail loudly at
// proxy build, where an operator can see and undo them.
func (s *Server) denyAlwaysReject(ctx context.Context, ws types.Workspace, host string) string {
	const caveat = " (this guard covers model-provider and required-integration hosts only; " +
		"a deny on another injected host fails loudly at proxy build instead)"
	if isModelProviderHost(host) {
		return "deny always on " + host + " would permanently break model access for this workspace: " +
			"proxy-side credential injection refuses a denied host" + caveat
	}
	for _, h := range s.integrationRequirementHosts(ctx, ws) {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return "deny always on " + host + " would break a required integration this workspace declares: " +
				"its credential is injected at that host" + caveat
		}
	}
	// M1 — a permanent deny must not contradict the workspace's own contract.
	if requiredEgressHost(ws, host) {
		return "deny always on " + host + " contradicts this workspace's requirements contract, which marks " +
			"egress:" + host + " required — a confined replay unions every required egress row into its own " +
			"allowlist, so the workspace would declare a need it can never satisfy. Turn that requirement off " +
			"(or set it optional) on the workspace first."
	}
	return ""
}

// requiredEgressHost reports whether ws's EFFECTIVE contract marks
// egress:<host> required — the M1 self-contradiction check behind
// denyAlwaysReject. confinedEgressDomains unions exactly these rows into a
// confined replay's AllowedDomains, so a workspace that both requires and
// permanently denies one host fails every replay on it. The path is reachable,
// not hypothetical: learnVerifyEgress writes precisely such a row on approve,
// so approve·always then deny·always the same host is one operator away.
//
// Refusing is chosen over silently clearing the requirement: this runs BEFORE
// Decide(), where a 4xx still means something and the operator learns which
// knob to turn, whereas clearing the row would delete an operator-declared
// contract entry as an invisible side effect of an approval click — from the
// write-back, after the decision is already durable and unauditable as a
// rejection.
func requiredEgressHost(ws types.Workspace, host string) bool {
	for key, req := range effectiveRequirements(ws) {
		if req.Level != "required" {
			continue
		}
		if typ, h, ok := types.SplitRequirementKey(key); ok && typ == "egress" &&
			strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

// persistWorkspaceEgressDecision is `always`'s durable half: the host the
// operator just decided lands on the run's PRIMARY workspace — approved_egress
// on approve, denied_egress on deny, and removed from the other list either way
// (deny beats allow everywhere the proxy evaluates policy, so a host left on
// both would make one direction a silent no-op) — so FUTURE runs inherit the
// decision instead of re-raising it.
//
// Host shape and the two direction-specific reject sets are validated in
// decide()'s rule 7, BEFORE Decide() flips the row. Nothing validating belongs
// here: this runs after a decision that is already durable and cannot be taken
// back, and answering 4xx on it would be worse than useless. Only genuine
// runtime failures reach here — cap reached, workspace deleted mid-flight — and
// they fail SILENT-BUT-AUDITED exactly like learnVerifyEgress: the approval
// itself stands either way, and the audit record is what lets an operator add
// the row by hand instead of wondering why the next run still asks.
//
// Audited under the workspace.egress.approve namespace the approved-egress PUT
// already owns, plus its workspace.egress.deny sibling, and with the same
// {"domains": [...]} payload shape, so one audit query answers "how did this
// host get onto this workspace's list" across all three writers.
func (s *Server) persistWorkspaceEgressDecision(ctx context.Context, ap types.ApprovalRequest, wsID uuid.UUID, allow bool, byType types.ActorType, by string) {
	action := "workspace.egress.deny"
	if allow {
		action = "workspace.egress.approve"
	}
	host := approvalHost(ap)
	data := map[string]any{"domains": []string{host}, "source": "approval:" + ap.ID.String()}
	// AUDIT the give-up paths too — this function's contract is fail-SILENT-BUT-
	// AUDITED, and a bare `return` here delivered only the first half. Both are
	// reachable and neither is cosmetic: a nil Store means the decision stands
	// with nothing durable behind it, and an empty host means we re-derived it
	// from the post-Decide RETURNING row rather than the `ap` rule 7 validated —
	// so if that RETURNING ever stops listing requested_scope, `always` silently
	// becomes a no-op. That is the same failure class as the SET-clause trap this
	// package already warns about, and it deserves the same visibility: an
	// operator who clicked Always and got a green UI must be able to find out
	// from the audit stream that nothing was written.
	if s.cfg.Store == nil || host == "" {
		reason := "no store configured"
		if s.cfg.Store != nil {
			reason = "approval carries no host in requested_scope"
		}
		data["detail"] = reason
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, action, wsID.String(), "failure", mustJSON(data)))
		return
	}
	if _, err := s.cfg.Store.AddWorkspaceEgressDecision(ctx, wsID, host, allow, maxApprovedEgress); err != nil {
		data["detail"] = err.Error()
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, action, wsID.String(), "failure", mustJSON(data)))
		return
	}
	s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, action, wsID.String(), "success", mustJSON(data)))
}

// learnVerifyEgress is the verify loop's write-back, hooked at the ONE
// chokepoint every approval decision funnels through: approving an
// egress_domain request raised DURING a workspace verify/record session lands
// the host as an `egress:<host>` row in THAT workspace's own requirements
// contract (required, operator_set — a human just clicked) the moment the
// decision is made. The gates are the trusted linkages, never sandbox input:
// the run row's WorkspaceID and its "workspace record" task discriminator — a
// PLAIN run's approval widens only its own run and writes NOTHING durable.
// The row lands on the WORKSPACE overlay, never a shared library source:
// approving a host for this aggregate must not leak the approval into every
// other workspace attaching the same source. Every guard fails silent — the
// approval itself already stands; this is the durable echo, not the decision.
func (s *Server) learnVerifyEgress(ctx context.Context, ap types.ApprovalRequest, byType types.ActorType, by string) {
	if ap.Kind != types.ApprovalEgressDomain || s.cfg.Store == nil {
		return
	}
	// Scope gate, reading the RETURNING echo (ap.DecisionScope) like the
	// deny·always write-back does: an explicitly EPHEMERAL decision must not
	// leave the widest durable mark this handler can make. `once` promised
	// "the next attempt asks again"; `until` promised a time-box — a permanent
	// required contract row contradicts both. ""/run (the verify loop's normal
	// path) and always keep learning. If time-boxed grants should teach the
	// contract after all, drop ScopeUntil here — a one-line owner decision.
	if sc := ap.DecisionScope.Normalize(); sc == types.ScopeOnce || sc == types.ScopeUntil {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
	if err != nil || run.WorkspaceID == nil || run.Task != "workspace record" {
		return
	}
	var scope struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(ap.RequestedScope, &scope) != nil {
		return
	}
	host := strings.ToLower(strings.TrimSpace(scope.Host))
	if host == "" || !hostrules.ValidApprovedHost(host) {
		// W19-W19b-5: this guard used to fail silent, unlike the merge-failure
		// path below it — an operator who approved a request whose requested_scope
		// carried an empty or malformed host got a green UI and no durable write,
		// with nothing in the audit trail to explain why the workspace never
		// learned the host.
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
			run.WorkspaceID.String(), "failure", mustJSON(map[string]any{
				"source": "verify:" + ap.RunID.String(), "detail": "invalid or empty host in requested_scope",
			})))
		return
	}
	key := "egress:" + host
	if _, err := s.cfg.Store.MergeWorkspaceRequirements(ctx, *run.WorkspaceID, map[string]types.WorkspaceRequirement{
		key: {Level: "required", Provenance: "operator_set"},
	}); err != nil {
		// The approval stands either way; the contract write is audited as the
		// miss it is (cap hit / workspace gone) so the operator can add the row
		// on Reach instead of wondering why the replay still denies the host.
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
			run.WorkspaceID.String(), "failure", mustJSON(map[string]any{
				"key": key, "source": "verify:" + ap.RunID.String(), "detail": err.Error(),
			})))
		return
	}
	s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
		run.WorkspaceID.String(), "success", mustJSON(map[string]any{
			"key": key, "source": "verify:" + ap.RunID.String(),
		})))
}
