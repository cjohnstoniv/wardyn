/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * Pins the corpus of attach-terminal.tsx behaviour that attach-terminal.test.tsx
 * never exercised (issue #134): paste coalescing (native vs Ctrl+V, and a large
 * paste staying one frame), the read-only label's pointer-events-none guard, an
 * in-place promotion forcing exactly one refit + one focus + clearing the
 * footer, and the close codes (1008 and 4401 never reconnect, 1006 does and drops
 * `mode` first).
 *
 * attach-terminal.test.tsx exports nothing (and is itself close to the size
 * gate's cap — see the doc atop attach-terminal.tsx), so this file re-declares
 * the same minimal harness shape — fake WebSocket, xterm mocks, the attach-mode
 * frame builder — rather than editing that file to add exports.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, act, screen, fireEvent } from "@testing-library/react";

const focusCalls = { n: 0 };
// Captured from attachCustomKeyEventHandler so the Ctrl+V path can be driven
// the same way a real keydown would, without a real DOM key event round trip.
let keyHandler: ((e: KeyboardEvent) => boolean) | null = null;

vi.mock("@xterm/xterm", () => {
  class Terminal {
    cols = 80;
    rows = 24;
    unicode = { activeVersion: "6" };
    loadAddon() {}
    open() {}
    write() {}
    writeln() {}
    resize() {}
    focus() {
      focusCalls.n++;
    }
    onData() {
      return { dispose() {} };
    }
    onBinary() {
      return { dispose() {} };
    }
    modes = { mouseTrackingMode: "none" };
    attachCustomWheelEventHandler() {}
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
// Force SSO mode (no admin token) so the component opens a WebSocket directly.
vi.mock("../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api/core")>()),
  getToken: () => null,
}));
vi.mock("../lib/api/runs", () => ({ runs: { attachTicket: vi.fn(async () => "ticket"), takeoverAttach: vi.fn() } }));

// Fake WebSocket — same shape as attach-terminal.test.tsx's own (that file has
// no exports; see the file doc above for why this isn't a shared import).
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
  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.();
  }
  drop(code: number, reason = "") {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code, reason });
  }
  send(data: unknown) {
    this.sent.push(data);
  }
  message(data: unknown) {
    this.onmessage?.({ data });
  }
  close() {}
}

import { AttachTerminal } from "./attach-terminal";
import { RUN_COCKPIT, TERMINAL } from "./wardyn/copy";
import { HttpError } from "../lib/api/core";
import { OperatorProvider } from "./wardyn/operator-context";
import { runs } from "../lib/api/runs";
import { ATTACH_CAP_REASON } from "../lib/run-entry";
import { aheadByHours } from "../lib/test-clock";
import { noteSessionExpiry } from "../lib/api/session-activity";

// The attach-mode control frame the daemon sends as a TEXT frame on every
// connect (internal/api/attach_holder.go).
function attachModeFrame(readOnly: boolean, principal: string) {
  return JSON.stringify({
    type: "attach-mode",
    read_only: readOnly,
    holder: { held: true, principal, since: aheadByHours(-1), cols: 132, rows: 50, source: "web" },
  });
}

// ArrayBuffer.isView, not `instanceof Uint8Array` — TextEncoder's output and
// this check can cross a jsdom/Node realm boundary where prototype identity
// does not hold, and a failed instanceof silently stringifies the byte array
// (as its comma-joined char codes) instead of throwing.
function isBinaryFrame(frame: unknown): frame is ArrayBufferView {
  return ArrayBuffer.isView(frame);
}

function decode(frame: unknown): string {
  return isBinaryFrame(frame) ? new TextDecoder().decode(frame as Uint8Array) : String(frame);
}

function pasteFrames(sent: unknown[]): string[] {
  return sent.filter(isBinaryFrame).map(decode);
}

function resizeFrames(sent: unknown[]): Array<{ cols: number; rows: number }> {
  return sent
    .map((s) => {
      if (typeof s !== "string") return null;
      try {
        return JSON.parse(s);
      } catch {
        return null;
      }
    })
    .filter((m): m is { type: string; cols: number; rows: number } => m?.type === "resize");
}

function stubTerminalEnv() {
  FakeWebSocket.instances = [];
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
}

describe("AttachTerminal — paste coalescing", () => {
  beforeEach(() => {
    stubTerminalEnv();
    keyHandler = null;
  });
  afterEach(() => vi.unstubAllGlobals());

  it("a native paste delivers the raw text once, and is not double-sent via the Ctrl+V path", async () => {
    const { container } = render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    const target = container.querySelector("[aria-description]")!;

    fireEvent.paste(target, { clipboardData: { getData: () => "pasted text" } });

    // A browser can fire a native paste event AND the Ctrl+V keydown for one
    // physical paste; the component's own coalescing window (sendPaste, 120ms)
    // must swallow the second one.
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { readText: vi.fn().mockResolvedValue("pasted text") },
    });
    expect(keyHandler).not.toBeNull();
    await act(async () => {
      keyHandler!({ type: "keydown", ctrlKey: true, shiftKey: false, altKey: false, metaKey: false, key: "v" } as unknown as KeyboardEvent);
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(pasteFrames(ws.sent)).toEqual(["pasted text"]);
  });

  it("a paste over 32 KiB is sent as one frame, not many", () => {
    const { container } = render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    const target = container.querySelector("[aria-description]")!;
    const big = "y".repeat(32 * 1024 + 1);

    fireEvent.paste(target, { clipboardData: { getData: () => big } });

    const frames = pasteFrames(ws.sent);
    expect(frames.length).toBe(1);
    expect(frames[0].length).toBe(big.length);
  });
});

describe("AttachTerminal — read-only label", () => {
  beforeEach(stubTerminalEnv);
  afterEach(() => vi.unstubAllGlobals());

  it("a read-only socket still renders the pointer-events-none label", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(true, "alice@example.com")));

    const label = screen.getByText(RUN_COCKPIT.watchingReadOnly);
    expect(label.closest(".pointer-events-none")).not.toBeNull();
  });
});

// The regression the issue names as the one that matters most: the promotion
// branch (refit(true) on a same-socket read_only true→false flip) ships today
// but the server has never sent the frame that exercises it, so nothing pinned
// it. Deleting that branch must turn this test red — see the delete/restore
// demonstration in the PR description.
describe("AttachTerminal — in-place promotion on the same socket", () => {
  beforeEach(() => {
    stubTerminalEnv();
    focusCalls.n = 0;
  });
  afterEach(() => vi.unstubAllGlobals());

  it("forces exactly one refit(true) and one term.focus(), and clears the footer", () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(true, "alice@example.com")));
    expect(screen.getByRole("button", { name: RUN_COCKPIT.takeOver })).toBeInTheDocument();

    ws.sent = [];
    focusCalls.n = 0;

    act(() => ws.message(attachModeFrame(false, "me@example.com")));

    // refit(true) sends a one-column-smaller nudge, then the real size — see
    // refit's own doc in attach-terminal.tsx. Exactly one call = exactly one
    // such pair; more or fewer means the promotion path fired zero or twice.
    const resizes = resizeFrames(ws.sent);
    expect(resizes.length).toBe(2);
    expect(resizes[0].cols).toBe(resizes[1].cols - 1);
    expect(resizes[0].rows).toBe(resizes[1].rows);

    expect(focusCalls.n).toBe(1);

    // read_only is now false and we were never displaced: the footer (holder
    // hint + Take-over button) must be gone.
    expect(screen.queryByRole("button", { name: RUN_COCKPIT.takeOver })).toBeNull();
    expect(screen.queryByText(RUN_COCKPIT.heldHint)).toBeNull();
  });
});

describe("AttachTerminal — close codes", () => {
  beforeEach(() => {
    stubTerminalEnv();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("a 1008 close (taken over) does NOT reconnect", async () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());

    act(() => ws.drop(1008, "taken over by bob@example.com"));
    await act(() => vi.advanceTimersByTimeAsync(30000));

    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it("a 4401 close (session ended) does NOT reconnect, and drops mode", async () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(false, "me@example.com")));
    expect(screen.getByText(RUN_COCKPIT.driving)).toBeInTheDocument();

    act(() => ws.drop(4401, "session ended"));
    expect(screen.queryByText(RUN_COCKPIT.driving)).toBeNull();
    await act(() => vi.advanceTimersByTimeAsync(30000));

    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it("a 4401 close reconnects once when the session was renewed past the expiry the socket opened under", async () => {
    noteSessionExpiry(new Date(Date.now() + 10 * 60_000).toISOString());
    try {
      render(<AttachTerminal runId="run_1" />);
      const ws = FakeWebSocket.instances[0];
      act(() => ws.open());
      // Renewed by the console's other requests; the socket still ends at the expiry it opened under.
      noteSessionExpiry(new Date(Date.now() + 60 * 60_000).toISOString());
      act(() => ws.drop(4401, "session ended"));
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(FakeWebSocket.instances).toHaveLength(2);

      // The reopened socket ending with no renewal since is final, as before.
      const again = FakeWebSocket.instances[1];
      act(() => again.open());
      act(() => again.drop(4401, "session ended"));
      await act(() => vi.advanceTimersByTimeAsync(30000));
      expect(FakeWebSocket.instances).toHaveLength(2);
    } finally {
      noteSessionExpiry(new Date(0).toISOString()); // an expiry in the past: no renewal left for later tests
    }
  });

  it("a 1006 close DOES reconnect, and drops mode first", async () => {
    render(<AttachTerminal runId="run_1" />);
    const ws = FakeWebSocket.instances[0];
    act(() => ws.open());
    act(() => ws.message(attachModeFrame(false, "me@example.com")));
    expect(screen.getByText(RUN_COCKPIT.driving)).toBeInTheDocument();

    act(() => ws.drop(1006, "abnormal"));
    // mode is cleared synchronously, in the same branch that schedules the
    // reconnect — before any timer fires, let alone a new socket opens.
    expect(screen.queryByText(RUN_COCKPIT.driving)).toBeNull();

    await act(() => vi.advanceTimersByTimeAsync(2000));
    expect(FakeWebSocket.instances.length).toBeGreaterThanOrEqual(2);
  });
});

describe("AttachTerminal — a handshake refused on the direct lane", () => {
  beforeEach(() => {
    stubTerminalEnv();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  const CAP = "too many open terminal sessions for this run (max 8) — close one and retry";

  it("says the run is at its session cap, in the server's sentence, and does not keep retrying", async () => {
    vi.mocked(runs.attachTicket).mockRejectedValueOnce(new HttpError(503, CAP, ATTACH_CAP_REASON));
    render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].drop(1006, ""));
    await act(() => vi.advanceTimersByTimeAsync(30000));
    expect(screen.getByText(CAP)).toBeInTheDocument();
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  // Spend the retry budget so Reconnect shows: every direct handshake fails and the mint finds no cap.
  async function spendBudget() {
    render(<AttachTerminal runId="run_1" />);
    for (let i = 0; i < 8; i++) {
      act(() => FakeWebSocket.instances[FakeWebSocket.instances.length - 1].drop(1006, ""));
      await act(() => vi.advanceTimersByTimeAsync(6000));
    }
    return screen.getByRole("button", { name: TERMINAL.RECONNECT });
  }

  it("one Reconnect opens one socket, even when an earlier attempt's ticket mint is still out", async () => {
    const button = await spendBudget();
    const before = FakeWebSocket.instances.length;
    let resolveMint: (t: string) => void = () => {};
    vi.mocked(runs.attachTicket).mockImplementationOnce(() => new Promise<string>((r) => (resolveMint = r)));
    // Both clicks land in one act, before the button re-renders away: the first attempt fails and leaves its
    // mint out when the second Reconnect lands.
    act(() => {
      button.click();
      FakeWebSocket.instances[FakeWebSocket.instances.length - 1].drop(1006, "");
      button.click();
    });
    expect(FakeWebSocket.instances).toHaveLength(before + 2);
    resolveMint("ticket");
    await act(() => vi.advanceTimersByTimeAsync(30000));
    expect(FakeWebSocket.instances).toHaveLength(before + 2);
  });

  it("one Reconnect opens one socket, even when a backoff timer is still pending", async () => {
    const button = await spendBudget();
    const before = FakeWebSocket.instances.length;
    act(() => {
      button.click();
      const sock = FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
      sock.open();
      sock.drop(1006, ""); // it had opened, so this schedules a backoff retry
      button.click();
    });
    expect(FakeWebSocket.instances).toHaveLength(before + 2);
    await act(() => vi.advanceTimersByTimeAsync(30000));
    expect(FakeWebSocket.instances).toHaveLength(before + 2);
  });

  it("retries as before when the ticket mint finds no cap", async () => {
    render(<AttachTerminal runId="run_1" />);
    act(() => FakeWebSocket.instances[0].drop(1006, ""));
    await act(() => vi.advanceTimersByTimeAsync(2000));
    expect(screen.queryByText(CAP)).toBeNull();
    expect(FakeWebSocket.instances.length).toBeGreaterThanOrEqual(2);
  });
});

// A member (no cookie lane) mints a ticket on every connect. A mint that fails because the network or the daemon is
// down is the same drop a closed socket is: it spends the reconnect budget and ends on Reconnect, not on a red
// error with no way back but a reload.
describe("AttachTerminal — a ticket mint that fails on the ticket lane", () => {
  beforeEach(() => {
    stubTerminalEnv();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  const member = () =>
    render(
      <OperatorProvider operator={false} principal="alice@example.com">
        <AttachTerminal runId="run_1" />
      </OperatorProvider>,
    );

  it.each([
    ["no network", () => new TypeError("Failed to fetch")],
    ["a 503 while the daemon restarts", () => new HttpError(503, "unavailable")],
  ])("%s: retries within the budget and ends on Reconnect", async (_name, failure) => {
    vi.mocked(runs.attachTicket).mockReset().mockRejectedValue(failure());
    member();
    await act(() => vi.advanceTimersByTimeAsync(60000));
    expect(vi.mocked(runs.attachTicket).mock.calls.length).toBeGreaterThan(1);
    expect(screen.queryByText(/Could not mint an attach ticket/)).toBeNull();
    expect(screen.getByRole("button", { name: TERMINAL.RECONNECT })).toBeInTheDocument();
  });

  it("recovers when the mint answers again", async () => {
    vi.mocked(runs.attachTicket).mockReset().mockRejectedValueOnce(new TypeError("Failed to fetch")).mockResolvedValue("ticket");
    member();
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(FakeWebSocket.instances.length).toBeGreaterThan(0);
    expect(FakeWebSocket.instances[FakeWebSocket.instances.length - 1].url).toContain("ticket=ticket");
  });

  it("a refusal that will not change still ends at once", async () => {
    vi.mocked(runs.attachTicket).mockReset().mockRejectedValue(new HttpError(404, "run not found"));
    member();
    await act(() => vi.advanceTimersByTimeAsync(60000));
    expect(vi.mocked(runs.attachTicket)).toHaveBeenCalledTimes(1);
    expect(screen.getByText(/Could not mint an attach ticket/)).toBeInTheDocument();
  });
});
