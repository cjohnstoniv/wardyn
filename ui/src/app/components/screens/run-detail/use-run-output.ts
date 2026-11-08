/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The read of GET /runs/{id}/output behind the Output tab and the Overview pane: one poll, one set of answers.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { runOutput } from "../../../lib/api/run-output";
import type { RunOutput } from "../../../lib/types";

const POLL_MS = 4000;
// After a good read, a failed poll is retried this many times, backing off up to MAX_BACKOFF_MS, before it is shown.
const MAX_BLIPS = 4;
const MAX_BACKOFF_MS = 30_000;
// A not-kept answer this soon after the run ended is the final row still
// being written (design §3, read order 5), not a verdict. So is an interactive
// answer this soon after a Wardyn stop: the pane snapshot row is written a
// moment after the run reaches STOPPED.
const SAVING_WINDOW_MS = 60_000;

export type Refusal = "off" | "not_kept" | "expired" | "erased" | "interactive" | "not_captured" | "mask" | "data_out" | "error";

const REFUSALS: Record<string, Refusal> = {
  run_output_off: "off",
  run_output_not_kept: "not_kept",
  run_output_expired: "expired",
  run_output_erased: "erased",
  // Told only to the run's owner or an operator; anyone else gets not_captured or not_kept.
  recording_erased: "erased",
  run_output_interactive: "interactive",
  run_output_not_captured: "not_captured",
  mask_state_unavailable: "mask",
  // The run's data_out rule keeps its output in. An answer, so it is not read again.
  data_out_blocked: "data_out",
};

/** enabled false reads nothing: the Overview pane asks only for the runs it may show output for. */
export function useRunOutput(runId: string, live: boolean, state: string, endedAt: string | undefined, enabled = true) {
  const [out, setOut] = React.useState<RunOutput | null>(null);
  const [refusal, setRefusal] = React.useState<Refusal | null>(null);
  // The server's own sentence for a mapped refusal; "" for any other.
  const [refusalText, setRefusalText] = React.useState("");
  const [loaded, setLoaded] = React.useState(false);
  // The `live` the answer was read under: a read from while the run lived is not the ended run's answer.
  const [readLive, setReadLive] = React.useState(live);
  const [attempt, setAttempt] = React.useState(0);
  const [saving, setSaving] = React.useState(false);
  const [retrying, setRetrying] = React.useState(false);
  const stopped = state === "STOPPED";

  // Whether this run's output has been read at least once: after that, one failed poll is a blip, not a verdict.
  const hadRead = React.useRef(false);
  React.useEffect(() => {
    hadRead.current = false;
  }, [runId]);

  React.useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let failures = 0;
    const tick = async () => {
      try {
        const o = await runOutput.get(runId);
        if (cancelled) return;
        hadRead.current = true;
        failures = 0;
        setOut(o);
        setReadLive(live);
        setRefusal(null);
        setRefusalText("");
        setSaving(false);
        setRetrying(false);
        setLoaded(true);
        if (!o.complete) timer = setTimeout(tick, POLL_MS);
      } catch (e) {
        if (cancelled) return;
        const r = e instanceof HttpError ? (REFUSALS[e.reason] ?? "error") : "error";
        // A 502 or a network blip while polling a healthy run: keep the output on screen and look again, backing
        // off. A mapped reason (not kept, interactive, ...) is an answer and still replaces it.
        if (r === "error" && hadRead.current && failures < MAX_BLIPS) {
          failures += 1;
          timer = setTimeout(tick, Math.min(POLL_MS * 2 ** failures, MAX_BACKOFF_MS));
          return;
        }
        setOut(null);
        setReadLive(live);
        setRefusal(r);
        setRefusalText(e instanceof HttpError && e.reason in REFUSALS ? e.message : "");
        setRetrying(false);
        setLoaded(true);
        // Only an answer around the end of a run is worth another look: not-kept
        // while the final row is written, interactive while the snapshot is.
        const justEnded = !!endedAt && Date.now() - Date.parse(endedAt) < SAVING_WINDOW_MS;
        const again = (r === "not_kept" && (live || justEnded)) || (r === "interactive" && stopped && justEnded);
        setSaving(again);
        if (again) timer = setTimeout(tick, POLL_MS);
      }
    };
    void tick();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
    // endedAt/live/stopped only steer whether to poll again; a change re-reads once.
  }, [runId, live, stopped, endedAt, attempt, enabled]);

  /** A press during a read in flight sends nothing. */
  const retry = () => {
    if (retrying) return;
    setRetrying(true);
    setAttempt((n) => n + 1);
  };
  return { out, refusal, refusalText, loaded, readLive, saving, retrying, retry };
}
