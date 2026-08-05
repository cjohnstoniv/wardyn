/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wizard's LIVE verify session — the buttons that used to be inert. One
// click launches a CONFINED session against the workspace the wizard already
// created (default-deny egress limited to the contract; anything else is HELD
// at the door). The terminal and the live approve/deny strip embed right
// here; approving a held host writes the row into THIS workspace's contract
// server-side (the decide() hook), and finishing refreshes the wizard's map
// so the new rows appear on Reach without re-typing anything.
import * as React from "react";
import { Loader2, Square } from "lucide-react";
import { toast } from "sonner";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import type { Workspace } from "../../../lib/types";
import { Button } from "../../ui/button";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { C, RD2 } from "../../../lib/workspace-copy";

export function WizardVerifySession({
  ws,
  nothingResolves,
  onContractChanged,
}: {
  ws: Workspace;
  /** Hides the agent-flavored button (RD2.RECORD_NEEDS renders above it). */
  nothingResolves: boolean;
  /** Called after the session ends — the wizard refetches and absorbs the
   *  rows the server wrote (approved hosts land as contract rows live). */
  onContractChanged: () => void;
}) {
  const [runId, setRunId] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [notice, setNotice] = React.useState<string | null>(null);
  const [finished, setFinished] = React.useState(false);

  const launch = async () => {
    setBusy(true);
    setNotice(null);
    setFinished(false);
    try {
      const r = await workspacesApi.recordTask(ws.id, "verify", true);
      if (!r.ok || !r.record_run_id) {
        setNotice(r.detail ?? "The session did not start.");
        return;
      }
      setRunId(r.record_run_id);
    } catch (e) {
      toast.error("Verify session failed to start", { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const finish = async () => {
    if (!runId) return;
    setBusy(true);
    try {
      await runsApi.killRun(runId);
      setRunId(null);
      setFinished(true);
      onContractChanged();
    } catch (e) {
      toast.error("Failed to stop the session", { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  if (runId) {
    return (
      <div className="space-y-2" data-testid="verify-session-live">
        <AttachTerminal runId={runId} heightClass="h-72" />
        <LiveApprovals
          runId={runId}
          reasonApprove="approved in verify"
          reasonDeny="rejected in verify"
          idleHint="Watching for off-contract egress — anything you run that isn't in the contract pauses here for you to approve or deny, live. Approving writes the row into this workspace immediately."
        />
        <div className="flex items-center justify-between gap-2">
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SESSION_SURVIVES}</p>
          <Button type="button" size="sm" variant="outline" onClick={() => void finish()} disabled={busy}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : <Square className="size-3.5" />}
            Done verifying
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-2" data-testid="verify-session-launch">
      {notice && (
        <p className="rounded-lg border border-warning/30 bg-warning-subtle p-2.5 text-xs leading-snug text-warning">
          {notice}
        </p>
      )}
      {finished && (
        <p className="text-[0.6875rem] leading-snug text-success">
          Session ended — anything you approved is in the contract now (see Reach).
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        {!nothingResolves && (
          <Button type="button" size="sm" onClick={() => void launch()} disabled={busy}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            Verify with a session
          </Button>
        )}
        <Button
          type="button"
          size="sm"
          variant={nothingResolves ? "default" : "outline"}
          onClick={() => void launch()}
          disabled={busy}
        >
          Verify in a terminal
        </Button>
      </div>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">
        {nothingResolves ? RD2.TERMINAL_ONLY : RD2.RECORD_SUB}
      </p>
    </div>
  );
}
