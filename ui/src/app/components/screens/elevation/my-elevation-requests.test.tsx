/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your requests" on Approvals. Expected strings are the approved copy's, spelled out; the lead is read by constant.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ElevationOffer, GovernanceRequest, GovernanceRequestList } from "../../../lib/types/governance-requests";

const rejectMock = vi.fn();
vi.mock("../../../lib/api/governance-requests", () => ({ requests: { reject: (...a: unknown[]) => rejectMock(...a) } }));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));
const dialogProps = vi.fn();
vi.mock("./elevation-request-dialog", () => ({
  ElevationRequestDialog: (p: { open: boolean; offers?: ElevationOffer[]; onSent?: () => void }) => {
    dialogProps(p);
    return p.open ? (
      <div role="dialog">
        <button onClick={() => p.onSent?.()}>sent</button>
      </div>
    ) : null;
  },
}));

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { ELEV } from "../../wardyn/copy/elevation";
import { MyElevationRequests } from "./my-elevation-requests";

const payments: ElevationOffer = { unit_id: "u1", unit_name: "Payments", max_elevation: "4h" };

function row(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return {
    id: "c1",
    target_kind: "governance_elevation",
    op: "create",
    target_key: "u1",
    state: "pending",
    kind: "elevation",
    unit_id: "u1",
    unit_name: "Payments",
    duration: "4h",
    proposed_by: "me@acme.example",
    proposed_at: aheadByHours(-0.05),
    expires_at: aheadByHours(24),
    diff: { before: null, after: null, changed: [] },
    mine: true,
    decidable: false,
    deciders: [{ kind: "unit_admin", unit_name: "Payments" }, { kind: "security" }],
    ...over,
  } as GovernanceRequest;
}
const list = (requests: GovernanceRequest[] = [], offered: ElevationOffer[] = [payments]): GovernanceRequestList => ({
  requests,
  offered,
  image_offered: false,
});

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const setup = (l: React.ComponentProps<typeof MyElevationRequests>["list"], onReload = vi.fn()) => {
  render(<MyElevationRequests list={l} onReload={onReload} />);
  return { onReload };
};

beforeEach(() => {
  rejectMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
  dialogProps.mockReset();
});

describe("MyElevationRequests: when it draws nothing", () => {
  it("draws nothing for an older server", () => {
    const { container } = render(<MyElevationRequests list={null} onReload={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("draws nothing with no offers and no rows", () => {
    const { container } = render(<MyElevationRequests list={list([], [])} onReload={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("draws only the header and its action with offers and no rows", () => {
    setup(list());
    expect(screen.getByText("Your requests")).toBeInTheDocument();
    expect(screen.getByText(ELEV.LIST_LEAD)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Request elevation" })).toBeInTheDocument();
    expect(screen.queryByText(ELEV.LIST_EMPTY)).not.toBeInTheDocument();
  });

  it("offers no action when no unit offers it, and still lists the person's own rows", () => {
    setup(list([row()], []));
    expect(screen.getByText("Your requests")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request elevation" })).not.toBeInTheDocument();
    expect(screen.getByText("Waiting for approval")).toBeInTheDocument();
  });
});

describe("MyElevationRequests: each state", () => {
  it("a waiting request says who decides it and offers Withdraw", () => {
    setup(list([row()]));
    expect(screen.getByText("Waiting for approval")).toBeInTheDocument();
    expect(screen.getByText("Payments")).toBeInTheDocument();
    expect(screen.getByText("4 hours")).toBeInTheDocument();
    expect(screen.getByText("Requested 3m ago. Admins of Payments or a security admin decide it.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Withdraw" })).toBeInTheDocument();
  });

  it("an approved request in force says when it ends and how long is left", () => {
    setup(list([row({ state: "applied", ends_at: aheadByHours(3), decided_by: "priya.nair", decided_at: aheadByHours(-0.5) })]));
    expect(screen.getByText("Approved")).toBeInTheDocument();
    expect(screen.getByText(/^Ends \S+ \S+ \(in [23]h \d+m\)\. Pick Payments when you start a run\.$/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Withdraw" })).not.toBeInTheDocument();
  });

  it("an approved request that has run out reads Ended", () => {
    setup(list([row({ state: "applied", ends_at: aheadByHours(-2) })]));
    expect(screen.getByText("Ended")).toBeInTheDocument();
    expect(screen.getByText("Ended 2h ago")).toBeInTheDocument();
  });

  it("an approved request with no end reads Ended from the decision time", () => {
    setup(list([row({ state: "applied", decided_at: aheadByHours(-5) })]));
    expect(screen.getByText("Ended 5h ago")).toBeInTheDocument();
  });

  it("a denied request names the decider and their reason", () => {
    setup(list([row({ state: "rejected", decided_by: "priya.nair", reason: "Use the support runbook instead.", decided_at: aheadByHours(-2) })]));
    expect(screen.getByText("Denied")).toBeInTheDocument();
    expect(screen.getByText("priya.nair: “Use the support runbook instead.” · 2h ago")).toBeInTheDocument();
  });

  it("a denied request with no reason reads the decider and the time", () => {
    setup(list([row({ state: "rejected", decided_by: "priya.nair", decided_at: aheadByHours(-2) })]));
    expect(screen.getByText("priya.nair · 2h ago")).toBeInTheDocument();
  });

  it("an expired request says nobody decided it", () => {
    setup(list([row({ state: "expired", expires_at: aheadByHours(-26) })]));
    expect(screen.getByText("Expired")).toBeInTheDocument();
    expect(screen.getByText("Nobody decided it in time · 1d ago")).toBeInTheDocument();
  });

  it("lists the newest request first", () => {
    setup(
      list([
        row({ id: "old", unit_name: "Billing", proposed_at: aheadByHours(-5) }),
        row({ id: "new", unit_name: "Support", proposed_at: aheadByHours(-1) }),
      ]),
    );
    const names = screen.getAllByText(/^(Billing|Support)$/).map((n) => n.textContent);
    expect(names).toEqual(["Support", "Billing"]);
  });
});

describe("MyElevationRequests: what is not listed", () => {
  it("does not list a withdrawn request (rejected by its own requester)", () => {
    setup(list([row({ state: "rejected", decided_by: "me@acme.example", decided_at: aheadByHours(-1) })]));
    expect(screen.queryByText("Denied")).not.toBeInTheDocument();
    expect(screen.queryByText("Payments")).not.toBeInTheDocument();
  });

  it("does not list a stale request", () => {
    setup(list([row({ state: "stale" })]));
    expect(screen.queryByText("Payments")).not.toBeInTheDocument();
  });

  it("does not list other people's requests", () => {
    setup(list([row({ mine: false, decidable: true })]));
    expect(screen.queryByText("Payments")).not.toBeInTheDocument();
  });

  it("does not list image rows", () => {
    setup(list([row({ kind: "image", unit_name: undefined, image: "registry.example/app:1" })], []));
    expect(screen.queryByText("Waiting for approval")).not.toBeInTheDocument();
    expect(screen.queryByText("Your requests")).not.toBeInTheDocument();
  });
});

describe("MyElevationRequests: Withdraw", () => {
  it("rejects the request with no reason, says so and reloads", async () => {
    const reject = deferred<unknown>();
    rejectMock.mockReturnValue(reject.promise);
    const { onReload } = setup(list([row({ id: "c9" })]));
    await userEvent.click(screen.getByRole("button", { name: "Withdraw" }));
    expect(rejectMock).toHaveBeenCalledWith("c9", "");
    expect(onReload).not.toHaveBeenCalled();
    reject.resolve({});
    await vi.waitFor(() => expect(onReload).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Request withdrawn");
  });

  it("a refusal is a toast with the server's sentence, and does not reload", async () => {
    rejectMock.mockRejectedValue(new HttpError(409, "That request was already decided.", "governance_change_not_pending"));
    const { onReload } = setup(list([row()]));
    await userEvent.click(screen.getByRole("button", { name: "Withdraw" }));
    await vi.waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Couldn't withdraw the request", { description: "That request was already decided." });
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(onReload).not.toHaveBeenCalled();
  });
});

describe("MyElevationRequests: Request elevation", () => {
  it("opens the dialog with the list's offers, and a sent request reloads the list", async () => {
    const { onReload } = setup(list([], [payments, { unit_id: "u2", unit_name: "Billing", max_elevation: "8h" }]));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Request elevation" }));
    const dialog = screen.getByRole("dialog");
    expect(dialogProps).toHaveBeenLastCalledWith(
      expect.objectContaining({ open: true, offers: [payments, { unit_id: "u2", unit_name: "Billing", max_elevation: "8h" }] }),
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "sent" }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });
});

describe("MyElevationRequests: loading and error", () => {
  it("draws the header over a skeleton while loading", () => {
    setup("loading");
    expect(screen.getByText("Your requests")).toBeInTheDocument();
    expect(screen.getByText(ELEV.LIST_LEAD)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request elevation" })).not.toBeInTheDocument();
  });

  it("draws an error with Retry that reloads", async () => {
    const { onReload } = setup("error");
    expect(screen.getByText("Your requests")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });
});
