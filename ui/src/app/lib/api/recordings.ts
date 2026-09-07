/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Session recordings (asciicast). The recording document is fetched as text and
// parsed into the Recording shape the terminal player renders.
//
// TWO entry points on purpose. getRecording() parses — one AsciicastEvent
// object per output frame — and is what a PLAYER needs. probeRecording() does
// not: the Recordings library opens by asking every run whether it has a
// recording at all, and it only renders a duration and a byte size per card, so
// building (and then holding) an event array per run turned that screen into
// tens of MB of live objects for output nothing ever replayed. The probe reads
// the same document, takes the last frame's timestamp and the payload's byte
// size, and keeps NOTHING else; the caller parses the one cast a viewer
// actually presses play on (parseCast below).
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

/**
 * What a recording card renders, without the parsed event stream behind it.
 * `cast` is the raw asciicast document, kept so pressing play needs no second
 * round trip; `durationSec`/`bytes` are measured, never fabricated (the same
 * two numbers the card showed before, derived the same way).
 */
export interface RecordingProbe {
  run_id: string;
  /** Real elapsed seconds, from the last captured output frame. */
  durationSec?: number;
  /** Real byte size of the fetched cast payload. */
  bytes: number;
  cast: string;
}

// The last output frame's timestamp, read WITHOUT materialising an event per
// frame — the whole point of the probe. Scans backwards and stops at the first
// `[t, "o", …]` line, which is the same frame parseRecording's events array
// would have ended on.
function lastOutputAt(text: string): number | undefined {
  const lines = text.split("\n");
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i].trim();
    if (!line.startsWith("[")) continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      continue;
    }
    if (Array.isArray(parsed) && parsed.length >= 3 && parsed[1] === "o") {
      return Number(parsed[0]) || 0;
    }
  }
  return undefined;
}

// The one fetch both entry points share: the document as text, or undefined for
// "this run has no recording" (404 / empty body). A non-404 failure throws —
// the library counts it as a run it could not CHECK, which is not the same
// thing as a run with no recording.
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

  // GET the same document, for a caller that only needs to know a recording
  // EXISTS and how big/long it is. Same 404-is-undefined contract as
  // getRecording; no event array is built and nothing but the document text is
  // retained, so a library screen probing hundreds of runs holds hundreds of
  // strings instead of hundreds of thousands of frame objects.
  async probeRecording(runId: string, key: string = runId): Promise<RecordingProbe | undefined> {
    const text = await fetchCast(runId, key);
    if (text === undefined) return undefined;
    return {
      run_id: runId,
      durationSec: lastOutputAt(text),
      bytes: new Blob([text]).size,
      cast: text,
    };
  },

  // Parse a cast a probe already fetched — the player's input, built on demand
  // when a viewer presses play rather than for every card on screen.
  parseCast(runId: string, text: string): Recording {
    return parseRecording(runId, text);
  },
};
