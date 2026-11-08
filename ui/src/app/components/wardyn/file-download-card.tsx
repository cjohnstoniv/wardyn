/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// FileDownloadCard: the console's rendering of an approval for ONE reviewed file download (a person asked to
// download a file from a run whose desktop.download is review; internal/types/file_download.go). Mounted where
// McpCallCard is, and built on its shell and props.
//
// NAME, SIZE AND HASH ONLY: a decider sees what the scope names and never the file. Nothing here fetches, previews or
// links the file. Approve is once and calls straight through (no scope menu, no reason dialog); the server refuses
// the asker's own approval, so the asker's card draws the pair disabled.
//
// THE SECOND-PERSON LINE is drawn on every card: it is the kind's fixed rule (secondHumanGate), not a field the row
// carries, so SecondPersonLine (which reads second_human) is not used. With no `deciders` on the row the security
// tier decides, and the Decided by line says so.
import { Check, Download, Loader2, X } from "lucide-react";
import type { AgentRun, ApprovalRequest, FileDownloadScope } from "../../lib/types";
import { isTerminalRunState } from "../../lib/types";
import { relativeTime, absoluteTime } from "../../lib/format";
import { APPROVAL, APPROVAL_BANNER_LABEL } from "./copy";
import { APPROVAL_MCP_CALL as MCP } from "./copy/approvals";
import { DECIDERS_INBOX } from "./copy/deciders-inbox";
import { FILE_DOWNLOAD_CARD as C } from "./copy/files-review";
import { Button } from "../ui/button";
import { ApprovalKindChip, Chip } from "./primitives";
import { joinOr, deciderPhrase } from "./decider-lines";
import { usePrincipal, useSecurityOperator } from "./operator-context";
import { transferSize } from "../screens/run-detail/desktop-files-panel";
import { escapeUnsafeRunes } from "../../lib/escape-runes";

export function FileDownloadCard({
  item,
  canDecide,
  run,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest & { requested_scope: FileDownloadScope };
  /** The viewer may decide this row (the caller's own rule for the kind). */
  canDecide: boolean;
  run: Pick<AgentRun, "state"> | null | undefined;
  busy: "approve" | "deny" | null;
  onApprove: () => void;
  onDeny: () => void;
}) {
  const scope = item.requested_scope;
  const principal = usePrincipal();
  const securityOperator = useSecurityOperator();
  const runEnded = !!run && isTerminalRunState(run.state);
  // The asker's own card: a decider's row names who asked, the asker's own list does not.
  const asker = !item.requested_by && !!principal && scope.requested_by === principal;
  const name = escapeUnsafeRunes(scope.name);
  const askedBy = escapeUnsafeRunes(scope.requested_by);
  const own = !item.requested_by && !securityOperator;
  const decidedBy = item.deciders && item.deciders.length > 0 ? joinOr(item.deciders.map((e) => deciderPhrase(e, own))) : DECIDERS_INBOX.SECURITY;
  const mayAct = canDecide && !asker;

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="file-download-card">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind="file_download" />
        <Download className="size-4 text-muted-foreground" aria-hidden="true" />
        <span className="min-w-0 break-all text-sm font-semibold text-foreground">{C.TITLE(name)}</span>
        <Chip tone="neutral">{MCP.ONCE}</Chip>
      </div>
      <p className="mt-1 text-xs text-muted-foreground">
        <span className="font-medium text-foreground">{DECIDERS_INBOX.DECIDED_BY_LABEL}</span> {decidedBy}
      </p>
      <p className="mt-1 text-xs text-muted-foreground">{DECIDERS_INBOX.SECOND_PERSON}</p>

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{C.FIELD_FILE}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{name}</dd>
        <dt className="text-muted-foreground">{C.FIELD_SIZE}</dt>
        <dd className="min-w-0 font-mono text-xs text-foreground">{C.SIZE(transferSize(scope.bytes), scope.bytes.toLocaleString("en-US"))}</dd>
        <dt className="text-muted-foreground">{C.FIELD_SHA}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{scope.sha256}</dd>
        <dt className="text-muted-foreground">{C.FIELD_ASKED_BY}</dt>
        <dd className="min-w-0 break-all font-mono text-xs text-foreground">{asker ? "you" : askedBy}</dd>
      </dl>

      {asker ? (
        <p className="mt-3 text-xs text-muted-foreground">{C.ASKER_NOTE}</p>
      ) : (
        <>
          <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
            <p className="text-foreground">
              <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span> {C.WHAT(askedBy, name)}
            </p>
            <p className="text-muted-foreground">
              <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span> {C.BLAST}
            </p>
          </div>
          <p className="mt-3 text-xs text-muted-foreground">{C.UNDER_BANNER}</p>
        </>
      )}

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        {runEnded ? (
          <p className="text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
        ) : (
          <>
            <Button size="sm" variant="info" disabled={!mayAct || busy !== null} onClick={onApprove}>
              {busy === "approve" ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />} {MCP.APPROVE}
            </Button>
            <Button size="sm" variant="outline" disabled={!mayAct || busy !== null} onClick={onDeny}>
              {busy === "deny" ? <Loader2 className="size-4 animate-spin" /> : <X className="size-4" />} {MCP.DENY}
            </Button>
            {!mayAct && <Chip tone="neutral">{DECIDERS_INBOX.SOMEONE_ELSE}</Chip>}
          </>
        )}
        <span className="ml-auto text-xs text-muted-foreground" title={absoluteTime(item.requested_at)}>
          requested {relativeTime(item.requested_at)}
        </span>
      </div>
    </div>
  );
}
