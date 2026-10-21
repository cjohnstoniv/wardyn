/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #483 — the terminal's WebSocket does not go through wfetch, so the
// signed-out hold cannot see it. While the console is signed out mid-page
// (the dialog or the read-only bar) no socket may be opened or kept: after a
// sign-in in another tab the session behind it can be someone else's, and
// keystrokes would reach their PTY. The terminal reconnects on resume.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { TERMINAL_SESSION } from "../lib/session-continuity-copy";
import type { ReactNode } from "react";

// How many terminals were built and disposed: a lapse must keep the one on screen.
// M12: what the terminal was told to write, and whether anything wiped its scrollback.
const xt = vi.hoisted(() => ({ made: 0, disposed: 0, written: [] as string[], wiped: 0 }));
vi.mock("@xterm/xterm", () => {
  class Terminal {
    constructor() {
      xt.made += 1;
    }
    cols = 80;
    rows = 24;
    resize() {}
    unicode = { activeVersion: "6" };
    loadAddon() {}
    open() {}
    write(d: string) {
      xt.written.push(String(d));
    }
    writeln(d: string) {
      xt.written.push(String(d));
    }
    clear() {
      xt.wiped += 1;
    }
    reset() {
      xt.wiped += 1;
    }
    focus() {}
    onData() {
      return { dispose() {} };
    }
    onBinary() {
      return { dispose() {} };
    }
    modes = { mouseTrackingMode: "none" };
    attachCustomWheelEventHandler() {}
    attachCustomKeyEventHandler() {}
    dispose() {
      xt.disposed += 1;
    }
  }
  return { Terminal };
});
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: class {
    fit() {}
    proposeDimensions() {
      return { cols: 92, rows: 30 };
    }
  },
}));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));
// No admin token: an operator on a cookie session takes the cookie lane, the
// one that opens the socket directly.
vi.mock("../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api/core")>()),
  getToken: () => null,
}));
vi.mock("../lib/api/runs", () => ({ runs: { attachTicket: vi.fn(async () => "ticket"), takeoverAttach: vi.fn() } }));

class FakeWebSocket {
  static OPEN = 1;
  static CONNECTING = 0;
  static CLOSED = 3;
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  binaryType = "";
  closed = false;
  onopen: (() => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((ev: unknown) => void) | null = null;
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  send() {}
  close() {
    this.closed = true;
    this.readyState = FakeWebSocket.CLOSED;
  }
}

import { AttachTerminal } from "./attach-terminal";
import { runs } from "../lib/api/runs";
import { ReauthContext, type Reauth, type ReauthPhase } from "../lib/reauth";

function reauthAt(phase: ReauthPhase): Reauth {
  const noop = () => {};
  return {
    phase,
    signedOut: phase === "dialog" || phase === "bar",
    renewal: null,
    watch: null,
    writeDropped: null,
    setPhase: noop,
    setWatch: noop,
    startRenew: noop,
    endRenew: noop,
    reloadAs: noop,
    clearWriteDropped: noop,
    writeDroppedClaimed: () => false,
    claimWriteDropped: () => noop,
  };
}
const at = (phase: ReauthPhase, children: ReactNode) => (
  <ReauthContext.Provider value={reauthAt(phase)}>{children}</ReauthContext.Provider>
);

beforeEach(() => {
  FakeWebSocket.instances = [];
  xt.made = 0;
  xt.disposed = 0;
  xt.written = [];
  xt.wiped = 0;
  vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
  if (!("fonts" in document)) {
    Object.defineProperty(document, "fonts", { configurable: true, value: { ready: Promise.resolve(), load: () => Promise.resolve([]) } });
  }
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});
afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AttachTerminal — signed out mid-page (#483)", () => {
  it("opens no socket while signed out, and connects once the same person is back", () => {
    const { rerender } = render(at("bar", <AttachTerminal runId="run_1" />));
    expect(FakeWebSocket.instances).toHaveLength(0);
    rerender(at("dialog", <AttachTerminal runId="run_1" />));
    expect(FakeWebSocket.instances).toHaveLength(0);
    rerender(at("none", <AttachTerminal runId="run_1" />));
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it("closes a live socket when the session ends, and reconnects on resume", () => {
    const { rerender } = render(at("none", <AttachTerminal runId="run_1" />));
    expect(FakeWebSocket.instances).toHaveLength(1);
    const live = FakeWebSocket.instances[0];
    rerender(at("dialog", <AttachTerminal runId="run_1" />));
    expect(live.closed).toBe(true);
    expect(FakeWebSocket.instances).toHaveLength(1);
    rerender(at("none", <AttachTerminal runId="run_1" />));
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(FakeWebSocket.instances[1].closed).toBe(false);
  });

  it("keeps the same terminal, and what it shows, through a lapse and the same person's return", () => {
    const { rerender } = render(at("none", <AttachTerminal runId="run_1" />));
    expect(xt.made).toBe(1);
    rerender(at("dialog", <AttachTerminal runId="run_1" />));
    expect(FakeWebSocket.instances[0].closed).toBe(true);
    expect(xt.disposed).toBe(0);
    rerender(at("none", <AttachTerminal runId="run_1" />));
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(xt.made).toBe(1);
    expect(xt.disposed).toBe(0);
  });

  it("does not retry a dropped socket while signed out", () => {
    vi.useFakeTimers();
    try {
      const { rerender } = render(at("none", <AttachTerminal runId="run_1" />));
      FakeWebSocket.instances[0].onclose?.({ code: 1006, reason: "" });
      rerender(at("dialog", <AttachTerminal runId="run_1" />));
      vi.advanceTimersByTime(30_000);
      expect(FakeWebSocket.instances).toHaveLength(1);
      rerender(at("none", <AttachTerminal runId="run_1" />));
      expect(FakeWebSocket.instances).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("a ticket mint still in flight across a sign-out and back opens no extra socket", async () => {
    vi.useFakeTimers();
    try {
      let resolveMint: (t: string) => void = () => {};
      vi.mocked(runs.attachTicket).mockImplementationOnce(() => new Promise<string>((r) => (resolveMint = r)));
      const { rerender } = render(at("none", <AttachTerminal runId="run_1" />));
      // The direct handshake fails before opening, so the terminal asks the mint why.
      FakeWebSocket.instances[0].onclose?.({ code: 1006, reason: "" });
      rerender(at("dialog", <AttachTerminal runId="run_1" />));
      rerender(at("none", <AttachTerminal runId="run_1" />));
      expect(FakeWebSocket.instances).toHaveLength(2);
      resolveMint("ticket");
      await vi.advanceTimersByTimeAsync(30_000);
      expect(FakeWebSocket.instances).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  // M12 (#1995): the scrollback survives the dialog, the person is told while signed out, and the
  // reconnect says so once in the terminal's own output.
  it("keeps the scrollback through a sign-out and says it reconnected, once, when the socket reopens", () => {
    const { rerender } = render(at("none", <AttachTerminal runId="run_1" />));
    FakeWebSocket.instances[0].readyState = FakeWebSocket.OPEN;
    FakeWebSocket.instances[0].onopen?.();
    expect(xt.written.filter((w) => w.includes("Signed in again at"))).toHaveLength(0);
    rerender(at("dialog", <AttachTerminal runId="run_1" />));
    expect(screen.getByText(TERMINAL_SESSION.SIGNED_OUT_NOTE)).toBeInTheDocument();
    rerender(at("none", <AttachTerminal runId="run_1" />));
    expect(screen.queryByText(TERMINAL_SESSION.SIGNED_OUT_NOTE)).toBeNull();
    // Not claimed until the new socket is open.
    expect(xt.written.filter((w) => w.includes("Signed in again at"))).toHaveLength(0);
    FakeWebSocket.instances[1].readyState = FakeWebSocket.OPEN;
    FakeWebSocket.instances[1].onopen?.();
    const resumed = xt.written.filter((w) => w.includes("Signed in again at"));
    expect(resumed).toHaveLength(1);
    expect(resumed[0]).toMatch(/── Signed in again at .+\. Reconnected; the output above is kept\. ──/);
    expect(xt.made).toBe(1);
    expect(xt.disposed).toBe(0);
    expect(xt.wiped).toBe(0);
  });

  it("neg: a terminal first opened after a sign-out has nothing above it to claim kept", () => {
    const { rerender } = render(at("dialog", <AttachTerminal runId="run_1" />));
    rerender(at("none", <AttachTerminal runId="run_1" />));
    FakeWebSocket.instances[0].readyState = FakeWebSocket.OPEN;
    FakeWebSocket.instances[0].onopen?.();
    expect(xt.written.filter((w) => w.includes("Signed in again at"))).toHaveLength(0);
  });

  it("keeps a live socket through a renewal from the expiry banner: nobody is signed out", () => {
    const { rerender } = render(at("none", <AttachTerminal runId="run_1" />));
    const live = FakeWebSocket.instances[0];
    rerender(at("renew", <AttachTerminal runId="run_1" />));
    expect(live.closed).toBe(false);
    expect(FakeWebSocket.instances).toHaveLength(1);
  });
});
