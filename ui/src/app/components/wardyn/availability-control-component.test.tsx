/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// G-1 (component lanes): an org component starts restricted with nobody listed.
// AvailabilityControl, given `nobodyYet`, says "Nobody yet" and never sends
// PUT /permissions/availability with restricted:true while the list is empty.
// The generic control (no `nobodyYet`) keeps the server-refusal path its own
// suite pins.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { aheadByHours } from "../../lib/test-clock";

const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
const upsertGrantMock = vi.fn();
const listUserTypesMock = vi.fn();
vi.mock("../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/permissions")>("../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      listUserTypes: (...a: unknown[]) => listUserTypesMock(...a),
    },
  };
});

import { AvailabilityControl } from "./availability-control";

const ID = "5b1f3c1e-7f0a-4a52-9d57-0c1d2e3f4a5b";
const NOBODY_YET = "Nobody yet. Add a person, group or user type below to let them use this component.";
const ADD_FIRST = "Add at least one person, group or user type before choosing Only, or nobody could use this.";
const view = (restricted: boolean, allowed_by: unknown[] = []) => ({ kind: "component", value: ID, restricted, allowed_by });
const DEV = {
  id: "g1",
  subject_type: "user_type" as const,
  subject: "developer",
  capability: "component",
  value: ID,
  effect: "allow" as const,
  created_at: aheadByHours(-1),
};

const control = (nobodyYet = true) => (
  <AvailabilityControl kind="component" value={ID} {...(nobodyYet && { nobodyYet: { note: NOBODY_YET, addFirst: ADD_FIRST } })} />
);

beforeEach(() => {
  getAvailabilityMock.mockReset();
  putAvailabilityMock.mockReset();
  upsertGrantMock.mockReset();
  listUserTypesMock.mockReset().mockResolvedValue([]);
});

describe("AvailabilityControl, the org component family", () => {
  it("restricted with nobody listed reads Nobody yet, with Only these selected", async () => {
    getAvailabilityMock.mockResolvedValue(view(true));
    render(control());
    expect(await screen.findByTestId("availability-nobody")).toHaveTextContent(NOBODY_YET);
    expect(screen.getByRole("radio", { name: "Only these" })).toBeChecked();
  });

  it("says nothing of the kind once someone is listed", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV]));
    render(control());
    await screen.findByText("developer");
    expect(screen.queryByTestId("availability-nobody")).toBeNull();
  });

  it("choosing Only these with nobody listed sends no PUT and says to add someone first", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    render(control());
    await screen.findByText("Available to");
    await userEvent.click(screen.getByRole("radio", { name: "Only these" }));
    expect(await screen.findByText(ADD_FIRST)).toBeInTheDocument();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
    expect(screen.getByRole("radio", { name: "Everyone" })).toBeChecked();
  });

  it("with someone listed, Only these still sends the PUT", async () => {
    getAvailabilityMock.mockResolvedValue(view(false, [DEV]));
    putAvailabilityMock.mockResolvedValue(view(true, [DEV]));
    render(control());
    await screen.findByText("developer");
    await userEvent.click(screen.getByRole("radio", { name: "Only these" }));
    await waitFor(() => expect(putAvailabilityMock).toHaveBeenCalledWith("component", ID, true));
  });

  it("without nobodyYet the generic control still sends the PUT and shows the server's refusal", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    putAvailabilityMock.mockRejectedValue(new Error(ADD_FIRST));
    render(control(false));
    await screen.findByText("Available to");
    await userEvent.click(screen.getByRole("radio", { name: "Only these" }));
    await waitFor(() => expect(putAvailabilityMock).toHaveBeenCalledWith("component", ID, true));
    expect(screen.queryByTestId("availability-nobody")).toBeNull();
  });
});
