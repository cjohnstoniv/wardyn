/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordPane — the workspace detail page's Sessions card body. The operator
// records one or more named sessions: each spins up an open (allow-all-egress)
// interactive sandbox with the repo cloned + the configured model provider
// wired, the operator drives the real activity (build, test, run the agent) in
// the embedded AttachTerminal, then clicks "Done recording" (capture happens
// on run termination). Once recorded, a session can be replayed confined
// (default-deny egress, limited to the approved set) to prove the approved set
// is enough — the least-privilege proof. Each session card lives through its
// own lifecycle in place: recording -> recorded -> [Replay confined] ->
// replaying -> replayed. There is no derived build/test taxonomy — sessions
// are whatever the operator names them, and no separate Record/Verify mode
// toggle — every session shows whichever stage it's actually in.
//
// This pane never navigates away and is never unmounted by its caller for an
// in-flight session — the detail page keeps sessions running across
// navigation (see workspace-copy.ts's C.SESSION_SURVIVES); it does not kill
// any in-flight run on its own unmount.
import * as React from "react";
import {
  Loader2,
  Radio,
  RotateCw,
  ShieldAlert,
  ShieldCheck,
  Square,
  TriangleAlert,
} from "lucide-react";
import type { ConfinementClass, Workspace, WorkspaceProfile } from "../../../lib/types";
import { useConsoleMode } from "../../wardyn/console-view";
import { AuthModeLine, DetectedHints, ModelAccessNote, stageChip } from "./record-pane-chips";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import {
  recordResult,
  recordSessions,
  orphanedVerifySessions,
  isRecording,
  lastCleanReplay,
  sessionStage,
  verifyKeyOf,
} from "./session-helpers";
import { relativeTime } from "../../../lib/format";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { CC_META } from "../../wardyn/cc-meta";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { C } from "../../../lib/workspace-copy";
import { RecordReviewCard, ConfinedReviewCard } from "./record-pane-review";

// A held request approved during a confined replay does more than
// release the connection — learnVerifyEgress (internal/api/approvals.go) folds
// the host into this workspace's required egress contract, so future runs
// never ask again. POLICIES.md's "Approval decision scopes" section already
// spells this out; the click surface itself must say so too. Local rather than
// workspace-copy.ts's mock-sourced canon — this line has no mock counterpart.
const VERIFY_APPROVE_LEARNS_HINT =
  "Approving a held request here also adds that host to this workspace's requirements — future runs won't ask again.";
import { useOperator, useSecurityOperator } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON, SECURITY_ONLY_REASON } from "../../wardyn/copy";

export function RecordPane({
  ws,
  notice,
  launch,
  busyTask,
  modelReady,
  hostClasses,
  onRecord,
  onReplayConfined,
  onDoneRecording,
  onPromoteEgress,
  onApproveHosts,
  onOpenProfile,
}: {
  ws: Workspace;
  // Inline notice from the last record/replay attempt (400 bad name; 503 no
  // runner; 409 another session already running).
  notice: { status: number; detail?: string } | null;
  // The last successful launch's own warnings + real confinement class
  // (handleRecordWorkspace's 202 body) — never dropped, and the
  // authoritative source for the CC1 banner below once a session has
  // actually launched (`tier` below is only a pre-launch guess).
  launch?: { warnings?: string[]; confinementClass?: string } | null;
  // The record_results key currently being kicked — a plain session key for an
  // open (re-)record, or verifyKeyOf(key) for a confined (re-)replay. Disables
  // just that session's matching button.
  busyTask: string | null;
  // Whether the operator has any working model/LLM path (subscription login, a
  // stored provider key, or a real composer backend) — a session runs the
  // agent, so without one the agent's model calls would be denied.
  modelReady: boolean;
  // The runner's declared confinement classes (setup status). A recording
  // launches under the strongest of these (workspace_run.go's bestClass), so
  // the banner's tier line derives from it pre-launch — never from New Run's
  // own pre-launch preview (an unrelated screen's guess).
  hostClasses?: ConfinementClass[] | null;
  // Start (or re-start) an open session by name; the server slugs it to the
  // record key.
  onRecord: (name: string) => void;
  // Replay an existing session confined (default-deny egress, limited to
  // approved) by its name — first run or a re-run.
  onReplayConfined: (name: string) => void;
  // Interactive "Done" — kills whichever run is active (open or confined); the
  // backend captures on termination.
  onDoneRecording: (runId: string) => void;
  // Approve an open session's observed hosts (promote-egress).
  onPromoteEgress: (taskKey: string) => void;
  // Approve the off-policy hosts a confined replay caught, as one requirements
  // write (widens the workspace's approved egress for every future replay).
  // `replayName` is the guided one-step loop: approve the selection, then
  // immediately replay that session confined again — the caller chains them so
  // the replay only fires once the approval has actually landed.
  onApproveHosts: (hosts: string[], replayName?: string) => void;
  // Open the existing ProfileReview drawer on a record run (Save profile).
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  // useSecurityOperator, not useOperator (0.7 §B): the decision route this
  // pane drives is on securityOps — .../record/{task}/promote-egress.
  // Promoting a workspace's observed egress into its allowlist is the security
  // tier's loop, so the pane as a whole opens to a security admin.
  //
  // Two families of control are the exception and gate themselves, in the
  // components that own them, because their calls land on operatorOnly:
  //   - launch (NewSessionForm, SessionCard): R1 moved POST
  //     /workspaces/{id}/record there — it decides no egress question, it
  //     starts a sandbox with open egress, the local_dir bind-mounted, the
  //     clone credential minted and the operator's LLM credential attached.
  //   - approve hosts (CaughtHosts): the approve-hosts path no longer PUTs
  //     .../approved-egress — workspace-detail.tsx's approveHosts writes one
  //     PUT .../requirements for N hosts, and requirements is operatorOnly
  //     (F031).
  // Same pattern as "Save profile", gated in profile-review.tsx over the
  // super-only /policies: the control is gated where its call lives, so this
  // pane never shows a security admin a live button the server refuses.
  const securityOperator = useSecurityOperator();
  // M-6/QM-10: which not-ready copy the model-access note below shows.
  const view = useConsoleMode();
  const sessions = recordSessions(ws);
  const orphans = orphanedVerifySessions(ws);
  // The record sandbox runs under the strongest class the host supports
  // (workspace_run.go's bestClass — never the policy floor, never the New-Run
  // default). Once a session has actually launched, `launch.confinementClass`
  // is the server's own verdict for that run — use it; before any launch,
  // derive the same answer from the runner's declared classes. The bare-CC1
  // fallback covers only an older server that reports neither.
  const tier = launch?.confinementClass ?? strongestAvailable(hostClasses ?? []) ?? "CC1";
  // Scan-detected commands become copy-paste hints so a clueless operator
  // knows what to run in the session — guidance without a taxonomy.
  const detected = ((ws.profile ?? {}) as WorkspaceProfile).setup_commands ?? [];
  // The workspace-wide roll-up — has the loop closed clean at
  // least once, for any session? Client-derived, no new WorkspaceStatus.
  const cleanReplay = lastCleanReplay(ws);

  return (
    // Every control in this pane (record/replay/approve-host/promote-egress)
    // needs at least the security tier server-side; a member would see them
    // all enabled and 403 on the first click. A native disabled fieldset gates
    // the whole subtree at once — same disabled:opacity-50 every Button here
    // already carries — instead of threading `disabled={!securityOperator}`
    // through SessionCard/RecordReviewCard/ConfinedReviewCard/NewSessionForm
    // one by one. The border/padding/min-width a bare <fieldset> adds are
    // reset so it stays visually identical to the plain <div> it replaces.
    // This is the floor, not the whole answer: the launch and approve-host
    // controls need the higher operatorOnly tier on top of it, and add their
    // own useOperator gate where they live (see the note above).
    <fieldset disabled={!securityOperator} className="m-0 min-w-0 border-0 p-0 space-y-4">
      {!securityOperator && <p className="text-xs text-muted-foreground">{SECURITY_ONLY_REASON}</p>}
      {/* No "Sessions" label here — the DetailSectionCard wrapping this pane already
          titles it; repeating it would show the same word twice on the page. */}
      <Chip tone="info">Recommended · skippable</Chip>
      <p className="text-sm leading-relaxed text-muted-foreground">
        Record a session for anything this workspace needs to do — build, run tests, drive the agent,
        deploy. Wardyn opens a sandbox with the repo and your model provider ready, watches what it
        reaches, and you approve those hosts. Once it&apos;s recorded, <strong>replay it confined</strong> —
        default-deny egress, only your approved access — to prove the approved set is enough; any
        off-policy host is <strong>blocked live</strong> and one click to approve.
      </p>

      <ModelAccessNote modelReady={modelReady} view={view} />

      <OpenEgressBanner tier={tier} />

      {/* 503: honest no-runner path. */}
      {notice?.status === 503 && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
          data-testid="record-no-runner"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>
            Recording or replaying needs a runner (this control plane runs{" "}
            <span className="font-mono">-runner none</span>).
          </p>
        </div>
      )}
      {notice?.status === 400 && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>{notice.detail || "Give the session a name (letters/digits) — e.g. “build & test”."}</p>
        </div>
      )}
      {notice?.status === 409 && (
        <p className="text-xs text-muted-foreground">
          {notice.detail || "Another session is already running for this workspace."}
        </p>
      )}
      {/* The last launch's own warnings (open-egress exfiltration window on
          weak confinement; the masking caveat) — the server's own caveats
          about THIS session, never silently dropped. */}
      {launch?.warnings && launch.warnings.length > 0 && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
          data-testid="record-launch-warnings"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <ul className="list-disc space-y-1 pl-4">
            {launch.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}

      {cleanReplay && (
        <p className="text-xs text-muted-foreground" data-testid="record-last-clean-replay">
          Last clean confined replay:{" "}
          <span className="font-medium text-foreground">{cleanReplay.label}</span>
          {cleanReplay.finishedAt && <> · {relativeTime(cleanReplay.finishedAt)}</>}
        </p>
      )}

      {sessions.length > 0 && (
        <div className="space-y-3" data-testid="record-tasks">
          {sessions.map((s) => (
            <SessionCard
              key={s.key}
              ws={ws}
              sessionKey={s.key}
              label={s.label}
              detected={detected.map((c) => c.command)}
              busyOpen={busyTask === s.key}
              busyConfined={busyTask === verifyKeyOf(s.key)}
              onRecord={onRecord}
              onReplayConfined={onReplayConfined}
              onDoneRecording={onDoneRecording}
              onPromoteEgress={onPromoteEgress}
              onApproveHosts={onApproveHosts}
              onOpenProfile={onOpenProfile}
            />
          ))}
        </div>
      )}
      {/* A confined session with no open sibling — e.g. a wizard Verify run
          left live (or just settled) after the wizard closed. Not one of the
          named sessions above (it has no "session" of its own to be a replay
          of), but it's a real, stoppable run and must be reachable from here. */}
      {orphans.length > 0 && (
        <div className="space-y-3" data-testid="record-orphaned-sessions">
          {orphans.map((s) => (
            <OrphanedSessionCard
              key={s.key}
              ws={ws}
              sessionKey={s.key}
              label={s.label}
              busy={busyTask === s.key}
              onDoneRecording={onDoneRecording}
              onApproveHosts={onApproveHosts}
              onOpenProfile={onOpenProfile}
            />
          ))}
        </div>
      )}
      {sessions.length === 0 && orphans.length === 0 && (
        <p className="text-xs text-muted-foreground">
          Never recorded. A session is the environment-proof: drive the workspace once in an open
          sandbox, then replay the capture confined.
        </p>
      )}
      <NewSessionForm
        existing={sessions.map((s) => s.label)}
        disabled={isRecording(ws) || notice?.status === 503}
        onRecord={onRecord}
      />
    </fieldset>
  );
}

// NewSessionForm — name a session and start recording it. Suggests "build & test"
// for the empty state so a first-time operator has a one-click starting point.
function NewSessionForm({
  existing,
  disabled,
  onRecord,
}: {
  existing: string[];
  disabled: boolean;
  onRecord: (name: string) => void;
}) {
  // A launch, not a decision: POST /workspaces/{id}/record is operatorOnly
  // (routes.go), so a security admin sees this whole form disabled rather than
  // enabled-then-403 — the pane's own rule for a super-only control. The form,
  // not just its button: a live name field over a dead Start is a worse lie
  // than a form that plainly says who may use it.
  const operator = useOperator();
  const [name, setName] = React.useState(existing.length === 0 ? "build & test" : "");
  const trimmed = name.trim();
  const start = () => {
    if (trimmed) onRecord(trimmed);
  };
  return (
    <div className="space-y-2 rounded-lg border border-dashed border-border p-3" data-testid="record-new-session">
      <SectionLabel>New session</SectionLabel>
      <div className="flex flex-wrap items-center gap-2">
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") start();
          }}
          placeholder="name this session — e.g. build & test"
          className="h-9 max-w-xs flex-1"
          aria-label="Session name"
          disabled={!operator}
        />
        <Button size="sm" onClick={start} disabled={disabled || !trimmed || !operator}>
          <Radio className="size-3.5" /> Start recording
        </Button>
      </div>
      {!operator && <p className="text-meta text-muted-foreground">{OPERATOR_ONLY_REASON}</p>}
      <p className="text-meta text-muted-foreground">
        Opens an attached terminal with the repo + your model provider ready. Do the real thing, then
        click Done recording to capture what it used. You can replay it confined once it settles.
      </p>
    </div>
  );
}

// Open-egress warning — every open recording allows all egress, on every tier
// (that is how it learns what the task uses), so the exfiltration window is
// real regardless of barrier strength and the banner always shows. The
// "weakest barrier" line is added only when the session genuinely runs under
// CC1 (Fence) — the shared-kernel case — per the launch's own verdict or the
// runner's strongest class; asserting a tier the run doesn't have is worse
// than no warning at all.
function OpenEgressBanner({ tier }: { tier: string }) {
  const weakest = tier === "CC1";
  const cc1 = CC_META.CC1;
  return (
    <div
      className={
        weakest
          ? "flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-subtle px-3 py-2.5 text-xs text-danger"
          : "flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
      }
      data-testid="record-open-egress-banner"
    >
      <ShieldAlert className="mt-0.5 size-4 shrink-0" />
      <div className="space-y-1">
        <p className="font-medium">
          {weakest
            ? `Open recording on ${cc1.label} — the weakest barrier, with egress unrestricted.`
            : "Open recording — egress unrestricted while it learns."}
        </p>
        <p className="leading-snug">
          To learn what a task really uses, this sandbox allows ALL egress — a task that misbehaves
          could send anything it can read out during the recording window.
          {weakest ? ` And on this host it runs under ${cc1.label}: ${cc1.metaphor}` : ""} Only record
          tasks you trust — replaying confined afterward re-runs them at least privilege.
        </p>
      </div>
    </div>
  );
}

// One session's lifecycle card: recording -> recorded -> [Replay confined] ->
// replaying -> replayed, all in place (no separate Record/Verify surfaces).
function SessionCard({
  ws,
  sessionKey,
  label,
  detected,
  busyOpen,
  busyConfined,
  onRecord,
  onReplayConfined,
  onDoneRecording,
  onPromoteEgress,
  onApproveHosts,
  onOpenProfile,
}: {
  ws: Workspace;
  sessionKey: string;
  label: string;
  detected: string[];
  busyOpen: boolean;
  busyConfined: boolean;
  onRecord: (name: string) => void;
  onReplayConfined: (name: string) => void;
  onDoneRecording: (runId: string) => void;
  onPromoteEgress: (taskKey: string) => void;
  onApproveHosts: (hosts: string[], replayName?: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  const stage = sessionStage(ws, sessionKey);
  const openRR = recordResult(ws, sessionKey);
  const confinedRR = recordResult(ws, verifyKeyOf(sessionKey));
  // Both buttons below fire the launch immediately on click;
  // the settled review they're sitting next to (RecordReviewCard /
  // ConfinedReviewCard) gets replaced by the live attach terminal on the
  // next poll with no chance to back out. Route through the same
  // AlertDialog idiom workspace-detail.tsx's own Rescan confirm uses.
  const [confirmKind, setConfirmKind] = React.useState<"record" | "replay" | null>(null);
  // The launch controls below (Re-record, Replay confined, Replay again) all
  // POST .../record, which R1 moved to operatorOnly — see the pane's own note.
  const operator = useOperator();

  return (
    <div className="rounded-lg border border-border p-3" data-testid={`session-${sessionKey}`}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-foreground">{label}</span>
        {stageChip(stage, confinedRR)}
      </div>

      {/* recording — embed the attach terminal + copy-paste command hints */}
      {stage === "recording" && openRR && (
        <div className="mt-3 space-y-2">
          <DetectedHints commands={detected} />
          {/* Operator-only, like every control in this pane. The session itself
              is an operator's (the launch routes are operatorOnly, routes.go),
              so a member viewing this workspace never owns the run — mounting
              the terminal for them only produces a failed ticket mint and one
              authz.denied{not_owner} row per mount. Nothing is hidden that they
              could otherwise have used. */}
          {operator && <AttachTerminal runId={openRR.run_id} />}
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(openRR.run_id)}>
            <Square className="size-3.5" /> Done recording
          </Button>
          <p className="text-meta leading-snug text-muted-foreground">{C.SESSION_SURVIVES}</p>
        </div>
      )}

      {/* recorded / record_failed — review card + re-record + (once recorded) Replay confined */}
      {(stage === "recorded" || stage === "record_failed") && openRR && (
        <div className="mt-3 space-y-3">
          <RecordReviewCard ws={ws} sessionKey={sessionKey} rr={openRR} onPromoteEgress={onPromoteEgress} onOpenProfile={onOpenProfile} />
          <div className="flex flex-wrap items-center gap-2">
            {/* Re-record and Replay confined both POST .../record (operatorOnly). */}
            <Button size="sm" variant="outline" onClick={() => setConfirmKind("record")} disabled={busyOpen || !operator}>
              {busyOpen ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
              Re-record
            </Button>
            {stage === "recorded" && (
              <Button size="sm" onClick={() => onReplayConfined(label)} disabled={busyConfined || !operator}>
                {busyConfined ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
                Replay confined
              </Button>
            )}
          </div>
        </div>
      )}

      {/* replaying — attach + live approvals + Done */}
      {stage === "replaying" && confinedRR && (
        <div className="mt-3 space-y-2">
          <AuthModeLine rr={confinedRR} />
          <DetectedHints commands={detected} />
          {operator && <AttachTerminal runId={confinedRR.run_id} />}
          {/* Off-policy egress escalates to a pending approval held live — decide it
              here without leaving the page. */}
          <LiveApprovals
            runId={confinedRR.run_id}
            reasonApprove="approved in replay"
            reasonDeny="rejected in replay"
            idleHint="Watching for off-policy egress — anything you run that isn't approved pauses here for you to approve or reject, live."
            hasWorkspace
            // No AgentRun in hand on this pane (confinedRR is a Recording,
            // not a run) — an Azure DevOps escalation is not expected here
            // (record/verify uses the pat/ssh lane, not live per-user Entra
            // dispatch); if one ever appears, a security operator still
            // decides it, and this component's own null-run handling shows
            // an honest "couldn't load this run" instead of a false claim.
            run={null}
          />
          <p className="text-meta leading-snug text-muted-foreground">{VERIFY_APPROVE_LEARNS_HINT}</p>
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(confinedRR.run_id)}>
            <Square className="size-3.5" /> Done
          </Button>
          <p className="text-meta leading-snug text-muted-foreground">{C.SESSION_SURVIVES}</p>
        </div>
      )}

      {/* replayed / replay_failed — containment review + re-run */}
      {(stage === "replayed" || stage === "replay_failed") && confinedRR && (
        <div className="mt-3 space-y-3">
          <ConfinedReviewCard
            ws={ws}
            rr={confinedRR}
            onApproveHosts={onApproveHosts}
            // The guided one-step loop only exists where there IS a session to
            // replay — an orphaned confined run has no open sibling to name.
            replayName={label}
            onOpenProfile={onOpenProfile}
          />
          <Button size="sm" variant="outline" onClick={() => setConfirmKind("replay")} disabled={busyConfined || !operator}>
            {busyConfined ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            Replay again
          </Button>
        </div>
      )}

      <AlertDialog open={confirmKind !== null} onOpenChange={(o) => !o && setConfirmKind(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{confirmKind === "record" ? "Re-record" : "Replay"} &quot;{label}&quot;?</AlertDialogTitle>
            <AlertDialogDescription>
              {confirmKind === "record"
                ? "The current recorded review for this session will be replaced once the new recording settles."
                : "The current confined-replay review for this session will be replaced once the new replay settles."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                const kind = confirmKind;
                setConfirmKind(null);
                if (kind === "record") onRecord(label);
                else if (kind === "replay") onReplayConfined(label);
              }}
            >
              {confirmKind === "record" ? "Re-record" : "Replay again"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// A confined result with no open sibling (orphanedVerifySessions): unlike a
// normal session's confined replay, there's no open half to derive a stage
// from — it's simply live or settled. Reuses ConfinedReviewCard for the
// settled review (the same containment breakdown a normal replay gets) and
// the same live-terminal + LiveApprovals + Done shape SessionCard's own
// "replaying" branch renders — no re-run affordance (there's no open session
// to name it after); Done is the one action this card offers.
function OrphanedSessionCard({
  ws,
  sessionKey,
  label,
  busy,
  onDoneRecording,
  onApproveHosts,
  onOpenProfile,
}: {
  ws: Workspace;
  sessionKey: string;
  label: string;
  busy: boolean;
  onDoneRecording: (runId: string) => void;
  onApproveHosts: (hosts: string[], replayName?: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  // Same operator gate as the sessions above: a live replay's run belongs to
  // the operator who launched it.
  const operator = useOperator();
  const rr = recordResult(ws, sessionKey);
  if (!rr) return null;
  const live = rr.status === "recording";

  return (
    <div className="rounded-lg border border-border p-3" data-testid={`session-${sessionKey}`}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-foreground">{label}</span>
        {stageChip(live ? "replaying" : rr.status === "record_failed" ? "replay_failed" : "replayed", rr)}
      </div>
      {live ? (
        <div className="mt-3 space-y-2">
          <AuthModeLine rr={rr} />
          {operator && <AttachTerminal runId={rr.run_id} />}
          <LiveApprovals
            runId={rr.run_id}
            reasonApprove="approved in replay"
            reasonDeny="rejected in replay"
            idleHint="Watching for off-policy egress — anything you run that isn't approved pauses here for you to approve or reject, live."
            hasWorkspace
            // See this pane's other LiveApprovals mount for why `run` is null.
            run={null}
          />
          <p className="text-meta leading-snug text-muted-foreground">{VERIFY_APPROVE_LEARNS_HINT}</p>
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(rr.run_id)} disabled={busy}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : <Square className="size-3.5" />}
            Done
          </Button>
          <p className="text-meta leading-snug text-muted-foreground">{C.SESSION_SURVIVES}</p>
        </div>
      ) : (
        <div className="mt-3">
          <ConfinedReviewCard ws={ws} rr={rr} onApproveHosts={onApproveHosts} onOpenProfile={onOpenProfile} />
        </div>
      )}
    </div>
  );
}
