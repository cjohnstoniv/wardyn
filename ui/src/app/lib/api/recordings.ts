/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Session recordings (asciicast). The recording document is fetched as text and
// parsed into the Recording shape the terminal player renders.
//
// R4-F077: this used to also export probeRecording()/parseCast() — the
// Recordings library opened by asking every run whether it had a recording at
// all (downloading the WHOLE document just to read a duration and a byte
// size). has_recording/recording_bytes/recording_duration_sec are now
// projected server-side onto every run listRuns() already returns
// (internal/types.AgentRun), so getRecording is the only fetch left: it is
// called once, for the one cast a viewer actually presses play on.
import type { AsciicastEvent, Recording } from "../types";
import { errText, HttpError, num, str, wfetch } from "./core";

// Parse an asciicast (v2) recording document into the Recording shape.
// Accepts either JSON ({header, events|stdout}) or raw asciicast text
// (header line + one JSON event array per line).
function parseRecording(runId: string, text: string): Recording {
  const events: AsciicastEvent[] = [];
  let header: Recording["header"] = { version: 2, width: 96, height: 26 };

  const lines = text.split("\n").filter((l) => l.trim().length > 0);
  for (let i = 0; i < lines.length; i++) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(lines[i]);
    } catch {
      continue;
    }
    if (i === 0 && parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const h = parsed as Record<string, unknown>;
      header = {
        version: num(h.version) ?? 2,
        width: num(h.width) ?? 96,
        height: num(h.height) ?? 26,
        title: str(h.title),
      };
      continue;
    }
    if (Array.isArray(parsed) && parsed.length >= 3 && parsed[1] === "o") {
      events.push([Number(parsed[0]) || 0, "o", String(parsed[2])]);
    }
  }

  return { run_id: runId, header, events, cast: text };
}

// The document fetch getRecording sits on: undefined for "this run has no
// recording" (404 / empty body). A non-404 failure throws.
async function fetchCast(runId: string, key: string): Promise<string | undefined> {
  const res = await wfetch(
    `/runs/${encodeURIComponent(runId)}/recording/${encodeURIComponent(key)}`,
    { method: "GET", headers: { Accept: "text/plain, application/json" } },
  );
  if (res.status === 404) return undefined;
  if (!res.ok) {
    throw new HttpError(res.status, await errText(res));
  }
  const text = await res.text();
  if (!text.trim()) return undefined;
  return text;
}

export const recordings = {
  // GET /api/v1/runs/{id}/recording/{key}  (asciicast text)
  //
  // key defaults to the run id — the agent's own session, stored under the bare
  // run id. An interactive ATTACH session is stored under the COMPOSITE key
  // `<run-id>~<session-uuid>` (recording.CastKey), which the run's audit trail
  // carries as session.recording's target; those casts were unreachable while
  // this only ever asked for the bare id.
  async getRecording(runId: string, key: string = runId): Promise<Recording | undefined> {
    const text = await fetchCast(runId, key);
    return text === undefined ? undefined : parseRecording(runId, text);
  },
};
