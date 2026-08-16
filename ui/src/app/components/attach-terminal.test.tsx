/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, act, screen, waitFor, within, fireEvent } from "@testing-library/react";

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
// importOriginal, not a bare stub: the component now needs the REAL HttpError
// class, because a 409 from take-over ("nobody is attached") is a distinct
// outcome from a failure — it means the holder left, so the observer reclaims
// rather than showing an error it cannot act on.
vi.mock("../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api/core")>()),
  getToken: () => null,
}));
// Ticket mint (owner-or-admin lane): unused by the operator/cookie-lane tests
// below (tokenOnlyMode=false, operator=true never takes this path), stubbed
// for the owner test that does.
vi.mock("../lib/api/runs", () => ({ runs: { attachTicket: vi.fn(), takeoverAttach: vi.fn() } }));

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
  /** Deliver a server frame. A STRING is a text/control frame (attach-mode). */
  message(data: unknown) {
    this.onmessage?.({ data });
  }
  close() {
    this.readyState = FakeWebSocket.CLOSED;
  }
}

import { AttachTerminal } from "./attach-terminal";
import { OperatorProvider } from "./wardyn/operator-context";
import { RUN_COCKPIT } from "./wardyn/copy";
import { runs } from "../lib/api/runs";
import { HttpError } from "../lib/api/core";

// The attach-mode control frame the daemon sends as a TEXT frame on EVERY
// connect (internal/api/attach_holder.go), read_only=false included.
function attachModeFrame(readOnly: boolean, principal: string) {
  return JSON.stringify({
    type: "attach-mode",
    read_only: readOnly,
    holder: {
      held: true,
      principal,
      since: "2026-08-16T12:00:00Z",
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

  // A11y (WCAG 2.1.2 no keyboard trap): fullscreen must be escapable via a
  // plain Escape keypress, not just the mouse-only toggle button. Before the
  // fix, xterm's PTY forwarding swallowed Escape entirely.
  it("Escape exits fullscreen instead of being swallowed by the PTY", async () => {
    const { getByLabelText, queryByLabelText } = render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());

    act(() => getByLabelText("Fullscreen").click());
    expect(queryByLabelText("Exit fullscreen")).not.toBeNull();

    act(() => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
    });

    expect(queryByLabelText("Exit fullscreen")).toBeNull();
    expect(queryByLabelText("Fullscreen")).not.toBeNull();
  });
});

// Role-aware console: attach is operator-only on BOTH lanes (ticket mint and
// the WS itself — see http.go). A viewer must never even open the socket.
describe("AttachTerminal — role-aware attach", () => {
  beforeEach(() => {
    FakeWebSocket.instances = [];
    vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
    if (!("fonts" in document)) {
      Object.defineProperty(document, "fonts", {
        configurable: true,
        value: { ready: Promise.resolve() },
      });
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
  afterEach(() => vi.unstubAllGlobals());

  it("viewer: never opens a socket, and shows the reason instead of a raw error", async () => {
    render(
      <OperatorProvider operator={false}>
        <AttachTerminal runId="run_1" />
      </OperatorProvider>,
    );
    expect(await screen.findByText(/attaching to a live sandbox requires the operator role/i)).toBeInTheDocument();
    expect(FakeWebSocket.instances).toHaveLength(0);
  });

  it("operator (today's default, no provider needed): connects normally", () => {
    render(<AttachTerminal runId="run_1" />);
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  // Regression (W17-S1-1): a member who owns the run must be able to attach —
  // the server's ticket lane is owner-or-admin (getRunAuthorized), not
  // operator-only, so the UI's gate and connection lane must match it.
  it("member who owns this run: mints a ticket (owner-or-admin lane) and opens the WS", async () => {
    const attachTicket = vi.mocked(runs.attachTicket);
    attachTicket.mockReset();
    attachTicket.mockResolvedValueOnce("tic_abc");
    render(
      <OperatorProvider operator={false} principal="alice@example.com">
        <AttachTerminal runId="run_1" createdBy="alice@example.com" />
      </OperatorProvider>,
    );
    // Ticket mint is async (a real POST in prod); wait for it to land.
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    expect(attachTicket).toHaveBeenCalledWith("run_1");
    expect(FakeWebSocket.instances[0].url).toContain("ticket=tic_abc");
  });

  // A non-owning member (createdBy set, but not to this principal) is still
  // refused — createdBy alone must not blanket-bypass the gate.
  it("member who does NOT own this run: still refused, no ticket minted", async () => {
    const attachTicket = vi.mocked(runs.attachTicket);
    attachTicket.mockReset();
    render(
      <OperatorProvider operator={false} principal="alice@example.com">
        <AttachTerminal runId="run_1" createdBy="bob@example.com" />
      </OperatorProvider>,
    );
    expect(await screen.findByText(/attaching to a live sandbox requires the operator role/i)).toBeInTheDocument();
    expect(attachTicket).not.toHaveBeenCalled();
    expect(FakeWebSocket.instances).toHaveLength(0);
  });

// The tmux shared-window clamp. A second client attaching SMALLER (a
// `wardyn attach` from another terminal) makes tmux fill this client's extra
// area with `·`, and the filler OUTLIVES that client: the browser's own size
// never changed, so a same-size resize frame is a no-op tmux ignores. Measured
// against a live run: 0 dots, 1001 while attached, 1001 after it detached, 0
// once the viewport actually changed size.
describe("AttachTerminal — forced refit clears a stale tmux clamp", () => {
  beforeEach(() => {
    FakeWebSocket.instances = [];
    vi.stubGlobal("WebSocket", FakeWebSocket as unknown as typeof WebSocket);
    if (!("fonts" in document)) {
      Object.defineProperty(document, "fonts", { value: { ready: Promise.resolve() }, configurable: true });
    }
  });

  it("Redraw sends a SMALLER size before the real one — a same-size frame would be ignored", async () => {
    const { getByLabelText } = render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].open());
    const ws = FakeWebSocket.instances[0];
    ws.sent.length = 0;

    act(() => getByLabelText("Redraw terminal").click());

    const resizes = ws.sent
      .filter((m) => typeof m === "string" && m.includes('"resize"'))
      .map((m) => JSON.parse(m as string));
    expect(resizes.length).toBeGreaterThanOrEqual(2);
    const [nudge, real] = resizes.slice(-2);
    expect(nudge.cols).toBe(real.cols - 1);
    expect(nudge.rows).toBe(real.rows);
  });
});
});

// Attach is a SHARED tmux session. The daemon admits a second client READ-ONLY
// and says so in an attach-mode TEXT frame on every connect; a take-over closes
// the displaced client with 1008 + `taken over by <principal>`. The regression
// that matters most is in here: a displaced client that RECONNECTS lands right
// back on top of the new holder — the two-clients-fighting state the holder
// registry exists to end.
describe("AttachTerminal — attach mode, displacement, take-over", () => {
  beforeEach(() => {
    stubTerminalEnv();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("read_only:true puts the terminal in the spectator state and names the holder", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(true, "alice@example.com")));

    expect(screen.getByText(RUN_COCKPIT.heldBy("alice@example.com"))).toBeInTheDocument();
    expect(screen.getByText(RUN_COCKPIT.watchingReadOnly)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: RUN_COCKPIT.takeOver })).toBeInTheDocument();
  });

  it("read_only:false leaves today's driving behaviour untouched", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(false, "me@example.com")));

    expect(screen.getByText(RUN_COCKPIT.driving)).toBeInTheDocument();
    expect(screen.queryByText(RUN_COCKPIT.watchingReadOnly)).toBeNull();
    expect(screen.queryByRole("button", { name: RUN_COCKPIT.takeOver })).toBeNull();
    // Still the writer's own socket — nothing was torn down by the frame.
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  // THE regression. Contrast with "reconnects (opens a new socket) after an
  // UNEXPECTED close" above: same non-1000 code shape, opposite required
  // behaviour, because this close means someone else is now typing.
  it("a close with code 1008 does NOT open a new socket", async () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());

    act(() => ws.drop(1008, "taken over by bob@example.com"));
    await act(() => vi.advanceTimersByTimeAsync(30000));

    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it("a close with code 1008 shows the taken-over state", async () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(false, "me@example.com")));

    act(() => ws.drop(1008, "taken over by bob@example.com"));

    expect(screen.getByText(RUN_COCKPIT.heldBy("bob@example.com"))).toBeInTheDocument();
    // The header must stop claiming we drive a PTY we no longer hold.
    expect(screen.queryByText(RUN_COCKPIT.driving)).toBeNull();
    expect(screen.getByRole("button", { name: RUN_COCKPIT.takeOver })).toBeInTheDocument();
  });

  // Only the reason survived (a proxy rewrote the code): still a displacement,
  // still no reconnect.
  it("the `taken over by ` reason alone is enough to stop the reconnect", async () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());

    act(() => ws.drop(1006, "taken over by bob@example.com"));
    await act(() => vi.advanceTimersByTimeAsync(30000));

    expect(FakeWebSocket.instances).toHaveLength(1);
  });
});

// Take-over EVICTS, it does not promote: after the POST returns, this client's
// socket is STILL the read-only one, so it has to reconnect to claim the writer
// slot (handleAttachTakeover's own note). Real timers — the confirm dialog is
// Radix, driven the same way live-approvals.test.tsx drives its deny confirm.
describe("AttachTerminal — take-over reconnects to claim the writer slot", () => {
  beforeEach(stubTerminalEnv);
  afterEach(() => vi.unstubAllGlobals());

  it("confirm → takeoverAttach → a NEW socket is opened", async () => {
    const takeover = vi.mocked(runs.takeoverAttach);
    takeover.mockReset();
    takeover.mockResolvedValue(undefined);

    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(true, "alice@example.com")));

    fireEvent.click(screen.getByRole("button", { name: RUN_COCKPIT.takeOver }));
    const dialog = await screen.findByRole("alertdialog");
    // The confirm names the human whose session this ends.
    expect(within(dialog).getByText(RUN_COCKPIT.takeOverConfirm("alice@example.com"))).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: RUN_COCKPIT.takeOver }));

    await waitFor(() => expect(takeover).toHaveBeenCalledWith("run_1"));
    // The evict-then-reconnect: a second socket, opened by us, not by backoff.
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
  });

  it("a failed take-over surfaces the server's reason and does NOT reconnect", async () => {
    const takeover = vi.mocked(runs.takeoverAttach);
    takeover.mockReset();
    // A GENUINE failure (500), not the 409 "nobody is attached" — that one now
    // means the holder left while we watched, and reclaiming is the correct
    // response to it (see the test below).
    takeover.mockRejectedValue(new HttpError(500, "save failed on the daemon"));

    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(true, "alice@example.com")));

    fireEvent.click(screen.getByRole("button", { name: RUN_COCKPIT.takeOver }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: RUN_COCKPIT.takeOver }));

    expect(await screen.findByText(/save failed on the daemon/)).toBeInTheDocument();
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  // A read-only observer whose holder LEAVES was a dead end: the attach-mode
  // frame is sent once, at connect, so nothing ever told the observer it could
  // now drive. Take-over then 409'd ("nobody is attached"), the reason landed
  // in the footer, and the terminal — the hero of this whole screen — stayed
  // permanently convinced it was read-only until a full page reload.
  it("a 409 take-over means the holder left, so it reclaims instead of erroring", async () => {
    const takeover = vi.mocked(runs.takeoverAttach);
    takeover.mockReset();
    takeover.mockRejectedValue(new HttpError(409, "nobody is attached to this run; nothing to take over"));

    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(true, "alice@example.com")));

    fireEvent.click(screen.getByRole("button", { name: RUN_COCKPIT.takeOver }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: RUN_COCKPIT.takeOver }));

    // Reconnects to claim the now-free writer slot...
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    // ...and does NOT show the 409 text as an error the operator cannot act on.
    expect(screen.queryByText(/nothing to take over/)).toBeNull();
  });
});
