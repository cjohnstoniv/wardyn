/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DESKTOP } from "../app/components/wardyn/copy/desktop";
import { DesktopViewer, readBoot, WINDOW_OPEN, WINDOW_OPEN_LEGACY_REASON, type Boot, type DesktopSocket } from "./viewer";

const OPEN: Boot = { state: "open", expires: 0, rfb: "/r/x/__desktop/__rfb", close: "/r/x/__desktop/__close" };

class FakeSocket implements DesktopSocket {
  binaryType: BinaryType = "blob";
  onopen: DesktopSocket["onopen"] = null;
  onmessage: DesktopSocket["onmessage"] = null;
  onclose: DesktopSocket["onclose"] = null;
  sent: Uint8Array[] = [];
  closed = false;
  send(b: Uint8Array) {
    this.sent.push(b);
  }
  close() {
    this.closed = true;
  }
  serve(bytes: number[]) {
    act(() => this.onmessage?.({ data: Uint8Array.from(bytes).buffer } as MessageEvent));
  }
  end(code: number, reason = "") {
    act(() => this.onclose?.({ code, reason } as CloseEvent));
  }
  opened() {
    act(() => this.onopen?.({} as Event));
  }
}

const HANDSHAKE = [...new TextEncoder().encode("RFB 003.008\n"), 1, 1, 0, 0, 0, 0, 0, 2, 0, 1, ...new Array(16).fill(0), 0, 0, 0, 0];

function open(small = false) {
  const sockets: FakeSocket[] = [];
  const connect = vi.fn(() => {
    const s = new FakeSocket();
    sockets.push(s);
    return s;
  });
  render(<DesktopViewer boot={OPEN} connect={connect} small={small} />);
  return { sockets, connect };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("DesktopViewer", () => {
  it("reads what the gateway wrote on the page", () => {
    const el = document.createElement("html");
    el.dataset.wardynDesktop = JSON.stringify(OPEN);
    expect(readBoot(el)).toEqual(OPEN);
    el.dataset.wardynDesktop = "";
    expect(readBoot(el)).toBeNull();
  });

  it("starts on the booting frame, says Connected once the desktop answers, and takes input", () => {
    const { sockets } = open();
    expect(screen.getByRole("status")).toHaveTextContent(DESKTOP.starting);
    expect(screen.getByText(DESKTOP.startingBody)).toBeInTheDocument();
    sockets[0].serve(HANDSHAKE);
    expect(screen.getByText(DESKTOP.connected)).toBeInTheDocument();
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.getByTestId("desktop-canvas")).toHaveAttribute("tabindex", "0");
    expect(screen.getByRole("button", { name: DESKTOP.fit })).toHaveAttribute("aria-pressed", "true");
    act(() => screen.getByRole("button", { name: DESKTOP.actualSize }).click());
    expect(screen.getByRole("button", { name: DESKTOP.actualSize })).toHaveAttribute("aria-pressed", "true");
  });

  it("names a macOS desktop that is still starting, from the run's machine kind in the boot data", () => {
    const el = document.createElement("html");
    el.dataset.wardynDesktop = JSON.stringify({ ...OPEN, machine_kind: "macos_vm" });
    const boot = readBoot(el);
    expect(boot).toEqual({ ...OPEN, machine_kind: "macos_vm" });
    render(<DesktopViewer boot={boot!} connect={() => new FakeSocket()} />);
    expect(screen.getByRole("status")).toHaveTextContent(DESKTOP.startingMacOS);
    expect(screen.getByText(DESKTOP.startingBody)).toBeInTheDocument();
    cleanup();
    render(<DesktopViewer boot={{ ...OPEN, machine_kind: "linux_vm" }} connect={() => new FakeSocket()} />);
    expect(screen.getByRole("status")).toHaveTextContent(DESKTOP.starting);
  });

  it("retries a desktop that is not reachable yet, on its own", () => {
    vi.useFakeTimers();
    const { sockets, connect } = open();
    sockets[0].opened();
    sockets[0].end(4004);
    expect(screen.getByRole("status")).toHaveTextContent(DESKTOP.starting);
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(connect).toHaveBeenCalledTimes(2);
  });

  it("asks the gateway why a socket never opened: a 403 stops the retries and says to open the desktop again", async () => {
    vi.useFakeTimers();
    const status = { code: 403 };
    const fetchMock = vi.fn(async () => new Response(null, { status: status.code }));
    vi.stubGlobal("fetch", fetchMock);
    const a = open();
    a.sockets[0].end(1006);
    await act(async () => {});
    expect(fetchMock).toHaveBeenCalledWith(OPEN.state === "open" ? OPEN.rfb : "", { credentials: "same-origin" });
    expect(screen.getByText(DESKTOP.sessionEnded)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.refusedBody)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: DESKTOP.close })).toBeNull();
    act(() => {
      vi.advanceTimersByTime(20_000);
    });
    expect(a.connect).toHaveBeenCalledTimes(1);
    cleanup();

    status.code = 426; // the session is good: the plain GET is just not an upgrade, so it retries
    const b = open();
    b.sockets[0].end(1006);
    await act(async () => {});
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(b.connect).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("status")).toHaveTextContent(DESKTOP.starting);
  });

  it("says the session ended at its bound, or the run ended, and does not retry", () => {
    vi.useFakeTimers();
    const a = open();
    a.sockets[0].end(4001);
    expect(screen.getByText(DESKTOP.sessionEnded)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.sessionEndedBody)).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(20_000);
    });
    expect(a.connect).toHaveBeenCalledTimes(1);
    cleanup();
    const b = open();
    b.sockets[0].end(4002);
    expect(screen.getByText(DESKTOP.runEnded)).toBeInTheDocument();
  });

  it("Close ends the session at the gateway", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const closeSpy = vi.spyOn(window, "close").mockImplementation(() => {});
    const { sockets } = open();
    sockets[0].serve(HANDSHAKE);
    await act(async () => screen.getByRole("button", { name: DESKTOP.close }).click());
    expect(fetchMock).toHaveBeenCalledWith(OPEN.state === "open" ? OPEN.close : "", { method: "POST", credentials: "same-origin" });
    expect(sockets[0].closed).toBe(true);
    expect(screen.getByText(DESKTOP.sessionEnded)).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP.sessionEndedBody)).toBeNull();
    closeSpy.mockRestore();
  });

  it("a Close the gateway did not store keeps the desktop open, says so, and can be tried again", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 503 }));
    vi.stubGlobal("fetch", fetchMock);
    const closeSpy = vi.spyOn(window, "close").mockImplementation(() => {});
    const { sockets } = open();
    sockets[0].serve(HANDSHAKE);
    await act(async () => screen.getByRole("button", { name: DESKTOP.close }).click());
    expect(screen.getByRole("alert")).toHaveTextContent("Close didn't go through, so this desktop is still open. Try again.");
    expect(DESKTOP.closeFailed).toBe("Close didn't go through, so this desktop is still open. Try again.");
    expect(sockets[0].closed).toBe(false);
    expect(closeSpy).not.toHaveBeenCalled();
    expect(screen.getByText(DESKTOP.connected)).toBeInTheDocument();

    fetchMock.mockImplementation(async () => new Response(null, { status: 204 }));
    await act(async () => screen.getByRole("button", { name: DESKTOP.close }).click());
    expect(sockets[0].closed).toBe(true);
    expect(closeSpy).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("alert")).toBeNull();
    closeSpy.mockRestore();
  });

  it("a socket that drops while a Close is in flight reconnects, and a Close that then fails says so", async () => {
    vi.useFakeTimers();
    let answer: (r: Response) => void = () => {};
    const fetchMock = vi.fn(() => new Promise<Response>((resolve) => (answer = resolve)));
    vi.stubGlobal("fetch", fetchMock);
    const closeSpy = vi.spyOn(window, "close").mockImplementation(() => {});
    const { sockets, connect } = open();
    sockets[0].opened();
    sockets[0].serve(HANDSHAKE);
    act(() => screen.getByRole("button", { name: DESKTOP.close }).click());
    sockets[0].end(1006);
    await act(async () => answer(new Response(null, { status: 503 })));
    expect(screen.getByRole("alert")).toHaveTextContent("Close didn't go through, so this desktop is still open. Try again.");
    expect(screen.getByRole("button", { name: DESKTOP.close })).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(connect).toHaveBeenCalledTimes(2);
    expect(closeSpy).not.toHaveBeenCalled();
    closeSpy.mockRestore();
  });

  it("opens view only on a small screen, and says so", () => {
    const { sockets } = open(true);
    sockets[0].serve(HANDSHAKE);
    expect(screen.getByText(DESKTOP.phoneTitle)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.phoneBody)).toBeInTheDocument();
    expect(screen.getByTestId("desktop-canvas")).toHaveAttribute("tabindex", "-1");
  });

  it("a view-only session says so on any screen, takes no input, and doesn't send people to a larger screen", () => {
    const sockets: FakeSocket[] = [];
    const connect = vi.fn(() => {
      const s = new FakeSocket();
      sockets.push(s);
      return s;
    });
    render(<DesktopViewer boot={{ ...OPEN, machine_kind: "macos_vm", view_only: true }} connect={connect} small={false} />);
    sockets[0].serve(HANDSHAKE);
    expect(screen.getByText(DESKTOP.viewOnlyTitle)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.viewOnlyBody)).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP.backgroundWatchBody)).toBeNull();
    expect(screen.queryByText(DESKTOP.phoneBody)).toBeNull();
    const canvas = screen.getByTestId("desktop-canvas");
    expect(canvas).toHaveAttribute("tabindex", "-1");
    const sent = sockets[0].sent.length;
    fireEvent.pointerMove(canvas, { clientX: 5, clientY: 5 });
    fireEvent.keyDown(canvas, { key: "a" });
    expect(sockets[0].sent.length).toBe(sent);
  });

  it("a used link gets the refusal card with the gateway's words", () => {
    render(<DesktopViewer boot={{ state: "refused", said: "Invalid, expired, or already-used attach ticket" }} />);
    const card = screen.getByRole("region", { name: DESKTOP.linkBlocked });
    for (const s of [DESKTOP.linkRefused, DESKTOP.linkBody, DESKTOP.whyWallBody, DESKTOP.whySetByBody, DESKTOP.whyEnforcedBody, DESKTOP.whyCanChangeBody, DESKTOP.gatewaySaid])
      expect(within(card).getByText(s)).toBeInTheDocument();
    expect(within(card).getByTestId("desktop-refused-said")).toHaveTextContent("Invalid, expired, or already-used attach ticket");
  });

  it("a background watch says so, sends no input, and a re-check's end has its own words and doesn't retry", () => {
    vi.useFakeTimers();
    const sockets: FakeSocket[] = [];
    const connect = vi.fn(() => {
      const s = new FakeSocket();
      sockets.push(s);
      return s;
    });
    render(<DesktopViewer boot={{ ...OPEN, machine_kind: "macos_vm", view_only: true, background: true }} connect={connect} small={false} />);
    sockets[0].opened();
    sockets[0].serve(HANDSHAKE);
    expect(screen.getByText(DESKTOP.viewOnlyTitle)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.backgroundWatchBody)).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP.viewOnlyBody)).toBeNull();
    const canvas = screen.getByTestId("desktop-canvas");
    expect(canvas).toHaveAttribute("tabindex", "-1");
    const sent = sockets[0].sent.length;
    fireEvent.pointerMove(canvas, { clientX: 5, clientY: 5 });
    expect(sockets[0].sent.length).toBe(sent);

    sockets[0].end(4005);
    expect(screen.getByText(DESKTOP.sessionEnded)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.watchEndedBody)).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP.sessionEndedBody)).toBeNull();
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(connect).toHaveBeenCalledTimes(1);
  });

  // The input messages (RFB KeyEvent 4, PointerEvent 5) a socket was sent after `from` messages.
  const inputs = (sock: FakeSocket, from: number) =>
    sock.sent.slice(from).filter((m) => m[0] === 4 || m[0] === 5).map((m) => (m[0] === 4 ? `key ${m[1] ? "down" : "up"} ${new DataView(m.buffer).getUint32(4).toString(16)}` : `pointer ${m[1]}`));

  it("releases held keys when the canvas, the window or the tab loses focus", () => {
    const { sockets } = open();
    sockets[0].serve(HANDSHAKE);
    const canvas = screen.getByTestId("desktop-canvas");
    for (const leave of [() => fireEvent.blur(canvas), () => fireEvent.blur(window), () => {
      vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
      fireEvent(document, new Event("visibilitychange"));
    }]) {
      const from = sockets[0].sent.length;
      fireEvent.keyDown(canvas, { key: "Alt", code: "AltLeft", location: 1 });
      fireEvent.keyDown(canvas, { key: "Control", code: "ControlLeft", location: 1 });
      leave();
      expect(inputs(sockets[0], from)).toEqual(["key down ffe9", "key down ffe3", "key up ffe9", "key up ffe3"]);
      leave(); // nothing is held any more, so nothing more is sent
      expect(inputs(sockets[0], from)).toHaveLength(4);
    }
  });

  it("says a window opened on the Mac, from the new close code or today's 4001 reason, and does not retry", () => {
    vi.useFakeTimers();
    expect(WINDOW_OPEN).toBe(4006);
    for (const [code, reason] of [[4006, ""], [4001, `${WINDOW_OPEN_LEGACY_REASON}, so this desktop only watches now. Reload to keep watching.`]] as const) {
      const a = open();
      a.sockets[0].end(code, reason);
      expect(screen.getByText(DESKTOP.windowOpenTitle)).toBeInTheDocument();
      expect(screen.getByText(DESKTOP.windowOpenBody)).toBeInTheDocument();
      expect(screen.queryByText(DESKTOP.sessionEnded)).toBeNull();
      act(() => {
        vi.advanceTimersByTime(20_000);
      });
      expect(a.connect).toHaveBeenCalledTimes(1);
      cleanup();
    }
    open().sockets[0].end(4001, "something else");
    expect(screen.getByText(DESKTOP.sessionEnded)).toBeInTheDocument();
  });

  it("stops on what the desktop sent, says the viewer's reason, and dials again only on Try again", () => {
    vi.useFakeTimers();
    const { sockets, connect } = open();
    sockets[0].opened();
    sockets[0].serve([...new TextEncoder().encode("RFB 003.008\n"), 1, 2]); // offers a password only
    expect(sockets[0].closed).toBe(true);
    sockets[0].end(1005);
    act(() => {
      vi.advanceTimersByTime(20_000);
    });
    expect(connect).toHaveBeenCalledTimes(1);
    expect(screen.getByText(DESKTOP.failedTitle)).toBeInTheDocument();
    expect(screen.getByText("It stopped trying to reconnect.")).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.failedReason("the desktop asks for a password"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: DESKTOP.close })).toBeInTheDocument();
    act(() => screen.getByRole("button", { name: DESKTOP.tryAgain }).click());
    expect(connect).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("status")).toHaveTextContent(DESKTOP.starting);
    expect(screen.queryByText(DESKTOP.stillTrying("the desktop asks for a password"))).toBeNull();
    sockets[1].serve(HANDSHAKE);
    expect(screen.getByText(DESKTOP.connected)).toBeInTheDocument();
  });

  it("a stopped socket that closes after Try again leaves the new desktop taking keys", () => {
    const { sockets } = open();
    sockets[0].opened();
    sockets[0].serve([...new TextEncoder().encode("RFB 003.008\n"), 1, 2]);
    act(() => screen.getByRole("button", { name: DESKTOP.tryAgain }).click());
    sockets[1].serve(HANDSHAKE);
    sockets[0].end(1005); // the stopped socket's close arrives only now
    expect(screen.getByText(DESKTOP.connected)).toBeInTheDocument();
    fireEvent.keyDown(screen.getByTestId("desktop-canvas"), { key: "a" });
    expect(sockets[1].sent.filter((m) => m[0] === 4)).toHaveLength(1);
  });

  it("shows the gateway's reason from the second failed dial, and says reconnecting after a drop", () => {
    vi.useFakeTimers();
    const { sockets } = open();
    sockets[0].opened();
    sockets[0].end(4004, "too many open UI connections for this run (max 4) - close a tab and retry");
    expect(screen.getByRole("status")).toHaveTextContent(DESKTOP.starting);
    expect(screen.queryByText(/Still trying/)).toBeNull();
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    sockets[1].opened();
    sockets[1].end(4004, "too many open UI connections for this run (max 4) - close a tab and retry");
    expect(screen.getByText(DESKTOP.stillTrying("too many open UI connections for this run (max 4) - close a tab and retry"))).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(2000);
    });
    sockets[2].opened();
    sockets[2].serve(HANDSHAKE);
    expect(screen.queryByText(/Still trying/)).toBeNull();
    sockets[2].end(1006);
    expect(screen.getByText(DESKTOP.reconnecting)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.reconnectingBody)).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP.starting)).toBeNull();
  });

  it("a drag keeps its pointer past the canvas edge and lets the button go when capture is lost", () => {
    const { sockets } = open();
    sockets[0].serve(HANDSHAKE);
    const canvas = screen.getByTestId("desktop-canvas");
    const capture = vi.fn();
    Object.assign(canvas, { setPointerCapture: capture, hasPointerCapture: () => true, releasePointerCapture: vi.fn() });
    const from = sockets[0].sent.length;
    fireEvent.pointerDown(canvas, { pointerId: 7, buttons: 1 });
    expect(capture).toHaveBeenCalledWith(7);
    fireEvent.lostPointerCapture(canvas, { pointerId: 7 });
    expect(inputs(sockets[0], from)).toEqual(["pointer 1", "pointer 0"]);
  });

  it("scrolls one notch per 50 px and maps a sideways swipe to the horizontal buttons", () => {
    const { sockets } = open();
    sockets[0].serve(HANDSHAKE);
    const canvas = screen.getByTestId("desktop-canvas");
    const wheel = (init: WheelEventInit) => {
      const from = sockets[0].sent.length;
      fireEvent.wheel(canvas, init);
      return inputs(sockets[0], from);
    };
    expect(wheel({ deltaY: 100 })).toEqual(["pointer 16", "pointer 0", "pointer 16", "pointer 0"]);
    expect(wheel({ deltaY: 30 })).toEqual([]);
    expect(wheel({ deltaY: 30 })).toEqual(["pointer 16", "pointer 0"]);
    expect(wheel({ deltaY: -90 })).toEqual(["pointer 8", "pointer 0"]);
    expect(wheel({ deltaX: -60, deltaY: 0 })).toEqual(["pointer 32", "pointer 0"]);
    expect(wheel({ deltaX: 60, deltaY: 0 })).toEqual(["pointer 64", "pointer 0"]);
    expect(wheel({ deltaX: 0, deltaY: 0 })).toEqual([]);
  });

  it("names the canvas, says clipboard doesn't reach it, and leaves it on the escape chord", () => {
    const { sockets } = open();
    sockets[0].serve(HANDSHAKE);
    const canvas = screen.getByLabelText(DESKTOP.canvasLabel);
    expect(canvas).toBe(screen.getByTestId("desktop-canvas"));
    expect(screen.getByText(DESKTOP.noClipboard)).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP.escapeHint)).toBeNull();
    act(() => canvas.focus());
    expect(screen.getByText("Ctrl+Shift+Backspace leaves the desktop")).toBeInTheDocument();
    const from = sockets[0].sent.length;
    fireEvent.keyDown(canvas, { key: "Control", code: "ControlLeft", location: 1, ctrlKey: true });
    fireEvent.keyDown(canvas, { key: "Shift", code: "ShiftLeft", location: 1, ctrlKey: true, shiftKey: true });
    fireEvent.keyDown(canvas, { key: "Backspace", code: "Backspace", ctrlKey: true, shiftKey: true });
    expect(inputs(sockets[0], from)).toEqual(["key down ffe3", "key down ffe1", "key up ffe3", "key up ffe1"]);
    expect(canvas).not.toHaveFocus();
    expect(screen.queryByText(DESKTOP.escapeHint)).toBeNull();
  });

  it("tells a macOS VM desktop to use the terminal, not Files", () => {
    const socket = new FakeSocket();
    render(<DesktopViewer boot={{ ...OPEN, machine_kind: "macos_vm" }} connect={() => socket} />);
    socket.serve(HANDSHAKE);
    expect(screen.getByText(DESKTOP.noClipboardMac)).toBeInTheDocument();
    expect(DESKTOP.noClipboardMac).not.toMatch(/Files/);
    expect(screen.queryByText(DESKTOP.noClipboard)).toBeNull();
  });

  it("offers no clipboard or escape line to a view-only session", () => {
    render(<DesktopViewer boot={{ ...OPEN, view_only: true }} connect={() => new FakeSocket()} />);
    expect(screen.queryByText(DESKTOP.noClipboard)).toBeNull();
  });

  it("links back to the run's page when the gateway names it", () => {
    render(<DesktopViewer boot={{ ...OPEN, run_title: "Fix the build", run_href: "https://console.example/runs/1" }} connect={() => new FakeSocket()} />);
    expect(screen.getByRole("link", { name: DESKTOP.backToRun("Fix the build") })).toHaveAttribute("href", "https://console.example/runs/1");
    cleanup();
    render(<DesktopViewer boot={{ ...OPEN, run_title: "Fix the build", run_href: "javascript:alert(1)" }} connect={() => new FakeSocket()} />);
    expect(screen.queryByRole("link")).toBeNull();
  });
});
