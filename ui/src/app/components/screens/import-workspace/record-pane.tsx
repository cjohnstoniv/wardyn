/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordPane — the import panel's OPEN recording step (recommended, skippable).
// The operator records one or more NAMED SESSIONS: each spins up an open
// (allow-all-egress) interactive sandbox with the repo cloned + the configured
// model provider wired, the operator drives the real activity (build, test, run the
// agent) in the embedded AttachTerminal, then clicks "Done recording"
// (api.killRun); capture happens on run termination. Each session's observed egress
// promotes into the workspace's single ApprovedEgress, so the confined Verify/runs
// afterward work. There is NO derived build/test taxonomy — sessions are whatever
// the operator names them. This pane NEVER navigates away (the attach terminal
// renders inline), preserving the import panel's never-route-away invariant.
import * as React from "react";
import {
  Check,
  Copy,
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
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import { recordResult, recordSessions, verifyKeyOf, policyNameFor, isEmptyCapture, newEgressHosts } from "./import-types";
import { Observations } from "../profile-review";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { CC_META } from "../../wardyn/cc-meta";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";

export function RecordPane({
  ws,
  confined = false,
  notice,
  busyTask,
  modelReady,
  onRecord,
  onDoneRecording,
  onPromoteEgress,
  onApproveHost,
  onOpenProfile,
}: {
  ws: Workspace;
  // Confined mode = the Verify step: default-deny egress limited to the approved
  // set. Same session/attach machinery as the open Record step; the operator
  // re-runs the same steps and any off-policy host is blocked live. When false
  // (default) this is the open learning Record step.
  confined?: boolean;
  // Inline record notice (400 bad name; 503 no runner; 409 another import step) —
  // same shape/handling as VerifyPane.
  notice: { status: number; detail?: string } | null;
  // The session key currently being kicked (disables its button).
  busyTask: string | null;
  // fix: whether the operator has ANY working model/LLM path (subscription
  // login, a stored provider key, or a real composer backend) — a session runs
  // the agent, so without one the agent's model calls would be denied. Drives
  // the warning. This is derived from GET /setup/status (hasLlmPath), NOT
  // composer-backend detection: a composer backend is optional server config,
  // so its absence must not warn an operator who has a perfectly good
  // connected subscription or API key.
  modelReady: boolean;
  // Start (or re-start) a session by NAME; the server slugs it to the record key.
  onRecord: (name: string) => void;
  // Interactive "Done recording" — kills the run; the backend captures on termination.
  onDoneRecording: (runId: string) => void;
  // Approve the session's observed hosts (promote-egress, 404-tolerant in the panel).
  onPromoteEgress: (taskKey: string) => void;
  // Approve a single off-policy host a confined verify session hit (widens the
  // workspace's approved egress). Used only by the confined review card.
  onApproveHost: (host: string) => void;
  // Open the existing ProfileReview drawer on the record run (Save session profile).
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  // Recordings are the named things the operator made in the Record step. Both
  // steps list the SAME recordings: Record runs them open (learn), Verify replays a
  // chosen one confined (validate) — you don't name anything new on Verify.
  const recordings = recordSessions(ws, false);
  // The record sandbox runs under the strongest class the host supports; the pane's
  // best proxy is the operator's persisted default tier (same source SecurityChip
  // uses). CC1 (Fence) is the loud case: open egress on a shared-kernel box.
  const tier = getDefaultCc() ?? "CC1";
  const recording = recordings.some((s) => recordResult(ws, s.key)?.status === "recording");
  // Scan-detected commands become copy-paste hints so a clueless operator knows
  // what to run in the session — guidance without a taxonomy.
  const detected = ((ws.profile ?? {}) as WorkspaceProfile).setup_commands ?? [];

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <SectionLabel>{confined ? "Verify — locked sandbox" : "Record sessions"}</SectionLabel>
        <Chip tone="info">{confined ? "Confined · re-run your steps" : "Recommended · skippable"}</Chip>
      </div>
      <p className="text-sm leading-relaxed text-muted-foreground">
        {confined ? (
          <>
            Pick a recording below and <strong>replay it in a locked sandbox</strong> — default-deny
            egress, only your approved access. Re-run its steps to prove they work confined; any
            off-policy host (a new website, cloud metadata) is <strong>blocked live</strong> and one
            click to approve. Same recording, run in verify mode instead of record mode.
          </>
        ) : (
          <>
            Record a session for anything this workspace needs to do — build, run tests, drive the
            agent, deploy. Wardyn opens a sandbox with the repo and your model provider ready, watches
            what it reaches, and you approve those hosts. Name each session whatever you like.
          </>
        )}
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

      {confined ? (
        <div className="flex items-start gap-2 rounded-lg border border-border bg-surface-2/60 px-3 py-2 text-xs text-muted-foreground">
          <ShieldCheck className="mt-0.5 size-4 shrink-0 text-success" />
          <p>
            Egress is <strong>default-deny</strong> — limited to the hosts you approved (plus the clone
            + package registries). Anything else is blocked and shows in Audit / Approvals.
          </p>
        </div>
      ) : (
        tier === "CC1" && <Cc1Banner />
      )}

      {/* 503: honest no-runner path (can't record; Verify/Finalize still work). */}
      {notice?.status === 503 && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
          data-testid="record-no-runner"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>
            {confined ? "Verifying" : "Recording"} needs a runner (this control plane runs{" "}
            <span className="font-mono">-runner none</span>); you can continue to Finalize as configured.
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
          {notice.detail || "Another import step is already running for this workspace."}
        </p>
      )}

      {confined ? (
        // VERIFY: pick a recording and replay it confined. No new names here — the
        // list is the recordings made on the Record step.
        recordings.length === 0 ? (
          <div
            className="flex items-start gap-2 rounded-lg border border-dashed border-border px-3 py-3 text-xs text-muted-foreground"
            data-testid="verify-no-recordings"
          >
            <Info className="mt-0.5 size-4 shrink-0" />
            <p>
              No recordings to verify yet. Go back to <strong>Record</strong> and record a session
              first — then replay it here in verify mode.
            </p>
          </div>
        ) : (
          <div className="space-y-3" data-testid="verify-recordings">
            {recordings.map((rec) => (
              <VerifyRecordingCard
                key={rec.key}
                ws={ws}
                recording={rec}
                detected={detected.map((c) => c.command)}
                busy={busyTask === verifyKeyOf(rec.key)}
                onVerify={onRecord}
                onDoneRecording={onDoneRecording}
                onApproveHost={onApproveHost}
                onOpenProfile={onOpenProfile}
              />
            ))}
          </div>
        )
      ) : (
        <>
          {recordings.length > 0 && (
            <div className="space-y-3" data-testid="record-tasks">
              {recordings.map((session) => (
                <SessionCard
                  key={session.key}
                  ws={ws}
                  sessionKey={session.key}
                  label={session.label}
                  detected={detected.map((c) => c.command)}
                  busy={busyTask === session.key}
                  onRecord={onRecord}
                  onDoneRecording={onDoneRecording}
                  onPromoteEgress={onPromoteEgress}
                  onOpenProfile={onOpenProfile}
                />
              ))}
            </div>
          )}
          <NewSessionForm
            existing={recordings.map((s) => s.label)}
            disabled={recording || notice?.status === 503}
            onRecord={onRecord}
          />
        </>
      )}
    </div>
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
        click Done recording to capture what it used. You&apos;ll replay it in verify mode next.
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
          anything it can read out during the recording window. Only record tasks you trust — Verify
          re-runs them CONFINED to least privilege afterward.
        </p>
      </div>
    </div>
  );
}

function SessionCard({
  ws,
  sessionKey,
  label,
  detected,
  busy,
  onRecord,
  onDoneRecording,
  onPromoteEgress,
  onOpenProfile,
}: {
  ws: Workspace;
  sessionKey: string;
  label: string;
  detected: string[];
  busy: boolean;
  onRecord: (name: string) => void;
  onDoneRecording: (runId: string) => void;
  onPromoteEgress: (taskKey: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  const rr = recordResult(ws, sessionKey);

  return (
    <div className="rounded-lg border border-border p-3" data-testid={`record-task-${sessionKey}`}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-foreground">{label}</span>
        {rr?.status === "recording" && (
          <Chip tone="info" dot pulse className="ml-auto">
            Recording…
          </Chip>
        )}
      </div>

      {/* recording — embed the attach terminal + copy-paste command hints */}
      {rr?.status === "recording" && (
        <div className="mt-3 space-y-2">
          <DetectedHints commands={detected} />
          <AttachTerminal runId={rr.run_id} />
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(rr.run_id)}>
            <Square className="size-3.5" /> Done recording
          </Button>
        </div>
      )}

      {/* settled — review card + re-record */}
      {rr && (rr.status === "recorded" || rr.status === "record_failed") && (
        <div className="mt-3 space-y-3">
          <RecordReviewCard
            ws={ws}
            sessionKey={sessionKey}
            rr={rr}
            onPromoteEgress={onPromoteEgress}
            onOpenProfile={onOpenProfile}
          />
          <Button size="sm" variant="outline" onClick={() => onRecord(label)} disabled={busy}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            Re-record
          </Button>
        </div>
      )}
    </div>
  );
}

// VerifyRecordingCard — one recording, replayable in verify (confined) mode. Its
// confined run lives under verifyKeyOf(recording.key) so it never clobbers the
// recording's open-mode capture. States: not-yet-verified (Verify button) →
// verifying (attach + Done) → settled (ConfinedReviewCard + Re-run).
function VerifyRecordingCard({
  ws,
  recording,
  detected,
  busy,
  onVerify,
  onDoneRecording,
  onApproveHost,
  onOpenProfile,
}: {
  ws: Workspace;
  recording: { key: string; label: string };
  detected: string[];
  busy: boolean;
  // Replay THIS recording confined — onVerify(label); the panel routes it to a
  // confined record run (same name, verify mode).
  onVerify: (name: string) => void;
  onDoneRecording: (runId: string) => void;
  onApproveHost: (host: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  const crr = recordResult(ws, verifyKeyOf(recording.key));
  const verifying = crr?.status === "recording";
  const settled = !!crr && (crr.status === "recorded" || crr.status === "record_failed");

  return (
    <div className="rounded-lg border border-border p-3" data-testid={`verify-recording-${recording.key}`}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-foreground">{recording.label}</span>
        {verifying && (
          <Chip tone="info" dot pulse className="ml-auto">
            Verifying…
          </Chip>
        )}
      </div>

      {/* not yet verified — offer to replay it confined */}
      {!crr && (
        <div className="mt-3 space-y-2">
          <DetectedHints commands={detected} />
          <Button size="sm" onClick={() => onVerify(recording.label)} disabled={busy}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
            Verify this recording
          </Button>
          <p className="text-[0.6875rem] text-muted-foreground">
            Replays it in a locked sandbox (only your approved access). Re-run its steps — off-policy
            hosts are blocked live.
          </p>
        </div>
      )}

      {/* verifying — attach + LIVE approvals + Done */}
      {verifying && crr && (
        <div className="mt-3 space-y-2">
          <AuthModeLine rr={crr} />
          <DetectedHints commands={detected} />
          <AttachTerminal runId={crr.run_id} />
          {/* Off-policy egress escalates to a pending approval held live — decide it
              here without leaving the panel (this modal covers the Approvals tab). */}
          <LiveApprovals
            runId={crr.run_id}
            onApproveHost={onApproveHost}
            reasonApprove="approved in verify"
            reasonDeny="rejected in verify"
            idleHint="Watching for off-policy egress — anything you run that isn't approved pauses here for you to approve or reject, live."
          />
          <Button size="sm" variant="outline" onClick={() => onDoneRecording(crr.run_id)}>
            <Square className="size-3.5" /> Done
          </Button>
        </div>
      )}

      {/* settled — containment review + re-run */}
      {settled && crr && (
        <div className="mt-3 space-y-3">
          <ConfinedReviewCard ws={ws} rr={crr} onApproveHost={onApproveHost} onOpenProfile={onOpenProfile} />
          <Button size="sm" variant="outline" onClick={() => onVerify(recording.label)} disabled={busy}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            Re-run verify
          </Button>
        </div>
      )}
    </div>
  );
}

// DetectedHints — scan-detected commands as copy pills, guidance for what to run in
// an attached session (open record or confined verify). No-op when none detected.
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
// Lets the operator SEE that verify uses their configured provider, not a fallback.
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

// Per-task review card, shown once a recording settles. Renders the SAME
// Observations block profile-review uses, a one-click egress-promotion diff,
// secrets proven-used chips, a Save-task-profile hand-off to the ProfileReview
// drawer, and the three non-negotiable honesty notes.
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

  // Egress promotion diff: hosts observed (allow_count>0) that aren't already
  // allowed → approvable; the rest are shown greyed for context.
  const newHosts = newEgressHosts(ws, sessionKey);
  const observed = (rr.observations?.domains ?? []).filter((d) => d.allow_count > 0).map((d) => d.host);
  const alreadyApproved = observed.filter((h) => !newHosts.includes(h));

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
            No new hosts to approve — everything this task reached is already allowed.
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
              "Secret masking is seed-ahead: any secret NOT declared in Configure that this open run touched is not masked in the logs or observations above. Treat anything here as sensitive.",
            ]
        ).map((c, i) => (
          <HonestyNote key={i} text={c} />
        ))}
        {rr.kernel_sensor_blind && (
          <HonestyNote text="This task recorded inside a hardware VM (Vault/CC3); the syscall sensor can't see into it, so exec, file-write, and connect observations may be incomplete. Egress (proxy-side) is still complete." />
        )}
      </div>

      {/* --- optional: persist this task's synthesized least-privilege profile --- */}
      <div className="flex justify-end">
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onOpenProfile(rr.run_id, policyNameFor(ws.name, rr.label ?? "recorded"))}
        >
          <Save className="size-3.5" /> Save task profile
        </Button>
      </div>
    </div>
  );
}

// Review card for a settled CONFINED verify session. Unlike the open-record card
// (which promotes newly-observed hosts), this proves least privilege: it splits
// what the run reached into ALLOWED (worked within the approved set), BLOCKED
// (off-policy, denied live — the containment proof), and PENDING (first-use, awaiting
// approval). Blocked/pending hosts are one click to approve if they're legitimately
// needed. All counts come straight from the capture — no extra fetch.
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
        <p>{rr.failure_hint || "The verify session captured no egress decisions — fix reachability and re-run."}</p>
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
  const { copied, copy } = useCopyToClipboard();
  return (
    <button
      type="button"
      onClick={() => copy(text)}
      className="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface-2/60 px-2 py-0.5 hover:text-foreground"
      aria-label="Copy command"
    >
      <Mono className="text-foreground">{text}</Mono>
      {copied ? <Check className="size-3 text-success" /> : <Copy className="size-3" />}
    </button>
  );
}
