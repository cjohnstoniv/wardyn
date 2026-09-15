/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HostStatusBar } from "./host-status-bar";

describe("HostStatusBar", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  // lastCheckedLabel arrives as a full fragment ("Checked just now" / "Last
  // checked 10:02"), so the bar must NOT prefix its own "last checked" literal —
  // else the joined copy reads "…last checked Checked just now" (F1).
  it("renders an aria-live=polite region carrying the last-checked label without doubled words", () => {
    const { container } = render(
      <HostStatusBar checking={false} lastCheckedLabel="Checked just now" onRecheck={vi.fn()} />,
    );
    const live = container.querySelector('[aria-live="polite"]');
    expect(live).toBeInTheDocument();
    expect(live).toHaveTextContent(/host status/i);
    expect(screen.getByText("Checked just now")).toBeInTheDocument();
    // The label already says "Checked" — the bar must not also prefix "last
    // checked" (which produced the doubled "…last checked Checked just now").
    expect(live).toHaveTextContent("Host status · Checked just now");
    expect(live).not.toHaveTextContent(/last checked/i);
  });

  // RV-01 (review follow-up): U2-02 stopped stamping a checked time on the
  // mount fetch, which is the honest answer — but it left this strip reading
  // "Host status ·" with a dangling interpunct until the first Re-check. The
  // separator belongs to the label, not to the heading (step-bodies.tsx renders
  // its own label only when there is one).
  it("an empty label drops the separator too — never a dangling interpunct", () => {
    const { container } = render(<HostStatusBar checking={false} lastCheckedLabel="" onRecheck={vi.fn()} />);
    const live = container.querySelector('[aria-live="polite"]');
    expect(live).toHaveTextContent(/host status/i);
    expect(live!.textContent).not.toContain("·");
  });

  it("Re-check click calls onRecheck", async () => {
    const onRecheck = vi.fn();
    render(<HostStatusBar checking={false} lastCheckedLabel="Checked just now" onRecheck={onRecheck} />);
    await user.click(screen.getByRole("button", { name: /re-check/i }));
    expect(onRecheck).toHaveBeenCalledTimes(1);
  });

  it("checking state shows the in-flight label and disables the Re-check button", () => {
    render(<HostStatusBar checking={true} lastCheckedLabel="Checked just now" onRecheck={vi.fn()} />);
    expect(screen.getByText(/checking wardyn.s setup/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-check/i })).toBeDisabled();
  });
});
