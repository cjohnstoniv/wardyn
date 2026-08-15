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
import type { RecordResult, Workspace, WorkspaceProfile } from "../../../lib/types";
import { CopyButton } from "../../wardyn/copy-button";
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
  isEmptyCapture,
  egressPromotionDiff,
  policyNameFor,
  sessionStage,
  verifyKeyOf,
  type SessionStage,
} from "./session-helpers";
import { Observations } from "../profile-review";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { CC_META } from "../../wardyn/cc-meta";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { C } from "../../../lib/workspace-copy";
import { useOperator } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";

export function RecordPane({
  ws,
  notice,
  launch,
  busyTask,
  modelReady,
  onRecord,
  onReplayConfined,
  onDoneRecording,
  onPromoteEgress,
  onApproveHost,
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
  // Approve a single off-policy host a confined replay hit (widens the
  // workspace's approved egress).
  onApproveHost: (host: string) => void;
  // Open the existing ProfileReview drawer on a record run (Save profile).
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  const operator = useOperator();
  const sessions = recordSessions(ws);
  const orphans = orphanedVerifySessions(ws);
  // The record sandbox runs under the strongest class the host supports. Once
  // a session has actually launched, `launch.confinementClass` is the SERVER's
  // own verdict for that run — use it; before any launch (or if the field is
  // absent on an older server), fall back to the operator's persisted default
  // tier, the same proxy SecurityChip uses. CC1 (Fence) is the loud case: open
  // egress on a shared-kernel box, so its banner always applies — every
  // session still starts as an open recording, confined replay is a later
  // step in the SAME lifecycle.
  const tier = launch?.confinementClass ?? getDefaultCc() ?? "CC1";
  // Scan-detected commands become copy-paste hints so a clueless operator
  // knows what to run in the session — guidance without a taxonomy.
  const detected = ((ws.profile ?? {}) as WorkspaceProfile).setup_commands ?? [];

  return (
    // Every control in this pane (record/replay/approve-host/promote-egress)
    // is operatorOnly server-side; a viewer would see them all enabled and
    // 403 on the first click. A native disabled fieldset gates the whole
    // subtree at once — same disabled:opacity-50 every Button here already
    // carries — instead of threading `disabled={!operator}` through
    // SessionCard/RecordReviewCard/ConfinedReviewCard/NewSessionForm one by
    // one. The border/padding/min-width a bare <fieldset> adds are reset so
    // it stays visually identical to the plain <div> it replaces.
    <fieldset disabled={!operator} className="m-0 min-w-0 border-0 p-0 space-y-4">
      {!operator && <p className="text-xs text-muted-foreground">{OPERATOR_ONLY_REASON}</p>}
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

      {tier === "CC1" && <Cc1Banner />}

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
              onApproveHost={onApproveHost}
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
              onApproveHost={onApproveHost}
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
      <p className="text-[0.6875rem] text-muted-foreground">
        Opens an attached terminal with the repo + your model provider ready. Do the real thing, then
        click Done recording to capture what it used. You can replay it confined once it settles.
      </p>
    </div>
  );
}

// CC1 (Fence) danger banner — honest cc-meta wording. Open egress on a shared
// kernel is the widest window this flow ever opens; say so plainly.
function Cc1Banner() {
  const cc1 = CC_META.CC1;
  return (
    <div
      className="flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-subtle px-3 py-2.5 text-xs text-danger"
      data-testid="record-cc1-banner"
    >
      <ShieldAlert className="mt-0.5 size-4 shrink-0" />
      <div className="space-y-1">
        <p className="font-medium">Open recording on {cc1.label} — the weakest barrier, with egress unrestricted.</p>
        <p className="leading-snug">
          To learn what a task really uses, this sandbox allows ALL egress. On this host it runs under{" "}
          {cc1.label}: {cc1.metaphor} Combined with allow-all egress, a task that misbehaves could send
          anything it can read out during the recording window. Only record tasks you trust — replaying
          confined afterward re-runs them at least privilege.
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
  onApproveHost,
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
  onApproveHost: (host: string) => void;
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
        {stageChip(stage)}
      </div>

      {/* recording — embed the attach terminal + copy-paste command hints */}
      {stage === "recording" && openRR && (
        <div className="mt-3 space-y-2">
          <DetectedHints commands={detected} />
          <AttachTerminal runId={openRR.run_id} />
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(openRR.run_id)}>
            <Square className="size-3.5" /> Done recording
          </Button>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SESSION_SURVIVES}</p>
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
          />
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(confinedRR.run_id)}>
            <Square className="size-3.5" /> Done
          </Button>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SESSION_SURVIVES}</p>
        </div>
      )}

      {/* replayed / replay_failed — containment review + re-run */}
      {(stage === "replayed" || stage === "replay_failed") && confinedRR && (
        <div className="mt-3 space-y-3">
          <ConfinedReviewCard ws={ws} rr={confinedRR} onApproveHost={onApproveHost} onOpenProfile={onOpenProfile} />
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
  onApproveHost,
  onOpenProfile,
}: {
  ws: Workspace;
  sessionKey: string;
  label: string;
  busy: boolean;
  onDoneRecording: (runId: string) => void;
  onApproveHost: (host: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  const rr = recordResult(ws, sessionKey);
  if (!rr) return null;
  const live = rr.status === "recording";

  return (
    <div className="rounded-lg border border-border p-3" data-testid={`session-${sessionKey}`}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-foreground">{label}</span>
        {stageChip(live ? "replaying" : rr.status === "record_failed" ? "replay_failed" : "replayed")}
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
          />
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(rr.run_id)} disabled={busy}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : <Square className="size-3.5" />}
            Done
          </Button>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SESSION_SURVIVES}</p>
        </div>
      ) : (
        <div className="mt-3">
          <ConfinedReviewCard ws={ws} rr={rr} onApproveHost={onApproveHost} onOpenProfile={onOpenProfile} />
        </div>
      )}
    </div>
  );
}

// Same table+one-Chip idiom as primitives.tsx's runStateMeta: one row per
// SessionStage instead of a 42-line if-chain of near-identical Chips.
const STAGE_CHIP_META: Record<
  SessionStage,
  { tone: "info" | "danger" | "success" | "neutral"; label: string; pulse?: boolean }
> = {
  recording: { tone: "info", label: "Recording…", pulse: true },
  replaying: { tone: "info", label: "Replaying confined…", pulse: true },
  record_failed: { tone: "danger", label: "Record failed" },
  replay_failed: { tone: "danger", label: "Replay failed" },
  replayed: { tone: "success", label: "Replayed confined" },
  recorded: { tone: "neutral", label: "Recorded" },
};

function stageChip(stage: SessionStage) {
  const m = STAGE_CHIP_META[stage];
  return (
    <Chip tone={m.tone} dot={!!m.pulse} pulse={m.pulse} className="ml-auto">
      {m.label}
    </Chip>
  );
}

// DetectedHints — scan-detected commands as copy pills, guidance for what to run in
// an attached session (open record or confined replay). No-op when none detected.
function DetectedHints({ commands }: { commands: string[] }) {
  if (commands.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-2 text-[0.6875rem] text-muted-foreground">
      <span>Detected commands:</span>
      {commands.slice(0, 4).map((c) => (
        <CopyPill key={c} text={c} />
      ))}
    </div>
  );
}

// AuthModeLine — the auth the session actually ran with (saved on the record result).
// Lets the operator SEE that a replay uses their configured provider, not a fallback.
function AuthModeLine({ rr }: { rr: RecordResult }) {
  if (!rr.llm_mode || rr.llm_mode === "none") return null;
  const label =
    rr.llm_mode === "subscription" ? "Claude subscription" : rr.llm_mode === "api-key" ? "API key" : rr.llm_mode;
  return (
    <p className="flex flex-wrap items-center gap-1.5 text-[0.6875rem] text-muted-foreground" data-testid="session-auth-mode">
      <ShieldCheck className="size-3 shrink-0 text-success" />
      Model access: <span className="font-medium text-foreground">{label}</span>
      {rr.model ? (
        <>
          {" · "}
          <Mono className="text-foreground">{rr.model}</Mono>
        </>
      ) : null}
    </p>
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
        {rr.egress_promoted ? (
          <Chip tone="success" dot>
            <Check className="size-3.5" /> Promoted
          </Chip>
        ) : newHosts.length === 0 ? (
          <p className="text-[0.6875rem] text-muted-foreground">
            {onlyPlumbingObserved
              ? "Nothing needed approval — observed hosts were platform plumbing."
              : "No new hosts to approve — everything this task reached is already allowed."}
          </p>
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
              <li key={h} className="flex items-center gap-1.5 text-[0.6875rem] text-muted-foreground">
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
  onApproveHost,
  onOpenProfile,
}: {
  ws: Workspace;
  rr: RecordResult;
  onApproveHost: (host: string) => void;
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
  const approved = new Set(ws.approved_egress ?? []);
  const allowed = domains.filter((d) => d.allow_count > 0).map((d) => d.host);
  const blocked = domains.filter((d) => d.deny_count > 0 && !approved.has(d.host)).map((d) => d.host);
  const pending = domains.filter((d) => d.pending_count > 0 && !approved.has(d.host)).map((d) => d.host);

  return (
    <div className="space-y-4 rounded-lg border border-border p-3" data-testid="verify-session-review">
      <AuthModeLine rr={rr} />
      {/* worked within the approved set */}
      <section className="space-y-2">
        <SectionLabel>Ran within your approved access</SectionLabel>
        {allowed.length === 0 ? (
          <p className="text-[0.6875rem] text-muted-foreground">
            No egress captured yet — re-run your build/test/agent steps in the session above.
          </p>
        ) : (
          <p className="flex items-center gap-1.5 text-sm text-success">
            <ShieldCheck className="size-4" /> {allowed.length} host{allowed.length === 1 ? "" : "s"} reached,
            all allowed.
          </p>
        )}
      </section>

      {/* off-policy attempts caught — the containment proof */}
      {(blocked.length > 0 || pending.length > 0) && (
        <section className="space-y-2" data-testid="verify-session-blocked">
          <SectionLabel>Off-policy attempts caught</SectionLabel>
          <ul className="space-y-1.5">
            {blocked.map((h) => (
              <li key={h} className="flex items-center gap-2">
                <ShieldAlert className="size-3.5 shrink-0 text-danger" />
                <Mono className="flex-1 text-foreground">{h}</Mono>
                <span className="text-[0.6875rem] text-danger">blocked</span>
                <Button size="sm" variant="outline" className="h-7" onClick={() => onApproveHost(h)}>
                  <Check className="size-3.5" /> Approve
                </Button>
              </li>
            ))}
            {pending.map((h) => (
              <li key={h} className="flex items-center gap-2">
                <Info className="size-3.5 shrink-0 text-warning" />
                <Mono className="flex-1 text-foreground">{h}</Mono>
                <span className="text-[0.6875rem] text-warning">pending approval</span>
                <Button size="sm" variant="outline" className="h-7" onClick={() => onApproveHost(h)}>
                  <Check className="size-3.5" /> Approve
                </Button>
              </li>
            ))}
          </ul>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            These were denied or held for approval because they aren&apos;t in your approved set. Approve
            one only if this workspace legitimately needs it — otherwise leave it blocked.
          </p>
        </section>
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

// A muted advisory note (the masking + sensor-blind honesty lines).
function HonestyNote({ text }: { text: string }) {
  return (
    <p className="flex items-start gap-1.5 text-[0.6875rem] leading-snug text-muted-foreground">
      <Info className="mt-0.5 size-3 shrink-0" />
      <span>{text}</span>
    </p>
  );
}

// A small copy-to-clipboard command pill for the interactive suggested command.
// Exported so the demo-sandbox screen can reuse the exact same pill for its
// numbered curl instructions instead of duplicating it.
export function CopyPill({ text }: { text: string }) {
  return (
    <CopyButton
      text={text}
      label="Copy command"
      iconClassName="size-3"
      className="gap-1.5 rounded-md border border-border bg-surface-2/60 px-2 py-0.5 hover:text-foreground"
    >
      <Mono className="text-foreground">{text}</Mono>
    </CopyButton>
  );
}
