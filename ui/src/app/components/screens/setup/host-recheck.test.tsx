/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HostRecheck } from "./host-recheck";

describe("HostRecheck", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  // lastCheckedLabel arrives as a full fragment ("Checked just now" / "Last checked 10:02"), so the control must not
  // prefix its own words, and (#2847) must not name a "Host status" it never showed.
  it("carries the last-checked label in an aria-live=polite region, with no 'Host status' words", () => {
    const { container } = render(<HostRecheck checking={false} lastCheckedLabel="Checked just now" onRecheck={vi.fn()} />);
    const live = container.querySelector('[aria-live="polite"]');
    expect(live).toHaveTextContent("Checked just now");
    expect(container).not.toHaveTextContent(/host status/i);
    expect(live).not.toHaveTextContent(/last checked/i);
  });

  // U2-02: the mount read is answered from the daemon's memo, so the honest first render says nothing about a check.
  it("with no label the live region is mounted and empty: no stamp, no interpunct", () => {
    const { container } = render(<HostRecheck checking={false} lastCheckedLabel="" onRecheck={vi.fn()} />);
    const live = container.querySelector('[aria-live="polite"]');
    expect(live).toBeInTheDocument();
    expect(live!.textContent).toBe("");
  });

  it("Re-check click calls onRecheck", async () => {
    const onRecheck = vi.fn();
    render(<HostRecheck checking={false} lastCheckedLabel="Checked just now" onRecheck={onRecheck} />);
    await user.click(screen.getByRole("button", { name: /re-check/i }));
    expect(onRecheck).toHaveBeenCalledTimes(1);
  });

  it("checking state shows the in-flight label and disables the Re-check button", () => {
    render(<HostRecheck checking={true} lastCheckedLabel="Checked just now" onRecheck={vi.fn()} />);
    expect(screen.getByText(/checking wardyn.s setup/i)).toBeInTheDocument();
    expect(screen.queryByText("Checked just now")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-check/i })).toBeDisabled();
  });
});
