/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HostCallCard: the console's rendering of an approval for ONE held request to a host a rule holds (the proxy holds
// the request and asks a person). Built on KubeCallCard's parts and mounted beside it.
//
// SCOPE IS THE WHOLE RUN: approving lets this method and path through on this host for the rest of the run, so the
// chip, the button and the blast radius say "this run", never "once". The scope has no body (the query and body are not covered by the key), so the card shows no body and no fingerprint.
//
// NO SCOPE MENU: Approve and Deny call straight through.
import * as React from "react";
import { Check, Loader2, X } from "lucide-react";
import type { AgentRun, ApprovalRequest, HostCallScope } from "../../lib/types";
import { isHeld, isTerminalRunState } from "../../lib/types";
import { relativeTime, absoluteTime } from "../../lib/format";
import { APPROVAL, APPROVAL_BANNER_LABEL } from "./copy";
import { APPROVAL_MCP_CALL as MCP, APPROVAL_SCOPE_LABEL } from "./copy/approvals";
import { HOST_CALL_CARD as C } from "./copy/files-review";
import { Button } from "../ui/button";
import { ApprovalKindChip, Chip } from "./primitives";
import { DecidedByLine, SecondPersonLine } from "./decider-lines";
import { escapeUnsafeRunes } from "../../lib/escape-runes";

export function HostCallCard({
  item,
  canDecide,
  run,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest & { requested_scope: HostCallScope };
  /** The viewer may decide this row (the same verdict the kubectl card takes). */
  canDecide: boolean;
  run: Pick<AgentRun, "state"> | null | undefined;
  busy: "approve" | "deny" | null;
  onApprove: () => void;
  onDeny: () => void;
}) {
  const scope = item.requested_scope;
  const runEnded = !!run && isTerminalRunState(run.state);
  const host = escapeUnsafeRunes(scope.host);
  const method = escapeUnsafeRunes(scope.method);
  const path = escapeUnsafeRunes(scope.path);

  // The held / no-longer-waiting note renders only when the wire supplies a reliable hold deadline (held_until). The
  // line flips when that deadline passes, as the kubectl card's does.
  const [, rerender] = React.useReducer((n: number) => n + 1, 0);
  React.useEffect(() => {
    if (!item.held_until) return;
    const msLeft = Date.parse(item.held_until) - Date.now();
    if (!(msLeft > 0)) return;
    const id = setTimeout(rerender, msLeft);
    return () => clearTimeout(id);
  }, [item.held_until]);
  const hasDeadline = !!item.held_until && !Number.isNaN(Date.parse(item.held_until));
  const held = isHeld(item);

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="host-call-card">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind="tool_call" />
        <span className="text-sm font-semibold text-foreground">{C.TITLE}</span>
        <Chip tone="neutral">{APPROVAL_SCOPE_LABEL.run}</Chip>
      </div>
      <DecidedByLine item={item} />
      <SecondPersonLine item={item} />

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{C.FIELD_HOST}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{host}</dd>
        <dt className="text-muted-foreground">{C.FIELD_METHOD}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{method}</dd>
        <dt className="text-muted-foreground">{C.FIELD_PATH}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{path}</dd>
      </dl>

      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="min-w-0 break-words text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span> {C.WHAT(method, path, host)}
        </p>
        <p className="text-muted-foreground">
          <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span> {C.BLAST}
        </p>
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        {runEnded ? (
          <p className="text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
        ) : canDecide ? (
          <>
            <Button size="sm" variant="info" disabled={busy !== null} onClick={onApprove}>
              {busy === "approve" ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />} {C.APPROVE}
            </Button>
            <Button size="sm" variant="outline" disabled={busy !== null} onClick={onDeny}>
              {busy === "deny" ? <Loader2 className="size-4 animate-spin" /> : <X className="size-4" />} {MCP.DENY}
            </Button>
          </>
        ) : (
          <Chip tone="neutral">{MCP.CANNOT_DECIDE}</Chip>
        )}
        <span className="ml-auto text-xs text-muted-foreground" title={absoluteTime(item.requested_at)}>
          requested {relativeTime(item.requested_at)}
        </span>
      </div>

      {!runEnded && hasDeadline && <p className="mt-2.5 text-xs text-muted-foreground">{held ? C.HELD_NOTE : C.HELD_EXPIRED}</p>}
      {!runEnded && canDecide && <p className="mt-1 text-xs text-muted-foreground">{C.DENY_NOTE}</p>}
    </div>
  );
}
