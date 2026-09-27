/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordingTab — run-detail.tsx's Recording tab, split out under that file's
// line cap (scripts/check-file-size.sh). Verbatim move: no behaviour change.
import { Loader2, SquareTerminal } from "lucide-react";
import { Link } from "react-router-dom";
import type { AuditEvent, Recording } from "../../../lib/types";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../ui/select";
import { EmptyState, ErrorState } from "../../wardyn/states";
import { TerminalPlayer } from "../../wardyn/terminal-player";
import { useOperator } from "../../wardyn/operator-context";
import {
  RECORDING_DISABLED_DESC,
  RECORDING_DISABLED_TITLE,
  RUN_COCKPIT,
} from "../../wardyn/copy";
import {
  sessionOptionLabel,
  RECORDING_MISSING_SESSION_TITLE,
  RECORDING_MISSING_SESSION_BODY,
} from "./recording-tab-copy";

export function RecordingTab({
  state,
  recording,
  recordingDisabled,
  runId,
  sessions,
  selected,
  onSelect,
  onRetry,
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
}) {
  // M-1b: the Recordings library is Admin view only (/admin/recordings) and,
  // until F1, not offered to a security admin either — a user reaches a
  // recording only through their own run's tab, this one.
  const operator = useOperator();
  return (
    <div className="max-w-4xl">
      {/* The picker sits ABOVE the body on purpose: a run whose OWN cast is
          missing still has to be able to reach its attach sessions. */}
      {sessions.length > 0 && (
        <div className="mb-3 flex items-center gap-2">
          <span className="text-xs text-muted-foreground">Session</span>
          <Select value={selected} onValueChange={onSelect}>
            <SelectTrigger size="sm" className="w-[280px]" aria-label="Recorded session">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={runId}>Agent session</SelectItem>
              {sessions.map((e) => (
                <SelectItem key={e.id} value={e.target!}>
                  {sessionOptionLabel(e)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}

      {state === "loading" || state === "idle" ? (
        <div className="flex h-[360px] items-center justify-center rounded-xl border border-border bg-card">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </div>
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
                : selected !== runId
                  ? RECORDING_MISSING_SESSION_BODY
                  : "This run has no captured terminal session. A recording is produced once an agent process runs in the sandbox."
            }
          />
        </div>
      ) : (
        <>
          <TerminalPlayer recording={recording} />
          <div className="mt-2 text-xs text-muted-foreground">
            Recorded when the run's runner supports session capture
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
