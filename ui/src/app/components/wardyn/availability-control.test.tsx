/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AvailabilityControl (UT-7b) — the "Available to" widget every resource
// editor embeds. Scoped to this file's own logic (the restricted-bit
// round-trip, the empty-"Only" 400 rendered verbatim, adding/removing an
// audience); the real-backend round trip through an actual resource editor
// is providers.spec.ts's own Playwright pin.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../lib/api/core";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
const upsertGrantMock = vi.fn();
const deleteGrantMock = vi.fn();
vi.mock("../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/permissions")>("../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      deleteGrant: (...a: unknown[]) => deleteGrantMock(...a),
    },
  };
});

import { AVAILABILITY } from "../../lib/availability-copy";
import { AvailabilityControl } from "./availability-control";

const EVERYONE = { kind: "workspace_provider", value: "azure_devops", restricted: false, allowed_by: [] };
const ONLY_WITH_ONE = {
  kind: "workspace_provider",
  value: "azure_devops",
  restricted: true,
  allowed_by: [
    {
      id: "g1",
      subject_type: "user_type" as const,
      subject: "developer",
      capability: "workspace_provider",
      value: "azure_devops",
      effect: "allow" as const,
      created_at: "2026-09-01T00:00:00Z",
    },
  ],
};

beforeEach(() => {
  getAvailabilityMock.mockReset();
  putAvailabilityMock.mockReset();
  upsertGrantMock.mockReset();
  deleteGrantMock.mockReset();
});

describe("AvailabilityControl", () => {
  it("loads the resource's current state and renders Everyone/Only", async () => {
    getAvailabilityMock.mockResolvedValue(EVERYONE);
    render(<AvailabilityControl kind="workspace_provider" value="azure_devops" />);

    expect(await screen.findByText(AVAILABILITY.LABEL)).toBeInTheDocument();
    expect(getAvailabilityMock).toHaveBeenCalledWith("workspace_provider", "azure_devops");
    expect(screen.getByRole("button", { name: AVAILABILITY.EVERYONE })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: AVAILABILITY.ONLY })).toHaveAttribute("aria-pressed", "false");
  });

  it("turning Only on with nobody listed renders the server's 400 verbatim and stays Everyone", async () => {
    getAvailabilityMock.mockResolvedValue(EVERYONE);
    putAvailabilityMock.mockRejectedValue(new HttpError(400, "Add at least one person, group or user type before choosing Only, or nobody could use this."));
    render(<AvailabilityControl kind="workspace_provider" value="azure_devops" />);
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ONLY }));

    expect(await screen.findByText(/Add at least one person, group or user type/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: AVAILABILITY.EVERYONE })).toHaveAttribute("aria-pressed", "true");
  });

  it("adding a user type posts the grant naming this exact kind/value, then reloads", async () => {
    getAvailabilityMock.mockResolvedValueOnce(EVERYONE).mockResolvedValueOnce(ONLY_WITH_ONE);
    upsertGrantMock.mockResolvedValue({ grant: ONLY_WITH_ONE.allowed_by[0], updated: false });
    render(<AvailabilityControl kind="workspace_provider" value="azure_devops" />);
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "developer");
    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }));

    await waitFor(() =>
      expect(upsertGrantMock).toHaveBeenCalledWith({
        subject_type: "user_type",
        subject: "developer",
        capability: "workspace_provider",
        value: "azure_devops",
        effect: "allow",
      }),
    );
    expect(await screen.findByText(/developer/)).toBeInTheDocument();
    expect(getAvailabilityMock).toHaveBeenCalledTimes(2);
  });

  it("removing an audience deletes its grant by id, then reloads", async () => {
    getAvailabilityMock.mockResolvedValueOnce(ONLY_WITH_ONE).mockResolvedValueOnce(EVERYONE);
    deleteGrantMock.mockResolvedValue(undefined);
    render(<AvailabilityControl kind="workspace_provider" value="azure_devops" />);
    await screen.findByText(/developer/);

    await userEvent.click(screen.getByRole("button", { name: /Remove.*developer/i }));

    await waitFor(() => expect(deleteGrantMock).toHaveBeenCalledWith("g1"));
    expect(getAvailabilityMock).toHaveBeenCalledTimes(2);
  });

  it("turning Only on with an existing allow row succeeds and flips the segment", async () => {
    getAvailabilityMock.mockResolvedValue(EVERYONE);
    putAvailabilityMock.mockResolvedValue(ONLY_WITH_ONE);
    render(<AvailabilityControl kind="workspace_provider" value="azure_devops" />);
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ONLY }));

    expect(putAvailabilityMock).toHaveBeenCalledWith("workspace_provider", "azure_devops", true);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: AVAILABILITY.ONLY })).toHaveAttribute("aria-pressed", "true"),
    );
  });
});
