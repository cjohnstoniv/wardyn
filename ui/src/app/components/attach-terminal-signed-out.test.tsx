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
import { render } from "@testing-library/react";
import type { ReactNode } from "react";

vi.mock("@xterm/xterm", () => {
  class Terminal {
    cols = 80;
    rows = 24;
    resize() {}
    loadAddon() {}
    open() {}
    write() {}
    writeln() {}
    focus() {}
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
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: class {
    fit() {}
    proposeDimensions() {
      return { cols: 92, rows: 30 };
    }
  },
}));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));
vi.mock("@fontsource/jetbrains-mono/latin-400.css", () => ({}));
vi.mock("@fontsource/jetbrains-mono/latin-ext-400.css", () => ({}));
// No admin token: an operator on a cookie session takes the cookie lane, the
// one that opens the socket directly.
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
import { ReauthContext, type Reauth, type ReauthPhase } from "../lib/reauth";

function reauthAt(phase: ReauthPhase): Reauth {
  const noop = () => {};
  return {
    phase,
    writeDropped: null,
    setPhase: noop,
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
  vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
  if (!("fonts" in document)) {
    Object.defineProperty(document, "fonts", { configurable: true, value: { ready: Promise.resolve() } });
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
});
