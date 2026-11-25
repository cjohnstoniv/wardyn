/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DesktopWatermark, WatermarkControl } from "./desktop-watermark";
import { DESKTOP } from "./copy/desktop";

afterEach(cleanup);

const WHO = "alex@example.com";
const RUN = "3f2c9a1e-7b44-4c0d-9e21-5a6b8d0c1f73";

describe("DesktopWatermark", () => {
  it("is hidden from assistive technology, takes no pointer input and cannot be selected", () => {
    render(<DesktopWatermark who={WHO} run={RUN} />);
    const layer = screen.getByTestId("desktop-watermark");
    expect(layer).toHaveAttribute("aria-hidden", "true");
    for (const c of ["pointer-events-none", "select-none", "overflow-hidden", "absolute"]) expect(layer).toHaveClass(c);
    expect(layer.querySelector("[tabindex]")).toBeNull();
    expect(layer.querySelector("a, button, input")).toBeNull();
  });

  it("draws who and the full run id, joined as the approved text", () => {
    render(<DesktopWatermark who={WHO} run={RUN} />);
    const layer = screen.getByTestId("desktop-watermark");
    expect(DESKTOP.watermarkText(WHO, RUN)).toBe(`${WHO} · ${RUN}`);
    const tiles = [...layer.querySelectorAll("text")];
    expect(tiles.length).toBeGreaterThan(0);
    for (const t of tiles) expect(t.textContent).toBe(`${WHO} · ${RUN}`);
  });

  it("is two-tone and uses tokens, never a status colour", () => {
    render(<DesktopWatermark who={WHO} run={RUN} />);
    const t = screen.getByTestId("desktop-watermark").querySelector("text")!;
    expect(t.getAttribute("class")).toContain("fill-foreground/20");
    expect(t.getAttribute("class")).toContain("stroke-background/20");
  });

  it("applies the caller's inset", () => {
    render(<DesktopWatermark who={WHO} run={RUN} className="inset-3.5 rounded-lg" />);
    expect(screen.getByTestId("desktop-watermark")).toHaveClass("inset-3.5", "rounded-lg");
  });
});

describe("WatermarkControl", () => {
  it("says the organisation requires it, with no button", () => {
    render(<WatermarkControl required on={false} onToggle={() => {}} />);
    expect(screen.getByText(DESKTOP.watermarkRequired)).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("is a toggle button with aria-pressed when it is optional", () => {
    const onToggle = vi.fn();
    const { rerender } = render(<WatermarkControl required={false} on={false} onToggle={onToggle} />);
    const b = screen.getByRole("button", { name: DESKTOP.watermarkToggle });
    expect(b).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(b);
    expect(onToggle).toHaveBeenCalledTimes(1);
    rerender(<WatermarkControl required={false} on onToggle={onToggle} />);
    expect(screen.getByRole("button", { name: DESKTOP.watermarkToggle })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByText(DESKTOP.watermarkRequired)).toBeNull();
  });
});
