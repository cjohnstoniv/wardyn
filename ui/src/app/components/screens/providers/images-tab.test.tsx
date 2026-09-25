/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Images tab (#923, editors.html section 3): the catalog's rows with
// their Available to, the empty state, and Add image asking who gets it.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";

const listMock = vi.fn();
const addMock = vi.fn();
vi.mock("../../../lib/api/base-images", () => ({
  baseImages: { list: (...a: unknown[]) => listMock(...a), add: (...a: unknown[]) => addMock(...a) },
}));
const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
const upsertGrantMock = vi.fn();
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      listUserTypes: async () => [
        { id: "portfolio-manager", name: "Portfolio manager", description: "", priority: 0, built_in: false },
      ],
    },
  };
});

import { AVAILABILITY, IMAGES } from "../../../lib/availability-copy";
import { ImagesTab } from "./images-tab";

const entry = (id: string, name: string, image: string) => ({
  id,
  kind: "byo" as const,
  name,
  image,
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
});
const TOOLBOX = entry("b1", "dev-toolbox", "ghcr.io/acme/dev-toolbox:1.4");
const ML = entry("b2", "python-ml", "ghcr.io/acme/python-ml:3.12");
const UNKNOWN_TYPE = 'The user type "portfolio-mgr" doesn\'t exist. Create it under User types first.';

const teal = () =>
  screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));

beforeEach(() => {
  listMock.mockReset();
  addMock.mockReset();
  getAvailabilityMock.mockReset();
  putAvailabilityMock.mockReset();
  upsertGrantMock.mockReset();
  getAvailabilityMock.mockImplementation(async (kind: string, value: string) => ({
    kind,
    value,
    restricted: false,
    allowed_by: [],
  }));
});

describe("ImagesTab", () => {
  it("lists each catalog image with its Available to, keyed by the reference, starting at Admins only", async () => {
    listMock.mockResolvedValue([TOOLBOX, ML]);
    render(<ImagesTab />);

    expect(await screen.findByText("dev-toolbox")).toBeInTheDocument();
    expect(screen.getByText("ghcr.io/acme/python-ml:3.12")).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toHaveLength(2));
    for (const r of screen.getAllByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })) expect(r).toBeChecked();
    expect(getAvailabilityMock).toHaveBeenCalledWith("image", "ghcr.io/acme/dev-toolbox:1.4");
    expect(getAvailabilityMock).toHaveBeenCalledWith("image", "ghcr.io/acme/python-ml:3.12");
    expect(screen.getAllByText(AVAILABILITY.IMAGE_HINT)).toHaveLength(2);
    expect(screen.getAllByText(AVAILABILITY.IMAGE_NOTE)).toHaveLength(2);
    // The lead, with --image as a literal; one teal: Add image.
    expect(screen.getByText("--image").closest("p")?.textContent).toBe(IMAGES.LEAD);
    expect(teal().map((b) => b.textContent)).toEqual([IMAGES.ADD_CTA]);
  });

  it("empty: says so, and carries Add image as its one teal", async () => {
    listMock.mockResolvedValue([]);
    render(<ImagesTab />);

    expect(await screen.findByText(IMAGES.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(IMAGES.EMPTY_BODY)).toBeInTheDocument();
    expect(teal().map((b) => b.textContent)).toEqual([IMAGES.ADD_CTA]);
  });

  it("Add image asks who gets it, starting at Admins only, and writes the image, then the list, then Only these", async () => {
    listMock.mockResolvedValue([]);
    const calls: string[] = [];
    addMock.mockImplementation(async (image: string) => {
      calls.push(`add ${image}`);
      return entry("b9", "dev-toolbox", image);
    });
    upsertGrantMock.mockImplementation(async (g: { subject: string; capability: string; value: string }) =>
      calls.push(`grant ${g.subject} ${g.capability} ${g.value}`),
    );
    putAvailabilityMock.mockImplementation(async (kind: string, value: string, restricted: boolean) =>
      calls.push(`restrict ${kind} ${value} ${restricted}`),
    );
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: IMAGES.ADD_CTA })).toBeInTheDocument();
    const ref = within(dialog).getByLabelText(IMAGES.REF);
    expect(ref).toHaveFocus();
    expect(ref).toBeRequired();
    expect(within(dialog).getByText(IMAGES.REF_HINT)).toBeInTheDocument();
    expect(within(dialog).getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    expect(within(dialog).getByText(AVAILABILITY.IMAGE_HINT)).toBeInTheDocument();

    await userEvent.type(ref, "ghcr.io/acme/dev-toolbox:1.4");
    await userEvent.type(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    await userEvent.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await userEvent.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.click(within(dialog).getByRole("button", { name: IMAGES.ADD_CTA }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls).toEqual([
      "add ghcr.io/acme/dev-toolbox:1.4",
      "grant portfolio-manager image ghcr.io/acme/dev-toolbox:1.4",
      "restrict image ghcr.io/acme/dev-toolbox:1.4 true",
    ]);
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it("Admins only on the form writes only the image", async () => {
    listMock.mockResolvedValue([]);
    addMock.mockResolvedValue(entry("b9", "x", "ghcr.io/acme/x:1"));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");

    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), "ghcr.io/acme/x:1{Enter}");

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(addMock).toHaveBeenCalledWith("ghcr.io/acme/x:1");
    expect(upsertGrantMock).not.toHaveBeenCalled();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
  });

  it("the image saved but the list was refused: stays open on it with the server's sentence and what holds now", async () => {
    listMock.mockResolvedValue([]);
    addMock.mockResolvedValue(TOOLBOX);
    upsertGrantMock.mockRejectedValue(new HttpError(400, UNKNOWN_TYPE));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");

    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), TOOLBOX.image);
    await userEvent.type(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-mgr");
    await userEvent.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await userEvent.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.click(within(dialog).getByRole("button", { name: IMAGES.ADD_CTA }));

    expect(await within(dialog).findByText(AVAILABILITY.CREATE_PARTIAL_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(UNKNOWN_TYPE)).toBeInTheDocument();
    expect(within(dialog).getByText(AVAILABILITY.CREATE_PARTIAL_IMAGE)).toBeInTheDocument();
    expect(within(dialog).getByRole("heading", { name: TOOLBOX.name })).toBeInTheDocument();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
    // The control below shows what the server holds, live.
    await waitFor(() => expect(getAvailabilityMock).toHaveBeenCalledWith("image", TOOLBOX.image));
    expect(await within(dialog).findByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    expect(within(dialog).queryByRole("button", { name: IMAGES.ADD_CTA })).not.toBeInTheDocument();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it("a refused image shows the server's sentence in the dialog and writes no list", async () => {
    listMock.mockResolvedValue([]);
    addMock.mockRejectedValue(new HttpError(400, "image must not contain whitespace or control characters"));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");

    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), "bad{Enter}");

    expect(await within(dialog).findByText("image must not contain whitespace or control characters")).toBeInTheDocument();
    expect(within(dialog).getByLabelText(IMAGES.REF)).toHaveAttribute("aria-invalid", "true");
    expect(upsertGrantMock).not.toHaveBeenCalled();
  });
});
