/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KubeCallCard: the console's rendering of an approval for ONE held kubectl write (the proxy holds the request and
// asks a person; internal/types/kube_call.go). Built on McpCallCard's parts and mounted beside it.
//
// EXACT TEXT: the body renders from item.call_text, the server's text of the canonical bytes, and is copied from the
// same string. Nothing here reads requested_scope.body: the scope arrives parsed, and a number past 2^53 would
// round. The server sets call_text on every kubectl row, with empty arguments for a write that has no body (a
// DELETE), so Approve is gated on call_text being present, never on its arguments being non-empty. Only a row with no
// call_text at all (an older server) shows no body and offers Deny only.
//
// NO SCOPE MENU: one request is approved once. Approve and Deny call straight through.
import * as React from "react";
import { Check, Loader2, X } from "lucide-react";
import type { AgentRun, ApprovalRequest, KubeCallScope } from "../../lib/types";
import { isHeld, isTerminalRunState } from "../../lib/types";
import { relativeTime, absoluteTime } from "../../lib/format";
import { APPROVAL, APPROVAL_BANNER_LABEL } from "./copy";
import { APPROVAL_MCP_CALL as MCP } from "./copy/approvals";
import { KUBE_CALL_CARD as C } from "./copy/files-review";
import { Button } from "../ui/button";
import { ApprovalKindChip, Chip } from "./primitives";
import { DecidedByLine, SecondPersonLine } from "./decider-lines";
import { ExactText } from "./mcp-call-card";
import { escapeUnsafeRunes } from "../../lib/escape-runes";

export function KubeCallCard({
  item,
  canDecide,
  run,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest & { requested_scope: KubeCallScope };
  /** The viewer may decide this row: a security admin, or the run's owner (canDecideApproval with the row). */
  canDecide: boolean;
  run: Pick<AgentRun, "state"> | null | undefined;
  busy: "approve" | "deny" | null;
  onApprove: () => void;
  onDeny: () => void;
}) {
  const scope = item.requested_scope;
  const hasText = item.call_text != null;
  const body = item.call_text ? escapeUnsafeRunes(item.call_text.arguments, true) : "";
  const runEnded = !!run && isTerminalRunState(run.state);
  const cluster = escapeUnsafeRunes(scope.cluster);
  const namespace = escapeUnsafeRunes(scope.namespace);
  const method = escapeUnsafeRunes(scope.method);

  // A held request waits only for a bounded time; the line flips when the window ends, as the MCP card's does.
  const [, rerender] = React.useReducer((n: number) => n + 1, 0);
  React.useEffect(() => {
    if (!item.held_until) return;
    const msLeft = Date.parse(item.held_until) - Date.now();
    if (!(msLeft > 0)) return;
    const id = setTimeout(rerender, msLeft);
    return () => clearTimeout(id);
  }, [item.held_until]);
  const proxyHeld = (scope.hold_seconds ?? 0) > 0;
  const held = isHeld(item);

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="kube-call-card">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind="tool_call" />
        <span className="text-sm font-semibold text-foreground">{C.TITLE}</span>
        <Chip tone="neutral">{MCP.ONCE}</Chip>
      </div>
      <DecidedByLine item={item} />
      <SecondPersonLine item={item} />

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{C.FIELD_CLUSTER}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{cluster}</dd>
        <dt className="text-muted-foreground">{C.FIELD_NAMESPACE}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{namespace}</dd>
        <dt className="text-muted-foreground">{C.FIELD_METHOD}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{method}</dd>
        <dt className="text-muted-foreground">{C.FIELD_PATH}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{escapeUnsafeRunes(scope.path)}</dd>
        {scope.query && (
          <>
            <dt className="text-muted-foreground">{C.FIELD_QUERY}</dt>
            <dd className="min-w-0 break-all font-mono text-xs text-foreground">{escapeUnsafeRunes(scope.query)}</dd>
          </>
        )}
        {scope.media_type && (
          <>
            <dt className="text-muted-foreground">{C.FIELD_MEDIA}</dt>
            <dd className="min-w-0 break-all font-mono text-xs text-foreground">{escapeUnsafeRunes(scope.media_type)}</dd>
          </>
        )}
      </dl>

      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span> {C.WHAT(method, cluster, namespace)}
        </p>
        <p className="text-muted-foreground">
          <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span> {C.BLAST}
        </p>
      </div>

      <div className="mt-3 space-y-3">
        {hasText ? (
          <div className="space-y-1">
            <p className="text-xs font-semibold text-foreground">{C.FIELD_BODY}</p>
            {body !== "" ? <ExactText text={body} label={C.FIELD_BODY} copyLabel={C.COPY_BODY} /> : <p className="text-xs text-muted-foreground">{C.NO_BODY}</p>}
          </div>
        ) : (
          <p className="text-xs text-muted-foreground">{C.NO_TEXT}</p>
        )}
        <p className="text-xs text-muted-foreground">
          <span className="font-mono" data-testid="kube-call-fingerprint">{MCP.FINGERPRINT(scope.call_sha256.slice(0, 12))}</span>
        </p>
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        {runEnded ? (
          <p className="text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
        ) : canDecide ? (
          <>
            <Button size="sm" variant="info" disabled={busy !== null || !hasText} onClick={onApprove}>
              {busy === "approve" ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />} {MCP.APPROVE}
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

      {!runEnded && proxyHeld && <p className="mt-2.5 text-xs text-muted-foreground">{held ? C.HELD_NOTE : C.HELD_EXPIRED}</p>}
      {!runEnded && canDecide && <p className="mt-1 text-xs text-muted-foreground">{MCP.DENY_NOTE}</p>}
    </div>
  );
}
