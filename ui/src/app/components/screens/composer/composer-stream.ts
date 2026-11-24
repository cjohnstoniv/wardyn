/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The composer's view of a session's events: one stream (an EventSource, resumed from the last seq after a drop) that
// falls back to the long-poll when a stream cannot be held, and the pure fold of those events into what the screen
// shows. Event text is carried as text and never interpreted.
import { HttpError } from "../../../lib/api/core";
import { assistant, type AssistantEvent } from "../../../lib/api/assistant";

// The kinds the server stores, which an EventSource names as the event type of each message.
const EVENT_KINDS = [
  "state",
  "text",
  "tool_call",
  "tool_result",
  "notice",
  "turn_end",
  "error",
  "user_message",
  "person_edit",
  "proposal",
  "tool_record",
  "build_files",
  "suggestion",
] as const;

// A stream that opens and gives nothing this many times in a row hands over to the long-poll.
const STREAM_FAILURES = 2;
// A long-poll that fails this many times in a row stops, and the screen says so.
const POLL_FAILURES = 5;

export interface ComposerStreamOptions {
  sessionId: string;
  /** The last seq already held; the stream starts after it. */
  after?: number;
  /** New events, in seq order, none twice. */
  onEvents: (events: readonly AssistantEvent[]) => void;
  /** The long-poll saw the session end. */
  onEnded?: () => void;
  /** The letters the server holds for the session, as the long-poll last answered. */
  onLetters?: (letters: Readonly<Record<string, string>>) => void;
  /** Neither a stream nor a long-poll can reach the session. */
  onFailed?: (error: unknown) => void;
  /** How long to wait after the nth failure in a row; tests pass 0. */
  delayMs?: (failures: number) => number;
}

/** Opens the session's events and returns the function that closes them. */
export function openComposerStream(opts: ComposerStreamOptions): () => void {
  let after = opts.after ?? 0;
  let closed = false;
  let source: EventSource | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const delay = opts.delayMs ?? ((n: number) => Math.min(1000 * n, 5000));

  const deliver = (events: readonly AssistantEvent[]) => {
    const fresh = events.filter((e) => e.seq > after);
    if (fresh.length === 0) return;
    after = fresh[fresh.length - 1].seq;
    opts.onEvents(fresh);
  };
  const wait = (ms: number) =>
    new Promise<void>((resolve) => {
      timer = setTimeout(resolve, ms);
    });

  async function poll() {
    let failures = 0;
    while (!closed) {
      try {
        const page = await assistant.pollEvents(opts.sessionId, after);
        if (closed) return;
        failures = 0;
        deliver(page.events);
        opts.onLetters?.(page.letters);
        if (page.state === "ended") {
          opts.onEnded?.();
          return;
        }
      } catch (e) {
        if (closed) return;
        failures++;
        if ((e instanceof HttpError && e.status === 404) || failures >= POLL_FAILURES) {
          opts.onFailed?.(e);
          return;
        }
        await wait(delay(failures));
      }
    }
  }

  function stream(failures: number) {
    if (closed) return;
    if (typeof EventSource === "undefined") {
      void poll();
      return;
    }
    let got = false;
    const es = assistant.streamEvents(opts.sessionId, after);
    source = es;
    for (const kind of EVENT_KINDS) {
      es.addEventListener(kind, (m) => {
        try {
          const ev = JSON.parse((m as MessageEvent<string>).data) as AssistantEvent;
          got = true;
          deliver([ev]);
        } catch {
          // A message that is not an event is skipped; the next beat or reconnect reads from the cursor again.
        }
      });
    }
    es.onerror = () => {
      es.close();
      source = null;
      if (closed) return;
      const next = got ? 1 : failures + 1;
      if (next >= STREAM_FAILURES) {
        void poll();
        return;
      }
      timer = setTimeout(() => stream(next), delay(next));
    };
  }

  stream(0);
  return () => {
    closed = true;
    source?.close();
    source = null;
    if (timer !== undefined) clearTimeout(timer);
  };
}

/** Puts each Person letter's name back, for display only: what is posted back is never changed. */
export function restoreLetters(text: string, letters: Readonly<Record<string, string>>): string {
  let out = text;
  for (const label of Object.keys(letters).sort((a, b) => b.length - a.length)) {
    if (!letters[label]) continue;
    const re = new RegExp(`${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?![A-Za-z0-9])`, "g");
    out = out.replace(re, () => letters[label]);
  }
  return out;
}

// ---- the fold ----

export interface ToolItem {
  readonly kind: "tool";
  readonly seq: number;
  readonly name: string;
  readonly args: string;
  readonly result: string;
  readonly ok: boolean | null;
  readonly dryRun: boolean | null;
}
export interface TextItem {
  readonly kind: "user" | "assistant";
  readonly seq: number;
  readonly text: string;
}
export type ChatItem = ToolItem | TextItem;

export interface ProposalPart {
  readonly section: string;
  readonly value: unknown;
}
export interface DroppedEntry {
  readonly host?: string;
  readonly field?: string;
  readonly unit?: string;
}
export interface Proposal {
  readonly parts: readonly ProposalPart[];
  readonly dropped: readonly DroppedEntry[];
}

export interface TurnUsage {
  readonly seq: number;
  readonly usage: Readonly<Record<string, number>>;
}
export interface Suggestion {
  readonly promptDigest: string;
  readonly outputDigest: string;
  readonly tools: readonly string[];
}

export interface ComposerState {
  readonly items: readonly ChatItem[];
  readonly lastSeq: number;
  /** A state event said the assistant is ready for a message. */
  readonly ready: boolean;
  /** A state event or a notice said the session is over. */
  readonly ended: boolean;
  /** The provider did not answer: the chat and the draft are kept. */
  readonly providerError: boolean;
  /** The relay could not offer the model its tools. */
  readonly toolsUnavailable: boolean;
  readonly proposal: Proposal | null;
  readonly turns: readonly TurnUsage[];
  readonly suggestion: Suggestion | null;
}

export const EMPTY_COMPOSER: ComposerState = {
  items: [],
  lastSeq: 0,
  ready: false,
  ended: false,
  providerError: false,
  toolsUnavailable: false,
  proposal: null,
  turns: [],
  suggestion: null,
};

const str = (p: Record<string, unknown>, ...keys: string[]): string => {
  for (const k of keys) {
    const v = p[k];
    if (typeof v === "string") return v;
    if (v != null && typeof v === "object") return JSON.stringify(v);
    if (typeof v === "number" || typeof v === "boolean") return String(v);
  }
  return "";
};

function numbers(v: unknown): Record<string, number> {
  const out: Record<string, number> = {};
  if (v && typeof v === "object") for (const [k, n] of Object.entries(v)) if (typeof n === "number") out[k] = n;
  return out;
}

function proposalOf(p: Record<string, unknown>): Proposal {
  const parts = Array.isArray(p.parts) ? p.parts : [];
  const dropped = Array.isArray(p.dropped) ? p.dropped : [];
  return {
    parts: parts.filter((x): x is Record<string, unknown> => !!x && typeof x === "object").map((x) => ({ section: str(x, "section"), value: x.value })),
    dropped: dropped
      .filter((x): x is Record<string, unknown> => !!x && typeof x === "object")
      .map((x) => ({ host: str(x, "host") || undefined, field: str(x, "field") || undefined, unit: str(x, "unit") || undefined })),
  };
}

function withLastTool(items: readonly ChatItem[], pick: (t: ToolItem) => boolean, change: (t: ToolItem) => ToolItem): ChatItem[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i];
    if (it.kind === "tool" && pick(it)) {
      const next = items.slice();
      next[i] = change(it);
      return next;
    }
  }
  return null;
}

/** Folds events, in seq order, into what the screen shows. A pure function: the same events give the same state. */
export function foldEvents(state: ComposerState, events: readonly AssistantEvent[]): ComposerState {
  let s = state;
  for (const ev of events) {
    if (ev.seq <= s.lastSeq) continue;
    const p = ev.payload ?? {};
    s = { ...s, lastSeq: ev.seq };
    switch (ev.kind) {
      case "user_message":
        s = { ...s, items: [...s.items, { kind: "user", seq: ev.seq, text: str(p, "text") }] };
        break;
      case "text": {
        const last = s.items[s.items.length - 1];
        // The relay posts a turn's text in pieces: consecutive pieces are one message.
        s =
          last && last.kind === "assistant"
            ? { ...s, items: [...s.items.slice(0, -1), { ...last, text: last.text + str(p, "text") }] }
            : { ...s, items: [...s.items, { kind: "assistant", seq: ev.seq, text: str(p, "text") }] };
        break;
      }
      case "tool_call":
        s = {
          ...s,
          items: [...s.items, { kind: "tool", seq: ev.seq, name: str(p, "name"), args: str(p, "input", "args", "arguments", "summary"), result: "", ok: null, dryRun: null }],
        };
        break;
      case "tool_result": {
        const name = str(p, "name");
        const ok = typeof p.ok === "boolean" ? p.ok : null;
        const result = str(p, "summary", "result", "output");
        const next = withLastTool(s.items, (t) => t.name === name && t.result === "" && t.ok === null, (t) => ({ ...t, result, ok }));
        s = { ...s, items: next ?? [...s.items, { kind: "tool", seq: ev.seq, name, args: "", result, ok, dryRun: null }] };
        break;
      }
      case "tool_record": {
        const name = str(p, "name");
        const dryRun = typeof p.dry_run === "boolean" ? p.dry_run : null;
        const next = withLastTool(s.items, (t) => t.name === name && t.dryRun === null, (t) => ({ ...t, dryRun }));
        if (next) s = { ...s, items: next };
        break;
      }
      case "state": {
        const st = str(p, "state");
        if (st === "ready") s = { ...s, ready: true, providerError: false };
        else if (st === "idle" || st === "ended") s = { ...s, ended: true };
        break;
      }
      case "notice": {
        const n = str(p, "notice");
        if (n === "provider_error") s = { ...s, providerError: true };
        else if (n === "idle") s = { ...s, ended: true };
        break;
      }
      case "error":
        if (str(p, "code") === "tools_unavailable") s = { ...s, toolsUnavailable: true };
        break;
      case "proposal":
        s = { ...s, proposal: proposalOf(p) };
        break;
      case "turn_end":
        s = { ...s, turns: [...s.turns, { seq: ev.seq, usage: numbers(p.usage ?? p) }] };
        break;
      case "suggestion":
        s = {
          ...s,
          suggestion: {
            promptDigest: str(p, "prompt_digest"),
            outputDigest: str(p, "output_digest"),
            tools: Array.isArray(p.tools) ? p.tools.filter((t): t is string => typeof t === "string") : [],
          },
        };
        break;
    }
  }
  return s;
}
