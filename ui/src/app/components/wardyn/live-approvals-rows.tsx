/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { ChevronDown, Clock } from "lucide-react";
import type { ApprovalRequest, ApprovalScope } from "../../lib/types";
import { REAUTH_ROW, reauthAudience, reauthRowHint } from "./model-access-copy";
import { useModelAccessDoor, useClaimModelAccessDoor, useShellSetupStatus } from "./model-access-context";
import { resolveDoor } from "../../lib/model-access";
import { OpenInUserView, viewOfPath } from "./console-view";
import { routerPath } from "../../lib/base-path";
import { Button } from "../ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { cn } from "../ui/utils";
import { Mono } from "./code-block";
import {
  ALWAYS_NEEDS_WORKSPACE,
  APPROVAL_SCOPE_HINT,
  APPROVAL_SCOPE_LABEL,
  DENY_SCOPE_HINT,
  DENY_SCOPE_LABEL,
  SECURITY_ONLY_REASON,
  UNTIL_PRESETS,
} from "./copy";

// outline-none + the three focus-visible: classes are CONSOLE-RULES.md §34's
// standard ring (button.tsx#buttonVariants carries the same three) — these
// buttons went keyboard-reachable under a Popover (review finding F3) and,
// without this, showed the browser's default outline instead (review
// finding 5).
const SCOPE_ITEM_CLS =
  "flex w-full flex-col items-start gap-0 rounded-sm px-2 py-1.5 text-left text-sm text-foreground outline-none hover:bg-accent hover:text-accent-foreground focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px] disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent";

// ScopeMenu — the split button's caret content (Approve and Deny each mount
// their own instance). Renders every option as a plain <button>, not
// DropdownMenuItem: Always must carry a REAL disabled attribute when gated
// (see reason-dialog.tsx's identical note) — a div-based menu item can never
// have one, only aria-disabled, and Playwright will happily "click" that.
// Held in a Popover, not a DropdownMenu (review finding F3): Radix's
// DropdownMenuContent runs its own roving-tabindex focus manager over
// registered DropdownMenuItems and swallows Tab, so a plain <button> inside
// it is dead to the keyboard — neither Tab nor the arrow keys ever reach it.
// Popover's content does not manage focus that way, so Tab walks these
// buttons in plain DOM order and Enter/Space activate them natively.
export function ScopeMenu({
  verb,
  hasWorkspace,
  securityOperator,
  triggerClassName,
  onPick,
}: {
  verb: "approve" | "deny";
  hasWorkspace: boolean;
  // decision_scope=always is gated by isSecurityOperator (approvals.go:604),
  // NOT isOperator — named for the predicate it actually carries.
  securityOperator: boolean;
  triggerClassName?: string;
  onPick: (scope: ApprovalScope, until?: string) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [untilMode, setUntilMode] = React.useState(false);
  // The sub-view's first control ("← Back") — focused when untilMode opens,
  // since swapping PopoverContent's children does not move focus on its own
  // and it would otherwise drop to the page body (#481).
  const backRef = React.useRef<HTMLButtonElement>(null);
  React.useEffect(() => {
    if (untilMode) backRef.current?.focus();
  }, [untilMode]);
  // The custom datetime-local's picked value, held here until the operator
  // explicitly confirms it — see the "Use this time" button below. A preset
  // click is already one deliberate, atomic action and commits straight
  // through pick(); this input is not, since the browser fires onChange on
  // every intermediate valid value while the operator is still scrubbing
  // hour/minute/AM-PM, and pick() commits the decision (or, for deny, opens
  // the confirm dialog pre-loaded with whatever value fired last).
  const [customUntil, setCustomUntil] = React.useState<string | null>(null);
  const labels = verb === "approve" ? APPROVAL_SCOPE_LABEL : DENY_SCOPE_LABEL;
  const hints = verb === "approve" ? APPROVAL_SCOPE_HINT : DENY_SCOPE_HINT;
  const alwaysDisabled = !hasWorkspace || !securityOperator;
  const alwaysReason = !securityOperator ? SECURITY_ONLY_REASON : ALWAYS_NEEDS_WORKSPACE;

  const pick = (scope: ApprovalScope, until?: string) => {
    setOpen(false);
    setUntilMode(false);
    setCustomUntil(null);
    onPick(scope, until);
  };

  return (
    <Popover
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) {
          setUntilMode(false);
          setCustomUntil(null);
        }
      }}
    >
      <PopoverTrigger asChild>
        {/* Deliberately not named "…approve…"/"…deny…" — see the row comment
            above this component's two mount sites. */}
        <Button size="sm" variant="outline" className={cn("h-7 w-6 p-0", triggerClassName)} aria-label="More options">
          <ChevronDown className="size-3.5" />
        </Button>
      </PopoverTrigger>
      {/* review finding 6: PopoverContent renders role="dialog" with no
          accessible name by default — label it to match the trigger it
          opens from. */}
      <PopoverContent align="end" className="w-64 space-y-0.5 p-1" aria-label="More options">
        {!untilMode ? (
          <>
            {(["once", "run"] as const).map((s) => (
              <button key={s} type="button" onClick={() => pick(s)} className={SCOPE_ITEM_CLS}>
                <span className="font-medium">{labels[s]}</span>
                <span className="text-meta text-muted-foreground">{hints[s]}</span>
              </button>
            ))}
            <button type="button" onClick={() => setUntilMode(true)} className={SCOPE_ITEM_CLS}>
              <span className="font-medium">{labels.until}</span>
              <span className="text-meta text-muted-foreground">{hints.until}</span>
            </button>
            <button
              type="button"
              disabled={alwaysDisabled}
              onClick={() => pick("always")}
              className={SCOPE_ITEM_CLS}
            >
              <span className="font-medium">{labels.always}</span>
              <span className="text-meta text-muted-foreground">
                {alwaysDisabled ? alwaysReason : hints.always}
              </span>
            </button>
          </>
        ) : (
          <>
            <button
              ref={backRef}
              type="button"
              onClick={() => {
                setUntilMode(false);
                setCustomUntil(null);
              }}
              className="px-2 py-1 text-meta text-muted-foreground hover:text-foreground"
            >
              ← Back
            </button>
            {UNTIL_PRESETS.map((p) => (
              <button
                key={p.label}
                type="button"
                onClick={() => pick("until", new Date(Date.now() + p.ms).toISOString())}
                className={SCOPE_ITEM_CLS}
              >
                {p.label}
              </button>
            ))}
            <div className="space-y-1 px-2 py-1.5">
              {/* Sets state only — same as reason-dialog.tsx's identical input.
                  Does NOT pick(): the browser fires onChange on every
                  intermediate valid value while the operator is still
                  scrubbing hour/minute/AM-PM, and pick() commits (or, for
                  deny, opens the confirm dialog) — committing mid-scrub is
                  the bug this split guards against. */}
              <input
                type="datetime-local"
                aria-label="Pick a time"
                onChange={(e) => setCustomUntil(e.target.value ? new Date(e.target.value).toISOString() : null)}
                className="h-7 w-full rounded-md border border-border bg-background px-1.5 text-xs text-foreground"
              />
              <Button
                size="sm"
                variant="outline"
                className="h-6 w-full text-meta"
                disabled={!customUntil}
                onClick={() => pick("until", customUntil ?? undefined)}
              >
                Use this time
              </Button>
            </div>
          </>
        )}
      </PopoverContent>
    </Popover>
  );
}

/**
 * ReauthRow — the strip's row for a mid-run AWS sign-in request.
 *
 * It is a DOOR, not a decision (UX round B2): the Approve/Deny pair is REMOVED
 * for this kind, not disabled — a disabled pair would say "an admin can do
 * this", and no tier can. The row's one control opens the SAME dialog the
 * global strip and the New Run rail open, so a person never learns two ways to
 * sign in to AWS.
 *
 * While it renders that control it CLAIMS the door (the
 * one-primary-recovery-action-per-state-per-screen rule): the global strip
 * keeps its sentence and drops its button on this page, so the cockpit offers
 * exactly one place to press.
 *
 * WHO gets the door is reauthAudience's one rule, shared with the /approvals
 * card and mirroring the server's own admission test: a per_user row is
 * resolvable only by the subject it names, a shared row only by an operator.
 * Everyone else gets a sentence — a shared-lane member "ask your admin", a
 * non-owner (the admin reading a member's held run) the sentence that names
 * whose sign-in is awaited — and no button the server would refuse (Codex #7
 * risk (d); round-2 general S5).
 *
 * M-7 (admin-member-modes-design.md §4.6, §6) — `adminView` narrows `canAct`
 * further, for the per_user lane only: the admin monitor carries no personal
 * door, even on the admin's OWN run, where `reauthAudience` would otherwise
 * grade this viewer able to act. The shared lane is untouched — a
 * shared-credential re-sign stays an admin-mode control until MP-4b — and on
 * the admin's own row the sentence gets a switch link back to the door
 * instead (packet M-B, QM-7).
 */
export function ReauthRow({
  request,
  runId,
  adminView = false,
}: {
  request: ApprovalRequest;
  /** This strip's own run — the switch link's target on the admin's own row
   *  (§below) needs no `useLocation()`: it is always this exact run's user
   *  twin, `/runs/{id}`, whatever admin subpath this strip happens to be
   *  mounted under. (The row itself does read `window.location` directly,
   *  for `viewOfPath` below — never the `useLocation()` hook, since this row
   *  is mounted without a router in its suites.) */
  runId: string;
  adminView?: boolean;
}) {
  const door = useModelAccessDoor();
  // door.operator / door.principal, never useOperator() / usePrincipal():
  // those answer the FAIL-OPEN default while /me is in flight, which is exactly
  // the window in which this row would paint a door for the wrong audience.
  // The door grades nothing until the viewer is known, and so does this.
  const audience = reauthAudience(request, {
    operator: door.operator,
    principal: door.principal,
    // The path, not useLocation(), as the door's own context reads it: this
    // row is mounted without a router in its suites.
    view: viewOfPath(routerPath()),
  });
  // M-7 narrows who is offered a door, and so which sentence the row reads.
  const mayAct = adminView && !audience.shared ? false : audience.canAct;
  // A hold whose provider this person has no door for any more gets its hint
  // alone, never a button that opens nothing (#543) — the sentence stays
  // `mayAct`'s, as /approvals' card keeps it.
  const { status } = useShellSetupStatus();
  const canAct = mayAct && (!audience.provider || !!resolveDoor(status, { provider: audience.provider }, "user"));
  const ownRow = adminView && !audience.shared && audience.mine;
  // Nobody should claim the door for a control they are not rendering.
  useClaimModelAccessDoor(canAct);
  return (
    <div className="flex items-center gap-2" data-testid="live-approval-row">
      <Clock className="size-3.5 shrink-0 text-warning" aria-label="request held live" />
      <div className="flex min-w-0 flex-1 flex-col">
        <Mono className="text-foreground" title={REAUTH_ROW.label}>
          {REAUTH_ROW.label}
        </Mono>
        <span className="text-meta font-normal normal-case text-muted-foreground">
          {reauthRowHint(mayAct === audience.canAct ? audience : { ...audience, canAct: mayAct })}
        </span>
      </div>
      {canAct && (
        <Button
          size="sm"
          variant="outline"
          className="h-7 shrink-0"
          aria-label={REAUTH_ROW.ariaLabel}
          // The hold's OWN provider's door (#543): the claude-code default
          // may be another AWS provider, whose sign-in cannot clear it.
          onClick={() => door.openDoor(audience.provider ? { for: { provider: audience.provider } } : undefined)}
        >
          {REAUTH_ROW.action}
        </Button>
      )}
      {ownRow && <OpenInUserView runId={runId} className="h-7 shrink-0" />}
    </div>
  );
}
