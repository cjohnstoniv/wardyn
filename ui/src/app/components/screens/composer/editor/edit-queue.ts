/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The person's edits to a session's draft, posted one at a time. Each post carries the whole draft, so an older one
// stored after a newer one would bring back what was discarded: while one is in flight the latest draft waits, with
// every field that changed since the last one sent. The queue belongs to the session, never to the editor on screen,
// so closing and opening the draft sheet cannot start a second line, and a caller (the chat) can wait for it to drain.
import { assistant } from "../../../../lib/api/assistant";
import { getErrorMessage } from "../../../../lib/format";

interface Pending {
  draft: Record<string, unknown>;
  changed: Map<string, string>;
  report: (failure: string | null) => void;
}
interface Line {
  busy: boolean;
  next: Pending | null;
  /** Settles when nothing is in flight and nothing waits. */
  drained: Promise<void>;
  drain: () => void;
}

const lines = new Map<string, Line>();

function send(id: string, line: Line, p: Pending): void {
  line.busy = true;
  assistant
    .postEdits(id, { draft: p.draft, changed: [...p.changed].map(([field, value]) => ({ field, value })) })
    .then(() => p.report(null))
    .catch((e: unknown) => p.report(getErrorMessage(e)))
    .finally(() => {
      const next = line.next;
      line.next = null;
      if (next) {
        send(id, line, next);
        return;
      }
      line.busy = false;
      line.drain();
    });
}

/** Posts the draft and the fields that changed; `report` is told the failure, or null once the post was taken. */
export function postEdit(id: string, draft: Record<string, unknown>, changed: Map<string, string>, report: (failure: string | null) => void): void {
  let line = lines.get(id);
  if (!line) {
    line = { busy: false, next: null, drained: Promise.resolve(), drain: () => undefined };
    lines.set(id, line);
  }
  if (line.busy) {
    line.next = { draft, changed: new Map([...(line.next?.changed ?? []), ...changed]), report };
    return;
  }
  line.drained = new Promise<void>((done) => (line.drain = done));
  send(id, line, { draft, changed, report });
}

/** Resolves once every edit the session has been given is posted (or has failed). */
export function flushEdits(id: string): Promise<void> {
  return lines.get(id)?.drained ?? Promise.resolve();
}
