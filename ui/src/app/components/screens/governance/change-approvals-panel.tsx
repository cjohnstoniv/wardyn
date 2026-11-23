/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The approvals panel of a change's own page: how many approvals it has and needs, who gave them, which ones no
// longer count, who can still approve and whether enough of them can. It counts nothing itself: every number and
// every verdict is the server's `approvals` block, rendered as it came. A change with no block is one a single
// person decides.
//
// Every string is from CHANGE_APPROVALS (change-review-copy.ts); this file adds none.
import { CHANGE_APPROVALS } from "../../../lib/change-review-copy";
import { relativeTime, shortTime } from "../../../lib/format";
import type {
  ApprovalDropped,
  ApprovalEntry,
  ApproverTier,
  GovernanceChange,
  GovernanceChangeApprover,
  GovernanceChangeApprovals,
} from "../../../lib/types";
import { Note } from "./display";

// "Security admin", "Admin" or "unit admin of {unit}"; a unit admin whose unit the server did not name reads "Unit admin".
function tierLabel(tier: ApproverTier, unit?: string): string {
  return tier === "unit_admin" && unit ? CHANGE_APPROVALS.TIER_UNIT(unit) : CHANGE_APPROVALS.TIER[tier];
}

// A tier label inside a sentence: its first letter lower-cased, a unit's name left as it is.
const inSentence = (label: string) => label.charAt(0).toLowerCase() + label.slice(1);

// The classes of person who could decide a change nobody else counts for, from the server's `approvers`.
function approverClasses(approvers: GovernanceChangeApprover[] | undefined): string[] {
  const labels = (approvers ?? []).map((a) => {
    if (a.kind === "security") return CHANGE_APPROVALS.TIER.security_admin;
    if (a.kind === "unit_admin") return a.unit_name ? CHANGE_APPROVALS.TIER_UNIT(a.unit_name) : CHANGE_APPROVALS.TIER.unit_admin;
    return a.name ?? "";
  });
  return [...new Set(labels.filter((l) => l !== ""))];
}

function Avatar({ name }: { name: string }) {
  return (
    <span
      aria-hidden="true"
      className="flex size-6 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-medium uppercase text-muted-foreground"
    >
      {name.charAt(0)}
    </span>
  );
}

function whyDropped(e: ApprovalEntry): string {
  const phrase = CHANGE_APPROVALS.WHY_DROPPED[e.dropped as ApprovalDropped];
  return phrase ? phrase(inSentence(tierLabel(e.tier, e.unit_name))) : (e.dropped ?? "");
}

function Entry({ e }: { e: ApprovalEntry }) {
  if (!e.counts) {
    return (
      <li data-testid="change-approval-dropped" className="flex items-start gap-2 text-body text-muted-foreground line-through">
        <Avatar name={e.approver} />
        <span className="min-w-0 [overflow-wrap:anywhere]">
          {CHANGE_APPROVALS.DROPPED(e.approver, whyDropped(e), e.dropped_at ? shortTime(e.dropped_at) : undefined)}
        </span>
      </li>
    );
  }
  return (
    <li data-testid="change-approval-entry" className="flex items-start gap-2 text-body text-foreground">
      <Avatar name={e.approver} />
      <span className="min-w-0 [overflow-wrap:anywhere]">
        {CHANGE_APPROVALS.APPROVED_ENTRY(e.approver, relativeTime(e.approved_at), tierLabel(e.tier, e.unit_name))}
        {e.reason && <q className="mt-0.5 block text-xs text-muted-foreground">{e.reason}</q>}
      </span>
    </li>
  );
}

// Whether enough people can still approve, and why not, from the server's `reach`.
function Reach({ approvals, change, proposer }: { approvals: GovernanceChangeApprovals; change: GovernanceChange; proposer: boolean }) {
  if (approvals.reach === "cant_confirm") {
    return (
      <Note tone="amber">
        <span data-testid="change-cant-confirm">{CHANGE_APPROVALS.CANT_CONFIRM(approvals.required)}</span>
      </Note>
    );
  }
  if (approvals.reach !== "cant_reach") return null;
  const expires = relativeTime(change.expires_at);
  const still = approvals.still_needed ?? 0;
  const remaining = approvals.remaining_eligible ?? 0;
  // The console renders the server's reason and never derives it. Only a field the server left out or sent in a
  // form this console predates falls back to the numbers: short of people reads as a count, anything else as security.
  const reason = approvals.cant_reach_reason === "count" || approvals.cant_reach_reason === "security"
    ? approvals.cant_reach_reason
    : remaining < still
      ? "count"
      : "security";
  return (
    <Note tone="amber">
      <span data-testid="change-cant-reach" data-reason={reason}>
        {reason === "count"
          ? CHANGE_APPROVALS.CANT_REACH_COUNT(approvals.required, still, remaining, expires, proposer)
          : CHANGE_APPROVALS.CANT_REACH_SECURITY(expires, proposer)}
      </span>
    </Note>
  );
}

function CanApprove({ approvals }: { approvals: GovernanceChangeApprovals }) {
  const eligible = approvals.eligible ?? [];
  const uncountable = approvals.uncountable ?? [];
  if (eligible.length === 0 && uncountable.length === 0) return null;
  return (
    <section data-testid="change-can-approve" className="space-y-1">
      <h4 className="text-xs font-medium text-foreground">{CHANGE_APPROVALS.CAN_APPROVE}</h4>
      <ul className="space-y-0.5 text-body text-foreground">
        {eligible.map((p) => (
          <li key={`${p.principal}/${p.tier}`} className="[overflow-wrap:anywhere]">
            {CHANGE_APPROVALS.ELIGIBLE(p.email || p.principal, tierLabel(p.tier))}
          </li>
        ))}
        {uncountable.map((u) => (
          <li key={u.why} className="text-muted-foreground">
            {u.why === "unconfirmed_people" ? CHANGE_APPROVALS.UNCONFIRMED(u.count ?? 0) : CHANGE_APPROVALS.TOO_MANY}
          </li>
        ))}
      </ul>
      <p className="text-xs text-muted-foreground">{CHANGE_APPROVALS.NEVER_SIGNED_IN}</p>
    </section>
  );
}

export function ChangeApprovalsPanel({ change, principal }: { change: GovernanceChange; principal: string }) {
  const approvals = change.approvals;
  const proposer = (!!principal && change.proposed_by === principal) || approvals?.viewer?.state === "proposer";

  if (!approvals) {
    const classes = approverClasses(change.approvers);
    return (
      <section data-testid="change-approvals" className="space-y-2 rounded-xl border border-border bg-card p-4">
        <h3 className="text-body font-medium text-foreground">{CHANGE_APPROVALS.TITLE}</h3>
        <p data-testid="change-one-person" className="text-body text-foreground">
          {CHANGE_APPROVALS.ONE_PERSON}
        </p>
        {classes.length > 0 && (
          <ul data-testid="change-approver-classes" className="space-y-0.5 text-xs text-muted-foreground">
            {classes.map((c) => (
              <li key={c}>{c}</li>
            ))}
          </ul>
        )}
      </section>
    );
  }

  return (
    <section data-testid="change-approvals" className="space-y-3 rounded-xl border border-border bg-card p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-body font-medium text-foreground">{CHANGE_APPROVALS.TITLE}</h3>
        <span data-testid="change-approvals-progress" className="text-body text-muted-foreground">
          {proposer ? CHANGE_APPROVALS.WAITING(approvals.have, approvals.required) : CHANGE_APPROVALS.PROGRESS(approvals.have, approvals.required)}
        </span>
      </div>
      <p data-testid="change-approvals-rule" className="text-xs text-muted-foreground">
        {CHANGE_APPROVALS.RULE(approvals.required, approvals.require_security)}
      </p>
      <ul className="space-y-2">
        {(approvals.entries ?? []).map((e) => (
          <Entry key={`${e.approver}/${e.approved_at}`} e={e} />
        ))}
        <li data-testid="change-proposer-line" className="flex items-start gap-2 text-body text-muted-foreground">
          <Avatar name={change.proposed_by} />
          <span className="min-w-0 [overflow-wrap:anywhere]">{CHANGE_APPROVALS.PROPOSER(change.proposed_by)}</span>
        </li>
      </ul>
      {approvals.viewer?.state === "cannot_be_last" && (
        <Note>
          <span data-testid="change-cannot-be-last">{CHANGE_APPROVALS.CANNOT_BE_LAST}</span>
        </Note>
      )}
      <Reach approvals={approvals} change={change} proposer={proposer} />
      <CanApprove approvals={approvals} />
    </section>
  );
}
