/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  canDecideAdoCapability,
  canDecideApproval,
  isAdoCapabilityRequest,
  runHasWorkspace,
  type ApprovalRequest,
  type AuditEvent,
  type CredentialGrant,
  type EgressDecision,
  type Recording,
  type RunDetail,
} from "../../../lib/types";
import { createRequestFromAudit } from "../../../lib/api/audit";
import { type HeldCredentials } from "../../../lib/held-credentials";
import { LiveApprovals, isHeld } from "../../wardyn/live-approvals";
import {
  useOperator,
  useOperatorResolved,
  usePrincipal,
  useSecurityOperator,
} from "../../wardyn/operator-context";
import { type ConsoleView } from "../../wardyn/console-view";
import { VIEWER_APPROVAL_BLOCKS_NOTE } from "../../wardyn/copy";
import { RunCanvas } from "./canvas";
import { RunFailureBlock } from "./failure-block";
import { LoginSandboxNote } from "./login-sandbox-note";
import { TerminalPane } from "./terminal-notice";
import type { WidgetContext } from "./widget-registry";

// Cockpit — the Overview tab. The terminal is the page: the Audit tab owns
// the event trail, so this component gives the terminal the full pane
// rather than sharing it with a timeline.
//
// Layout: a CONFIGURABLE canvas (design board 2b) — see run-detail/canvas.tsx.
// This component's whole job is to assemble the WidgetContext every widget
// reads from, including the terminal hero itself: the canvas PLACES the
// terminal, it does not build it, because building it needs the attach /
// recording / approvals graph that lives here.
export function Cockpit({
  run, view, terminal,
  grants,
  egress,
  audit,
  held,
  outcomeReady,
  pending,
  recording,
  recState,
  recordingDisabled,
  onGoAudit,
  onGoPolicy,
  onGoRecording,
}: {
  run: RunDetail;
  view: ConsoleView; // M-7: no relaunch/SSH/credential door in admin view.
  terminal: boolean;
  grants: CredentialGrant[];
  egress: EgressDecision[];
  audit: AuditEvent[];
  /** What a killed run held that a kill cannot revoke; undefined while loading. */
  held: HeldCredentials | undefined;
  /** False while a KILLED run's ending facts are still being read: no outcome block yet. */
  outcomeReady: boolean;
  /** This run's PENDING approvals — the viewer note, and (B3) the ONE live
   *  held count every widget reads; the decision surface itself is
   *  LiveApprovals' own poll. */
  pending: ApprovalRequest[];
  recording: Recording | null;
  recState: "idle" | "loading" | "error" | "ready";
  recordingDisabled: boolean;
  onGoAudit: () => void;
  onGoPolicy: () => void;
  onGoRecording: () => void;
}) {
  const principal = usePrincipal();
  // The run's REQUEST-scoped facts, off its run.create audit row — the only
  // durable record of task_mode, interactive_start, seed_auto_tools and
  // tool_approvals, none of which lands on AgentRun. Read once here; the exec
  // pane reads it below.
  const createRequest = createRequestFromAudit(audit);
  // useSecurityOperator, not useOperator (0.7 §B): this banner says "you can't
  // decide any of these", and authorizeUserDecision (approvals.go:392)
  // early-returns for the security tier — so a security admin can decide every
  // one of them and must never be told otherwise. The SUPER-only surfaces on
  // this page (attach, take-over) read useOperator in their own components.
  const securityOperator = useSecurityOperator();
  // The SUPER-admin question, for the widget context: ConnectSSHCard reads it
  // itself, and RUN_WIDGETS.ssh.available has to ask the same one.
  const operator = useOperator();
  const operatorResolved = useOperatorResolved();
  // "blocked until an admin decides" is FALSE for a re-auth row (UX round B2):
  // no admin decides it, and the person who can fix it is the credential's own
  // owner. The kind is excluded from the predicate rather than the sentence
  // reworded — a run whose ONLY pending row is a re-auth is not blocked on
  // anyone's decision at all, and the strip's own heading says what it needs.
  //
  // S10 round 2 (F2): an Azure DevOps escalation this viewer OWNS is ALSO
  // excluded — canDecideApproval doesn't know the ADO ownership carve-out
  // (canDecideAdoCapability does), so without this a run's own owner read
  // this "you're blocked" note over a card that, two lines below, lets them
  // decide it.
  const isRunOwner = run.created_by === principal;
  const viewerBlocked =
    pending.length > 0 &&
    !securityOperator &&
    pending.some((p) => p.kind !== "credential_reauth") &&
    !pending.some(
      (p) =>
        p.kind !== "credential_reauth" &&
        (canDecideApproval(false, p.kind) || (isAdoCapabilityRequest(p) && canDecideAdoCapability(false, isRunOwner))),
    );

  // The terminal widget's contents. Unchanged from the fixed-rail cockpit: the
  // session, and directly beneath it the approval that is HOLDING the session —
  // not in a sidebar, not a toast, because the person who has to decide it is
  // already looking here. Interactive OR autonomous, as long as the run is live.
  // task_mode lives only in the run.create audit event (request-scoped, never
  // on AgentRun) — this page already holds the full trail, so the pane can
  // speak honestly about a no-harness run for free.
  const execMode = createRequest.task_mode === "exec";
  const terminalPane = (
    <>
      {/* M7(b): above the terminal, because on a run that ended badly the
          replay is not the news — why it ended is. Inside the terminal widget
          rather than beside it so the canvas keeps placing exactly one hero,
          and nothing on this page moves for a run that ended fine (the block
          renders null unless the audit trail says otherwise). The clone door
          lives on the run header instead (0.7.3 F7), a strict superset of
          the states this block explains, so this block takes no onClone. */}
      <LoginSandboxNote run={run} />
      {outcomeReady && <RunFailureBlock run={run} audit={audit} held={held} onGoAudit={onGoAudit} />}
      <TerminalPane
        run={run}
        terminal={terminal}
        recording={recording}
        recState={recState}
        recordingDisabled={recordingDisabled}
        onGoRecording={onGoRecording}
        execMode={execMode}
      />
      {run.state === "RUNNING" && (
        <div className="shrink-0 space-y-2 pt-2.5">
          {/* Said once, where a viewer actually feels the consequence: the
              run is stopped and they personally cannot unstick it — the
              command bar states the pending count and the strip below is the
              decision surface, so this is the VIEWER-only half of that
              story. Same canDecideApproval kind-question: the page is
              already owner-scoped, so a member here owns every approval
              shown; egress_domain is theirs to decide, credential and
              tool_call stay admin-only regardless. */}
          {viewerBlocked && (
            <p className="rounded-lg border border-border bg-muted/40 px-2.5 py-2 text-xs leading-relaxed text-muted-foreground">
              {VIEWER_APPROVAL_BLOCKS_NOTE}
            </p>
          )}
          <LiveApprovals runId={run.id} hasWorkspace={runHasWorkspace(run)} run={run} adminView={view === "admin"} />
        </div>
      )}
    </>
  );

  const ctx: WidgetContext = {
    run,
    finished: terminal,
    principal,
    operator, operatorResolved, view, // ssh widget: owner-or-admin AND the user view (M-7).
    grants,
    egress,
    // B3 — the SAME derivation the command bar's "sandbox held" and the board's
    // card state already use (isHeld, live-approvals.tsx), not a second copy
    // and not a count of audit rows. `egress` above stays the history the rows
    // render; this is the state the alarm chip states.
    heldCount: pending.filter(isHeld).length,
    audit,
    onGoAudit,
    onGoPolicy,
    terminalPane,
  };

  return <RunCanvas ctx={ctx} />;
}
