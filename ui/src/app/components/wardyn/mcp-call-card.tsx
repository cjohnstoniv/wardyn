/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// McpCallCard: the console's rendering of an approval for ONE MCP tool call (the proxy holds the call and asks a
// person; internal/types/mcp_call.go). Mounted where PushContentCard is: the Approvals screen, a run's Approvals tab
// and the live strip.
//
// EXACT TEXT: the arguments and the other parameters render from item.call_text, the server's text of the canonical
// bytes, and are copied from the same string. Nothing here parses requested_scope.arguments: JSON.parse would round an
// integer past 2^53 and shorten a long decimal, while the approval binds the exact value. A row with no call_text
// (an older server) shows no arguments and offers Deny only.
//
// NO SCOPE MENU: one call is approved once; decide() refuses any wider scope. Approve and Deny call straight through,
// as the push card's do.
import * as React from "react";
import { Check, Loader2, X } from "lucide-react";
import type { AgentRun, ApprovalRequest, MCPCallScope } from "../../lib/types";
import { isHeld, isTerminalRunState } from "../../lib/types";
import { relativeTime, absoluteTime } from "../../lib/format";
import { APPROVAL, APPROVAL_BANNER_LABEL } from "./copy";
import { APPROVAL_MCP_CALL as C } from "./copy/approvals";
import { Button } from "../ui/button";
import { ApprovalKindChip, Chip } from "./primitives";
import { DecidedByLine, SecondPersonLine } from "./decider-lines";
import { CopyButton } from "./copy-button";
import { escapeUnsafeRunes, quoteToASCII } from "../../lib/escape-runes";

export type McpCallCardRun = Pick<AgentRun, "state">;

/** The arguments or other parameters: the server's text, wrapped, never cut, reachable by keyboard when it scrolls. */
function ExactText({ text, label, copyLabel }: { text: string; label: string; copyLabel: string }) {
  return (
    <div className="group relative rounded-lg border border-border bg-surface-2/60">
      <CopyButton
        text={text}
        label={copyLabel}
        className="absolute right-2 top-2 z-10 size-7 justify-center rounded-md border border-border bg-card text-muted-foreground opacity-0 transition focus-visible:opacity-100 group-focus-within:opacity-100 group-hover:opacity-100 hover:text-foreground"
      />
      <pre
        tabIndex={0}
        role="region"
        aria-label={label}
        className="scroll-thin max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg p-3 pr-11 text-xs leading-relaxed outline-none focus-visible:ring-[3px] focus-visible:ring-ring"
      >
        <code className="font-mono">{text}</code>
      </pre>
    </div>
  );
}

export function McpCallCard({
  item,
  canDecide,
  run,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest & { requested_scope: MCPCallScope };
  /** The viewer may decide this row: a security admin, or the run's owner (canDecideApproval with the row). */
  canDecide: boolean;
  run: McpCallCardRun | null | undefined;
  busy: "approve" | "deny" | null;
  onApprove: () => void;
  onDeny: () => void;
}) {
  const scope = item.requested_scope;
  const text = item.call_text && {
    arguments: escapeUnsafeRunes(item.call_text.arguments, true),
    extra: item.call_text.extra ? escapeUnsafeRunes(item.call_text.extra, true) : undefined,
  };
  const runEnded = !!run && isTerminalRunState(run.state);
  const tool = escapeUnsafeRunes(scope.tool);
  // The call key binds the raw method the server knows, which the display name can hide (delete.issue and delete_issue
  // both read mcp__jira__delete_issue): it always has its own line, quoted and escaped as the CLI prints it.
  const prefix = `mcp__${scope.mcp_server}__`;
  const rawName = scope.mcp_tool || (scope.tool.startsWith(prefix) ? scope.tool.slice(prefix.length) : scope.tool);
  const rawTool = quoteToASCII(rawName);
  const serverKey = escapeUnsafeRunes(scope.mcp_server);
  const server = scope.mcp_host ? `${serverKey} · ${escapeUnsafeRunes(scope.mcp_host)}` : serverKey;

  // A proxy-held call (hold_seconds on the row) waits only for a bounded time; the line flips when the window ends,
  // as the push card's does. A row the gate raised has no hold_seconds and no line.
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
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="mcp-call-card">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind="tool_call" />
        <span className="text-sm font-semibold text-foreground">{C.TITLE}</span>
        <Chip tone="neutral">{C.ONCE}</Chip>
      </div>
      <DecidedByLine item={item} />
      <SecondPersonLine item={item} />

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{C.FIELD_SERVER}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{server}</dd>
        <dt className="text-muted-foreground">{C.FIELD_TOOL}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{tool}</dd>
        <dt className="text-muted-foreground">{C.FIELD_SERVER_TOOL}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground" data-testid="mcp-call-raw-tool">{rawTool}</dd>
      </dl>

      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span> {C.WHAT(tool, serverKey)}
        </p>
        <p className="text-muted-foreground">
          <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span> {C.BLAST}
        </p>
      </div>

      <div className="mt-3 space-y-3">
        {text ? (
          <>
            <div className="space-y-1">
              <p className="text-xs font-semibold text-foreground">{C.ARGUMENTS}</p>
              <ExactText text={text.arguments} label={C.ARGUMENTS} copyLabel={C.COPY_ARGUMENTS} />
            </div>
            {text.extra && (
              <div className="space-y-1">
                <p className="text-xs font-semibold text-foreground">{C.EXTRA}</p>
                <ExactText text={text.extra} label={C.EXTRA} copyLabel={C.COPY_EXTRA} />
              </div>
            )}
            <p className="text-xs text-muted-foreground">
              <span className="font-mono" data-testid="mcp-call-fingerprint">{C.FINGERPRINT(scope.call_sha256.slice(0, 12))}</span>
              {". "}
              {C.APPROVE_NOTE}
            </p>
          </>
        ) : (
          <p className="text-xs text-muted-foreground">{C.NO_TEXT}</p>
        )}
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        {runEnded ? (
          <p className="text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
        ) : canDecide ? (
          <>
            <Button size="sm" variant="info" disabled={busy !== null || !text} onClick={onApprove}>
              {busy === "approve" ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />} {C.APPROVE}
            </Button>
            <Button size="sm" variant="outline" disabled={busy !== null} onClick={onDeny}>
              {busy === "deny" ? <Loader2 className="size-4 animate-spin" /> : <X className="size-4" />} {C.DENY}
            </Button>
          </>
        ) : (
          <Chip tone="neutral">{C.CANNOT_DECIDE}</Chip>
        )}
        <span className="ml-auto text-xs text-muted-foreground" title={absoluteTime(item.requested_at)}>
          requested {relativeTime(item.requested_at)}
        </span>
      </div>

      {!runEnded && proxyHeld && <p className="mt-2.5 text-xs text-muted-foreground">{held ? C.HELD_NOTE : C.HELD_EXPIRED}</p>}
      {!runEnded && canDecide && <p className="mt-1 text-xs text-muted-foreground">{C.DENY_NOTE}</p>}
    </div>
  );
}
