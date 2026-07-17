/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Session recordings (asciicast). The recording document is fetched as text and
// parsed into the Recording shape the terminal player renders.
import type { AsciicastEvent, Recording } from "../types";
import { HttpError, num, str, wfetch } from "./core";

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

export const recordings = {
  // GET /api/v1/runs/{id}/recording/{id}  (asciicast text)
  async getRecording(runId: string): Promise<Recording | undefined> {
    const res = await wfetch(
      `/runs/${encodeURIComponent(runId)}/recording/${encodeURIComponent(runId)}`,
      { method: "GET", headers: { Accept: "text/plain, application/json" } },
    );
    if (res.status === 404) return undefined;
    if (!res.ok) {
      throw new HttpError(res.status, `HTTP ${res.status}: failed to load recording`);
    }
    const text = await res.text();
    if (!text.trim()) return undefined;
    return parseRecording(runId, text);
  },
};
