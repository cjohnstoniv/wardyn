/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, act } from "@testing-library/react";

// HIGH fix (terminal reconnect): on an UNEXPECTED WebSocket drop the component
// must re-attach to the persistent tmux session with a bounded number of
// retries (rather than just printing "[connection closed]" and giving up). A
// CLEAN close (code 1000) must NOT reconnect. These tests drive a fake
// WebSocket to pin both behaviors.

// --- Mock xterm so we don't need real DOM measurement in jsdom -------------
const writeln = vi.fn();
const resizeCalls: Array<[number, number]> = [];
vi.mock("@xterm/xterm", () => {
  class Terminal {
    cols = 80;
    rows = 24;
    resize(cols: number, rows: number) {
      this.cols = cols;
      this.rows = rows;
      resizeCalls.push([cols, rows]);
    }
    loadAddon() {}
    open() {}
    write() {}
    writeln(...a: unknown[]) {
      writeln(...a);
    }
    onData() {
      return { dispose() {} };
    }
    onBinary() {
      return { dispose() {} };
    }
    attachCustomKeyEventHandler() {}
    dispose() {}
  }
  return { Terminal };
});
vi.mock("@xterm/addon-fit", () => {
  class FitAddon {
    fit() {}
    proposeDimensions() {
      return { cols: 92, rows: 30 };
    }
  }
  return { FitAddon };
});
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));
vi.mock("@fontsource/jetbrains-mono/latin-400.css", () => ({}));
vi.mock("@fontsource/jetbrains-mono/latin-ext-400.css", () => ({}));
// Force SSO mode (no admin token) so the component actually opens a WebSocket.
vi.mock("../lib/api/core", () => ({ getToken: () => null }));

// --- Fake WebSocket --------------------------------------------------------
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
  drop(code: number, reason = "") {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code, reason });
  }
  sent: string[] = [];
  send(data: string) {
    this.sent.push(data);
  }
  close() {
    this.readyState = FakeWebSocket.CLOSED;
  }
}

import { AttachTerminal } from "./attach-terminal";

describe("AttachTerminal reconnect", () => {
  beforeEach(() => {
    FakeWebSocket.instances = [];
    writeln.mockClear();
    vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
    // jsdom has no FontFaceSet; the component awaits document.fonts.ready.
    if (!("fonts" in document)) {
      Object.defineProperty(document, "fonts", {
        configurable: true,
        value: { ready: Promise.resolve() },
      });
    }
    // jsdom has no ResizeObserver; the terminal observes its container.
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("reconnects (opens a new socket) after an UNEXPECTED close", async () => {
    render(<AttachTerminal runId="run_1" />);
    // First socket created on mount.
    expect(FakeWebSocket.instances).toHaveLength(1);
    const first = FakeWebSocket.instances[0];
    act(() => first.open());

    // Unexpected drop (non-1000 code) should schedule a reconnect.
    act(() => first.drop(1006, "abnormal"));
    // Advance past the first backoff delay.
    await act(() => vi.advanceTimersByTimeAsync(2000));

    expect(FakeWebSocket.instances.length).toBeGreaterThanOrEqual(2);
  });

  it("does NOT reconnect after a CLEAN close (code 1000)", async () => {
    const onClose = vi.fn();
    render(<AttachTerminal runId="run_1" onClose={onClose} />);
    expect(FakeWebSocket.instances).toHaveLength(1);
    const first = FakeWebSocket.instances[0];
    act(() => first.open());

    act(() => first.drop(1000, "run finished"));
    await act(() => vi.advanceTimersByTimeAsync(10000));

    // No second socket; onClose was invoked synchronously for the graceful close.
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(onClose).toHaveBeenCalled();
  });

  it("gives up after the bounded number of reconnect attempts", async () => {
    render(<AttachTerminal runId="run_1" />);
    let idx = 0;
    act(() => FakeWebSocket.instances[0].open());

    // Drop repeatedly; each unexpected drop schedules another attempt until the
    // budget (4) is exhausted. 1 initial + 4 reconnects = 5 sockets max.
    for (let i = 0; i < 8; i++) {
      const sock = FakeWebSocket.instances[idx];
      if (!sock) break;
      act(() => sock.drop(1006, "abnormal"));
      await act(() => vi.advanceTimersByTimeAsync(6000));
      idx = FakeWebSocket.instances.length - 1;
    }

    // Bounded: never more than 1 + MAX_RECONNECT_ATTEMPTS (4) = 5 sockets.
    expect(FakeWebSocket.instances.length).toBeLessThanOrEqual(5);
    expect(FakeWebSocket.instances.length).toBeGreaterThanOrEqual(2);
  });

  // ptyCols pins the VISUAL grid to the forced width (rows still fit): the login
  // CLI is a full-screen TUI that cursor-addresses whatever grid it is told, so
  // PTY and xterm must agree — the old decoupled mode (wide PTY, container-fit
  // view) interleaved redraw frames into garbage on screen.
  it("ptyCols pins the xterm grid AND the PTY resize message to the same width", async () => {
    resizeCalls.length = 0;
    render(<AttachTerminal runId="run_1" ptyCols={512} />);
    act(() => FakeWebSocket.instances[0].open());
    await act(() => vi.advanceTimersByTimeAsync(100));

    // The visual grid was resized to the pinned width, rows from the fit proposal.
    expect(resizeCalls).toContainEqual([512, 30]);
    // And the PTY resize frame agrees with the visual grid — no decoupling.
    const resizeFrames = FakeWebSocket.instances[0].sent
      .map((s: string) => {
        try {
          return JSON.parse(s);
        } catch {
          return null;
        }
      })
      .filter((m: { type?: string } | null) => m?.type === "resize");
    expect(resizeFrames.length).toBeGreaterThan(0);
    expect(resizeFrames[resizeFrames.length - 1]).toMatchObject({ cols: 512, rows: 30 });
  });
});
