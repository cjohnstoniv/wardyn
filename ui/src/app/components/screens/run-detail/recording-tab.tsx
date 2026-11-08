/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordingTab — run-detail.tsx's Recording tab, split out under that file's
// line cap (scripts/check-file-size.sh). Verbatim move: no behaviour change.
import { Loader2, RotateCw, SquareTerminal } from "lucide-react";
import { Link } from "react-router-dom";
import type { AuditEvent, Recording, RecordingLevel, RunNotRecorded } from "../../../lib/types";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../ui/select";
import { Button } from "../../ui/button";
import { Notice } from "../../wardyn/notice";
import { EmptyState, ErrorState } from "../../wardyn/states";
import { DATA_OUT_RUN } from "../../wardyn/copy/data-out-run";
import { TerminalPlayer } from "../../wardyn/terminal-player";
import { useOperator } from "../../wardyn/operator-context";
import {
  RECORDING_DISABLED_DESC,
  RECORDING_DISABLED_TITLE,
  RUN_COCKPIT,
} from "../../wardyn/copy";
import {
  sessionNotes,
  sessionOptionLabel,
  RECORDING_MISSING_SESSION_TITLE,
  RECORDING_MISSING_SESSION_BODY,
} from "./recording-tab-copy";
import { RECORDING_FOOTER_MACOS, RECORDING_REPLAY, RECORDING_STRIP, recordingLevelLabel } from "../../wardyn/copy/recording-level";
import { RUN_RECORDING } from "../../wardyn/copy/runners";
import { clockTime } from "../../../lib/format";
import { DesktopReplay, desktopSessions } from "./desktop-replay";

// The level the run is recorded at, in the run strip's words (M7-RunStrip states 1, 3, 4 and 5). A run with a
// screen says at Screen and actions what that keeps. A macOS VM task run marked runner_declined (its agent's
// output was not recorded) keeps the level's own title and note, then says so; its terminal sessions and its
// desktop follow the level as any run's do.
function LevelStrip({ level, notRecorded, storeOff, macOS, screen }: { level?: RecordingLevel; notRecorded?: RunNotRecorded; storeOff: boolean; macOS: boolean; screen: boolean }) {
  const [levelTitle, levelNote] =
    level === "screen_actions"
      ? [RECORDING_STRIP.ON(recordingLevelLabel(level)), screen ? RECORDING_STRIP.SCREEN_NOTE : ""]
      : [RECORDING_STRIP.TERMINAL_ONLY, RECORDING_STRIP.TERMINAL_ONLY_NOTE];
  const [title, note] = storeOff
    ? [RECORDING_STRIP.UNAVAILABLE, RECORDING_DISABLED_TITLE]
    : macOS && notRecorded === "runner_declined" && level !== "off"
      ? [levelTitle, [levelNote, RECORDING_STRIP.AGENT_NOT_RECORDED].filter(Boolean).join(" ")]
    : notRecorded
      ? [RECORDING_STRIP.OFF, notRecorded === "policy_off" ? RECORDING_STRIP.OFF_NOTE : RUN_RECORDING[notRecorded]]
      : [levelTitle, levelNote];
  return (
    <div className="mb-3 flex flex-wrap items-baseline gap-2 text-body" data-testid="recording-level-strip">
      <span className="font-medium">{title}</span>
      {note && <span className="text-meta text-muted-foreground">{note}</span>}
    </div>
  );
}

export function RecordingTab({
  state,
  recording,
  recordingDisabled,
  runId,
  sessions,
  selected,
  onSelect,
  onRetry,
  level,
  notRecorded,
  macOS = false,
  screen = false,
  interactive = false,
  ended = false,
  recordingAudit = [],
  onGoAudit,
  refusal,
}: {
  state: "idle" | "loading" | "error" | "ready";
  recording: Recording | null;
  /** This deployment's recording store never came up — a missing
   *  cast means "it can't", not "it hasn't yet". */
  recordingDisabled: boolean;
  runId: string;
  // The run's interactive attach sessions (session.recording.write audit events).
  sessions: AuditEvent[];
  selected: string;
  onSelect: (key: string) => void;
  onRetry: () => void;
  /** The run's data_out rule refused the replay: its reason and the server's sentence, printed as sent. */
  refusal?: { reason: string; message: string };
  /** The run's level and why it is not recorded (AgentRun.recording_level, not_recorded). */
  level?: RecordingLevel;
  notRecorded?: RunNotRecorded;
  /** The run is a macOS VM (machine_kind): not_recorded runner_declined then leaves its terminal sessions and desktop recorded, and it has no cast under its own key. */
  macOS?: boolean;
  /** The run has a screen (a VM), so the strip can say what the screen recording keeps. */
  screen?: boolean;
  /** The run is interactive: it has no agent, so no agent output recording comes of it. */
  interactive?: boolean;
  /** The run has ended: a missing recording will not appear later. */
  ended?: boolean;
  /** The run's session.recording and desktop.recording rows, for the desktop recordings and gaps. */
  recordingAudit?: AuditEvent[];
  /** Opens the run's Audit tab (the desktop replay's "Open in Audit"). */
  onGoAudit?: () => void;
}) {
  // M-1b: the Recordings library is Admin view only (/admin/recordings) and,
  // until F1, not offered to a security admin either — a user reaches a
  // recording only through their own run's tab, this one.
  const operator = useOperator();
  const desktop = desktopSessions(recordingAudit);
  const desktopSelected = desktop.some((e) => e.target === selected);
  const notes = sessionNotes(sessions.find((e) => e.target === selected));
  return (
    <div className="max-w-4xl">
      <LevelStrip level={level} notRecorded={notRecorded} storeOff={recordingDisabled} macOS={macOS} screen={screen} />
      {/* The picker sits ABOVE the body on purpose: a run whose OWN cast is
          missing still has to be able to reach its attach sessions. */}
      {sessions.length + desktop.length > 0 && (
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <span className="text-xs text-muted-foreground">Session</span>
          <Select value={selected} onValueChange={onSelect}>
            <SelectTrigger size="sm" className="w-full max-w-[280px]" aria-label="Recorded session">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {/* A macOS VM run's sandbox is not asked to record, so nothing is stored under the run's own key. */}
              {!macOS && <SelectItem value={runId}>Agent session</SelectItem>}
              {sessions.map((e) => (
                <SelectItem key={e.id} value={e.target!}>
                  {sessionOptionLabel(e)}
                </SelectItem>
              ))}
              {desktop.map((e) => (
                <SelectItem key={e.id} value={e.target!}>
                  {RECORDING_REPLAY.SESSION_OPTION(clockTime(e.time))}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
      {notes.length > 0 && (
        <div className="-mt-1 mb-3 space-y-0.5" data-testid="recording-session-notes">
          {notes.map((n) => (
            <p key={n} className="text-meta text-muted-foreground">
              {n}
            </p>
          ))}
        </div>
      )}

      {desktopSelected ? (
        <DesktopReplay runId={runId} recKey={selected} audit={recordingAudit} onGoAudit={onGoAudit} />
      ) : state === "loading" || state === "idle" ? (
        <div className="flex h-[360px] items-center justify-center rounded-xl border border-border bg-card">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </div>
      ) : state === "error" && refusal?.reason === "data_out_blocked" ? (
        <div data-testid="recording-data-out" className="rounded-xl border border-border bg-card">
          <EmptyState icon={SquareTerminal} title={DATA_OUT_RUN.RECORDING_TITLE} description={refusal.message} />
        </div>
      ) : state === "error" && refusal?.reason === "data_out_unreadable" ? (
        <Notice
          tone="danger"
          actions={
            <Button variant="outline" size="sm" onClick={onRetry}>
              <RotateCw className="size-3.5" /> {DATA_OUT_RUN.RETRY}
            </Button>
          }
        >
          {refusal.message}
        </Notice>
      ) : state === "error" ? (
        <div className="rounded-xl border border-border bg-card">
          <ErrorState message={RUN_COCKPIT.recordingError} onRetry={onRetry} />
        </div>
      ) : !recording ? (
        <div className="rounded-xl border border-border bg-card">
          <EmptyState
            icon={SquareTerminal}
            title={
              recordingDisabled
                ? RECORDING_DISABLED_TITLE
                : notRecorded === "policy_off" && selected === runId
                  ? RECORDING_REPLAY.EMPTY_TITLE
                // F1-F11: a SPECIFIC attach session's missing cast is not a
                // fact about the whole run — the picker above is already
                // looking at one session, so the empty state must say so too.
                : selected !== runId
                  ? RECORDING_MISSING_SESSION_TITLE
                  : "No recording available"
            }
            description={
              recordingDisabled
                ? RECORDING_DISABLED_DESC
                : notRecorded === "policy_off" && selected === runId
                  ? RECORDING_REPLAY.OFF_BY_POLICY
                : selected !== runId
                  ? RECORDING_MISSING_SESSION_BODY
                  : macOS
                    ? ended
                      ? RUN_COCKPIT.recordingNoneForRun
                      : interactive
                        ? RUN_COCKPIT.recordingMissingMacOSInteractive
                        : RUN_COCKPIT.recordingMissingMacOS
                    : RUN_COCKPIT.recordingMissing
            }
          />
        </div>
      ) : (
        <>
          <TerminalPlayer recording={recording} />
          <div className="mt-2 text-xs text-muted-foreground">
            {macOS ? RECORDING_FOOTER_MACOS : "Recorded when the run's runner supports session capture"}
            {operator && (
              <>
                {" "}·{" "}
                <Link to="/admin/recordings" className="text-primary hover:underline">
                  Recordings library
                </Link>
              </>
            )}
          </div>
        </>
      )}
    </div>
  );
}
