/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PushContentCard — the console's rendering of a push_content approval (#181,
// built on #180/#494's server side): a brokered git push that matched
// push_rules.require_review_paths and is parked at the proxy for an admin's
// decision (internal/egress/proxy/push_hold.go). Shared by
// screens/approvals.tsx's PendingCard and live-approvals.tsx's strip, the
// same two-mount pattern ado-capability-card.tsx uses and for the same
// reason: a held push needs fields (repository, branch, the paths under
// review) neither surface's generic row can show.
//
// ADMIN-ONLY, NO OWNERSHIP CARVE-OUT (unlike Azure DevOps's escalation card):
// canDecideApproval(securityOperator, "push_content") falls through to the
// same operator-only branch credential/tool_call take — a member approving
// their own run's workflow-file edit is the exfiltration the rule exists to
// stop (see internal/api/approvals_push.go's own doc). So this card takes no
// run-owner/viewerPrincipal props at all, only `run.state` — to withdraw the
// decision pair once the run has ended, the same defensive check
// screens/approvals.tsx's generic PendingCard makes for every other kind.
//
// NO SCOPE MENU: a push decision covers this push only (decide's rule 4
// refuses a decision_scope on this kind — approvals_push.go). Approve/Deny
// call straight through to the API with no ReasonDialog stop and no staged
// scope, the same direct-decide shape the ADO card's own onApprove/onDeny
// take, and for the same reason: the frozen mock (packet 7) draws this
// card's own control, not the generic reason dialog.
import * as React from "react";
import { Check, Loader2, X } from "lucide-react";
import type { AgentRun, ApprovalRequest } from "../../lib/types";
import { isHeld, isTerminalRunState, PUSH_HOLD_CEILING_MS, type PushContentScope } from "../../lib/types";
import { relativeTime } from "../../lib/format";
import { PUSH } from "./copy/push";
import { APPROVAL, APPROVAL_BANNER_LABEL, SECURITY_ONLY_REASON } from "./copy";
import { Button } from "../ui/button";
import { ApprovalKindChip, ApprovalStateBadge, Chip } from "./primitives";

// The run this card needs to know about — only whether it has ENDED (the
// same defensive re-check screens/approvals.tsx's generic PendingCard makes:
// a terminal run's PENDING approvals are cancelled by the lifecycle cascade a
// beat later, so offering Approve/Deny in that window would be a dead
// control). No `created_by`: decidability here is role-only, never ownership.
export type PushCardRun = Pick<AgentRun, "state">;

export function PushContentCard({
  item,
  securityOperator,
  run,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest & { requested_scope: PushContentScope };
  securityOperator: boolean;
  run: PushCardRun | null | undefined;
  /** Which decide call is in flight, else null — both buttons disable, only
   *  the pressed one spins (#458, the same shape as ado-capability-card.tsx). */
  busy: "approve" | "deny" | null;
  onApprove: () => void;
  onDeny: () => void;
}) {
  const scope = item.requested_scope;
  const runEnded = !!run && isTerminalRunState(run.state);
  const shown = scope.paths ?? [];
  const moreCount = Math.max(0, (scope.paths_total ?? shown.length) - shown.length);

  // Review finding 2 — the proxy's own hold is BOUNDED (isHeld's own
  // push_content doc, lib/types/approvals.ts): HELD_NOTE ("lets it through
  // now") is only true while it is, so the card has to flip to HELD_EXPIRED
  // on its own once the window passes, the same live-timer shape
  // ado-capability-card.tsx's stillHeld/REQ_HELD_EXPIRED takes — a poll tick
  // eventually catches it too, but a member sitting on this card between
  // ticks must not keep reading a promise that already lapsed.
  //
  // No caller hands this card the run's own push_rules.hold_seconds (no
  // client surface reads a resolved policy — same gap isHeld's own doc
  // names), so the timer schedules off PUSH_HOLD_CEILING_MS, the same
  // conservative 600s default isHeld(item) falls back to absent one.
  const [, forceRerenderAtWindowEnd] = React.useReducer((n: number) => n + 1, 0);
  React.useEffect(() => {
    const requestedAt = Date.parse(item.requested_at);
    if (Number.isNaN(requestedAt)) return; // isHeld already fails toward "held" — nothing to flip to
    const msLeft = PUSH_HOLD_CEILING_MS - (Date.now() - requestedAt);
    if (msLeft <= 0) return; // already past the window
    const id = setTimeout(forceRerenderAtWindowEnd, msLeft);
    return () => clearTimeout(id);
  }, [item.requested_at]);
  const held = isHeld(item);

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="push-content-card">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind="push_content" />
        <span className="text-sm font-semibold text-foreground">{PUSH.CARD_TITLE}</span>
        <span className="ml-auto">
          <ApprovalStateBadge state={item.state} />
        </span>
      </div>

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{PUSH.FIELD_REPOSITORY}</dt>
        <dd className="font-mono text-xs text-foreground">{scope.repo}</dd>
        <dt className="text-muted-foreground">{PUSH.FIELD_BRANCH}</dt>
        <dd className="font-mono text-xs text-foreground">{scope.branch}</dd>
        {/* acts_as_label ONLY — never scope.acts_as, a credential reference
            ("<grant kind>:<uuid>"), not a principal. */}
        <dt className="text-muted-foreground">{PUSH.FIELD_ACTS_AS}</dt>
        <dd className="text-xs text-foreground">{scope.acts_as_label || "—"}</dd>
      </dl>

      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span>{" "}
          {PUSH.WHAT(scope.repo, scope.acts_as_label || "—")}
        </p>
        <p className="text-muted-foreground">
          <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span> {PUSH.BLAST}
        </p>
      </div>

      <div className="mt-3">
        <p className="text-xs font-semibold text-foreground">{PUSH.PATHS_TITLE}</p>
        {/* Q181-2: ten paths, then "+N more" — no expanding. The full list
            (paths_total may exceed the ten names here) lives in the run's
            audit trail, not behind a control on this card. */}
        <ul className="mt-1 space-y-0.5">
          {shown.map((p) => (
            <li key={p} className="truncate font-mono text-xs text-muted-foreground" title={p}>
              {p}
            </li>
          ))}
        </ul>
        {moreCount > 0 && <p className="mt-1 text-xs text-muted-foreground">{PUSH.PATHS_MORE(moreCount)}</p>}
        <p className="mt-1.5 text-xs text-muted-foreground">{PUSH.PATHS_NOTE}</p>
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        {runEnded ? (
          <p className="max-w-[72ch] text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
        ) : securityOperator ? (
          <>
            <Button size="sm" variant="info" disabled={busy !== null} onClick={onApprove}>
              {busy === "approve" ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />} Approve
            </Button>
            <Button size="sm" variant="outline" disabled={busy !== null} onClick={onDeny}>
              {busy === "deny" ? <Loader2 className="size-4 animate-spin" /> : <X className="size-4" />} Deny
            </Button>
          </>
        ) : (
          <Chip tone="neutral">{SECURITY_ONLY_REASON}</Chip>
        )}
        <span className="ml-auto text-xs text-muted-foreground" title={item.requested_at}>
          requested {relativeTime(item.requested_at)}
        </span>
      </div>

      {!runEnded && (
        <>
          <p className="mt-2.5 max-w-[72ch] text-xs text-muted-foreground">
            {held ? PUSH.HELD_NOTE : PUSH.HELD_EXPIRED}
          </p>
          {securityOperator && <p className="mt-1 max-w-[72ch] text-xs text-muted-foreground">{PUSH.APPROVE_NOTE}</p>}
        </>
      )}
    </div>
  );
}
