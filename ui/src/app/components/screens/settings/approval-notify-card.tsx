/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// notify-e4 (packet M6 S3, approved 2026-10-03) — "Approval notifications": the
// seventh Admin Settings card. Read-only, no buttons: the channels are set by
// WARDYN_APPROVAL_NOTIFY at boot, so this card only says whether each one is
// delivering. It reads GET /approval-notify/status, which carries a destination
// HOST and a failure class and never a URL. Mounted only from
// AdminSettingsScreen, which a super admin alone can open.
import * as React from "react";
import { approvalNotify, type ApprovalNotifyStatus } from "../../../lib/api/approval-notify";
import { absoluteTime, relativeTime } from "../../../lib/format";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { Mono } from "../../wardyn/code-block";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { APPROVAL_NOTIFY as COPY } from "../../wardyn/copy/approval-notify";
import { ErrorState, TableSkeleton } from "../../wardyn/states";

export function ApprovalNotifyCard() {
  const [data, setData] = React.useState<ApprovalNotifyStatus | null>(null);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");

  const load = React.useCallback(() => {
    setStatus("loading");
    approvalNotify
      .getStatus()
      .then((s) => {
        setData(s);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const channels = data?.channels ?? [];
  // "N failed" counts channels with a dead row in the last hour (M6: 3 channels · 1 failed), not rows.
  const failed = channels.filter((c) => c.failed_last_hour > 0).length;
  // Absent while unloaded or failed, like every other card's summary: the
  // expanded body's ErrorState already says the read failed.
  const summary =
    status !== "ready"
      ? undefined
      : channels.length === 0
        ? COPY.SUMMARY_OFF
        : failed > 0
          ? COPY.SUMMARY_FAILED(channels.length, failed)
          : COPY.SUMMARY_OK(channels.length);

  return (
    <CollapsibleCard title={COPY.TITLE} summary={summary} testId="approval-notify-card">
      {status === "loading" ? (
        <TableSkeleton rows={1} cols={6} />
      ) : status === "error" ? (
        <ErrorState onRetry={load} />
      ) : channels.length === 0 ? (
        <p className="text-body leading-snug text-muted-foreground">
          {COPY.OFF_BODY[0]}
          <Mono>{COPY.ENV_NAME}</Mono>
          {COPY.OFF_BODY[1]}
        </p>
      ) : (
        <>
          <p className="text-body leading-snug text-muted-foreground">
            {COPY.LEDE[0]}
            <Mono>{COPY.ENV_NAME}</Mono>
            {COPY.LEDE[1]}
          </p>
          <div className="mt-3 overflow-hidden rounded-lg border border-border">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{COPY.COLS.CHANNEL}</TableHead>
                  <TableHead>{COPY.COLS.TYPE}</TableHead>
                  <TableHead>{COPY.COLS.DESTINATION}</TableHead>
                  <TableHead>{COPY.COLS.DELIVERED}</TableHead>
                  <TableHead>{COPY.COLS.ERROR}</TableHead>
                  <TableHead className="text-right">{COPY.COLS.FAILED}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {channels.map((c) => (
                  <TableRow key={c.id}>
                    <TableCell>
                      <Mono>{c.id}</Mono>
                    </TableCell>
                    <TableCell>{COPY.TYPE[c.type] ?? c.type}</TableCell>
                    <TableCell>
                      <Mono>{c.destination_host}</Mono>
                    </TableCell>
                    <TableCell>
                      {c.last_success_at ? (
                        <span title={absoluteTime(c.last_success_at)}>{relativeTime(c.last_success_at)}</span>
                      ) : (
                        COPY.NEVER
                      )}
                    </TableCell>
                    <TableCell>
                      {c.last_error ? (
                        <span className="inline-flex items-baseline gap-1.5">
                          <Mono>{c.last_error}</Mono>
                          {c.last_error_at && (
                            <span className="text-muted-foreground" title={absoluteTime(c.last_error_at)}>
                              {relativeTime(c.last_error_at)}
                            </span>
                          )}
                        </span>
                      ) : (
                        COPY.NONE
                      )}
                    </TableCell>
                    <TableCell className="text-right">{c.failed_last_hour}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <p className="mt-3 text-meta text-muted-foreground">
            {COPY.AUDIT_NOTE[0]}
            <Mono>{COPY.AUDIT_ACTION}</Mono>
            {COPY.AUDIT_NOTE[1]}
          </p>
        </>
      )}
    </CollapsibleCard>
  );
}
