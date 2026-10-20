/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DESKTOP } from "../app/components/wardyn/copy/desktop";
import { DesktopViewer, readBoot, type Boot, type DesktopSocket } from "./viewer";

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
  end(code: number) {
    act(() => this.onclose?.({ code } as CloseEvent));
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

  it("opens view only on a small screen, and says so", () => {
    const { sockets } = open(true);
    sockets[0].serve(HANDSHAKE);
    expect(screen.getByText(DESKTOP.phoneTitle)).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.phoneBody)).toBeInTheDocument();
    expect(screen.getByTestId("desktop-canvas")).toHaveAttribute("tabindex", "-1");
  });

  it("a used link gets the refusal card with the gateway's words", () => {
    render(<DesktopViewer boot={{ state: "refused", said: "invalid, expired, or already-used attach ticket" }} />);
    const card = screen.getByRole("region", { name: DESKTOP.linkBlocked });
    for (const s of [DESKTOP.linkRefused, DESKTOP.linkBody, DESKTOP.whyWallBody, DESKTOP.whySetByBody, DESKTOP.whyEnforcedBody, DESKTOP.whyCanChangeBody, DESKTOP.gatewaySaid])
      expect(within(card).getByText(s)).toBeInTheDocument();
    expect(within(card).getByTestId("desktop-refused-said")).toHaveTextContent("invalid, expired, or already-used attach ticket");
  });
});
