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
}
interface Line {
  busy: boolean;
  next: Pending | null;
  /** The sentence for the last post that failed, until a later one is taken; whichever editor is open for the session shows it. */
  failure: string | null;
  listeners: Set<(failure: string | null) => void>;
  /** Settles when nothing is in flight and nothing waits. */
  drained: Promise<void>;
  drain: () => void;
}

const lines = new Map<string, Line>();

/** Whether the session has a line: one that is posting, waiting, holding a failure or watched. */
export const hasLine = (id: string): boolean => lines.has(id);

function forgetIfIdle(id: string, line: Line): void {
  if (!line.busy && !line.failure && line.listeners.size === 0 && lines.get(id) === line) lines.delete(id);
}

function settle(line: Line, failure: string | null): void {
  line.failure = failure;
  line.listeners.forEach((l) => l(failure));
}

function lineOf(id: string): Line {
  let line = lines.get(id);
  if (!line) {
    line = { busy: false, next: null, failure: null, listeners: new Set(), drained: Promise.resolve(), drain: () => undefined };
    lines.set(id, line);
  }
  return line;
}

function send(id: string, line: Line, p: Pending): void {
  line.busy = true;
  assistant
    .postEdits(id, { draft: p.draft, changed: [...p.changed].map(([field, value]) => ({ field, value })) })
    .then(() => settle(line, null))
    .catch((e: unknown) => settle(line, getErrorMessage(e)))
    .finally(() => {
      const next = line.next;
      line.next = null;
      if (next) {
        send(id, line, next);
        return;
      }
      line.busy = false;
      line.drain();
      forgetIfIdle(id, line);
    });
}

/** Posts the draft and the fields that changed. A failure is kept on the session's line, see `onEditFailure`. */
export function postEdit(id: string, draft: Record<string, unknown>, changed: Map<string, string>): void {
  const line = lineOf(id);
  if (line.busy) {
    line.next = { draft, changed: new Map([...(line.next?.changed ?? []), ...changed]) };
    return;
  }
  line.drained = new Promise<void>((done) => (line.drain = done));
  send(id, line, { draft, changed });
}

/** Tells `listener` the session's last edit failure now and whenever it changes, whichever editor posted it. Returns the way to stop. */
export function onEditFailure(id: string, listener: (failure: string | null) => void): () => void {
  const line = lineOf(id);
  line.listeners.add(listener);
  listener(line.failure);
  return () => {
    line.listeners.delete(listener);
    forgetIfIdle(id, line);
  };
}

/**
 * Resolves once every edit the session has been given is posted, with the sentence for the last post's failure, or
 * null when it was taken (or none was made). A caller that must not act on an unsaved draft stops on a sentence.
 */
export async function flushEdits(id: string): Promise<string | null> {
  const line = lines.get(id);
  if (!line) return null;
  await line.drained;
  return line.failure;
}
