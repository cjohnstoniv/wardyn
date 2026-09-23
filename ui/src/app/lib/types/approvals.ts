/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approval requests — human-gated credential/egress/tool decisions.
// credential_reauth (0.7.6): the run's OWN model credential lapsed mid-run and
// the proxy is holding its next credential exchange while the credential's
// owner signs in again. It is a REQUEST, never a decision — see
// canDecideApproval below and internal/types/types.go ApprovalCredentialReauth.
// push_content (#181/#494): a brokered git push matched push_rules.
// require_review_paths and is parked at the proxy for an admin's decision —
// see PushContentScope below and internal/types/push_content.go.
export type ApprovalKind = "credential" | "egress_domain" | "tool_call" | "credential_reauth" | "push_content";

// CANCELLED is the terminal state a run's own end writes: the run reached
// COMPLETED/FAILED/STOPPED/KILLED while this approval was still PENDING, so
// nobody decided it (internal/types/types.go ApprovalCancelled). Mirrors the Go
// union; hand-maintained, so a value added there and not here is silent drift.
export type ApprovalState = "PENDING" | "APPROVED" | "DENIED" | "EXPIRED" | "CANCELLED";

// How far a human's approve/deny decision reaches — egress_domain carries all
// four; a credential carries only "run" (the per-run lease: a git_pat approved
// once is re-mintable for the rest of the run); a tool_call is clamped and
// carries none. Mirrors internal/types/types.go's ApprovalScope
// EXACTLY (Valid()'s four values) — hand-maintained, no parity test, so a
// value added on the Go side and not here is a silent client-side drift.
export type ApprovalScope = "once" | "run" | "until" | "always";

export interface ApprovalRequest {
  id: string;
  run_id: string;
  grant_id?: string;
  kind: ApprovalKind;
  // Real wire field is free-form JSON; keep `unknown` and let the screens
  // narrow it. Index signature lets UI read arbitrary keys safely.
  requested_scope: Record<string, unknown>;
  state: ApprovalState;
  requested_at: string;
  decided_at?: string;
  decided_by?: string;
  minted_jti?: string;
  reason?: string;
  // The two fields below are named `decision_*` ON THE WIRE deliberately —
  // NOT `scope`/`expires_at` — because `scope` would collide with
  // requested_scope above (the raised request's own JSON, part of the dedup
  // index) and a bare `expires_at` would collide with this table's existing
  // expiry concept (EXPIRED state, ExpireStale). See internal/types/types.go's
  // ApprovalRequest for the same naming note.
  //
  // Absent on a PENDING row (nothing decided yet) and on any row decided
  // before this feature shipped. Present only on an egress_domain decision.
  decision_scope?: ApprovalScope;
  // Set only when decision_scope is "until". ISO 8601; nothing server-side
  // transitions the row once this passes — the console derives "expired"
  // client-side (see copy.ts's approvalScopeBadge).
  decision_expires_at?: string;
}

// canDecideApproval mirrors internal/api/approvals.go's decide() exactly: an
// operator/admin may decide anything; a member may decide only an
// egress_domain approval — credential and tool_call stay admin-only
// REGARDLESS of ownership (self-approving your own run's credential mint or
// re-opening the clamp's tool_call bound would be self-authorizing under the
// operator's own ceiling). Both callers (approvals.tsx, run-detail.tsx) already
// only ever render rows the caller owns (the list itself is server-scoped —
// see handleListApprovals's creator-pager branch / a run-detail page's
// getRunAuthorized gate), so ownership is a precondition of the row existing
// at all, not something this predicate needs to re-check.
export function canDecideApproval(operator: boolean, kind: ApprovalKind): boolean {
  // credential_reauth is NOT DECIDABLE BY ANYONE, security operator included:
  // it is resolved by its owner signing in, and the server answers 409 to an
  // approve or a deny (decide()'s rule 3b). A Deny would read as an act of
  // governance and change nothing — findPendingDup matches PENDING only, so
  // the sidecar's next resolve would raise a fresh row. The console renders a
  // door for this kind instead of an Approve/Deny pair.
  if (kind === "credential_reauth") return false;
  return operator || kind === "egress_domain";
}

// approve/deny's optional third argument — egress_domain only (the server
// 400s a scope on any other kind; see internal/api/approvals.go's rule 4).
// `until` is an ISO decision_expires_at, required by the server iff scope is
// "until".
export interface DecisionOptions {
  scope?: ApprovalScope;
  until?: string;
}

// Builds approve/deny's trailing args: NONE for the default "run" scope — a
// literal 2-argument call, byte-identical to the pre-scope wire body — and
// one DecisionOptions object otherwise. Every decision surface (live-
// approvals, run-detail's cockpit, the approvals console) MUST spread this
// rather than pass a possibly-undefined third positional arg: vitest's
// toHaveBeenCalledWith matches call arity exactly, so `approve(id, reason,
// undefined)` is a 3-element call and fails an assertion pinned to 2 — four
// such assertions exist across the suite.
//
// Deliberately in lib/types, NOT lib/api, even though it exists only to feed
// api/approvals.ts's approve/deny: every UI test that decides an approval
// mocks "lib/api/approvals" wholesale (a hand-written factory naming only
// `approvals.{listApprovals,approve,deny}`), so a second named export living
// in that module resolves to undefined under those mocks — the caller's
// `decisionArgs(...)` call throws, caught by the decide() try/catch, and the
// api call it was building never fires. lib/types is never mocked.
export function decisionArgs(scope: ApprovalScope, until?: string): [] | [DecisionOptions] {
  return scope === "run" ? [] : [{ scope, until }];
}

// isHeld
//
// isHeld lives here, not in wardyn/live-approvals.tsx, because
// screens/runs/board-groups.ts is EAGER (the board is the landing route and
// App.tsx's attention badge shares the rule): importing this predicate from
// live-approvals.tsx would hoist the WHOLE strip — Rollup assigns chunks per
// MODULE — and once the strip grows the mid-run sign-in row, wardyn/model-
// access-copy.ts and through it lib/workspace-providers-copy.ts, past
// bundle-split.test.ts's budget. Here it costs the eager graph the predicate
// and nothing else; live-approvals.tsx re-exports it so its own readers keep
// their import path.
//
// This file is the right home for the same reason decisionArgs is: it is the
// module both sides already depend on, and it is never mocked.

const HOLD_TIMEOUT_MS = 30_000;

// #160 — the ceiling for the two UNCONDITIONAL arms below (tool_call,
// credential_reauth), a different arm from HOLD_TIMEOUT_MS above: that one
// bounds an egress wait_for_review connection that fails closed in seconds.
// A tool call a human answers can legitimately sit for a long time, so a
// short ceiling would hide a genuine hold — which matters more here than
// forgiving a truly abandoned one. 60 minutes, per the issue's own call.
const STALE_HOLD_CEILING_MS = 60 * 60 * 1000;

// True once `requestedAt` is old enough to cross `ceilingMs` — and only once:
// an unparseable timestamp fails TOWARD showing the hold (not stale), the same
// direction isHeld's own unparseable case below takes.
function isStale(requestedAt: string, ceilingMs: number): boolean {
  const t = Date.parse(requestedAt);
  return !Number.isNaN(t) && Date.now() - t >= ceilingMs;
}

// A held request is one the sandbox is still parked on. TWO shapes reach that
// state and only one of them carries a mode:
//
//  - tool_call — wardyn-toolgate blocks the agent's tool call on the PENDING
//    row itself and polls until it is decided (cmd/wardyn-toolgate/main.go's
//    -deadline is a 24h ceiling for a control plane that stopped answering,
//    not a hold timeout), and the scope it raises is {tool,cmd,env} with no
//    mode at all (internal/egress/proxy/local_routes.go). PENDING alone IS the
//    hold here, so nothing client-side bounds it the way HOLD_TIMEOUT_MS
//    bounds the egress case — the row's own server-side expiry ends it. It
//    IS bounded by STALE_HOLD_CEILING_MS below, a much longer window: a row
//    a human hasn't answered in an hour reads as abandoned, not live.
//  - egress wait_for_review — the proxy carries the mode in the approval's
//    requested_scope so the UI can flag it, but PENDING alone doesn't mean
//    "still holding the sandbox": the connection fails closed at
//    HOLD_TIMEOUT_MS while the approval row itself stays PENDING for up to 24h
//    afterward.
//
// Exported because the run cockpit's command bar and the board's card state
// the same fact ("N waiting · sandbox held"). Two copies of this test would be
// two truths that can disagree, and the disagreement would read as "nothing is
// holding the sandbox" while the sandbox is, in fact, held.
// ─── Azure DevOps capability escalation (plan slice S10) ───────────────────
//
// The canonical scope of a tool_call raised by injection_ado_capability.go's
// answerADOCapability — see that file's adoCapabityScope doc. Cmd and Tool
// are server-COMPOSED (adoCapabilityCmd), never client-derived: the console
// renders them, it does not reconstruct them from the other fields.
export interface AdoCapabilityScope {
  lane: "azure_devops";
  provider_id: string;
  org: string;
  grant_id: string;
  capability: string;
  repo: string;
  ref_class?: "protected" | "";
  tool: string;
  cmd: string;
}

// The canonical scope of an Azure DevOps credential_reauth: raiseADOConsent's
// Entra-consent-missing chain (mechanism entra_consent, with the scopes still
// needed) or holdForADOSignIn's mid-run sign-in request (mechanism
// entra_signin, reason "signin", no scopes) — internal/api's
// injection_ado_capability.go and injection_ado_signin.go.
export interface AdoConsentScope {
  lane: "azure_devops";
  mechanism: "entra_consent" | "entra_signin";
  reason?: "signin";
  owner: string;
  provider_id: string;
  scopes?: string[];
}

// Structural, never scope-key-based (mirrors the server's own
// adoEscalationScope, internal/api/injection_ado_capability.go): a tool_call
// with grant_id set is a control-plane-raised ADO escalation, an older
// console's generic tool_call card is not.
//
// Takes a Pick, not the full ApprovalRequest: copy.ts's approvalScopeBadge
// (F11, round 2) calls this with its own narrower Pick, and a full-shape
// parameter would refuse that caller structurally even though every field
// this function actually reads is present.
export function isAdoCapabilityRequest<T extends Pick<ApprovalRequest, "kind" | "grant_id" | "requested_scope">>(
  a: T,
): a is T & { requested_scope: AdoCapabilityScope } {
  return a.kind === "tool_call" && !!a.grant_id && a.requested_scope?.lane === "azure_devops";
}

export function isAdoConsentRequest(
  a: ApprovalRequest,
): a is ApprovalRequest & { requested_scope: AdoConsentScope } {
  return (
    a.kind === "credential_reauth" &&
    a.requested_scope?.lane === "azure_devops" &&
    (a.requested_scope?.mechanism === "entra_consent" || a.requested_scope?.mechanism === "entra_signin")
  );
}

// The canonical scope of a push_content approval — mirrors
// internal/types/push_content.go's PushContentScope exactly. ActsAsKind/
// ActsAsLabel are SERVER-SET (the control plane resolves and stamps them
// before the row is stored); a raise that carried either is refused, so
// every row this console ever reads has both. Commits is present on the
// wire but MUST NEVER be rendered as "commits": for an Azure DevOps REST
// push it is the SHA-256 of the request body, not an object id (see
// push_content.go's own field doc).
export interface PushContentScope {
  repo: string;
  branch: string;
  acts_as: string;
  paths: string[];
  paths_total: number;
  commits: string[];
  paths_digest: string;
  acts_as_kind: "github_app" | "git_pat" | "ado_entra";
  acts_as_label: string;
}

export function isPushContentRequest(
  a: Pick<ApprovalRequest, "kind" | "requested_scope">,
): a is ApprovalRequest & { requested_scope: PushContentScope } {
  return a.kind === "push_content";
}

// canDecideApproval's ADO carve-out: authorizeMemberDecision
// (internal/api/approvals.go) lets the run's OWNER decide their own run's
// escalation, on top of the security-operator tier ownsRunOrAdmin
// (internal/api/helpers.go) already covers — unlike every other tool_call,
// which stays admin-only regardless of ownership. `securityOperator`, not a
// general operator/admin flag: ownsRunOrAdmin bypasses ownership for
// isSecurityOperator only, matching every other decide-path gate in this
// codebase (0.7 §B). Kept as its own function rather than folded into
// canDecideApproval: that one has no ownership parameter today and every
// other kind it decides needs none.
export function canDecideAdoCapability(securityOperator: boolean, isRunOwner: boolean): boolean {
  return securityOperator || isRunOwner;
}

export function isHeld(a: ApprovalRequest): boolean {
  if (a.kind === "tool_call") return !isStale(a.requested_at, STALE_HOLD_CEILING_MS);
  // A credential_reauth row is raised BECAUSE the proxy is holding a request.
  // It carries no first_use mode of its own — the mode vocabulary belongs to
  // the egress lane — so without this it would read as a passive pending and
  // the run would show no hold while a model call was parked.
  if (a.kind === "credential_reauth") return !isStale(a.requested_at, STALE_HOLD_CEILING_MS);
  // A push_content row is the SAME shape: git is parked on the open
  // connection for as long as the row stays PENDING (push_hold.go), and it
  // carries no first_use mode either — PENDING alone IS the hold.
  if (a.kind === "push_content") return !isStale(a.requested_at, STALE_HOLD_CEILING_MS);
  if (String((a.requested_scope?.mode as string) ?? "") !== "wait_for_review") return false;
  const requestedAt = Date.parse(a.requested_at);
  if (Number.isNaN(requestedAt)) return true; // unparseable timestamp — fail toward showing the hold
  return Date.now() - requestedAt < HOLD_TIMEOUT_MS;
}

// A tool_call/credential_reauth row old enough that isHeld no longer counts
// it live — distinguishes "was held, now stale" from "never held at all" for
// a caller that has to say something different for the two (the runs board's
// group chip and card, #160). Egress wait_for_review is not this arm: past its
// own HOLD_TIMEOUT_MS it is a passive pending, not a stale hold, because
// nothing ever promised the connection would still be parked.
export function isStaleHold(a: ApprovalRequest): boolean {
  if (a.kind !== "tool_call" && a.kind !== "credential_reauth" && a.kind !== "push_content") return false;
  return isStale(a.requested_at, STALE_HOLD_CEILING_MS);
}
