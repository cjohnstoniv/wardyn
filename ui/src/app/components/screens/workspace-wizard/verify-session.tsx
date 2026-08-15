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
  runId,
  onRunIdChange,
  onContractChanged,
}: {
  ws: Workspace;
  /** Hides the agent-flavored button (RD2.RECORD_NEEDS renders above it). */
  nothingResolves: boolean;
  /** UI-WS-10: lifted into the WIZARD's own state, not local — this
   *  component only mounts while the wizard is on the Verify step, so local
   *  state orphaned the run the instant a Back/rail click unmounted it: the
   *  session kept going server-side (C.SESSION_SURVIVES) with no way back to
   *  it from here, and relaunching hit a 409. */
  runId: string | null;
  onRunIdChange: (runId: string | null) => void;
  /** Called after the session ends — the wizard refetches and absorbs the
   *  rows the server wrote (approved hosts land as contract rows live). */
  onContractChanged: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [notice, setNotice] = React.useState<string | null>(null);
  const [finished, setFinished] = React.useState(false);

  // Reattach to a session already running server-side that this mount never
  // saw start — the wizard's own lifted state only survives a Back/rail click
  // WITHIN one mounted wizard; a fresh "Edit workspace…" open (or a page
  // refresh) has none of that, so re-derive it from the workspace itself.
  // record.go always stores the wizard's Verify session under the fixed key
  // "verify:verify" (verifyKeyOf("verify"), session-helpers.ts).
  React.useEffect(() => {
    if (runId) return;
    const rr = ws.record_results?.["verify:verify"];
    if (rr?.status === "recording") onRunIdChange(rr.run_id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ws.record_results]);

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
      onRunIdChange(r.record_run_id);
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
      onRunIdChange(null);
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
      {/* ui-wsWizard-1: this used to be two buttons — "Verify with a session"
          and "Verify in a terminal" — both wired to the identical launch(),
          which starts the same CONFINED session (terminal + live approvals)
          either way. Two labels for one action is a broken promise, not a
          choice; one truthfully-labeled button, contextual by nothingResolves. */}
      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" onClick={() => void launch()} disabled={busy}>
          {busy && <Loader2 className="size-3.5 animate-spin" />}
          {nothingResolves ? "Verify in a terminal" : "Verify with a session"}
        </Button>
      </div>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">
        {nothingResolves ? RD2.TERMINAL_ONLY : RD2.RECORD_SUB}
      </p>
    </div>
  );
}
