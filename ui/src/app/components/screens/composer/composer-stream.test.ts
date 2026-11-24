/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The composer's event stream: which URL an EventSource is given (under the console's base path), where it resumes
// after a drop, when it hands over to the long-poll, and how events fold into what the screen shows.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { assistant, type AssistantEvent } from "../../../lib/api/assistant";
import { EMPTY_COMPOSER, foldEvents, openComposerStream, restoreLetters } from "./composer-stream";

class FakeSource {
  static all: FakeSource[] = [];
  closed = false;
  onerror: (() => void) | null = null;
  private handlers = new Map<string, ((m: { data: string }) => void)[]>();
  constructor(public url: string) {
    FakeSource.all.push(this);
  }
  addEventListener(kind: string, fn: (m: { data: string }) => void) {
    this.handlers.set(kind, [...(this.handlers.get(kind) ?? []), fn]);
  }
  close() {
    this.closed = true;
  }
  emit(ev: AssistantEvent) {
    for (const fn of this.handlers.get(ev.kind) ?? []) fn({ data: JSON.stringify(ev) });
  }
  drop() {
    this.onerror?.();
  }
}

const ev = (seq: number, kind: string, payload: Record<string, unknown> = {}): AssistantEvent => ({ seq, kind, payload, created_at: "2026-11-24T09:00:00Z" });
const ID = "5b1d2f3a-0000-4000-8000-000000000001";

beforeEach(() => {
  FakeSource.all = [];
  vi.stubGlobal("EventSource", FakeSource);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  delete document.documentElement.dataset.wardynBase;
});

describe("the stream's URL", () => {
  it("starts with the console's base path, never a root-absolute string", () => {
    document.documentElement.dataset.wardynBase = "/wardyn";
    const stop = openComposerStream({ sessionId: ID, onEvents: () => {}, delayMs: () => 0 });
    expect(FakeSource.all[0].url.startsWith("/wardyn/api/v1/")).toBe(true);
    expect(FakeSource.all[0].url).toBe(`/wardyn/api/v1/assistant/sessions/${ID}/events?after=0`);
    stop();
  });

  it("starts with /api/v1/ when the console has no base path", () => {
    const stop = openComposerStream({ sessionId: ID, onEvents: () => {}, delayMs: () => 0 });
    expect(FakeSource.all[0].url.startsWith("/api/v1/")).toBe(true);
    stop();
  });
});

describe("resuming", () => {
  it("reopens after a drop from the last seq it delivered, and delivers no event twice", async () => {
    vi.useFakeTimers();
    const seen: number[] = [];
    const stop = openComposerStream({ sessionId: ID, onEvents: (e) => seen.push(...e.map((x) => x.seq)), delayMs: () => 0 });
    FakeSource.all[0].emit(ev(1, "text", { text: "a" }));
    FakeSource.all[0].emit(ev(2, "text", { text: "b" }));
    FakeSource.all[0].drop();
    expect(FakeSource.all[0].closed).toBe(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(FakeSource.all).toHaveLength(2);
    expect(FakeSource.all[1].url).toBe(`/api/v1/assistant/sessions/${ID}/events?after=2`);
    // A replay of what was already delivered is dropped.
    FakeSource.all[1].emit(ev(2, "text", { text: "b" }));
    FakeSource.all[1].emit(ev(3, "text", { text: "c" }));
    expect(seen).toEqual([1, 2, 3]);
    stop();
    vi.useRealTimers();
  });

  it("closes its source when stopped", () => {
    const stop = openComposerStream({ sessionId: ID, onEvents: () => {}, delayMs: () => 0 });
    stop();
    expect(FakeSource.all[0].closed).toBe(true);
  });
});

describe("the long-poll fallback", () => {
  it("takes over after a stream that gives nothing twice, resumes from the cursor and stops when the session ended", async () => {
    vi.useFakeTimers();
    const poll = vi.spyOn(assistant, "pollEvents").mockResolvedValue({ events: [ev(4, "text", { text: "d" })], state: "ended", letters: { "Person A": "Alice" } });
    const seen: number[] = [];
    const ended = vi.fn();
    const letters = vi.fn();
    const stop = openComposerStream({ sessionId: ID, after: 3, onEvents: (e) => seen.push(...e.map((x) => x.seq)), onEnded: ended, onLetters: letters, delayMs: () => 0 });
    FakeSource.all[0].drop();
    await vi.advanceTimersByTimeAsync(0);
    FakeSource.all[1].drop();
    await vi.advanceTimersByTimeAsync(0);
    expect(poll).toHaveBeenCalledWith(ID, 3);
    expect(seen).toEqual([4]);
    expect(ended).toHaveBeenCalledTimes(1);
    expect(letters).toHaveBeenCalledWith({ "Person A": "Alice" });
    stop();
    vi.useRealTimers();
  });

  it("polls straight away when the browser has no EventSource", async () => {
    vi.stubGlobal("EventSource", undefined);
    const poll = vi.spyOn(assistant, "pollEvents").mockResolvedValue({ events: [], state: "ended", letters: {} });
    const ended = vi.fn();
    const stop = openComposerStream({ sessionId: ID, onEvents: () => {}, onEnded: ended, delayMs: () => 0 });
    await vi.waitFor(() => expect(ended).toHaveBeenCalled());
    expect(poll).toHaveBeenCalledWith(ID, 0);
    stop();
  });

  it("reports a poll that keeps failing instead of looping forever", async () => {
    vi.stubGlobal("EventSource", undefined);
    vi.spyOn(assistant, "pollEvents").mockRejectedValue(new Error("down"));
    const failed = vi.fn();
    const stop = openComposerStream({ sessionId: ID, onEvents: () => {}, onFailed: failed, delayMs: () => 0 });
    await vi.waitFor(() => expect(failed).toHaveBeenCalledTimes(1));
    stop();
  });
});

describe("restoreLetters", () => {
  it("puts names back for display, longest label first, and leaves other text alone", () => {
    const letters = { "Person A": "Alice", "Person AA": "Zed" };
    expect(restoreLetters("Person A asked, Person AA agreed, Person B stayed", letters)).toBe("Alice asked, Zed agreed, Person B stayed");
  });
});

describe("foldEvents", () => {
  it("builds the chat, joins a turn's text pieces, pairs a tool call with its result and its dry-run mark", () => {
    const s = foldEvents(EMPTY_COMPOSER, [
      ev(1, "state", { state: "ready" }),
      ev(2, "user_message", { text: "hello", turn: "x" }),
      ev(3, "text", { text: "Hel" }),
      ev(4, "text", { text: "lo" }),
      ev(5, "tool_call", { name: "read", input: "unit Payments" }),
      ev(6, "tool_record", { name: "read", ok: true, dry_run: true }),
      ev(7, "tool_result", { name: "read", ok: true, summary: "1 ceiling" }),
      ev(8, "turn_end", { usage: { input_tokens: 10, output_tokens: 4 } }),
    ]);
    expect(s.ready).toBe(true);
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant", "tool"]);
    expect(s.items[1]).toMatchObject({ text: "Hello" });
    expect(s.items[2]).toMatchObject({ name: "read", args: "unit Payments", result: "1 ceiling", ok: true, dryRun: true });
    expect(s.turns).toEqual([{ seq: 8, usage: { input_tokens: 10, output_tokens: 4 } }]);
    expect(s.lastSeq).toBe(8);
  });

  it("keeps a proposal, replaces it with the next, and notes the provider error, the tools error and the end", () => {
    let s = foldEvents(EMPTY_COMPOSER, [ev(1, "proposal", { parts: [{ section: "image", value: "node-20-dev" }], dropped: [{ host: "a.example", unit: "Payments" }] })]);
    expect(s.proposal).toEqual({ parts: [{ section: "image", value: "node-20-dev" }], dropped: [{ host: "a.example", field: undefined, unit: "Payments" }] });
    s = foldEvents(s, [ev(2, "proposal", { parts: [], dropped: [] }), ev(3, "notice", { notice: "provider_error" }), ev(4, "error", { code: "tools_unavailable", status: 404 }), ev(5, "state", { state: "ended" })]);
    expect(s.proposal).toEqual({ parts: [], dropped: [] });
    expect([s.providerError, s.toolsUnavailable, s.ended]).toEqual([true, true, true]);
  });

  it("ignores an event at or before its cursor", () => {
    const s = foldEvents(EMPTY_COMPOSER, [ev(2, "text", { text: "a" })]);
    expect(foldEvents(s, [ev(2, "text", { text: "a" }), ev(1, "text", { text: "z" })]).items).toHaveLength(1);
  });
});
