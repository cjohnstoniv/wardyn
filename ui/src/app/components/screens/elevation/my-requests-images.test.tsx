/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your requests" with both kinds: the two actions, the three leads, every image state, Withdrawn and Not applied
// for either kind, the empty line and the read-failed state. Expected strings are the approved copy's, spelled out.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ElevationOffer, GovernanceRequest, GovernanceRequestList } from "../../../lib/types/governance-requests";

const rejectMock = vi.fn();
vi.mock("../../../lib/api/governance-requests", () => ({ requests: { reject: (...a: unknown[]) => rejectMock(...a) } }));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));
const dialogProps = vi.fn();
vi.mock("./request-dialog", () => ({
  RequestDialog: (p: { open: boolean; initialKind: string; offers: ElevationOffer[]; imageOffered: boolean; onSent?: () => void }) => {
    dialogProps(p);
    return p.open ? (
      <div role="dialog" data-kind={p.initialKind}>
        <button onClick={() => p.onSent?.()}>sent</button>
      </div>
    ) : null;
  },
}));

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { MyElevationRequests } from "./my-elevation-requests";

const payments: ElevationOffer = { unit_id: "u1", unit_name: "Payments", max_elevation: "4h" };
const HEX = "4f2a91c3" + "0".repeat(56);
const REF = `ghcr.io/acme/build-tools@sha256:${HEX}`;

function imageRow(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return {
    id: "i1",
    target_kind: "governance_image_request",
    op: "create",
    target_key: `me|sha256:${HEX}`,
    state: "pending",
    kind: "image",
    image: REF,
    unit_name: "Payments",
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
function elevationRow(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return imageRow({
    id: "e1",
    target_kind: "governance_elevation",
    kind: "elevation",
    image: undefined,
    unit_id: "u1",
    duration: "4h",
    ...over,
  });
}
const list = (rows: GovernanceRequest[] = [], offered: ElevationOffer[] = [payments], image_offered = true): GovernanceRequestList => ({
  requests: rows,
  offered,
  image_offered,
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

const setup = (l: React.ComponentProps<typeof MyElevationRequests>["list"], onReload = vi.fn(), error?: unknown) => {
  const utils = render(<MyElevationRequests list={l} error={error} onReload={onReload} />);
  return { onReload, ...utils };
};

beforeEach(() => {
  rejectMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
  dialogProps.mockReset();
});
afterEach(() => vi.useRealTimers());

describe("Your requests: the actions", () => {
  it("both kinds offered: both actions, in the header", () => {
    setup(list());
    const card = screen.getByText("Your requests").closest("section") as HTMLElement;
    expect(within(card).getByRole("button", { name: "Request elevation" })).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Request an image" })).toBeInTheDocument();
  });

  it("only elevation offered: no image action", () => {
    setup(list([], [payments], false));
    expect(screen.getByRole("button", { name: "Request elevation" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request an image" })).toBeNull();
  });

  it("only images offered: no elevation action", () => {
    setup(list([], [], true));
    expect(screen.getByRole("button", { name: "Request an image" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request elevation" })).toBeNull();
  });

  it("each action opens the one dialog on its own kind, with the offers and the image flag", async () => {
    const { onReload } = setup(list());
    expect(screen.queryByRole("dialog")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Request an image" }));
    expect(screen.getByRole("dialog")).toHaveAttribute("data-kind", "image");
    expect(dialogProps).toHaveBeenLastCalledWith(expect.objectContaining({ open: true, initialKind: "image", offers: [payments], imageOffered: true }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "sent" }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  it("Request elevation opens it on elevation", async () => {
    setup(list());
    await userEvent.click(screen.getByRole("button", { name: "Request elevation" }));
    expect(screen.getByRole("dialog")).toHaveAttribute("data-kind", "elevation");
  });

  it("loading draws the header and no action", () => {
    setup("loading");
    expect(screen.getByText("Your requests")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request elevation" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Request an image" })).toBeNull();
  });
});

describe("Your requests: the lead", () => {
  it("both kinds", () => {
    setup(list());
    expect(screen.getByText("Time-limited access to a profile, and images by digest. A profile is a governance unit.")).toBeInTheDocument();
  });
  it("elevation only", () => {
    setup(list([], [payments], false));
    expect(screen.getByText("Time-limited access to a profile. A profile is a governance unit.")).toBeInTheDocument();
  });
  it("images only", () => {
    setup(list([], [], true));
    expect(screen.getByText("Images by digest.")).toBeInTheDocument();
  });
  it("no lead when nothing is offered and only rows are listed", () => {
    setup(list([imageRow()], [], false));
    expect(screen.getByText("Your requests")).toBeInTheDocument();
    expect(screen.queryByText(/by digest/)).toBeNull();
    expect(screen.queryByText(/Time-limited access/)).toBeNull();
  });
});

describe("Your requests: image rows", () => {
  it("shows the name in mono and the short digest, with the full digest as its title", () => {
    setup(list([imageRow()]));
    const name = screen.getByText("ghcr.io/acme/build-tools");
    expect(name).toHaveClass("font-mono");
    const digest = screen.getByText("sha256:4f2a91c3…");
    expect(digest).toHaveClass("font-mono");
    expect(digest).toHaveAttribute("title", `sha256:${HEX}`);
  });

  it("waiting: the chip, the line and Withdraw", () => {
    setup(list([imageRow()]));
    expect(screen.getByText("Waiting for approval")).toBeInTheDocument();
    expect(screen.getByText("Requested 3m ago · waiting for a decision")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Withdraw" })).toBeInTheDocument();
  });

  it("approved", () => {
    setup(list([imageRow({ state: "applied", decided_by: "priya.nair", decided_at: aheadByHours(-0.5) })]));
    expect(screen.getByText("Approved")).toBeInTheDocument();
    expect(screen.getByText("You can launch exactly this image now. The digest rule and image checks still apply.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Withdraw" })).toBeNull();
  });

  it("denied: the decider's reason", () => {
    setup(list([imageRow({ state: "rejected", decided_by: "priya.nair", reason: "Use the pinned base image.", decided_at: aheadByHours(-2) })]));
    expect(screen.getByText("Denied")).toBeInTheDocument();
    expect(screen.getByText("priya.nair: “Use the pinned base image.” · 2h ago")).toBeInTheDocument();
  });

  it("denied with no reason: the decider and the time", () => {
    setup(list([imageRow({ state: "rejected", decided_by: "priya.nair", decided_at: aheadByHours(-2) })]));
    expect(screen.getByText("priya.nair · 2h ago")).toBeInTheDocument();
  });

  it("withdrawn: rejected by its own requester", () => {
    setup(list([imageRow({ state: "rejected", decided_by: "me@acme.example", decided_at: aheadByHours(-1) })]));
    expect(screen.getByText("Withdrawn")).toBeInTheDocument();
    expect(screen.getByText("You withdrew it · 1h ago")).toBeInTheDocument();
    expect(screen.queryByText("Denied")).toBeNull();
  });

  it("not applied: a stale request", () => {
    setup(list([imageRow({ state: "stale" })]));
    expect(screen.getByText("Not applied")).toBeInTheDocument();
    expect(
      screen.getByText("Something changed before it was approved, so nothing was granted. Ask again if you still need it."),
    ).toBeInTheDocument();
  });

  it("expired", () => {
    setup(list([imageRow({ state: "expired", expires_at: aheadByHours(-26) })]));
    expect(screen.getByText("Expired")).toBeInTheDocument();
    expect(screen.getByText("Nobody decided it in time · 1d ago")).toBeInTheDocument();
  });

  it("does not list other people's image requests", () => {
    setup(list([imageRow({ mine: false, decidable: true })]));
    expect(screen.queryByText("ghcr.io/acme/build-tools")).toBeNull();
  });

  it("Withdraw rejects the request with no reason, says so and reloads", async () => {
    const d = deferred<unknown>();
    rejectMock.mockReturnValue(d.promise);
    const { onReload } = setup(list([imageRow({ id: "i9" })]));
    await userEvent.click(screen.getByRole("button", { name: "Withdraw" }));
    expect(rejectMock).toHaveBeenCalledWith("i9", "");
    expect(onReload).not.toHaveBeenCalled();
    d.resolve({});
    await vi.waitFor(() => expect(onReload).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Request withdrawn");
  });

  it("a refused Withdraw is a toast with the server's sentence", async () => {
    rejectMock.mockRejectedValue(new HttpError(409, "That request was already decided.", "governance_change_not_pending"));
    const { onReload } = setup(list([imageRow()]));
    await userEvent.click(screen.getByRole("button", { name: "Withdraw" }));
    await vi.waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Couldn't withdraw the request", { description: "That request was already decided." });
    expect(onReload).not.toHaveBeenCalled();
  });
});

describe("Your requests: both kinds in one list", () => {
  it("lists the newest first across kinds", () => {
    setup(
      list([
        elevationRow({ id: "old", unit_name: "Billing", proposed_at: aheadByHours(-5) }),
        imageRow({ id: "new", proposed_at: aheadByHours(-1) }),
        elevationRow({ id: "mid", unit_name: "Support", proposed_at: aheadByHours(-3) }),
      ]),
    );
    const names = screen.getAllByText(/^(Billing|Support|ghcr\.io\/acme\/build-tools)$/).map((n) => n.textContent);
    expect(names).toEqual(["ghcr.io/acme/build-tools", "Support", "Billing"]);
  });

  it("an elevation request withdrawn by its requester reads Withdrawn", () => {
    setup(list([elevationRow({ state: "rejected", decided_by: "me@acme.example", decided_at: aheadByHours(-1) })]));
    expect(screen.getByText("Withdrawn")).toBeInTheDocument();
    expect(screen.getByText("You withdrew it · 1h ago")).toBeInTheDocument();
    expect(screen.getByText("Payments")).toBeInTheDocument();
    expect(screen.queryByText("Denied")).toBeNull();
  });

  it("a stale elevation request reads Not applied", () => {
    setup(list([elevationRow({ state: "stale" })]));
    expect(screen.getByText("Not applied")).toBeInTheDocument();
    expect(
      screen.getByText("Something changed before it was approved, so nothing was granted. Ask again if you still need it."),
    ).toBeInTheDocument();
  });

  it("an approved elevation in force spells its time left as 3 h 42 min", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const ends = new Date(Date.now() + (3 * 60 + 42) * 60_000).toISOString();
    setup(list([elevationRow({ state: "applied", ends_at: ends, decided_by: "priya.nair", decided_at: new Date(Date.now() - 60_000).toISOString() })]));
    expect(screen.getByText(/^Ends .+ \(in 3 h 42 min\)\. Pick Payments when you start a run\.$/)).toBeInTheDocument();
  });
});

describe("Your requests: the states of the card", () => {
  it("read failed: the card's own words, Retry, and no action", async () => {
    const { onReload } = setup("error");
    expect(screen.getByText("Your requests")).toBeInTheDocument();
    expect(screen.getByText("Couldn't load your requests")).toBeInTheDocument();
    expect(screen.getByText("Wardyn isn't answering. Try again.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request an image" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Request elevation" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["a timeout", new HttpError(0, "Could not reach the control plane."), "Wardyn isn't answering. Try again."],
    ["a 502 with no sentence", Object.assign(new HttpError(502, "Bad Gateway"), { said: "" }), "Wardyn couldn't load your requests. Try again."],
    ["a 404 with no sentence", Object.assign(new HttpError(404, "Not Found"), { said: "" }), "Wardyn couldn't load your requests."],
  ])("read failed with %s: says so", (_name, error, text) => {
    setup("error", vi.fn(), error);
    expect(screen.getByText(text)).toBeInTheDocument();
  });

  it("offers and no rows: You have no requests.", () => {
    setup(list([], [], true));
    expect(screen.getByText("You have no requests.")).toBeInTheDocument();
  });

  it("an image row is drawn without an offer", () => {
    setup(list([imageRow()], [], false));
    expect(screen.getByText("Waiting for approval")).toBeInTheDocument();
    expect(screen.queryByText("You have no requests.")).toBeNull();
  });

  it("nothing offered and no rows: nothing", () => {
    const { container } = setup(list([], [], false));
    expect(container).toBeEmptyDOMElement();
  });

  it("an older server: nothing", () => {
    const { container } = setup(null);
    expect(container).toBeEmptyDOMElement();
  });
});
