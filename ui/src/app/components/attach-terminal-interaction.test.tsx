/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from attach-terminal.test.tsx (#195): that file was over the 800-line
// test gate. The connect/reconnect/take-over describes stay there; the
// handshake-timeout, keyboard-trap-escape, focus, and mode-flip-refit
// describes — which only need a lighter mock surface — live here.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, act, screen, fireEvent } from "@testing-library/react";

// Mock xterm so we don't need real DOM measurement in jsdom
const writeln = vi.fn();
// D3: how many times the component asked xterm to take focus — the fix under
// test never touches real DOM focus (jsdom's own click-to-focus isn't
// wired up by this mock), so a call counter is the only observable.
const focusCalls = { n: 0 };
// The handler AttachTerminal installs via attachCustomKeyEventHandler (F144).
let keyHandler: ((e: KeyboardEvent) => boolean) | null = null;
vi.mock("@xterm/xterm", () => {
  class Terminal {
    cols = 80;
    rows = 24;
    resize(cols: number, rows: number) {
      this.cols = cols;
      this.rows = rows;
    }
    loadAddon() {}
    open() {}
    write() {}
    writeln(...a: unknown[]) {
      writeln(...a);
    }
    focus() {
      focusCalls.n++;
    }
    onData() {
      return { dispose() {} };
    }
    onBinary() {
      return { dispose() {} };
    }
    // F144: captured, not swallowed. The escape chord is bound through this
    // hook, so a no-op mock would make the keyboard-trap fix untestable — and
    // untestable is how it got filed in the first place.
    attachCustomKeyEventHandler(fn: (e: KeyboardEvent) => boolean) {
      keyHandler = fn;
    }
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
vi.mock("../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api/core")>()),
  getToken: () => null,
}));
// Ticket mint / take-over lanes are unused by the describes in this file
// (none of them exercise the member or take-over paths), stubbed only so the
// component's own import of this module resolves.
vi.mock("../lib/api/runs", () => ({ runs: { attachTicket: vi.fn(), takeoverAttach: vi.fn() } }));

// Fake WebSocket
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
  /** Deliver a server frame. A STRING is a text/control frame (attach-mode). */
  message(data: unknown) {
    this.onmessage?.({ data });
  }
  close() {
    // Faithful to the spec's "fail the WebSocket connection": closing a socket
    // that is still CONNECTING aborts the handshake and DOES fire a close
    // event, abnormally (1006). Without that, a fake cannot express the one
    // failure R4-F143 is about — a 101 upgrade a proxy accepted and then sat
    // on. An OPEN socket keeps the old silent behaviour: the component's own
    // teardown closes those, and it has already detached the handlers.
    const wasConnecting = this.readyState === FakeWebSocket.CONNECTING;
    this.readyState = FakeWebSocket.CLOSED;
    if (wasConnecting) this.onclose?.({ code: 1006, reason: "" });
  }
}

import { AttachTerminal } from "./attach-terminal";
import { TERMINAL } from "./wardyn/copy";
import { aheadByHours } from "../lib/test-clock";

// The attach-mode control frame the daemon sends as a TEXT frame on EVERY
// connect (internal/api/attach_holder.go), read_only=false included.
function attachModeFrame(readOnly: boolean, principal: string) {
  return JSON.stringify({
    type: "attach-mode",
    read_only: readOnly,
    holder: {
      held: true,
      principal,
      since: aheadByHours(-1),
      cols: 132,
      rows: 50,
      source: "web",
    },
  });
}

// Shared jsdom shims: the component measures fonts and observes its container.
function stubTerminalEnv() {
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
}

// R4-F143: every state in this component came off the socket's open/close/error
// events, and a stalled 101 upgrade fires none of them. The deploy README warns
// about exactly that proxy ("must not buffer or strip the 101 upgrade"), and
// measured against one, the panel read "Connecting…" with a spinner and no
// message for 25s and would have read it forever. A connect deadline makes the
// silence a failed attempt, so the panel reaches the same bounded, honest
// closed state a refused socket already reached.
describe("AttachTerminal — a handshake that never completes is a failure, not a spinner", () => {
  beforeEach(() => {
    stubTerminalEnv();
    writeln.mockClear();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("waits, then spends the reconnect budget and says the panel is closed", async () => {
    render(<AttachTerminal runId="run_1" />);
    expect(FakeWebSocket.instances).toHaveLength(1);

    // Still inside the deadline: nothing has been given up on, and the socket
    // is deliberately left alone (a slow-but-live upgrade must not be killed).
    await act(() => vi.advanceTimersByTimeAsync(14_000));
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(screen.getByText("Connecting…")).toBeInTheDocument();

    // Past it: the attempt is abandoned and the existing backoff takes over —
    // no second failure vocabulary, the same bounded 1 + MAX_RECONNECT_ATTEMPTS.
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(FakeWebSocket.instances.length).toBeGreaterThan(1);

    // ...and it ENDS. Every later socket stalls the same way; the budget runs
    // out and the panel stops claiming it is connecting.
    await act(() => vi.advanceTimersByTimeAsync(120_000));
    expect(FakeWebSocket.instances.length).toBeLessThanOrEqual(5);
    expect(screen.queryByText("Connecting…")).toBeNull();
    // #216 — the bar goes back to just naming the run (no more `[closed]`);
    // TERMINAL.CLOSED_TITLE / CLOSED_BODY and a Reconnect button, OUTSIDE the
    // scrollback, carry the state and the way back in.
    expect(screen.getByText("run_1")).toBeInTheDocument();
    expect(screen.queryByText("[closed] run_1")).toBeNull();
    expect(screen.getByText(TERMINAL.CLOSED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(TERMINAL.CLOSED_BODY(4))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: TERMINAL.RECONNECT })).toBeInTheDocument();
    // Never written into xterm's own buffer — the writeln mock proves it.
    expect(writeln).not.toHaveBeenCalledWith(expect.stringContaining("[connection closed after"));
  });

  // #216 — the whole point: a spent budget must not be a dead end. Reconnect
  // drops the exhausted state and attaches again, keeping the same xterm
  // instance (and its scrollback) rather than tearing the panel down.
  it("Reconnect, after the budget is spent, attaches again", async () => {
    render(<AttachTerminal runId="run_1" />);
    let idx = 0;
    act(() => FakeWebSocket.instances[0].open());
    for (let i = 0; i < 8; i++) {
      const sock = FakeWebSocket.instances[idx];
      if (!sock) break;
      act(() => sock.drop(1006, "abnormal"));
      await act(() => vi.advanceTimersByTimeAsync(6000));
      idx = FakeWebSocket.instances.length - 1;
    }
    const reconnectButton = screen.getByRole("button", { name: TERMINAL.RECONNECT });
    const socketsBefore = FakeWebSocket.instances.length;

    act(() => reconnectButton.click());

    expect(FakeWebSocket.instances.length).toBe(socketsBefore + 1);
    expect(screen.queryByText(TERMINAL.CLOSED_TITLE)).toBeNull();
    act(() => FakeWebSocket.instances[FakeWebSocket.instances.length - 1].open());
    expect(screen.getByText("attach — run_1")).toBeInTheDocument();
  });

  it("leaves a handshake that DOES complete alone — no deadline fires on a live socket", async () => {
    render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());

    await act(() => vi.advanceTimersByTimeAsync(120_000));
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(FakeWebSocket.instances[0].readyState).toBe(FakeWebSocket.OPEN);
  });
});

// R4-F144 (WCAG 2.1.2, No Keyboard Trap): xterm takes Tab, Shift+Tab and
// Escape into the PTY — correct for a terminal, and it means a keyboard user
// who focuses this panel cannot leave the page without a pointer. 2.1.2
// allows a non-standard exit only if it is advised on entry, so the chord and
// its announcement are one feature: either alone still fails the criterion.
//
// The advertised chord is Ctrl+Shift+Backspace (#133) — the earlier Ctrl+]
// never fired on DE/FR/ES layouts, where AltGr (needed to type `]`) arrives
// at the browser as ctrlKey && altKey. Ctrl+] still works, silently,
// as a US-only fallback, but must not fire when altKey is held — that is
// AltGr typing a bracket, not the chord. The per-layout matrix lives in
// attach-terminal-keys.test.ts; these tests pin the wiring into the widget.
describe("AttachTerminal — the keyboard trap has an advertised exit", () => {
  // ticket: F144
  beforeEach(() => {
    keyHandler = null;
    FakeWebSocket.instances = [];
    vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
  });
  afterEach(() => vi.unstubAllGlobals());

  const chord = () =>
    ({
      type: "keydown",
      key: "Backspace",
      ctrlKey: true,
      shiftKey: true,
      altKey: false,
      metaKey: false,
    }) as unknown as KeyboardEvent;

  it("advertises the chord in the chrome AND to a screen reader", () => {
    const { container } = render(<AttachTerminal runId="run_1" />);
    // Visible, in the title bar, before anyone is trapped.
    expect(screen.getByText(TERMINAL.ESCAPE_CHORD_HINT)).toBeInTheDocument();
    // …and on the grid itself, for the reader who never sees the title bar.
    expect(
      container.querySelector(`[aria-description="${TERMINAL.ESCAPE_CHORD_HINT}"]`),
    ).not.toBeNull();
  });

  it("Ctrl+Shift+Backspace moves focus OUT of the terminal and is not forwarded to the PTY", () => {
    render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());
    expect(keyHandler).not.toBeNull();

    const handled = keyHandler!(chord());

    // false = xterm must not also send it to the shell. Without this the chord
    // would leave the terminal AND type into the agent's session.
    expect(handled).toBe(false);
    // Focus is on the panel — tabIndex -1, so Tab continues from here in
    // document order rather than restarting at the top of the page.
    expect(document.activeElement).not.toBeNull();
    expect((document.activeElement as HTMLElement).tabIndex).toBe(-1);
  });

  it("Ctrl+] (US, no AltGr) still escapes silently", () => {
    render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());
    const bracket = { ...chord(), key: "]", shiftKey: false } as unknown as KeyboardEvent;
    expect(keyHandler!(bracket)).toBe(false);
  });

  it("Ctrl+] with altKey held (DE AltGr+9 typing a bracket) reaches the PTY, not the escape", () => {
    render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());
    const altGr = { ...chord(), key: "]", shiftKey: false, altKey: true } as unknown as KeyboardEvent;
    expect(keyHandler!(altGr)).toBe(true);
  });

  it("leaves an ordinary ] alone — the terminal still gets its bracket", () => {
    render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());
    const plain = { ...chord(), key: "]", shiftKey: false, ctrlKey: false } as unknown as KeyboardEvent;
    expect(keyHandler!(plain)).toBe(true);
  });
});

// D3: a mousedown anywhere in the terminal container must focus the terminal.
// xterm's own click-to-focus only fires on its inner `.xterm-screen`, so a
// click on the container's padding, or the dead space below the last row,
// lands nowhere — which reads as needing a very specific click location, or
// a second click that happens to land on the screen.
describe("AttachTerminal — focus", () => {
  // ticket: D3
  beforeEach(() => {
    focusCalls.n = 0;
    stubTerminalEnv();
  });
  afterEach(() => vi.unstubAllGlobals());

  it("a mousedown anywhere in the terminal container focuses the terminal", () => {
    const { container } = render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());
    focusCalls.n = 0; // isolate from any focus() the connect/open sequence issued

    // The container is the element the aria-description lives on (same query
    // the F144 test above uses) — it is NOT the inner `.xterm-screen` xterm
    // itself would focus, which is exactly the gap this fix closes: a click
    // that never reaches that inner element must still focus the terminal.
    const el = container.querySelector(`[aria-description="${TERMINAL.ESCAPE_CHORD_HINT}"]`);
    expect(el).not.toBeNull();
    fireEvent.mouseDown(el!);

    expect(focusCalls.n).toBeGreaterThan(0);
  });

  it("a writable socket opening focuses the terminal without requiring a click", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    focusCalls.n = 0;

    act(() => ws.message(attachModeFrame(false, "me@example.com")));

    expect(focusCalls.n).toBeGreaterThan(0);
  });

  it("a read-only attach-mode frame does NOT steal focus (nothing to type into yet)", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    focusCalls.n = 0;

    act(() => ws.message(attachModeFrame(true, "alice@example.com")));

    expect(focusCalls.n).toBe(0);
  });
});

// Promoted-in-place geometry: the attach-mode frame can, in principle, arrive
// more than once on the same socket (the server pushes an update when the
// holder slot changes); when one flips read_only true→false, this client
// inherits the departed holder's tmux geometry (the server skips handshake
// geometry for a non-writer and drops an observer's resize frames), so
// refit() must run on a mode change to correct it.
describe("AttachTerminal — refit(true) when read_only flips true→false", () => {
  beforeEach(stubTerminalEnv);
  afterEach(() => vi.unstubAllGlobals());

  it("forces the resize nudge on the flip, the same shape Redraw sends", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(true, "alice@example.com")));
    ws.sent.length = 0; // isolate from connect-time sends

    act(() => ws.message(attachModeFrame(false, "me@example.com")));

    const resizes = ws.sent
      .filter((m) => typeof m === "string" && m.includes('"resize"'))
      .map((m) => JSON.parse(m as string));
    expect(resizes.length).toBeGreaterThanOrEqual(2);
    const [nudge, real] = resizes.slice(-2);
    expect(nudge.cols).toBe(real.cols - 1);
    expect(nudge.rows).toBe(real.rows);
  });

  it("does NOT force a nudge on the FIRST attach-mode frame (never was read-only)", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    ws.sent.length = 0; // isolate from the connect-time refit(true) in onopen

    act(() => ws.message(attachModeFrame(false, "me@example.com")));

    // onopen's own unconditional refit(true) already ran before this frame;
    // the flip-specific nudge must not ALSO fire for a connection that was
    // never read-only in the first place.
    const resizes = ws.sent
      .filter((m) => typeof m === "string" && m.includes('"resize"'))
      .map((m) => JSON.parse(m as string));
    expect(resizes.length).toBe(0);
  });
});
