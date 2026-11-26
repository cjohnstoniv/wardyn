/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Follows a run's event stream (GET /runs/{id}/events, server-sent events) through wfetch, so it carries a stored
// admin token as well as the session cookie; EventSource sends only the cookie. Each event names a change in the
// run, and the caller reads the run again. A dropped stream reopens from the last event id. A refusal, the run's
// end, or two streams in a row that end at once with no event stop it, and the caller's own poll carries on.

import { HttpError, wfetch } from "./core";

/** Longer than the server's 5-minute hold, so the server, not wfetch's deadline, ends a quiet stream. */
export const RUN_EVENTS_STREAM_MS = 6 * 60_000;
/** A stream held open this long counts as healthy even when no event came. */
const HELD_OPEN_MS = 60_000;
/** Streams in a row that ended at once with no event before following stops. */
const STREAM_FAILURES = 2;

/** Calls onEvent with each event's type until the run ends, the stream is refused, or the returned close is called. */
export function followRunEvents(runId: string, onEvent: (type: string) => void, retryMs = 1000): () => void {
  const ctrl = new AbortController();
  void (async () => {
    let lastId = "";
    let failures = 0;
    while (!ctrl.signal.aborted && failures < STREAM_FAILURES) {
      const opened = Date.now();
      let got = false;
      try {
        const headers: Record<string, string> = { Accept: "text/event-stream" };
        if (lastId) headers["Last-Event-ID"] = lastId;
        const res = await wfetch(
          `/runs/${encodeURIComponent(runId)}/events`,
          { method: "GET", headers, signal: ctrl.signal },
          RUN_EVENTS_STREAM_MS,
        );
        if (!res.ok || !res.body || !(res.headers.get("Content-Type") ?? "").startsWith("text/event-stream")) {
          await res.body?.cancel();
          return;
        }
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += decoder.decode(value, { stream: true });
          for (let cut = buf.indexOf("\n\n"); cut >= 0; cut = buf.indexOf("\n\n")) {
            const block = buf.slice(0, cut);
            buf = buf.slice(cut + 2);
            let type = "";
            for (const line of block.split("\n")) {
              if (line.startsWith("event: ")) type = line.slice(7);
              else if (line.startsWith("id: ")) lastId = line.slice(4);
            }
            // A block with no event line is the keepalive comment.
            if (!type) continue;
            got = true;
            onEvent(type);
            if (type === "ended") return void ctrl.abort();
          }
        }
      } catch (e) {
        if (ctrl.signal.aborted || (e instanceof HttpError && e.status === 401)) return;
      }
      failures = got || Date.now() - opened >= HELD_OPEN_MS ? 0 : failures + 1;
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, retryMs);
        ctrl.signal.addEventListener("abort", () => {
          clearTimeout(t);
          resolve();
        });
      });
    }
  })();
  return () => ctrl.abort();
}
