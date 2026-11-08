/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The image decider card under Waiting on you. Expected strings are the approved copy's, spelled out.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GovernanceRequest } from "../../../lib/types/governance-requests";

const approveMock = vi.fn();
const rejectMock = vi.fn();
vi.mock("../../../lib/api/governance-requests", () => ({
  requests: { approve: (...a: unknown[]) => approveMock(...a), reject: (...a: unknown[]) => rejectMock(...a) },
}));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { ImageDeciderCard } from "./image-decider-card";

const PERSON = "dana.reyes@acme.example";
const NAME = "ghcr.io/acme/build-tools";
const DIGEST = "sha256:4f2a91c37be0d5a8e61c29f04b7d3a5c8e9f1a2b3c4d5e6f708192a3b4c5d6e7";
const REASON = "The release job needs the pinned build-tools image.";

function request(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return {
    id: "i1",
    target_kind: "governance_image_request",
    op: "create",
    target_key: "u1",
    state: "pending",
    kind: "image",
    unit_id: "u1",
    unit_name: "Payments",
    image: `${NAME}@${DIGEST}`,
    proposed_by: PERSON,
    proposed_at: aheadByHours(-0.05),
    expires_at: aheadByHours(24),
    diff: { before: null, after: null, changed: [] },
    note: { reason: REASON, ticket: "INC-2210" },
    mine: false,
    decidable: true,
    deciders: [{ kind: "security" }, { kind: "unit_admin" }, { kind: "group", name: "platform-leads" }],
    ...over,
  } as GovernanceRequest;
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const setup = (r: GovernanceRequest = request()) => {
  const onDone = vi.fn();
  const view = render(<ImageDeciderCard request={r} onDone={onDone} />);
  return { onDone, view };
};

beforeEach(() => {
  approveMock.mockReset();
  rejectMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
});

describe("ImageDeciderCard: what it says", () => {
  it("names the kind and the person in the title, with no image name", () => {
    setup();
    expect(screen.getByText("Image", { selector: "span" })).toBeInTheDocument();
    const title = screen.getByText(`Grant ${PERSON} one image, by digest`);
    expect(title).toBeInTheDocument();
    expect(title).not.toHaveTextContent(NAME);
  });

  it("shows Requested by with the ticket in mono beside it", () => {
    setup();
    expect(screen.getByText(/^Requested by/)).toHaveTextContent(`Requested by ${PERSON}`);
    const ticket = screen.getByText("INC-2210");
    expect(ticket).toHaveClass("font-mono");
    expect(ticket.closest("div")).toHaveTextContent(`Requested by ${PERSON}Ticket INC-2210`);
  });

  it("shows no ticket when there is none", () => {
    setup(request({ note: { reason: REASON } }));
    expect(screen.queryByText("INC-2210")).not.toBeInTheDocument();
    expect(screen.queryByText("Ticket")).not.toBeInTheDocument();
    expect(screen.queryByText(/Ticket/)).not.toBeInTheDocument();
  });

  it("says who decides, in the server's order", () => {
    setup();
    expect(screen.getByText("Decided by:").parentElement).toHaveTextContent(
      "Decided by: a security admin, admins of this unit or the platform-leads group",
    );
  });

  it("draws the six body lines in order, with the image name and the full digest", () => {
    setup();
    const labels = ["For:", "Image:", "Digest:", "Their reason:", "What you're approving:", "Blast radius:"];
    const lines = labels.map((l) => screen.getByText(l).parentElement as HTMLElement);
    const body = lines[0].parentElement as HTMLElement;
    expect(Array.from(body.children)).toEqual(lines);
    expect(lines[0]).toHaveTextContent(`For: ${PERSON}, asked under Payments, their default profile`);
    expect(lines[1]).toHaveTextContent(`Image: ${NAME}`);
    expect(lines[1]).not.toHaveTextContent("sha256");
    expect(lines[2]).toHaveTextContent(`Digest: ${DIGEST}`);
    expect(lines[2].textContent).toContain(DIGEST);
    expect(lines[3]).toHaveTextContent(`Their reason: ${REASON}`);
    expect(lines[4]).toHaveTextContent(
      `What you're approving: ${PERSON} can launch exactly this image, in any profile they run in. The grant stays until an admin deletes it.`,
    );
    expect(lines[5]).toHaveTextContent(
      "Blast radius: The digest rule, a deny of the image and image admission still apply at every launch. Nothing else changes.",
    );
  });

  it("draws the name and the digest in mono, the digest breaking anywhere", () => {
    setup();
    expect(screen.getByText(NAME)).toHaveClass("font-mono");
    const digest = screen.getByText(DIGEST);
    expect(digest).toHaveClass("font-mono");
    expect(digest).toHaveClass("break-all");
  });

  it("uses the requester's string everywhere", () => {
    const { view } = setup();
    const text = view.container.textContent ?? "";
    expect(text.split(PERSON)).toHaveLength(5);
  });

  it("says when it was requested", () => {
    setup();
    expect(screen.getByText("requested 3m ago")).toBeInTheDocument();
  });
});

describe("ImageDeciderCard: deciding", () => {
  it("Approve calls approve with the id, says so and finishes", async () => {
    const approve = deferred<unknown>();
    approveMock.mockReturnValue(approve.promise);
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: /Approve/ }));
    expect(approveMock).toHaveBeenCalledWith("i1");
    expect(onDone).not.toHaveBeenCalled();
    approve.resolve({});
    await vi.waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Request approved");
  });

  it("Deny calls reject with the id and an empty reason, says so and finishes", async () => {
    rejectMock.mockResolvedValue({});
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: /Deny/ }));
    expect(rejectMock).toHaveBeenCalledWith("i1", "");
    await vi.waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Request denied");
  });

  it("both buttons are off while a decision is in flight", async () => {
    const approve = deferred<unknown>();
    approveMock.mockReturnValue(approve.promise);
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: /Approve/ }));
    expect(screen.getByRole("button", { name: /Approve/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Deny/ })).toBeDisabled();
    approve.resolve({});
    await vi.waitFor(() => expect(onDone).toHaveBeenCalled());
  });
});

describe("ImageDeciderCard: refusals", () => {
  const stale =
    "the target, or the deployment default, has changed since this change was proposed: it was not applied and is now stale; propose it again";
  const cases: Array<{ name: string; button: RegExp; status: number; reason: string; sentence: string; title: string }> = [
    { name: "stale", button: /Approve/, status: 409, reason: "governance_change_stale", sentence: stale, title: "Couldn't approve this request" },
    {
      name: "no longer pending",
      button: /Deny/,
      status: 409,
      reason: "governance_change_not_pending",
      sentence: "this change is no longer pending (applied)",
      title: "Couldn't deny this request",
    },
    {
      name: "outside your units",
      button: /Approve/,
      status: 403,
      reason: "unit_scope_denied",
      sentence: "this is outside the units you administer",
      title: "Couldn't approve this request",
    },
    {
      name: "not found",
      button: /Approve/,
      status: 404,
      reason: "governance_change_not_found",
      sentence: "governance change not found",
      title: "Couldn't approve this request",
    },
  ];
  for (const c of cases) {
    it(`${c.name}: the toast names the button and the server's sentence, then the card finishes`, async () => {
      const err = new HttpError(c.status, c.sentence, c.reason);
      approveMock.mockRejectedValue(err);
      rejectMock.mockRejectedValue(err);
      const { onDone } = setup();
      await userEvent.click(screen.getByRole("button", { name: c.button }));
      await vi.waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
      expect(toastError).toHaveBeenCalledWith(c.title, { description: c.sentence });
      expect(toastSuccess).not.toHaveBeenCalled();
      await vi.waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    });
  }
});
