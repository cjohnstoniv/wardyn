/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { DESKTOP } from "../app/components/wardyn/copy/desktop";
import { DesktopViewer, type Boot, type DesktopSocket } from "./viewer";

const WHO = "alex@example.com";
const RUN = "3f2c9a1e-7b44-4c0d-9e21-5a6b8d0c1f73";
const BASE = { state: "open", expires: 0, rfb: "/r/x/__desktop/__rfb", close: "/r/x/__desktop/__close" } as const;

class FakeSocket implements DesktopSocket {
  binaryType: BinaryType = "blob";
  onopen: DesktopSocket["onopen"] = null;
  onmessage: DesktopSocket["onmessage"] = null;
  onclose: DesktopSocket["onclose"] = null;
  send() {}
  close() {}
  serve(bytes: number[]) {
    act(() => this.onmessage?.({ data: Uint8Array.from(bytes).buffer } as MessageEvent));
  }
}

// The bytes that bring the viewer to "connected" (the same as viewer.test.tsx).
const HANDSHAKE = [...new TextEncoder().encode("RFB 003.008\n"), 1, 1, 0, 0, 0, 0, 0, 2, 0, 1, ...new Array(16).fill(0), 0, 0, 0, 0];

function show(boot: Boot, connected = true) {
  const sockets: FakeSocket[] = [];
  render(<DesktopViewer boot={boot} connect={() => { const s = new FakeSocket(); sockets.push(s); return s; }} />);
  if (connected) sockets[0].serve(HANDSHAKE);
}

const watermark = (required: boolean): Boot => ({ ...BASE, watermark: { who: WHO, run: RUN, required } });
const toggle = () => screen.queryByRole("button", { name: DESKTOP.watermarkToggle });

afterEach(cleanup);

describe("DesktopViewer watermark", () => {
  it("shows no toggle and no layer when the gateway sent no watermark", () => {
    show({ ...BASE });
    expect(toggle()).toBeNull();
    expect(screen.queryByText(DESKTOP.watermarkRequired)).toBeNull();
    expect(screen.queryByTestId("desktop-watermark")).toBeNull();
  });

  it("optional: off to start, then a toggle button turns the layer on and off", () => {
    show(watermark(false));
    expect(toggle()).toHaveAttribute("aria-pressed", "false");
    expect(screen.queryByTestId("desktop-watermark")).toBeNull();
    fireEvent.click(toggle()!);
    expect(toggle()).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("desktop-watermark")).toHaveTextContent(`${WHO} · ${RUN}`);
    fireEvent.click(toggle()!);
    expect(screen.queryByTestId("desktop-watermark")).toBeNull();
  });

  it("required: the layer is on, with the required line and no toggle", () => {
    show(watermark(true));
    expect(screen.getByTestId("desktop-watermark")).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.watermarkRequired)).toBeInTheDocument();
    expect(toggle()).toBeNull();
  });

  it("draws no layer while the desktop is not connected, however the rule reads", () => {
    show(watermark(true), false);
    expect(screen.queryByTestId("desktop-watermark")).toBeNull();
  });

  it("keeps the layer beside <main> in the non-scrolling picture wrapper, and off the header and the strips", () => {
    show({ ...watermark(true), view_only: true, background: true });
    const layer = screen.getByTestId("desktop-watermark");
    const picture = screen.getByTestId("desktop-picture");
    expect(layer.parentElement).toBe(picture);
    expect(screen.getByRole("main").parentElement).toBe(picture);
    expect(screen.getByRole("main")).not.toContainElement(layer);
    expect(picture).not.toHaveClass("overflow-auto");
    expect(within(screen.getByRole("banner")).queryByTestId("desktop-watermark")).toBeNull();
    expect(picture).not.toContainElement(screen.getByRole("banner"));
    expect(picture).not.toContainElement(screen.getByText(DESKTOP.viewOnlyTitle));
  });

  it("stays over the visible picture in Actual size, where <main> scrolls", () => {
    show(watermark(true));
    fireEvent.click(screen.getByRole("button", { name: DESKTOP.actualSize }));
    expect(screen.getByRole("main")).toHaveClass("overflow-auto");
    expect(screen.getByTestId("desktop-watermark").parentElement).toBe(screen.getByTestId("desktop-picture"));
  });
});
