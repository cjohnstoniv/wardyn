/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// UserDrivesCard — one component in two homes (the setup Workspaces step and
// Settings' fifth card, user-drives mock state 9). What is pinned here is what
// the card DECIDES, not how it looks:
//
//   1. it counts ALLOCATIONS, never people — a group allocation is one row and
//      Wardyn holds no directory read, so "14 people" would be a claim,
//   2. no drives is its own sentence, and the link still goes to /drives,
//   3. a failed read leaves the summary ABSENT rather than claiming "No drives
//      yet." over an unloaded snapshot,
//   4. SUPER only — a security admin has nothing to act on there and sees no
//      card at all, which is the same reason /drives has no nav item,
//   5. ZERO teal, both homes: the funnel's Next and Settings' delegation own
//      the affirmative action.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getDrivesMock = vi.fn();
vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return { ...actual, drives: { ...actual.drives, getDrives: () => getDrivesMock() } };
});

const navigateMock = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => navigateMock };
});

import type { UserDriveGrant, UserDriveListItem } from "../../../lib/api/drives";
import { DRIVES } from "../../../lib/user-drives-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { UserDrivesCard } from "./user-drives-card";

const rows = (drives: number, grants: number) => ({
  drives: Array.from({ length: drives }, (_, i) => ({ id: `d${i}` }) as UserDriveListItem),
  grants: Array.from({ length: grants }, (_, i) => ({ id: `g${i}` }) as UserDriveGrant),
  host_roots_configured: false,
  runner_target: "docker",
});

function renderCard(operator = true) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={operator}>
        <UserDrivesCard />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getDrivesMock.mockReset();
  navigateMock.mockReset();
});

describe("UserDrivesCard", () => {
  it("summarises drives and ALLOCATIONS, and its link goes to /drives", async () => {
    getDrivesMock.mockResolvedValue(rows(2, 4));
    renderCard();

    expect(
      await screen.findByText(DRIVES.CARD_SUMMARY(DRIVES.CARD_DRIVES(2), DRIVES.CARD_ALLOCATIONS(4))),
    ).toBeInTheDocument();
    expect(screen.getByText(DRIVES.TITLE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.CARD_LEAD)).toBeInTheDocument();

    await userEvent.click(screen.getByText(DRIVES.CARD_OPEN));
    expect(navigateMock).toHaveBeenCalledWith("/drives");
  });

  it("pluralises through the inline ternary — one drive, one allocation", async () => {
    getDrivesMock.mockResolvedValue(rows(1, 1));
    renderCard();
    expect(
      await screen.findByText(DRIVES.CARD_SUMMARY(DRIVES.CARD_DRIVES(1), DRIVES.CARD_ALLOCATIONS(1))),
    ).toBeInTheDocument();
  });

  it("no drives is its own sentence, and the link still goes where the teal New drive lives", async () => {
    getDrivesMock.mockResolvedValue(rows(0, 0));
    renderCard();
    expect(await screen.findByText(DRIVES.CARD_EMPTY)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.CARD_OPEN)).toBeInTheDocument();
  });

  it("a failed read leaves the summary absent rather than claiming there are no drives", async () => {
    getDrivesMock.mockRejectedValue(new Error("boom"));
    renderCard();
    expect(await screen.findByText(DRIVES.CARD_OPEN)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.CARD_EMPTY)).not.toBeInTheDocument();
  });

  it("renders nothing for a caller without the SUPER tier, and asks the server nothing", () => {
    getDrivesMock.mockResolvedValue(rows(2, 4));
    renderCard(false);
    expect(screen.queryByTestId("user-drives-card")).not.toBeInTheDocument();
    expect(getDrivesMock).not.toHaveBeenCalled();
  });

  it("carries zero teal in either home", async () => {
    getDrivesMock.mockResolvedValue(rows(2, 4));
    renderCard();
    await screen.findByText(DRIVES.CARD_OPEN);
    const teal = screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
    expect(teal).toEqual([]);
  });
});
