/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin > Credentials, the State column and the revoked-rows toggle on the admin-minted token list
// (#1508, mock M-SVC v2). The rest of the list is covered by credentials.test.tsx.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AdminMintedToken } from "../../lib/api/credentials";
import { aheadByHours } from "../../lib/test-clock";
import { MINTED } from "../wardyn/copy/credentials";

const listMintedMock = vi.fn();
vi.mock("../../lib/api/credentials", () => ({
  credentials: {
    listAdminMintedTokens: (...a: unknown[]) => listMintedMock(...a),
    revokeToken: vi.fn(),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { AdminMintedTokens } from "./credentials-minted-tokens";

const token = (id: string, person: string, disposition: AdminMintedToken["disposition"], over: Partial<AdminMintedToken> = {}): AdminMintedToken => ({
  id,
  principal: `${person}-sub`,
  email: `${person}@example.com`,
  name: `job-${person}`,
  minted_by: "ops-admin@example.com",
  created_at: aheadByHours(-24 * 120),
  disposition,
  ...over,
});
const live = [
  token("m1", "alice", "needs_decision"),
  token("m2", "ben", "awaiting_owner"),
  token("m3", "cara", "linked_to_service_identity", { service_identity: "ci-nightly" }),
];
const revoked = [
  token("m4", "dana", "replaced_by_owner", { revoked_at: aheadByHours(-2 * 24) }),
  token("m5", "eli", "moved_to_service_identity", { revoked_at: aheadByHours(-9 * 24), service_identity: "etl-service" }),
  token("m6", "fay", "revoked", { revoked_at: aheadByHours(-30 * 24) }),
];

beforeEach(() => {
  listMintedMock.mockReset().mockImplementation(async (includeRevoked?: boolean) => (includeRevoked ? [...live, ...revoked] : live));
});
afterEach(() => vi.clearAllMocks());

const user = userEvent.setup({ pointerEventsCheck: 0 });
const rowOf = (email: string) => within(screen.getByText(email).closest("tr")!);

describe("the State column", () => {
  it("says where each live token stands, with what is true now and the identity it was linked to", async () => {
    render(<AdminMintedTokens />);
    await screen.findByText("alice@example.com");
    expect(screen.getByRole("columnheader", { name: MINTED.COL_STATE })).toBeInTheDocument();
    expect(rowOf("alice@example.com").getByText("Needs a decision")).toBeInTheDocument();
    expect(rowOf("alice@example.com").getByText("Still works as the person. Invite them, or move it to a service identity.")).toBeInTheDocument();
    expect(rowOf("ben@example.com").getByText("Waiting for the owner")).toBeInTheDocument();
    expect(rowOf("ben@example.com").getByText("An invitation to replace it is pending.")).toBeInTheDocument();
    const cara = rowOf("cara@example.com");
    expect(cara.getByText("Linked to a service identity")).toBeInTheDocument();
    expect(cara.getByText("Still works. Revoke it once the job uses the new token.")).toBeInTheDocument();
    expect(cara.getByText(/Service identity:/)).toHaveTextContent("Service identity: ci-nightly");
    expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(3);
  });

  it("counts only the tokens that still work, and points to the CLI for what the console does not do", async () => {
    render(<AdminMintedTokens />);
    await screen.findByText("alice@example.com");
    expect(screen.getByText(MINTED.CHIP(3))).toBeInTheDocument();
    expect(screen.getByText(MINTED.COUNT(3))).toBeInTheDocument();
    expect(screen.getByText(/The console does neither\./)).toHaveTextContent(
      "Invite the owner with wardyn person invite, or move the token with wardyn token move. The console does neither.",
    );
  });

  it("hides revoked tokens until asked, then asks the server for them and says how many came with", async () => {
    render(<AdminMintedTokens />);
    await screen.findByText("alice@example.com");
    expect(screen.queryByText("dana@example.com")).not.toBeInTheDocument();
    expect(listMintedMock).toHaveBeenLastCalledWith(false);
    await user.click(screen.getByRole("checkbox", { name: MINTED.INCLUDE_REVOKED }));
    expect(await screen.findByText("dana@example.com")).toBeInTheDocument();
    expect(listMintedMock).toHaveBeenLastCalledWith(true);
    expect(screen.getByRole("status")).toHaveTextContent("Showing 3 revoked tokens as well.");
    expect(rowOf("dana@example.com").getByText("Replaced by the owner")).toBeInTheDocument();
    expect(rowOf("dana@example.com").getByText("Revoked 2 days ago, when they claimed their own.")).toBeInTheDocument();
    expect(rowOf("eli@example.com").getByText("Moved to a service identity")).toBeInTheDocument();
    expect(rowOf("eli@example.com").getByText("Revoked 9 days ago, after the move.")).toBeInTheDocument();
    expect(rowOf("eli@example.com").getByText(/Service identity:/)).toHaveTextContent("Service identity: etl-service");
    expect(rowOf("fay@example.com").getByText("Revoked 30 days ago, with no replacement.")).toBeInTheDocument();
    // a revoked row has nothing to revoke, and the "still work" count stays the live ones
    expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(3);
    expect(screen.getByText(MINTED.CHIP(3))).toBeInTheDocument();
    // and the toggle keeps its place
    expect(screen.getByRole("checkbox", { name: MINTED.INCLUDE_REVOKED })).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: MINTED.INCLUDE_REVOKED }));
    await waitFor(() => expect(screen.queryByText("dana@example.com")).not.toBeInTheDocument());
  });

  it("a slow earlier read never overwrites the toggle's current choice (FUG-L356)", async () => {
    render(<AdminMintedTokens />);
    await screen.findByText("alice@example.com");
    const toggle = () => screen.getByRole("checkbox", { name: MINTED.INCLUDE_REVOKED });
    await user.click(toggle()); // on: answers at once
    expect(await screen.findByText("dana@example.com")).toBeInTheDocument();
    let release: (rows: AdminMintedToken[]) => void = () => {};
    listMintedMock.mockImplementationOnce(() => new Promise<AdminMintedToken[]>((r) => { release = r; }));
    await user.click(toggle()); // off: the slow read
    await user.click(toggle()); // on again: answers at once
    await waitFor(() => expect(listMintedMock).toHaveBeenLastCalledWith(true));
    expect(await screen.findByText("dana@example.com")).toBeInTheDocument();
    release(live); // the old read ends last, with no revoked rows
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByText("dana@example.com")).toBeInTheDocument();
    expect(toggle()).toBeChecked();
  });

  it("with only revoked tokens, says none work and keeps the toggle", async () => {
    listMintedMock.mockImplementation(async (includeRevoked?: boolean) => (includeRevoked ? revoked : []));
    render(<AdminMintedTokens />);
    expect(await screen.findByText(MINTED.EMPTY)).toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: MINTED.INCLUDE_REVOKED }));
    expect(await screen.findByText(MINTED.NONE_LIVE)).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Showing 3 revoked tokens as well.");
    expect(screen.queryByText(MINTED.CHIP(0))).not.toBeInTheDocument();
  });

  it("shows a state this console does not know as written, in a neutral chip, and a row with none shows nothing", async () => {
    listMintedMock.mockResolvedValue([
      token("x1", "gus", "parked" as AdminMintedToken["disposition"]),
      token("x2", "hal", undefined),
    ]);
    render(<AdminMintedTokens />);
    await screen.findByText("gus@example.com");
    expect(rowOf("gus@example.com").getByText("parked")).toBeInTheDocument();
    expect(rowOf("hal@example.com").queryByText(/Needs|Waiting|Linked|Revoked|Moved|Replaced/)).not.toBeInTheDocument();
  });

  it("an empty list keeps the toggle beside the empty line", async () => {
    listMintedMock.mockResolvedValue([]);
    render(<AdminMintedTokens />);
    expect(await screen.findByText(MINTED.EMPTY)).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: MINTED.INCLUDE_REVOKED })).toBeInTheDocument();
  });
});
