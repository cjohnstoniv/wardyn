/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The decision footer of a change's own page: the decider's reason, what approving would do for this person, and
// the buttons. It follows the server's `approvals.viewer`; the console never works out who may approve. A viewer the
// server does not know to be ineligible (the admin token, or `not_eligible`) gets today's Approve and Reject, and the
// server's own refusal is shown when it says no.
//
// Below 640 px the buttons pin to a bar at the bottom of the screen while the reason box stays in the flow, above it.
//
// Every string is from CHANGES (governance-copy.ts), CHANGE_NOTE (wardyn/copy/change-note.ts) or CHANGE_APPROVALS
// (change-review-copy.ts); this file adds none.
import * as React from "react";
import { CHANGE_APPROVALS } from "../../../lib/change-review-copy";
import { CHANGES } from "../../../lib/governance-copy";
import { relativeTime } from "../../../lib/format";
import type { GovernanceChange } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { cn } from "../../ui/utils";
import { Mono } from "../../wardyn/code-block";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { Field } from "../../wardyn/form-primitives";
import { changeTarget } from "../../../lib/change-review";
import { Note } from "./display";
import { isRequest } from "./changes";

export interface ChangeDecisionFooterProps {
  change: GovernanceChange;
  principal: string;
  /** The change can no longer apply: both buttons are off and the banner says why. */
  stale: boolean;
  busy: boolean;
  /** The note form is open: the decider's controls give way to it. */
  editing: boolean;
  reason: string;
  onReason: (reason: string) => void;
  /** The server's sentence for a decision it refused, shown as it came. */
  refused: string | null;
  /** Below 640 px: the buttons pin to the bottom of the screen. */
  pinned: boolean;
  hasNote: boolean;
  onEditNote: () => void;
  onApprove: () => void;
  onReject: () => void;
  onTakeBack: () => void;
}

export function ChangeDecisionFooter(p: ChangeDecisionFooterProps) {
  const { change, principal, stale, busy, editing, reason, refused, pinned } = p;
  const approvals = change.approvals;
  const viewer = approvals?.viewer;
  const request = isRequest(change);
  const own = (!!principal && change.proposed_by === principal) || viewer?.state === "proposer";
  const withdrawal = own && request;
  // Whether only security can decide this change, read from the change itself: the list does not carry approvers.
  const securityOnly = !!change.approvers?.length && change.approvers.every((a) => a.kind === "security");
  const approved = viewer?.state === "approved";
  const [missing, setMissing] = React.useState(false);

  // A proposer's own Reject is a withdrawal: its reason is optional whatever the approvers owe.
  const reasonRequired = !!approvals?.reject_reason_required && !own;
  const setReason = (v: string) => {
    setMissing(false);
    p.onReason(v);
  };
  const reject = () => {
    if (reasonRequired && reason.trim() === "") {
      setMissing(true);
      return;
    }
    p.onReject();
  };

  let status: string | null = null;
  if (approvals && viewer?.state === "can_approve") {
    status = viewer.last ? CHANGE_APPROVALS.STATUS_LAST : CHANGE_APPROVALS.STATUS_COUNTS(approvals.have + 1, approvals.required);
  } else if (approvals && approved) {
    const still = approvals.still_needed ?? Math.max(0, approvals.required - approvals.have);
    status = CHANGE_APPROVALS.STATUS_APPROVED(viewer?.approved_at ? relativeTime(viewer.approved_at) : "", still);
  }

  const showHint = !!approvals && !own && !approved;
  const barClass = cn("flex flex-wrap items-center justify-end gap-2", pinned && "fixed inset-x-0 bottom-0 z-20 border-t border-border bg-background px-4 pt-3");

  return (
    <div data-testid="change-decision-footer" className="space-y-3">
      {stale && (
        <Note tone="amber">
          <span data-testid="change-stale">{CHANGE_APPROVALS.STALE(changeTarget(change), change.proposed_by)}</span>
        </Note>
      )}
      {own &&
        (withdrawal ? (
          <Note>{CHANGE_NOTE.OWN_REQUEST}</Note>
        ) : securityOnly ? (
          <Note>
            <b className="font-semibold">{CHANGE_NOTE.OWN_SECURITY_TITLE}</b>
            <span>{CHANGE_NOTE.OWN_SECURITY_BODY}</span>
          </Note>
        ) : (
          <Note>{CHANGES.OWN_NOTE}</Note>
        ))}
      {own && !editing && (
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="outline" size="sm" onClick={p.onEditNote}>
            {p.hasNote ? CHANGE_NOTE.EDIT : CHANGE_NOTE.ADD}
          </Button>
          {!p.hasNote && <span className="text-xs text-muted-foreground">{CHANGE_NOTE.NO_NOTE_HINT}</span>}
        </div>
      )}
      {status && (
        <p data-testid="change-viewer-status" className="text-body text-foreground">
          {status}
        </p>
      )}
      {!editing && !withdrawal && !approved && (
        <Field
          label={reasonRequired ? CHANGE_APPROVALS.REASON_REQUIRED_LABEL : CHANGES.REASON_LABEL}
          htmlFor="governance-change-reason"
          hint={CHANGES.REASON_HINT}
        >
          <Input
            id="governance-change-reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            disabled={busy}
            maxLength={512}
            autoComplete="off"
          />
        </Field>
      )}
      {missing && (
        <p data-testid="change-reason-missing" role="alert" className="text-xs text-danger">
          {CHANGE_APPROVALS.REASON_NEEDED}
        </p>
      )}
      {refused && (
        <Note tone="red" role="alert">
          <b className="font-semibold">{CHANGES.DECIDE_REFUSED_TITLE}</b>
          <Mono className="text-inherit">{refused}</Mono>
        </Note>
      )}
      {!editing && (
        <div
          data-testid="change-decision-bar"
          data-pinned={pinned ? "true" : undefined}
          className={barClass}
          style={pinned ? { paddingBottom: "max(0.75rem, env(safe-area-inset-bottom))" } : undefined}
        >
          {approved ? (
            <Button variant="outline" disabled={busy || stale} onClick={p.onTakeBack}>
              {CHANGE_APPROVALS.TAKE_BACK}
            </Button>
          ) : (
            <>
              <Button variant="outline" disabled={busy || stale} onClick={reject}>
                {withdrawal ? CHANGE_NOTE.WITHDRAW_REQUEST : viewer?.state === "proposer" ? CHANGE_APPROVALS.WITHDRAW : CHANGES.REJECT}
              </Button>
              {viewer?.state !== "proposer" && (
                <Button disabled={busy || stale || own || viewer?.state === "cannot_be_last"} onClick={p.onApprove}>
                  {CHANGES.APPROVE}
                </Button>
              )}
            </>
          )}
        </div>
      )}
      {showHint && !editing && (
        <p data-testid="change-reject-note" className="text-xs text-muted-foreground">
          {CHANGE_APPROVALS.REJECT_NOTE}
        </p>
      )}
    </div>
  );
}
