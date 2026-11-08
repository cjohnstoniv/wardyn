/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The image request dialog. Expected strings are the approved copy's, spelled out rather than read back from
// the copy module.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GovernanceRequest } from "../../../lib/types/governance-requests";

const createMock = vi.fn();
vi.mock("../../../lib/api/governance-requests", () => ({ requests: { create: (...a: unknown[]) => createMock(...a) } }));

import { HttpError } from "../../../lib/api/core";
import { absoluteTime } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import { ImageRequestDialog } from "./image-request-dialog";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const HEX = "4f2a91c3" + "0".repeat(56);
const REF = `ghcr.io/acme/build-tools@sha256:${HEX}`;
const REASON = "The nightly build needs the new toolchain INC-2210";

function sentRequest(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return {
    id: "c1",
    state: "pending",
    kind: "image",
    image: REF,
    unit_name: "Payments",
    mine: true,
    decidable: false,
    deciders: [{ kind: "unit_admin", unit_name: "Payments" }, { kind: "group", name: "platform-leads" }, { kind: "security" }],
    expires_at: aheadByHours(24),
    ...over,
  } as GovernanceRequest;
}

type Props = Partial<React.ComponentProps<typeof ImageRequestDialog>>;
function setup(props: Props = {}) {
  const onOpenChange = vi.fn();
  const onSent = vi.fn();
  const utils = render(<ImageRequestDialog open onOpenChange={onOpenChange} onSent={onSent} {...props} />);
  return { onOpenChange, onSent, ...utils };
}

const send = () => screen.getByRole("button", { name: "Request image" });
const image = () => screen.getByLabelText("Image");
const reason = () => screen.getByLabelText("Why do you need it?");
const ticket = () => screen.getByLabelText(/^Ticket/);

async function fill(ref = REF, why = REASON) {
  await userEvent.type(image(), ref);
  await userEvent.type(reason(), why);
}

beforeEach(() => createMock.mockReset());

describe("ImageRequestDialog: the form", () => {
  it("opens with the approved wording", () => {
    setup();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getByText("New image request")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Ask to launch one image, named by its digest. The people who decide for your default profile see your request. If one of them approves, you can launch exactly this image.",
      ),
    ).toBeInTheDocument();
    expect(image()).toHaveAttribute("placeholder", "registry.example.com/team/tool@sha256:…");
    expect(image()).toHaveClass("font-mono");
    expect(image()).toBeRequired();
    expect(screen.getByText("By digest only: <name>@sha256: and 64 hex characters. Copy it from your registry.")).toBeInTheDocument();
    expect(reason()).toHaveAttribute("placeholder", "Say what you need the image for. The people who decide read this.");
    expect(reason()).toHaveAttribute("maxlength", "500");
    expect(screen.getByText("Shown to the people who decide, and kept in the audit trail.")).toBeInTheDocument();
    expect(ticket()).toHaveAttribute("maxlength", "128");
    expect(ticket()).toHaveClass("font-mono");
    expect(screen.getByLabelText("Ticket (optional)")).toBe(ticket());
    expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled();
  });

  it("draws the kind choice between the lead and the first field when it is given", () => {
    setup({ kindChoice: <div data-testid="kinds">choice</div> });
    const kinds = screen.getByTestId("kinds");
    expect(kinds.compareDocumentPosition(image()) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText(/^Ask to launch one image/).compareDocumentPosition(kinds) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps Request image off until there is an image and a reason", async () => {
    setup();
    expect(send()).toBeDisabled();
    await userEvent.type(image(), REF);
    expect(send()).toBeDisabled();
    await userEvent.type(reason(), "   ");
    expect(send()).toBeDisabled();
    await userEvent.type(reason(), REASON);
    expect(send()).toBeEnabled();
  });

  it("keeps Request image off for a reason and no image, and for a blank image", async () => {
    setup();
    await userEvent.type(reason(), REASON);
    expect(send()).toBeDisabled();
    await userEvent.type(image(), "   ");
    expect(send()).toBeDisabled();
  });

  it("counts the reason against 500", async () => {
    setup();
    await userEvent.type(reason(), "x".repeat(133));
    expect(screen.getByText("133 of 500")).toBeInTheDocument();
  });

  it("sends the trimmed image and reason, and the ticket only when there is one", async () => {
    setup();
    createMock.mockResolvedValueOnce(sentRequest());
    await fill(`  ${REF}  `, `  ${REASON}  `);
    await userEvent.type(ticket(), " INC-2210 ");
    await userEvent.click(send());
    expect(createMock).toHaveBeenCalledTimes(1);
    expect(createMock).toHaveBeenCalledWith({ kind: "image", image: REF, reason: REASON, ticket: "INC-2210" });
  });

  it("sends no ticket key without a ticket, and never a unit or a duration", async () => {
    setup();
    createMock.mockResolvedValueOnce(sentRequest());
    await fill();
    await userEvent.click(send());
    expect(createMock.mock.calls[0][0]).toEqual({ kind: "image", image: REF, reason: REASON });
    expect(Object.keys(createMock.mock.calls[0][0])).toEqual(["kind", "image", "reason"]);
  });
});

describe("ImageRequestDialog: sending and sent", () => {
  it("turns both buttons off and reads Sending… while the request is out", async () => {
    setup();
    const d = deferred<GovernanceRequest>();
    createMock.mockReturnValueOnce(d.promise);
    await fill();
    await userEvent.click(send());
    expect(screen.getByRole("button", { name: "Sending…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    await act(async () => d.resolve(sentRequest()));
    expect(screen.queryByRole("button", { name: "Sending…" })).toBeNull();
  });

  it("names who decides, ending with a security admin, calls onSent once, and Done closes", async () => {
    const { onSent, onOpenChange } = setup();
    const r = sentRequest();
    createMock.mockResolvedValueOnce(r);
    await fill();
    await userEvent.click(send());
    expect(await screen.findByText("Request sent")).toBeInTheDocument();
    expect(
      screen.getByText(
        `It is waiting for admins of Payments, the platform-leads group or a security admin. You'll see the answer under Approvals. If nobody decides it by ${absoluteTime(r.expires_at)}, it expires.`,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Why do you need it?")).toBeNull();
    expect(screen.queryByRole("button", { name: "Request image" })).toBeNull();
    expect(onSent).toHaveBeenCalledTimes(1);
    expect(onSent).toHaveBeenCalledWith(r);
    expect(onOpenChange).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onSent).toHaveBeenCalledTimes(1);
  });
});

describe("ImageRequestDialog: refusals", () => {
  async function refuse(err: unknown) {
    const { onOpenChange, onSent } = setup();
    createMock.mockRejectedValueOnce(err);
    await fill();
    await userEvent.type(ticket(), "INC-2210");
    await userEvent.click(send());
    return { onOpenChange, onSent };
  }

  function expectFieldsKept() {
    expect(screen.getByText("New image request")).toBeInTheDocument();
    expect(image()).toHaveValue(REF);
    expect(reason()).toHaveValue(REASON);
    expect(ticket()).toHaveValue("INC-2210");
    expect(send()).toBeEnabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled();
  }

  it("image_request_not_pinned: the server's sentence under Image, in red, with the field marked", async () => {
    const sentence = "request an image by digest: <name>@sha256:<64 hex>";
    const { onSent } = await refuse(new HttpError(400, sentence, "image_request_not_pinned"));
    expect(await screen.findByText(sentence)).toHaveClass("text-danger");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(image()).toBeInvalid();
    expectFieldsKept();
    expect(onSent).not.toHaveBeenCalled();
  });

  it("a new send clears the field refusal", async () => {
    await refuse(new HttpError(400, "request an image by digest: <name>@sha256:<64 hex>", "image_request_not_pinned"));
    await screen.findByText(/^request an image by digest/);
    createMock.mockReturnValueOnce(deferred<GovernanceRequest>().promise);
    await userEvent.click(send());
    expect(screen.queryByText(/^request an image by digest/)).toBeNull();
    expect(image()).not.toBeInvalid();
  });

  it("image_already_granted: the sentence in the red note, the reference breaking anywhere and the rest at words", async () => {
    await refuse(new HttpError(409, `${REF} is already granted to you`, "image_already_granted"));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
    expect(note).toHaveTextContent(`${REF} is already granted to you`);
    const sentence = note.querySelector("span:not(.font-semibold)") as HTMLElement;
    expect(sentence).not.toHaveClass("font-mono");
    expect(within(note).getByText(REF)).toHaveClass("break-all");
    expectFieldsKept();
  });

  it("governance_change_pending: the console's sentence, not the server's change id", async () => {
    await refuse(new HttpError(409, "a change to this target is already waiting for approval: c-77", "governance_change_pending"));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
    expect(within(note).getByText("You already have a request waiting for this image. Withdraw it first, or wait for an answer.")).toBeInTheDocument();
    expect(screen.queryByText(/c-77/)).toBeNull();
    expectFieldsKept();
  });

  it.each([
    ["image_request_not_offered", 400, "your default unit offers no image requests; ask an admin for an image grant"],
    ["image_request_not_offered", 400, "image grants are off in this deployment, so an approved image would not launch; ask an admin"],
    ["deciders_unreadable", 503, "the deciders rules could not be read; try again"],
    ["request_reason_required", 400, "a request needs a reason"],
    ["request_reason_too_long", 400, "a request's reason is at most 500 characters"],
    ["something_else", 500, "the server said no"],
  ])("%s (%i): the server's sentence under the red heading", async (code, status, sentence) => {
    await refuse(new HttpError(status, sentence, code));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
    expect(within(note).getByText(sentence)).toBeInTheDocument();
    expect(within(note).getByText(sentence)).not.toHaveClass("font-mono");
    expectFieldsKept();
  });

  it("a failure that is not an HttpError shows its message", async () => {
    await refuse(new Error("network down"));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
    expect(within(note).getByText("network down")).toBeInTheDocument();
    expectFieldsKept();
  });
});

describe("ImageRequestDialog: closing", () => {
  it("opens fresh after a close: nothing typed carries over", async () => {
    const { rerender, onOpenChange, onSent } = setup();
    await fill();
    rerender(<ImageRequestDialog open={false} onOpenChange={onOpenChange} onSent={onSent} />);
    expect(screen.queryByRole("dialog")).toBeNull();
    rerender(<ImageRequestDialog open onOpenChange={onOpenChange} onSent={onSent} />);
    expect(image()).toHaveValue("");
    expect(reason()).toHaveValue("");
    expect(send()).toBeDisabled();
  });

  it("Cancel asks to close", async () => {
    const { onOpenChange } = setup();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
