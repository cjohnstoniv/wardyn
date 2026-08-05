/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The tier-1 DIRECTORIES & REPOS library tab: rows with status/contract/used-by,
// an add dialog that upserts, and the delete-in-use refusal that names the
// attaching workspaces before offering the detach-everywhere escape.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../lib/api/core";
import type { Source, Workspace } from "../../lib/types";

const listSourcesMock = vi.fn();
const createSourceMock = vi.fn();
const scanSourceMock = vi.fn();
const deleteSourceMock = vi.fn();
vi.mock("../../lib/api/sources", () => ({
  sourcesApi: {
    listSources: (...a: unknown[]) => listSourcesMock(...a),
    createSource: (...a: unknown[]) => createSourceMock(...a),
    scanSource: (...a: unknown[]) => scanSourceMock(...a),
    deleteSource: (...a: unknown[]) => deleteSourceMock(...a),
  },
  baseImagesApi: {},
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { SourcesLibrary, contractSummary, sourceUsage } from "./sources-library";

function src(over: Partial<Source> = {}): Source {
  return {
    id: "s-1",
    kind: "repo",
    locator: "github.com/acme/payments",
    ref: "main",
    name: "payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  };
}

function ws(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "w-1",
    name: "w",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  } as Workspace;
}

beforeEach(() => {
  listSourcesMock.mockReset().mockResolvedValue([]);
  createSourceMock.mockReset();
  scanSourceMock.mockReset();
  deleteSourceMock.mockReset();
});

describe("sourceUsage / contractSummary — pure helpers", () => {
  it("counts attaching workspaces per source id", () => {
    const usage = sourceUsage([
      ws({ attachments: [{ source_id: "a" }, { source_id: "b" }] }),
      ws({ id: "w-2", attachments: [{ source_id: "a" }, { ephemeral: true }] }),
    ]);
    expect(usage.get("a")).toBe(2);
    expect(usage.get("b")).toBe(1);
  });

  it("summarizes the source's own contract by lane", () => {
    expect(contractSummary(src())).toBe("No contract yet");
    expect(
      contractSummary(
        src({
          requirements: {
            "secret:STRIPE_KEY": { level: "required", provenance: "operator_set" },
            "egress:api.stripe.com": { level: "required", provenance: "scan_seeded" },
            "write:/x": { level: "optional", provenance: "operator_set" },
          },
        }),
      ),
    ).toBe("2 required · 1 optional");
  });
});

describe("SourcesLibrary", () => {
  it("renders library rows with identity, status word, contract and used-by", async () => {
    listSourcesMock.mockResolvedValue([
      src({
        requirements: { "egress:api.stripe.com": { level: "required", provenance: "scan_seeded" } },
      }),
    ]);
    render(<SourcesLibrary workspaces={[ws({ attachments: [{ source_id: "s-1" }] })]} />);

    expect(await screen.findByText("payments")).toBeInTheDocument();
    expect(screen.getByText(/repo · github.com\/acme\/payments @main/)).toBeInTheDocument();
    expect(screen.getByText("Usable")).toBeInTheDocument();
    expect(screen.getByText("1 required")).toBeInTheDocument();
    expect(screen.getByText("1 workspace")).toBeInTheDocument();
  });

  it("adds a repo to the library through the dialog", async () => {
    createSourceMock.mockResolvedValue(src({ id: "s-9", name: "lib" }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /add directory or repo/i }));
    await user.click(screen.getByRole("radio", { name: /repository/i }));
    await user.type(screen.getByLabelText(/repo slug or clone url/i), "acme/lib");
    await user.type(screen.getByLabelText(/git ref/i), "main");
    await user.click(screen.getByRole("button", { name: /add to library/i }));

    await waitFor(() =>
      expect(createSourceMock).toHaveBeenCalledWith({
        kind: "repo",
        locator: "acme/lib",
        ref: "main",
        name: undefined,
      }),
    );
  });

  it("delete-in-use surfaces the server's 409 naming workspaces, then forces on the explicit escape", async () => {
    listSourcesMock.mockResolvedValue([src()]);
    deleteSourceMock
      .mockRejectedValueOnce(new HttpError(409, "source is attached by 2 workspace(s): pay, billing"))
      .mockResolvedValueOnce(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /actions for payments/i }));
    await user.click(screen.getByRole("menuitem", { name: /delete/i }));
    const dialog = screen.getByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Delete" }));

    expect(await within(dialog).findByText(/pay, billing/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: /detach everywhere & delete/i }));
    await waitFor(() => expect(deleteSourceMock).toHaveBeenLastCalledWith("s-1", true));
  });

  it("scan action hits the per-source endpoint", async () => {
    listSourcesMock.mockResolvedValue([src()]);
    scanSourceMock.mockResolvedValue({});
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /actions for payments/i }));
    await user.click(screen.getByRole("menuitem", { name: /scan/i }));
    await waitFor(() => expect(scanSourceMock).toHaveBeenCalledWith("s-1"));
  });
});
