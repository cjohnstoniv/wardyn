/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One dialog for both kinds: the choice shows only when both are offered, and each kind opens its own dialog.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ElevationOffer, GovernanceRequest } from "../../../lib/types/governance-requests";

const createMock = vi.fn();
vi.mock("../../../lib/api/governance-requests", () => ({ requests: { list: vi.fn(), create: (...a: unknown[]) => createMock(...a) } }));

import { RequestDialog } from "./request-dialog";

const payments: ElevationOffer = { unit_id: "u1", unit_name: "Payments", max_elevation: "4h" };

type Props = Partial<React.ComponentProps<typeof RequestDialog>>;
function setup(props: Props = {}) {
  const onOpenChange = vi.fn();
  const onSent = vi.fn();
  const el = (p: Props) => (
    <RequestDialog open onOpenChange={onOpenChange} initialKind="image" offers={[payments]} imageOffered onSent={onSent} {...p} />
  );
  const utils = render(el(props));
  return { ...utils, onOpenChange, onSent, rerender: (p: Props) => utils.rerender(el(p)) };
}

const kind = (name: string) => screen.getByRole("radio", { name });

beforeEach(() => createMock.mockReset());

describe("RequestDialog: both kinds offered", () => {
  it("opens on the kind asked for, with the choice in the image dialog", () => {
    setup({ initialKind: "image" });
    expect(screen.getByText("New image request")).toBeInTheDocument();
    const group = screen.getByRole("radiogroup", { name: "What do you need?" });
    expect(within(group).getAllByRole("radio").map((r) => r.closest("label")?.textContent)).toEqual(["Elevation", "Image"]);
    expect(kind("Image")).toBeChecked();
    expect(kind("Elevation")).not.toBeChecked();
  });

  it("draws the keyboard focus ring inside the clipped kind choice", () => {
    setup({ initialKind: "image" });
    const track = kind("Image").closest("label")!.querySelector("span")!;
    expect(track.className).toContain("peer-focus-visible:ring-inset");
  });

  it("opens on Elevation with the choice in the elevation dialog", () => {
    setup({ initialKind: "elevation" });
    expect(screen.getByText("New elevation request")).toBeInTheDocument();
    expect(screen.getByRole("radiogroup", { name: "What do you need?" })).toBeInTheDocument();
    expect(kind("Elevation")).toBeChecked();
  });

  it("picking Elevation shows the elevation request fields, and Image brings the image field back", async () => {
    setup({ initialKind: "image" });
    await userEvent.click(kind("Elevation"));
    expect(screen.getByText("New elevation request")).toBeInTheDocument();
    expect(screen.queryByText("New image request")).toBeNull();
    expect(screen.getByRole("combobox", { name: "Unit" })).toHaveTextContent("Payments");
    expect(kind("Elevation")).toBeChecked();
    await userEvent.click(kind("Image"));
    expect(screen.getByText("New image request")).toBeInTheDocument();
    expect(screen.getByLabelText("Image", { selector: "textarea" })).toBeInTheDocument();
  });

  it("what was typed does not carry over to the other kind", async () => {
    setup({ initialKind: "image" });
    await userEvent.type(screen.getByLabelText("Why do you need it?"), "for the build");
    await userEvent.click(kind("Elevation"));
    expect(screen.getByLabelText("Why do you need it?")).toHaveValue("");
  });

  it("starts from the kind it is asked for on each open", async () => {
    const { rerender } = setup({ initialKind: "image" });
    await userEvent.click(kind("Elevation"));
    rerender({ open: false });
    rerender({ open: true, initialKind: "image" });
    expect(screen.getByText("New image request")).toBeInTheDocument();
    expect(kind("Image")).toBeChecked();
  });

  it("a sent request from either dialog reaches onSent", async () => {
    const { onSent } = setup({ initialKind: "image" });
    const r = { id: "c1", kind: "image", deciders: [], expires_at: new Date().toISOString() } as unknown as GovernanceRequest;
    createMock.mockResolvedValueOnce(r);
    await userEvent.type(screen.getByLabelText("Image", { selector: "textarea" }), `x@sha256:${"a".repeat(64)}`);
    await userEvent.type(screen.getByLabelText("Why do you need it?"), "for the build");
    await userEvent.click(screen.getByRole("button", { name: "Request image" }));
    await screen.findByText("Request sent");
    expect(onSent).toHaveBeenCalledWith(r);
  });
});

describe("RequestDialog: a request that is out", () => {
  it.each([
    ["image", "Request image", "Elevation"],
    ["elevation", "Request elevation", "Image"],
  ] as const)("keeps the %s dialog and its kind until the answer shows", async (initialKind, send, other) => {
    let answer: ((r: GovernanceRequest) => void) | undefined;
    createMock.mockReturnValue(new Promise((done) => (answer = done)));
    setup({ initialKind });
    if (initialKind === "image") await userEvent.type(screen.getByLabelText("Image", { selector: "textarea" }), `x@sha256:${"a".repeat(64)}`);
    await userEvent.type(screen.getByLabelText("Why do you need it?"), "for the build");
    await userEvent.click(screen.getByRole("button", { name: send }));
    expect(kind(other)).toBeDisabled();
    await userEvent.click(kind(other));
    expect(screen.getByRole("button", { name: /Sending/ })).toBeInTheDocument();
    await act(async () => answer?.({ id: "c1", kind: initialKind, deciders: [], expires_at: new Date().toISOString() } as unknown as GovernanceRequest));
    expect(await screen.findByText("Request sent")).toBeInTheDocument();
  });
});

describe("RequestDialog: one kind offered", () => {
  it("images only: the image dialog and no choice", () => {
    setup({ offers: [], imageOffered: true, initialKind: "image" });
    expect(screen.getByText("New image request")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup")).toBeNull();
  });

  it("elevation only: the elevation dialog and no choice", () => {
    setup({ offers: [payments], imageOffered: false, initialKind: "elevation" });
    expect(screen.getByText("New elevation request")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "What do you need?" })).toBeNull();
  });

  it("closed draws neither dialog", () => {
    setup({ open: false });
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
