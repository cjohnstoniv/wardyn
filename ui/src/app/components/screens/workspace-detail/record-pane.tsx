/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordPane — the workspace detail page's Sessions card body. The operator
// records one or more NAMED SESSIONS: each spins up an open (allow-all-egress)
// interactive sandbox with the repo cloned + the configured model provider
// wired, the operator drives the real activity (build, test, run the agent) in
// the embedded AttachTerminal, then clicks "Done recording" (capture happens
// on run termination). Once recorded, a session can be REPLAYED CONFINED
// (default-deny egress, limited to the approved set) to prove the approved set
// is enough — the least-privilege proof. Each session card lives through its
// own lifecycle in place: recording -> recorded -> [Replay confined] ->
// replaying -> replayed. There is NO derived build/test taxonomy — sessions
// are whatever the operator names them, and NO separate Record/Verify mode
// toggle — every session shows whichever stage it's actually in.
//
// This pane never navigates away and is never unmounted by its caller for an
// in-flight session — the detail page keeps sessions running across
// navigation (see workspace-copy.ts's C.SESSION_SURVIVES); it does not kill
// any in-flight run on its own unmount, unlike the retired import panel.
import * as React from "react";
import {
  Check,
  Info,
  Loader2,
  Radio,
  RotateCw,
  Save,
  ShieldAlert,
  ShieldCheck,
  Square,
  TriangleAlert,
} from "lucide-react";
import type { ConfinementClass, RecordResult, Workspace, WorkspaceProfile } from "../../../lib/types";
import { AuthModeLine, DetectedHints, HonestyNote, stageChip } from "./record-pane-chips";
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
  approvedEgressSet,
  recordResult,
  recordSessions,
  orphanedVerifySessions,
  isRecording,
  isEmptyCapture,
  egressPromotionDiff,
  lastCleanReplay,
  policyNameFor,
  sessionStage,
  verifyKeyOf,
} from "./session-helpers";
import { relativeTime } from "../../../lib/format";
import { Observations } from "../profile-review";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { CC_META } from "../../wardyn/cc-meta";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { C } from "../../../lib/workspace-copy";

// W19-W19b-1: a held request approved during a confined replay does more than
// release the connection — learnVerifyEgress (internal/api/approvals.go) folds
// the host into this workspace's required egress contract, so future runs
// never ask again. POLICIES.md's "Approval decision scopes" section already
// spells this out; the click surface itself didn't. Local rather than
// workspace-copy.ts's mock-sourced canon — this line has no mock counterpart.
const VERIFY_APPROVE_LEARNS_HINT =
  "Approving a held request here also adds that host to this workspace's requirements — future runs won't ask again.";
import { useSecurityOperator } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";

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
  // The last successful launch's own warnings + REAL confinement class
  // (handleRecordWorkspace's 202 body) — W20-S1-2: never dropped, and the
  // authoritative source for the CC1 banner below once a session has
  // actually launched (getDefaultCc() is only a pre-launch guess).
  launch?: { warnings?: string[]; confinementClass?: string } | null;
  // The record_results key currently being kicked — a plain session key for an
  // open (re-)record, or verifyKeyOf(key) for a confined (re-)replay. Disables
  // just that session's matching button.
  busyTask: string | null;
  // Whether the operator has ANY working model/LLM path (subscription login, a
  // stored provider key, or a real composer backend) — a session runs the
  // agent, so without one the agent's model calls would be denied.
  modelReady: boolean;
  // The runner's declared confinement classes (setup status). A recording
  // launches under the STRONGEST of these (workspace_run.go's bestClass), so
  // the banner's tier line derives from it pre-launch — never from the
  // operator's persisted New-Run default, which is an unrelated preference
  // and once printed "Fence" under a Vault capture, on camera.
  hostClasses?: ConfinementClass[] | null;
  // Start (or re-start) an OPEN session by name; the server slugs it to the
  // record key.
  onRecord: (name: string) => void;
  // Replay an existing session CONFINED (default-deny egress, limited to
  // approved) by its name — first run or a re-run.
  onReplayConfined: (name: string) => void;
  // Interactive "Done" — kills whichever run is active (open or confined); the
  // backend captures on termination.
  onDoneRecording: (runId: string) => void;
  // Approve an open session's observed hosts (promote-egress).
  onPromoteEgress: (taskKey: string) => void;
  // Approve the off-policy hosts a confined replay caught, as ONE requirements
  // write (widens the workspace's approved egress for every future replay).
  // `replayName` is the guided one-step loop: approve the selection, then
  // immediately replay THAT session confined again — the caller chains them so
  // the replay only fires once the approval has actually landed.
  onApproveHosts: (hosts: string[], replayName?: string) => void;
  // Open the existing ProfileReview drawer on a record run (Save profile).
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  // useSecurityOperator, not useOperator (0.7 §B): every route this pane
  // drives is on securityOps — POST /workspaces/{id}/record and
  // .../record/{task}/promote-egress (routes.go:388-389), and the
  // approve-hosts path's PUT .../approved-egress (routes.go:363). Recording a
  // workspace's real egress and promoting it into the allowlist IS the
  // security tier's loop. The ONE control under this fieldset that reaches a
  // super-only route is "Save profile", whose drawer POSTs /policies — gated
  // separately in profile-review.tsx, where that call actually lives.
  const securityOperator = useSecurityOperator();
  const sessions = recordSessions(ws);
  const orphans = orphanedVerifySessions(ws);
  // The record sandbox runs under the strongest class the host supports
  // (workspace_run.go's bestClass — never the policy floor, never the New-Run
  // default). Once a session has actually launched, `launch.confinementClass`
  // is the SERVER's own verdict for that run — use it; before any launch,
  // derive the same answer from the runner's declared classes. The bare-CC1
  // fallback covers only an older server that reports neither.
  const tier = launch?.confinementClass ?? strongestAvailable(hostClasses ?? []) ?? "CC1";
  // Scan-detected commands become copy-paste hints so a clueless operator
  // knows what to run in the session — guidance without a taxonomy.
  const detected = ((ws.profile ?? {}) as WorkspaceProfile).setup_commands ?? [];
  // Workstream B3: the workspace-wide roll-up — has the loop closed clean at
  // least once, for ANY session? Client-derived, no new WorkspaceStatus.
  const cleanReplay = lastCleanReplay(ws);

  return (
    // Every control in this pane (record/replay/approve-host/promote-egress)
    // is operatorOnly server-side; a viewer would see them all enabled and
    // 403 on the first click. A native disabled fieldset gates the whole
    // subtree at once — same disabled:opacity-50 every Button here already
    // carries — instead of threading `disabled={!securityOperator}` through
    // SessionCard/RecordReviewCard/ConfinedReviewCard/NewSessionForm one by
    // one. The border/padding/min-width a bare <fieldset> adds are reset so
    // it stays visually identical to the plain <div> it replaces.
    <fieldset disabled={!securityOperator} className="m-0 min-w-0 border-0 p-0 space-y-4">
      {!securityOperator && <p className="text-xs text-muted-foreground">{OPERATOR_ONLY_REASON}</p>}
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

      {/* Model-access note: a session runs the agent, so it uses the configured provider. */}
      {modelReady ? (
        <div className="flex items-start gap-2 rounded-lg border border-border bg-surface-2/60 px-3 py-2 text-xs text-muted-foreground">
          <Info className="mt-0.5 size-4 shrink-0 text-primary" />
          <p>
            Sessions run with your configured model provider (injected proxy-side — nothing sensitive
            stays resident) so the agent can make changes.
          </p>
        </div>
      ) : (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>
            No model provider is configured, so an agent won&apos;t reach a model in a session — set one up
            in Getting started. You can still record plain build/test sessions.
          </p>
        </div>
      )}

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
        />
        <Button size="sm" onClick={start} disabled={disabled || !trimmed}>
          <Radio className="size-3.5" /> Start recording
        </Button>
      </div>
      <p className="text-meta text-muted-foreground">
        Opens an attached terminal with the repo + your model provider ready. Do the real thing, then
        click Done recording to capture what it used. You can replay it confined once it settles.
      </p>
    </div>
  );
}

// Open-egress warning — every open recording allows ALL egress, on every tier
// (that is how it learns what the task uses), so the exfiltration window is
// real regardless of barrier strength and the banner always shows. The
// "weakest barrier" line is ADDED only when the session genuinely runs under
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
  // ui-wsDetail-3: both buttons below fire the launch immediately on click;
  // the settled review they're sitting next to (RecordReviewCard /
  // ConfinedReviewCard) gets replaced by the live attach terminal on the
  // next poll with no chance to back out. Route through the same
  // AlertDialog idiom workspace-detail.tsx's own Rescan confirm uses.
  const [confirmKind, setConfirmKind] = React.useState<"record" | "replay" | null>(null);

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
          <AttachTerminal runId={openRR.run_id} />
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
            <Button size="sm" variant="outline" onClick={() => setConfirmKind("record")} disabled={busyOpen}>
              {busyOpen ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
              Re-record
            </Button>
            {stage === "recorded" && (
              <Button size="sm" onClick={() => onReplayConfined(label)} disabled={busyConfined}>
                {busyConfined ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
                Replay confined
              </Button>
            )}
          </div>
        </div>
      )}

      {/* replaying — attach + LIVE approvals + Done */}
      {stage === "replaying" && confinedRR && (
        <div className="mt-3 space-y-2">
          <AuthModeLine rr={confinedRR} />
          <DetectedHints commands={detected} />
          <AttachTerminal runId={confinedRR.run_id} />
          {/* Off-policy egress escalates to a pending approval held live — decide it
              here without leaving the page. */}
          <LiveApprovals
            runId={confinedRR.run_id}
            reasonApprove="approved in replay"
            reasonDeny="rejected in replay"
            idleHint="Watching for off-policy egress — anything you run that isn't approved pauses here for you to approve or reject, live."
            hasWorkspace
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
          <Button size="sm" variant="outline" onClick={() => setConfirmKind("replay")} disabled={busyConfined}>
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
          <AttachTerminal runId={rr.run_id} />
          <LiveApprovals
            runId={rr.run_id}
            reasonApprove="approved in replay"
            reasonDeny="rejected in replay"
            idleHint="Watching for off-policy egress — anything you run that isn't approved pauses here for you to approve or reject, live."
            hasWorkspace
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

// Per-session review card, shown once the OPEN recording settles. Renders the
// SAME Observations block profile-review uses, a one-click egress-promotion
// diff, secrets proven-used chips, a Save-profile hand-off to the
// ProfileReview drawer, and the honesty notes.
function RecordReviewCard({
  ws,
  sessionKey,
  rr,
  onPromoteEgress,
  onOpenProfile,
}: {
  ws: Workspace;
  sessionKey: string;
  rr: RecordResult;
  onPromoteEgress: (taskKey: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  const empty = isEmptyCapture(rr);

  // Empty capture is a FAILURE, never a success — render the reachability hint and
  // stop (there are no trustworthy observations to promote from).
  if (rr.status === "record_failed" || empty) {
    return (
      <div
        className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
        data-testid="record-empty-capture"
      >
        <TriangleAlert className="mt-0.5 size-4 shrink-0" />
        <p>
          {rr.failure_hint ||
            "The recording captured no egress. This almost always means the sandbox couldn't reach the control plane to report its decisions (e.g. WSL2 NAT) — NOT that the task needs no egress. Fix reachability, then re-record."}
        </p>
      </div>
    );
  }

  // Egress promotion diff: hosts observed (allow_count>0) bucketed into
  // approvable / already-approved / platform-plumbing (W20-S1-1) — one
  // function so a host can't land in more than one bucket. selfHost mirrors
  // the server's own control-plane-host exclusion; the console is always
  // same-origin with wardynd (lib/api/core.ts's relative BASE), so the
  // browser's own hostname IS that host.
  const diff = egressPromotionDiff(ws, sessionKey, window.location.hostname);
  const newHosts = diff.approvable;
  const alreadyApproved = diff.alreadyApproved;
  // Distinct from "nothing NEW because it's already allowed": these hosts
  // were never approvable at all (harness/control-plane plumbing), so
  // claiming "already allowed" would misattribute them to an operator
  // decision that never happened.
  const onlyPlumbingObserved = newHosts.length === 0 && alreadyApproved.length === 0 && diff.plumbing.length > 0;

  // Secrets proven-used = the workspace's DECLARED required-secret names that this
  // run actually minted a grant for. Render-derived intersection — never mutates
  // the scan-owned profile.
  const profile = (ws.profile ?? {}) as WorkspaceProfile;
  const required = (profile.required_secrets ?? []).map((s) => s.name);
  const minted = rr.secret_names_minted ?? [];
  const proven = required.filter((n) => minted.includes(n));

  return (
    <div className="space-y-4 rounded-lg border border-border p-3" data-testid="record-review">
      {/* --- observed egress + one-click promotion --- */}
      <section className="space-y-2">
        <SectionLabel>Observed egress</SectionLabel>
        {/* egress_promoted is a BOOLEAN the server flips on any promoted>0, and
            which hosts a promote landed is not persisted — so a partial promote
            (now reachable: the confirm's checkboxes send a subset) can't be
            reported as "N of M". The only honest count is what's STILL
            approvable right now, straight from the buckets; the remainder keeps
            its listing and its Approve button instead of hiding behind a
            green all-done chip. */}
        {rr.egress_promoted && (
          <Chip tone={newHosts.length === 0 ? "success" : "warning"} dot>
            <Check className="size-3.5" />
            {newHosts.length === 0
              ? "Promoted"
              : `Promoted — ${newHosts.length} still need${newHosts.length === 1 ? "s" : ""} approval`}
          </Chip>
        )}
        {newHosts.length === 0 ? (
          !rr.egress_promoted && (
            <p className="text-meta text-muted-foreground">
              {onlyPlumbingObserved
                ? "Nothing needed approval — observed hosts were platform plumbing."
                : "No new hosts to approve — everything this task reached is already allowed."}
            </p>
          )
        ) : (
          <>
            <ul className="space-y-1.5" data-testid="record-new-hosts">
              {newHosts.map((h) => (
                <li key={h}>
                  <Mono className="text-foreground">{h}</Mono>
                </li>
              ))}
            </ul>
            <Button size="sm" variant="outline" onClick={() => onPromoteEgress(sessionKey)}>
              <Check className="size-3.5" /> Approve {newHosts.length} observed host
              {newHosts.length === 1 ? "" : "s"}
            </Button>
          </>
        )}
        {alreadyApproved.length > 0 && (
          <ul className="space-y-1 pt-1" aria-label="Already approved">
            {alreadyApproved.map((h) => (
              <li key={h} className="flex items-center gap-1.5 text-meta text-muted-foreground">
                <Check className="size-3 shrink-0" />
                <span className="font-mono line-through">{h}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* --- secrets proven used (intersection, render-derived) --- */}
      {proven.length > 0 && (
        <section className="space-y-1.5">
          <SectionLabel>Secrets proven used</SectionLabel>
          <div className="flex flex-wrap gap-1.5" data-testid="record-proven-secrets">
            {proven.map((n) => (
              <Chip key={n} tone="info" mono>
                {n}
              </Chip>
            ))}
          </div>
        </section>
      )}

      {/* --- the raw observations block (same as profile-review) --- */}
      {rr.observations && <Observations observations={rr.observations} />}

      {/* --- honesty notes (all non-negotiable) --- */}
      <div className="space-y-1.5">
        {(rr.caveats?.length
          ? rr.caveats
          : [
              "Secret masking is seed-ahead: any secret NOT declared in Requirements that this open run touched is not masked in the logs or observations above. Treat anything here as sensitive.",
            ]
        ).map((c, i) => (
          <HonestyNote key={i} text={c} />
        ))}
        {rr.kernel_sensor_blind && (
          <HonestyNote text="This task recorded inside a hardware VM (Vault/CC3); the syscall sensor can't see into it, so exec, file-write, and connect observations may be incomplete. Egress (proxy-side) is still complete." />
        )}
      </div>

      {/* --- optional: persist this session's synthesized least-privilege profile --- */}
      <div className="flex justify-end">
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onOpenProfile(rr.run_id, policyNameFor(ws.name, rr.label ?? "recorded"))}
        >
          <Save className="size-3.5" /> Save session profile
        </Button>
      </div>
    </div>
  );
}

// Review card for a settled CONFINED replay. Unlike the open-record card
// (which promotes newly-observed hosts), this proves least privilege: it splits
// what the run reached into ALLOWED (worked within the approved set), BLOCKED
// (off-policy, denied live — the containment proof), and PENDING (first-use,
// awaiting approval). Blocked/pending hosts are one click to approve if they're
// legitimately needed. All counts come straight from the capture — no extra fetch.
function ConfinedReviewCard({
  ws,
  rr,
  replayName,
  onApproveHosts,
  onOpenProfile,
}: {
  ws: Workspace;
  rr: RecordResult;
  // Present only where a re-replay is possible (a named session, not an
  // orphaned confined run) — gates the guided "approve selected + replay
  // again" button; the per-host Approve buttons render either way.
  replayName?: string;
  onApproveHosts: (hosts: string[], replayName?: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  // record_failed is a real failure (couldn't reach the control plane to report) —
  // show the hint and stop; there's nothing trustworthy to render.
  if (rr.status === "record_failed") {
    return (
      <div
        className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
        data-testid="verify-session-failed"
      >
        <TriangleAlert className="mt-0.5 size-4 shrink-0" />
        <p>{rr.failure_hint || "The confined replay captured no egress decisions — fix reachability and re-run."}</p>
      </div>
    );
  }

  const domains = rr.observations?.domains ?? [];
  // Subtract the SAME union egressPromotionDiff subtracts (legacy
  // ApprovedEgress + profile.egress_domains + egress: requirement rows), not
  // ws.approved_egress alone — promote and the per-host approve both write the
  // REQUIREMENTS lane now, so a bucket reading only the legacy lane kept
  // listing hosts that are already granted, under a button that would fold in
  // nothing. (Chip vs bucket CAN diverge, by design: the chip is the server's
  // verdict for the replay as it happened — an immutable fact — while these
  // buckets are "what is still off-policy RIGHT NOW". Approve a caught host
  // and the bucket empties while "Replayed — caught 2" stands, because it did.
  // The loop's answer to a stale verdict is a fresh replay, not a re-render.)
  const approved = approvedEgressSet(ws);
  const allowed = domains.filter((d) => d.allow_count > 0).map((d) => d.host);
  // ONE row per caught host: a host both denied AND held used to render twice
  // (two independent filters), and the checkbox list below can't have a host
  // in two states at once. `denied` wins the label — it's the stronger fact,
  // and it's what defaults the checkbox OFF.
  const caught = domains
    .filter((d) => (d.deny_count > 0 || d.pending_count > 0) && !approved.has(d.host))
    .map((d) => ({ host: d.host, denied: d.deny_count > 0 }));

  return (
    <div className="space-y-4 rounded-lg border border-border p-3" data-testid="verify-session-review">
      <AuthModeLine rr={rr} />
      {/* worked within the approved set */}
      <section className="space-y-2">
        <SectionLabel>Ran within your approved access</SectionLabel>
        {allowed.length === 0 ? (
          <p className="text-meta text-muted-foreground">
            No egress captured yet — re-run your build/test/agent steps in the session above.
          </p>
        ) : (
          <p className="flex items-center gap-1.5 text-sm text-success">
            <ShieldCheck className="size-4" /> {allowed.length} host{allowed.length === 1 ? "" : "s"} reached,
            all allowed.
          </p>
        )}
      </section>

      {/* off-policy attempts caught — the containment proof, and the loop's
          one-step continuation (approve what was genuinely missed, replay). */}
      {caught.length > 0 && (
        <CaughtHosts caught={caught} replayName={replayName} onApproveHosts={onApproveHosts} />
      )}

      {/* the raw observations block (same as profile-review / open-record card) */}
      {rr.observations && <Observations observations={rr.observations} />}

      <div className="flex justify-end">
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onOpenProfile(rr.run_id, policyNameFor(ws.name, rr.label ?? "recorded"))}
        >
          <Save className="size-3.5" /> Save session profile
        </Button>
      </div>
    </div>
  );
}

// The off-policy hosts a confined replay caught, each selectable, plus the
// loop's one-step continuation: approve the selection and replay again.
//
// Bulk-approving everything caught is a footgun the moment one of them is a
// genuine villain — which is the whole reason the replay ran. So a host that
// was DENIED LIVE (deny_count > 0) starts UNCHECKED and a merely-held one
// starts checked: the default is "approve the misses, leave the denials out",
// and changing it is a visible, deliberate click. The per-host Approve buttons
// stay for the one-off case (and route through the untrusted-content confirm
// exactly as before).
function CaughtHosts({
  caught,
  replayName,
  onApproveHosts,
}: {
  caught: { host: string; denied: boolean }[];
  replayName?: string;
  onApproveHosts: (hosts: string[], replayName?: string) => void;
}) {
  // A settled replay's observations are immutable, so seeding once is right —
  // and it means an operator's un/checking is never stomped by the detail
  // page's poll. (A re-replay unmounts this card via the "replaying" stage.)
  const [selected, setSelected] = React.useState<Set<string>>(
    () => new Set(caught.filter((c) => !c.denied).map((c) => c.host)),
  );
  // Intersect with what's STILL caught: approving a host shrinks the list
  // under us, and a stale selection must never widen the next write.
  const picked = caught.filter((c) => selected.has(c.host)).map((c) => c.host);

  return (
    <section className="space-y-2" data-testid="verify-session-blocked">
      <SectionLabel>Off-policy attempts caught</SectionLabel>
      <ul className="space-y-1.5">
        {caught.map(({ host, denied }) => (
          <li key={host} className="flex items-center gap-2">
            <Checkbox
              id={`caught-${host}`}
              checked={selected.has(host)}
              onCheckedChange={(v) =>
                setSelected((prev) => {
                  const next = new Set(prev);
                  if (v === true) next.add(host);
                  else next.delete(host);
                  return next;
                })
              }
              aria-label={`Approve ${host}`}
            />
            {denied ? (
              <ShieldAlert className="size-3.5 shrink-0 text-danger" />
            ) : (
              <Info className="size-3.5 shrink-0 text-warning" />
            )}
            <label htmlFor={`caught-${host}`} className="flex-1 cursor-pointer">
              <Mono className="text-foreground">{host}</Mono>
            </label>
            <span className={denied ? "text-meta text-danger" : "text-meta text-warning"}>
              {denied ? "blocked" : "pending approval"}
            </span>
            <Button size="sm" variant="outline" className="h-7" onClick={() => onApproveHosts([host])}>
              <Check className="size-3.5" /> Approve
            </Button>
          </li>
        ))}
      </ul>
      {replayName && (
        <Button size="sm" disabled={picked.length === 0} onClick={() => onApproveHosts(picked, replayName)}>
          <ShieldCheck className="size-3.5" /> Approve {picked.length} selected host
          {picked.length === 1 ? "" : "s"} and replay again
        </Button>
      )}
      <p className="text-meta leading-snug text-muted-foreground">
        These were denied or held for approval because they aren&apos;t in your approved set. Approve one
        only if this workspace legitimately needs it — otherwise leave it blocked. Anything denied live
        starts unchecked.
      </p>
    </section>
  );
}

