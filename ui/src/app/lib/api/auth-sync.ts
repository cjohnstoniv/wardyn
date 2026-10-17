/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Cross-tab auth sync (DR-0020, DR-0007). A tab that signs out, signs in or switches mode tells the
// other tabs of this browser with a same-origin BroadcastChannel, so they re-check who is signed in at
// once instead of on their own next request (a hidden tab never makes one).
//
// The event is an invalidation and nothing more. It carries no principal, role or token, and a
// receiver never takes its word for who is signed in: it re-reads /me. Nothing is replayed from it,
// and it reaches only this browser's tabs. It does not sign out another device or another browser.
//
// Free of copy and UI, like core.ts: it is on the eager graph.

/** "signed_out": a deliberate sign-out. "changed": any other auth event (a sign-in, a mode switch, a renewal, a token change). */
export type AuthEventKind = "signed_out" | "changed";

export const AUTH_CHANNEL = "wardyn-auth";
/** Events that arrive within this window of each other are one re-check. */
export const COALESCE_MS = 50;

interface AuthEvent {
  v: 1;
  /** The sending tab. */
  tab: string;
  /** Counts up per sending tab: a repeat or an older one is dropped. */
  seq: number;
  /** When the sender's auth event finished, in epoch milliseconds. */
  at: number;
  kind: AuthEventKind;
}

const tabId = (): string => globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;

let tab = tabId();
let sent = 0;
let lastSeq = new Map<string, number>();
// Start of the latest GET /me this tab got an answer to: a read that began after an event already saw it.
let lastReadStart = 0;
let pending: AuthEventKind | null = null;
let timer: ReturnType<typeof setTimeout> | undefined;
let channel: BroadcastChannel | null | undefined;
const listeners = new Set<(kind: AuthEventKind) => void>();

const KINDS: readonly string[] = ["signed_out", "changed"];

/** The event if `data` is exactly the shape this file sends, else null. Anything extra on it is never read. */
function parse(data: unknown): AuthEvent | null {
  if (typeof data !== "object" || data === null) return null;
  const d = data as Record<string, unknown>;
  if (d.v !== 1 || typeof d.tab !== "string" || d.tab === "" || d.tab.length > 100) return null;
  if (typeof d.seq !== "number" || !Number.isSafeInteger(d.seq) || d.seq < 1) return null;
  if (typeof d.at !== "number" || !Number.isFinite(d.at)) return null;
  if (typeof d.kind !== "string" || !KINDS.includes(d.kind)) return null;
  return { v: 1, tab: d.tab, seq: d.seq, at: d.at, kind: d.kind as AuthEventKind };
}

function flush(): void {
  const kind = pending;
  pending = null;
  timer = undefined;
  if (kind) for (const listener of [...listeners]) listener(kind);
}

function receive(data: unknown): void {
  const ev = parse(data);
  if (!ev || ev.tab === tab) return;
  // Out of order or repeated: this sender's later event already won.
  if (ev.seq <= (lastSeq.get(ev.tab) ?? 0)) return;
  lastSeq.set(ev.tab, ev.seq);
  // Stale: this tab read /me after the event happened, so that read already saw its effect.
  if (ev.at < lastReadStart) return;
  pending = pending === "signed_out" || ev.kind === "signed_out" ? "signed_out" : "changed";
  timer ??= setTimeout(flush, COALESCE_MS);
}

function open(): BroadcastChannel | null {
  if (channel !== undefined) return channel;
  channel = null;
  try {
    if (typeof BroadcastChannel !== "undefined") {
      channel = new BroadcastChannel(AUTH_CHANNEL);
      channel.addEventListener("message", (e: MessageEvent) => receive(e.data));
      // Node (the test runner) would otherwise stay alive for an open channel; a browser has no unref.
      (channel as unknown as { unref?: () => void }).unref?.();
    }
  } catch {
    channel = null;
  }
  return channel;
}

/** Tell the other tabs an auth event just finished in this one. Best effort: no channel, no sync. */
export function announceAuthChange(kind: AuthEventKind): void {
  try {
    const event: AuthEvent = { v: 1, tab, seq: ++sent, at: Date.now(), kind };
    open()?.postMessage(event);
  } catch {
    /* a closed channel or a clone failure leaves the other tabs to their next request */
  }
}

/** Subscribe to the other tabs' auth events, one call per burst. Returns the unsubscribe. */
export function onRemoteAuthChange(fn: (kind: AuthEventKind) => void): () => void {
  open();
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

/** A GET /me that started at `startedAt` was answered (200 or 401). */
export function noteIdentityRead(startedAt: number): void {
  lastReadStart = Math.max(lastReadStart, startedAt);
}

/** Test seam: forget every event, subscriber and open channel. */
export function resetAuthSyncForTest(): void {
  if (timer !== undefined) clearTimeout(timer);
  timer = undefined;
  pending = null;
  listeners.clear();
  lastSeq = new Map();
  lastReadStart = 0;
  sent = 0;
  tab = tabId();
  channel?.close();
  channel = undefined;
}
