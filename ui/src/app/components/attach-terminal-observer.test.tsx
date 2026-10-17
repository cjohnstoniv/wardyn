/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * Pins term-t16: a read-only viewer's grid is pinned to the writer's size on
 * EVERY attach-mode frame, and trackpad wheel events become at most one
 * coalesced SGR mouse-report frame per animation frame.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, act } from "@testing-library/react";

const term = {
  mouse: "none" as "none" | "any",
  wheel: null as ((e: WheelEvent) => boolean) | null,
  resizes: [] as Array<[number, number]>,
};

vi.mock("@xterm/xterm", () => {
  class Terminal {
    cols = 80;
    rows = 24;
    element = null;
    unicode = { activeVersion: "6" };
    get modes() {
      return { mouseTrackingMode: term.mouse };
    }
    loadAddon() {}
    open() {}
    write() {}
    writeln() {}
    resize(c: number, r: number) {
      this.cols = c;
      this.rows = r;
      term.resizes.push([c, r]);
    }
    focus() {}
    onData() {
      return { dispose() {} };
    }
    onBinary() {
      return { dispose() {} };
    }
    attachCustomKeyEventHandler() {}
    attachCustomWheelEventHandler(fn: (e: WheelEvent) => boolean) {
      term.wheel = fn;
    }
    dispose() {}
  }
  return { Terminal };
});
vi.mock("@xterm/addon-fit", () => {
  class FitAddon {
    fit() {}
    proposeDimensions() {
      return { cols: 80, rows: 24 };
    }
  }
  return { FitAddon };
});
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));
vi.mock("@fontsource/jetbrains-mono/latin-400.css", () => ({}));
vi.mock("@fontsource/jetbrains-mono/latin-ext-400.css", () => ({}));
vi.mock("../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api/core")>()),
  getToken: () => null,
}));
vi.mock("../lib/api/runs", () => ({ runs: { attachTicket: vi.fn(), takeoverAttach: vi.fn() } }));

class FakeWebSocket {
  static OPEN = 1;
  static CONNECTING = 0;
  static CLOSED = 3;
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  binaryType = "";
  onopen: (() => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((ev: unknown) => void) | null = null;
  sent: unknown[] = [];
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  send(data: unknown) {
    this.sent.push(data);
  }
  close() {}
}

import { AttachTerminal } from "./attach-terminal";

const frame = (readOnly: boolean, cols: number, rows: number) =>
  JSON.stringify({ type: "attach-mode", read_only: readOnly, holder: { held: true, principal: "p", cols, rows, source: "web" } });

function open() {
  const utils = render(<AttachTerminal runId="run_1" />);
  const ws = FakeWebSocket.instances[0];
  act(() => {
    ws.readyState = FakeWebSocket.OPEN;
    ws.onopen?.();
  });
  return { ws, ...utils };
}
const mode = (ws: FakeWebSocket, f: string) => act(() => ws.onmessage?.({ data: f }));

let rafs: FrameRequestCallback[] = [];

beforeEach(() => {
  FakeWebSocket.instances = [];
  term.mouse = "none";
  term.wheel = null;
  term.resizes = [];
  rafs = [];
  vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
  vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => rafs.push(cb));
  vi.stubGlobal("cancelAnimationFrame", () => {});
  Object.defineProperty(document, "fonts", { configurable: true, value: { ready: Promise.resolve(), load: () => Promise.resolve([]) } });
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});
afterEach(() => vi.unstubAllGlobals());

describe("AttachTerminal — observer grid", () => {
  it("pins to the writer's size on every attach-mode frame, and unpins on promotion", () => {
    const { ws } = open();
    mode(ws, frame(true, 120, 40));
    expect(term.resizes.at(-1)).toEqual([120, 40]);
    mode(ws, frame(true, 140, 50));
    expect(term.resizes.at(-1)).toEqual([140, 50]);
    mode(ws, frame(false, 140, 50));
    // Unpinned: the promotion refit fits the container and tells the PTY.
    expect(ws.sent.some((m) => typeof m === "string" && m.includes('"resize"'))).toBe(true);
  });

  it("sends no resize frame while pinned", () => {
    const { ws } = open();
    mode(ws, frame(true, 120, 40));
    expect(ws.sent.filter((s) => typeof s === "string" && s.includes("resize") && s.includes("120"))).toEqual([]);
  });
});

describe("AttachTerminal — wheel coalescing", () => {
  const wheel = (deltaY: number) => ({ deltaY, deltaMode: 1, clientX: 0, clientY: 0 }) as WheelEvent;
  const binary = (ws: FakeWebSocket) => ws.sent.filter((s) => ArrayBuffer.isView(s)).map((s) => new TextDecoder().decode(s as Uint8Array));

  it("N wheel events in one frame send exactly one report frame carrying all N lines", () => {
    const { ws } = open();
    mode(ws, frame(false, 80, 24));
    term.mouse = "any";
    for (let i = 0; i < 5; i++) expect(term.wheel!(wheel(1))).toBe(false);
    expect(binary(ws)).toEqual([]);
    act(() => rafs.splice(0).forEach((cb) => cb(0)));
    const out = binary(ws);
    expect(out).toHaveLength(1);
    expect(out[0]).toBe("\x1b[<65;1;1M".repeat(5));
  });

  it("scrolling up uses the up button, and with mouse reporting off xterm keeps the event", () => {
    const { ws } = open();
    mode(ws, frame(false, 80, 24));
    expect(term.wheel!(wheel(-1))).toBe(true);
    term.mouse = "any";
    term.wheel!(wheel(-2));
    act(() => rafs.splice(0).forEach((cb) => cb(0)));
    expect(binary(ws)).toEqual(["\x1b[<64;1;1M".repeat(2)]);
  });

  it("an observer's wheel is swallowed and sends nothing", () => {
    const { ws } = open();
    mode(ws, frame(true, 120, 40));
    term.mouse = "any";
    rafs.length = 0;
    expect(term.wheel!(wheel(3))).toBe(false);
    expect(rafs).toHaveLength(0);
    expect(binary(ws)).toEqual([]);
  });
});
