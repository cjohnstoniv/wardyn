/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// TierPicker (#1200): the installed ∧ allowed filter, the decided/requirement
// edges, the ~360px shape breakpoint, the info popover and the Compare
// barriers dialog. Mirrors the try-it states of the approved packet
// (mock-08/tier-picker-1200-packet.html §2/§3).
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { allowedFromFloor, pickerShape, TierPicker, visibleTiers } from "./tier-picker";

describe("visibleTiers — installed ∧ allowed", () => {
  it("keeps only tiers that are both installed and allowed, weakest-first", () => {
    expect(visibleTiers(["CC2", "CC1"], ["CC1", "CC2", "CC3"])).toEqual(["CC1", "CC2"]);
  });

  it("with no allowed list (undefined/null), every installed tier is visible", () => {
    expect(visibleTiers(["CC1", "CC2"])).toEqual(["CC1", "CC2"]);
    expect(visibleTiers(["CC1", "CC2"], null)).toEqual(["CC1", "CC2"]);
  });

  it("a floor that forbids everything installed leaves nothing visible", () => {
    expect(visibleTiers(["CC1"], ["CC3"])).toEqual([]);
  });

  it("an installed tier the floor forbids is dropped, not shown disabled", () => {
    expect(visibleTiers(["CC1", "CC2", "CC3"], ["CC2", "CC3"])).toEqual(["CC2", "CC3"]);
  });
});

describe("allowedFromFloor", () => {
  it("a floor allows itself and every stronger tier", () => {
    expect(allowedFromFloor("CC2")).toEqual(["CC2", "CC3"]);
    expect(allowedFromFloor("CC3")).toEqual(["CC3"]);
    expect(allowedFromFloor("CC1")).toEqual(["CC1", "CC2", "CC3"]);
  });

  it("no floor (null/undefined/unrecognised) means no restriction", () => {
    expect(allowedFromFloor(null)).toBeNull();
    expect(allowedFromFloor(undefined)).toBeNull();
  });
});

describe("pickerShape — the ~360px breakpoint (T-1)", () => {
  it("segmented at and above 360px, and at an unmeasured (0) width", () => {
    expect(pickerShape(360)).toBe("segmented");
    expect(pickerShape(720)).toBe("segmented");
    expect(pickerShape(0)).toBe("segmented");
  });

  it("dropdown only under 360px", () => {
    expect(pickerShape(359)).toBe("dropdown");
    expect(pickerShape(1)).toBe("dropdown");
  });
});

describe("TierPicker — the decided state (exactly one visible tier)", () => {
  it("renders no control at all, just the frozen 'set by your admin' line", () => {
    render(<TierPicker tiers={["CC3"]} selected={null} onSelect={vi.fn()} />);
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.queryByRole("radio")).toBeNull();
    expect(screen.getByText(/Vault · set by your admin/)).toBeInTheDocument();
  });
});

describe("TierPicker — the requirement card (zero visible tiers, T-9)", () => {
  it("names the requirement instead of a generic no-runner claim", () => {
    render(
      <TierPicker
        tiers={[]}
        selected={null}
        onSelect={vi.fn()}
        requirementNote="Your admin requires Vault, and this host can't run it: no /dev/kvm."
      />,
    );
    expect(screen.getByText(/Your admin requires Vault/)).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup")).toBeNull();
  });

  it("falls back to the generic no-runner sentence when the caller has no specific reason", () => {
    render(<TierPicker tiers={[]} selected={null} onSelect={vi.fn()} />);
    expect(screen.getByText(/No sandbox runner/)).toBeInTheDocument();
  });
});

describe("TierPicker — a real choice (two or more visible tiers)", () => {
  it("segmented shape renders one selectable radio per tier", async () => {
    const onSelect = vi.fn();
    render(<TierPicker tiers={["CC1", "CC2", "CC3"]} selected="CC2" onSelect={onSelect} recommended="CC3" />);
    const group = screen.getByRole("radiogroup", { name: /barrier tier/i });
    expect(within(group).getByRole("radio", { name: "Wall" })).toHaveAttribute("aria-checked", "true");
    expect(within(group).getByRole("radio", { name: "Fence" })).toHaveAttribute("aria-checked", "false");

    await userEvent.click(within(group).getByRole("radio", { name: "Vault" }));
    expect(onSelect).toHaveBeenCalledWith("CC3");

    expect(screen.getByText(/pick one/i)).toBeInTheDocument();
  });

  it("dropdown shape (forced, <360px) renders a single trigger, not a radiogroup", () => {
    render(<TierPicker tiers={["CC1", "CC2"]} selected="CC1" onSelect={vi.fn()} shape="dropdown" />);
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.getByRole("combobox", { name: /barrier tier/i })).toBeInTheDocument();
  });
});

// #1238 — a tier the host was not seen to build must not read "Ready".
describe("TierPicker — readiness", () => {
  it("shows Unverified, never Ready, on every row once the probe answered without a list", () => {
    render(<TierPicker tiers={["CC1", "CC2", "CC3"]} selected="CC1" onSelect={vi.fn()} readiness="unverified" />);
    expect(screen.getAllByText("Unverified")).toHaveLength(3);
    expect(screen.queryByText("Ready")).toBeNull();
  });

  // Pinned character for character: the ellipsis is the single U+2026.
  it("shows Checking\u2026 on every row while the probe is pending", () => {
    render(<TierPicker tiers={["CC1", "CC2", "CC3"]} selected="CC1" onSelect={vi.fn()} readiness="checking" />);
    expect(screen.getAllByText("Checking\u2026")).toHaveLength(3);
    expect(screen.queryByText("Unverified")).toBeNull();
    expect(screen.queryByText("Ready")).toBeNull();
  });

  it("the decided single row follows the same word", () => {
    const { rerender } = render(<TierPicker tiers={["CC3"]} readiness="unverified" />);
    expect(screen.getByText("Unverified")).toBeInTheDocument();
    rerender(<TierPicker tiers={["CC3"]} readiness="checking" />);
    expect(screen.getByText("Checking\u2026")).toBeInTheDocument();
    expect(screen.queryByText("Ready")).toBeNull();
  });

  it("a probed host keeps Ready", () => {
    render(<TierPicker tiers={["CC1", "CC2"]} selected="CC1" onSelect={vi.fn()} />);
    expect(screen.getAllByText("Ready")).toHaveLength(2);
    expect(screen.queryByText("Unverified")).toBeNull();
  });
});

describe("TierPicker — the info popover", () => {
  it("opens on the row's info button and shows the tier's mechanism and residual risk", async () => {
    render(<TierPicker tiers={["CC1", "CC2"]} selected="CC1" onSelect={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "About Fence" }));
    expect(await screen.findByText(/Shared-kernel container/)).toBeInTheDocument();
    expect(screen.getByText(/Doesn't stop:/)).toBeInTheDocument();
  });
});

describe("TierPicker — Compare barriers dialog", () => {
  it("opens the full matrix and links to the docs", async () => {
    render(<TierPicker tiers={["CC1", "CC2", "CC3"]} selected="CC1" onSelect={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /compare barriers/i }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /read the docs/i })).toHaveAttribute(
      "href",
      expect.stringContaining("docs/OPERATIONS.md"),
    );
    // The full matrix's own honesty row, reused verbatim from cc-meta.ts.
    expect(screen.getAllByText(/A kernel exploit can't reach your host kernel/).length).toBeGreaterThan(0);
  });
});

describe("TierPicker — display mode (Settings' Host card, member Getting-started)", () => {
  it("renders every given tier as a read-only row, never a radiogroup, even with several", () => {
    render(<TierPicker tiers={["CC1", "CC2"]} mode="display" />);
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.queryByRole("radio")).toBeNull();
    expect(screen.getAllByRole("status").length).toBe(2);
    // display mode never renders the governed "set by your admin" line — it
    // states a host/allowed fact, not a per-user decision.
    expect(screen.queryByText(/set by your admin/)).toBeNull();
  });

  it("with nothing to show, says so instead of rendering an empty control", () => {
    render(<TierPicker tiers={[]} mode="display" />);
    expect(screen.getByText(/no barrier is installed/i)).toBeInTheDocument();
  });
});
