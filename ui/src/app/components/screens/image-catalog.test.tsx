/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The tier-2 BASE IMAGE catalog tab: rows with kind/image/used-by, the add
// dialog (custom recipes split steps by line), and the delete-in-use refusal
// whose escape honestly means "fall back to the derived recommended build".
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../lib/api/core";
import type { BaseImageEntry, Workspace } from "../../lib/types";

const listBaseImagesMock = vi.fn();
const createBaseImageMock = vi.fn();
const deleteBaseImageMock = vi.fn();
vi.mock("../../lib/api/sources", () => ({
  sourcesApi: {},
  baseImagesApi: {
    listBaseImages: (...a: unknown[]) => listBaseImagesMock(...a),
    createBaseImage: (...a: unknown[]) => createBaseImageMock(...a),
    deleteBaseImage: (...a: unknown[]) => deleteBaseImageMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { ImageCatalog, imageUsage } from "./image-catalog";

function img(over: Partial<BaseImageEntry> = {}): BaseImageEntry {
  return {
    id: "i-1",
    kind: "custom",
    name: "go-build-base",
    image: "ubuntu:24.04",
    steps: ["RUN apt-get update"],
    created_at: "",
    updated_at: "",
    ...over,
  };
}

beforeEach(() => {
  listBaseImagesMock.mockReset().mockResolvedValue([]);
  createBaseImageMock.mockReset();
  deleteBaseImageMock.mockReset();
});

describe("imageUsage", () => {
  it("counts workspaces per referenced catalog id (NULL = derived, uncounted)", () => {
    const ws = (base?: string): Workspace =>
      ({ id: Math.random().toString(), base_image_id: base }) as unknown as Workspace;
    const usage = imageUsage([ws("a"), ws("a"), ws(undefined)]);
    expect(usage.get("a")).toBe(2);
    expect(usage.size).toBe(1);
  });
});

describe("ImageCatalog", () => {
  it("renders catalog rows with kind, image + steps, and used-by", async () => {
    listBaseImagesMock.mockResolvedValue([img()]);
    render(
      <ImageCatalog workspaces={[{ id: "w", base_image_id: "i-1" } as unknown as Workspace]} />,
    );
    expect(await screen.findByText("go-build-base")).toBeInTheDocument();
    expect(screen.getByText("Custom recipe")).toBeInTheDocument();
    expect(screen.getByText(/ubuntu:24.04 · 1 step/)).toBeInTheDocument();
    expect(screen.getByText("1 workspace")).toBeInTheDocument();
  });

  it("adds a custom recipe, splitting build steps by line", async () => {
    createBaseImageMock.mockResolvedValue(img({ id: "i-9", name: "new" }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<ImageCatalog workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /add base image/i }));
    await user.click(screen.getByRole("radio", { name: /custom recipe/i }));
    await user.type(screen.getByLabelText("Base image"), "ubuntu:24.04");
    await user.type(screen.getByLabelText(/build steps/i), "RUN apt-get update\n\nENV CGO_ENABLED=1");
    await user.click(screen.getByRole("button", { name: /add to catalog/i }));

    await waitFor(() =>
      expect(createBaseImageMock).toHaveBeenCalledWith({
        kind: "custom",
        image: "ubuntu:24.04",
        name: undefined,
        steps: ["RUN apt-get update", "ENV CGO_ENABLED=1"],
      }),
    );
  });

  it("delete-in-use surfaces the 409 and the recommended-build fallback framing before forcing", async () => {
    listBaseImagesMock.mockResolvedValue([img()]);
    deleteBaseImageMock
      .mockRejectedValueOnce(new HttpError(409, "base image is used by 1 workspace(s): pay"))
      .mockResolvedValueOnce(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<ImageCatalog workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /actions for go-build-base/i }));
    await user.click(screen.getByRole("menuitem", { name: /delete/i }));
    const dialog = screen.getByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Delete" }));

    expect(await within(dialog).findByText(/used by 1 workspace/)).toBeInTheDocument();
    expect(within(dialog).getByText(/derived recommended build/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: /detach everywhere & delete/i }));
    await waitFor(() => expect(deleteBaseImageMock).toHaveBeenLastCalledWith("i-1", true));
  });
});
