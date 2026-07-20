/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LiveApprovals — an inline approve/deny strip co-located with the run's live
// view (the attached terminal on run detail, or the verify panel during a
// recording). It polls the run's PENDING approvals every 2s and decides them in
// place, so a human watching the terminal never has to leave it to unblock an
// off-policy egress request.
//
// It highlights a HELD request (first_use mode wait_for_review): the sandbox
// connection is parked live waiting for this decision, so approving it lets the
// request through transparently. A passive deny_with_review pending is shown too,
// but without the "waiting" urgency.
import * as React from "react";
import { ShieldAlert, Clock, Check, X } from "lucide-react";
import { toast } from "sonner";
import type { ApprovalRequest } from "../../lib/types";
import { approvals as api } from "../../lib/api/approvals";
import { getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import { Mono } from "./code-block";
import { SectionLabel } from "./primitives";

const POLL_MS = 2000;

// A held request is a wait_for_review first-use approval — the proxy carries the
// mode in the approval's requested_scope so the UI can flag the live hold.
function isHeld(a: ApprovalRequest): boolean {
  return String((a.requested_scope?.mode as string) ?? "") === "wait_for_review";
}

export function LiveApprovals({
  runId,
  onApproveHost,
  reasonApprove = "approved live",
  reasonDeny = "rejected live",
  idleHint = "Watching for off-policy egress — anything the agent tries that isn't allow-listed surfaces here to approve or deny, live.",
}: {
  runId: string;
  onApproveHost?: (host: string) => void;
  reasonApprove?: string;
  reasonDeny?: string;
  idleHint?: string;
}) {
  const [pending, setPending] = React.useState<ApprovalRequest[]>([]);
  const [busy, setBusy] = React.useState<string | null>(null);

  const refresh = React.useCallback(async () => {
    try {
      const all = await api.listApprovals("PENDING");
      setPending(all.filter((a) => a.run_id === runId));
    } catch {
      /* transient poll error — keep the last snapshot */
    }
  }, [runId]);

  React.useEffect(() => {
    void refresh();
    const t = setInterval(() => void refresh(), POLL_MS);
    return () => clearInterval(t);
  }, [refresh]);

  const decide = async (a: ApprovalRequest, approve: boolean) => {
    const host = String((a.requested_scope?.host as string) ?? "");
    setBusy(a.id);
    try {
      if (approve) {
        await api.approve(a.id, reasonApprove);
        if (host) onApproveHost?.(host); // widen the workspace's approved egress
      } else {
        await api.deny(a.id, reasonDeny);
      }
      await refresh();
    } catch (e) {
      toast.error(approve ? "Approve failed" : "Deny failed", {
        description: getErrorMessage(e),
      });
    } finally {
      setBusy(null);
    }
  };

  if (pending.length === 0) {
    return (
      <p className="text-[0.6875rem] text-muted-foreground" data-testid="live-approvals-idle">
        {idleHint}
      </p>
    );
  }

  const anyHeld = pending.some(isHeld);

  return (
    <div
      className="space-y-1.5 rounded-lg border border-warning/40 bg-warning-subtle p-2.5"
      data-testid="live-approvals"
    >
      <SectionLabel>
        {anyHeld ? "Sandbox is waiting — approve to let it through" : "Approval needed — off-policy egress"}
      </SectionLabel>
      {pending.map((a) => {
        const host = String((a.requested_scope?.host as string) ?? "unknown host");
        const held = isHeld(a);
        return (
          <div key={a.id} className="flex items-center gap-2" data-testid="live-approval-row">
            {held ? (
              <Clock className="size-3.5 shrink-0 text-warning" aria-label="request held live" />
            ) : (
              <ShieldAlert className="size-3.5 shrink-0 text-warning" />
            )}
            <Mono className="flex-1 text-foreground">{host}</Mono>
            {held && <span className="text-[0.625rem] uppercase tracking-wide text-warning">waiting</span>}
            <Button size="sm" variant="outline" className="h-7" disabled={busy === a.id} onClick={() => decide(a, true)}>
              <Check className="size-3.5" /> Approve
            </Button>
            <Button size="sm" variant="outline" className="h-7" disabled={busy === a.id} onClick={() => decide(a, false)}>
              <X className="size-3.5" /> Deny
            </Button>
          </div>
        );
      })}
    </div>
  );
}
