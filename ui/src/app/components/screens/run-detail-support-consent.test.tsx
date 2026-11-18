/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The owner's consent screen: every door off and no time chosen to start, Accept off until both, the section 7
// words with this run's recording answer, the reason and the place, and the lapsed screen.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { makeRun } from "../../../test/factories";
import type { RunDetail } from "../../lib/types";
import type { SupportGrantView } from "../../lib/api/support";
import { HttpError } from "../../lib/api/core";
import { SUPPORT_CONSENT } from "../wardyn/copy/support";

const accept = vi.fn();
const decline = vi.fn();
vi.mock("../../lib/api/support", () => ({ support: { accept: (...a: unknown[]) => accept(...a), decline: (...a: unknown[]) => decline(...a) } }));

import { SupportConsentDialog } from "./run-detail-support-consent";

const run = (o: Partial<RunDetail> = {}) => ({ ...makeRun({ created_by: "maya" }), ...o }) as RunDetail;
const fresh = () => new Date(Date.now() - 2 * 60_000).toISOString();
const grant = (o: Partial<SupportGrantView> = {}): SupportGrantView => ({
  grant: "g-1",
  helper: "Sam Rivera",
  reason: "The build keeps stalling on the same step.",
  doors_asked: ["terminal", "commands", "files"],
  requested_at: fresh(),
  state: "requested",
  ...o,
});

const onClose = vi.fn();
const onAnswered = vi.fn();
function open(g: SupportGrantView, r: RunDetail = run(), recorded: SupportGrantView["doors_asked"] = ["terminal"]) {
  return render(<SupportConsentDialog run={r} grant={g} recordedDoors={recorded} onClose={onClose} onAnswered={onAnswered} />);
}
const acceptButton = () => screen.getByRole("button", { name: "Accept" }) as HTMLButtonElement;

beforeEach(() => {
  [accept, decline, onClose, onAnswered].forEach((m) => m.mockReset());
});
afterEach(cleanup);

describe("consent: as it opens", () => {
  it("names the helper, shows the reason, lists only the doors asked, all unticked, and no minutes chosen", () => {
    open(grant());
    expect(screen.getByText("Sam Rivera wants to help in this run")).toBeTruthy();
    expect(screen.getByText("Nothing opens until you accept.")).toBeTruthy();
    expect(screen.getByTestId("support-reason").textContent).toBe("The build keeps stalling on the same step.");
    const boxes = screen.getAllByRole("checkbox");
    expect(boxes.map((b) => b.getAttribute("aria-checked"))).toEqual(["false", "false", "false"]);
    expect(screen.queryByRole("checkbox", { name: /Forward/ })).toBeNull();
    for (const r of screen.getAllByRole("radio")) expect(r.getAttribute("aria-checked")).toBe("false");
    expect(screen.getAllByRole("radio").map((r) => r.id)).toEqual(["consent-minutes-15", "consent-minutes-30", "consent-minutes-60"]);
    expect(acceptButton().disabled).toBe(true);
  });

  it("shows section 7's words with the run's recording answer, and the activity line", () => {
    open(grant(), run(), ["terminal"]);
    const copy = screen.getByTestId("support-consent-copy").textContent ?? "";
    expect(copy).toContain(SUPPORT_CONSENT.RECORDING("recorded"));
    expect(copy).toContain(SUPPORT_CONSENT.REACH);
    expect(screen.getByText(SUPPORT_CONSENT.ACTIVITY)).toBeTruthy();
    expect(screen.queryByText(/^Desktop:/)).toBeNull(); // desktop not asked
    cleanup();
    open(grant(), run(), []);
    expect(screen.getByTestId("support-consent-copy").textContent).toContain("Terminal: not recorded on this run.");
  });

  it("adds the desktop line when Desktop was asked, with its own recording answer", () => {
    open(grant({ doors_asked: ["terminal", "desktop"] }), run(), ["terminal", "desktop"]);
    expect(screen.getByText("Desktop: recorded on this run.")).toBeTruthy();
    cleanup();
    open(grant({ doors_asked: ["terminal", "desktop"] }), run(), ["terminal"]);
    expect(screen.getByText("Desktop: not recorded on this run.")).toBeTruthy();
  });

  it("says where the sandbox runs: a pool, or the owner's own computer with the server-relay line", () => {
    open(grant(), run({ placement: "remote", runner_pool_name: "build-pool" }));
    expect(screen.getByTestId("support-place").textContent).toBe("build-pool pool");
    expect(screen.queryByText(SUPPORT_CONSENT.OWN_COMPUTER)).toBeNull();
    cleanup();
    open(grant(), run({ placement: "local", runner_id: "r1", runner_name: "Maya Patel's MacBook" }));
    expect(screen.getByTestId("support-place").textContent).toBe("Maya Patel's MacBook");
    expect(screen.getByText(SUPPORT_CONSENT.OWN_COMPUTER)).toBeTruthy();
  });
});

describe("consent: answering", () => {
  it("accepts with the ticked doors and the chosen minutes, only once both are given", async () => {
    accept.mockResolvedValue(grant({ state: "active" }));
    const user = userEvent.setup();
    open(grant());
    await user.click(screen.getByRole("checkbox", { name: /Files/ }));
    expect(acceptButton().disabled).toBe(true); // a door, no time
    await user.click(screen.getByRole("radio", { name: "30 minutes" }));
    expect(acceptButton().disabled).toBe(false);
    await user.click(screen.getByRole("checkbox", { name: /Terminal/ }));
    await user.click(acceptButton());
    expect(accept).toHaveBeenCalledWith("run-1", "g-1", ["terminal", "files"], 30);
    expect(onAnswered).toHaveBeenCalled();
  });

  it("declines without any door or time", async () => {
    decline.mockResolvedValue(grant({ state: "ended" }));
    await userEvent.setup().click((open(grant()), screen.getByRole("button", { name: "Decline" })));
    expect(decline).toHaveBeenCalledWith("run-1", "g-1");
    expect(onAnswered).toHaveBeenCalled();
  });

  it("shows the lapsed screen for a request older than 15 minutes, with only Close", () => {
    open(grant({ requested_at: new Date(Date.now() - 16 * 60_000).toISOString() }));
    expect(screen.getByText(SUPPORT_CONSENT.LAPSED)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Accept" })).toBeNull();
    // the dialog's own X and the footer Close
    expect(within(screen.getByTestId("support-consent-lapsed")).getAllByRole("button", { name: "Close" })).toHaveLength(2);
  });

  it("turns to the lapsed screen when the server no longer finds the request", async () => {
    accept.mockRejectedValue(new HttpError(404, "No such open help request", "support_grant_not_found"));
    const user = userEvent.setup();
    open(grant());
    await user.click(screen.getByRole("checkbox", { name: /Terminal/ }));
    await user.click(screen.getByRole("radio", { name: "15 minutes" }));
    await user.click(acceptButton());
    expect(await screen.findByText(SUPPORT_CONSENT.LAPSED)).toBeTruthy();
    expect(onAnswered).not.toHaveBeenCalled();
  });
});
