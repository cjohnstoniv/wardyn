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
import { usePoll } from "../../lib/use-poll";
import { Button } from "../ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { Mono } from "./code-block";
import { OperatorOnlyHint, SectionLabel } from "./primitives";
import { useOperator } from "./operator-context";

const POLL_MS = 2000;

// A held request is a wait_for_review first-use approval — the proxy carries the
// mode in the approval's requested_scope so the UI can flag the live hold.
//
// Exported because the run cockpit's command bar states the same fact ("N
// waiting · sandbox held") one row above this strip. Two copies of the
// wait_for_review test would be two truths that can disagree, and the
// disagreement would read as "nothing is holding the sandbox" while the
// sandbox is, in fact, held.
export function isHeld(a: ApprovalRequest): boolean {
  return String((a.requested_scope?.mode as string) ?? "") === "wait_for_review";
}

export function LiveApprovals({
  runId,
  reasonApprove = "approved live",
  reasonDeny = "rejected live",
  idleHint = "Watching for off-policy egress — anything the agent tries that isn't allow-listed surfaces here to approve or deny, live.",
}: {
  runId: string;
  reasonApprove?: string;
  reasonDeny?: string;
  idleHint?: string;
}) {
  // Decides here go straight to the API with no ReasonDialog stop, so this is
  // the one gate for all three mount sites (run detail, demo screen, the
  // record-mode verify panel) — see approvals.tsx's PendingCard for the
  // queue-screen equivalent.
  const operator = useOperator();
  const [pending, setPending] = React.useState<ApprovalRequest[]>([]);
  const [busy, setBusy] = React.useState<string | null>(null);
  // Deny is irreversible for the rest of the session (there is no re-raise
  // once a host is denied) — a confirm stop, mirroring DeleteConfirmDialog's
  // pattern, so one misclick can't silently poison a host the operator meant
  // to keep. Approve stays a single click: it is the low-risk, correctable
  // direction (a wrongly-approved request is still visible in the audit log).
  const [denyTarget, setDenyTarget] = React.useState<ApprovalRequest | null>(null);

  const refresh = React.useCallback(async () => {
    try {
      const all = await api.listApprovals("PENDING");
      // Scoped to egress_domain only: the header/host/deny copy below is
      // egress-specific. credential and tool_call approvals for this run
      // still surface via the run detail's "Waiting for your confirmation"
      // banner, which routes to the full Approvals screen's kind-aware UI.
      setPending(all.filter((a) => a.run_id === runId && a.kind === "egress_domain"));
    } catch {
      /* transient poll error — keep the last snapshot */
    }
  }, [runId]);

  // usePoll drives the BACKGROUND refreshes only; the initial load is ours.
  React.useEffect(() => {
    void refresh();
  }, [refresh]);
  usePoll(refresh, POLL_MS, false);

  const decide = async (a: ApprovalRequest, approve: boolean) => {
    setBusy(a.id);
    try {
      if (approve) {
        // The SERVER writes the durable echo: approving a verify session's
        // egress request lands the host as an egress: requirement row in that
        // workspace's contract at the decide() chokepoint. No client-side
        // second write — the old onApproveHost callback wrote the legacy
        // approved_egress lane on top of it, two truths behind one click.
        await api.approve(a.id, reasonApprove);
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

  const confirmDeny = async () => {
    if (!denyTarget) return;
    await decide(denyTarget, false);
    setDenyTarget(null);
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
      <div className="flex items-center gap-2">
        <SectionLabel>
          {anyHeld ? "Sandbox is waiting — approve to let it through" : "Approval needed — off-policy egress"}
        </SectionLabel>
        {/* Named once for the whole panel, not per row. */}
        {!operator && <OperatorOnlyHint />}
      </div>
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
            <Button
              size="sm"
              variant="outline"
              className="h-7"
              disabled={!operator || busy === a.id}
              onClick={() => decide(a, true)}
            >
              <Check className="size-3.5" /> Approve
            </Button>
            <Button
              size="sm"
              variant="outline"
              className="h-7"
              disabled={!operator || busy === a.id}
              onClick={() => setDenyTarget(a)}
            >
              <X className="size-3.5" /> Deny
            </Button>
          </div>
        );
      })}
      <AlertDialog open={!!denyTarget} onOpenChange={(o) => !o && setDenyTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Deny{" "}
              <Mono>{denyTarget ? String((denyTarget.requested_scope?.host as string) ?? "unknown host") : ""}</Mono>?
            </AlertDialogTitle>
            <AlertDialogDescription>
              Denying blocks this host for the rest of the session — there is no undo and no re-raise
              once it's denied.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                confirmDeny();
              }}
              className="bg-danger text-danger-foreground hover:bg-danger/90"
            >
              <X className="size-3.5" /> Deny
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
