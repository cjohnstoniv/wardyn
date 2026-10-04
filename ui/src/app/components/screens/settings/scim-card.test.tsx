/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.8.6 scim-a7: the Settings SCIM card renders each state of M5's approved copy. The strings here are
// written out, not read from lib/scim-copy.ts, so a drift in the copy module fails this suite.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SCIMStatus } from "../../../lib/types";
import { expandCard } from "../../../lib/test-dom";

const getStatusMock = vi.fn();
vi.mock("../../../lib/api/scim", () => ({ scim: { getStatus: () => getStatusMock() } }));

import { ScimCard } from "./scim-card";

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const DAY = 86_400_000;

function status(over: Partial<SCIMStatus> = {}): SCIMStatus {
  return {
    configured: true,
    last_token_slot: "primary",
    purge_after_seconds: 30 * 86400,
    keep_workspaces: false,
    deactivated: [],
    pending: [],
    drives: [],
    ...over,
  };
}

async function open(s: SCIMStatus) {
  getStatusMock.mockReset().mockResolvedValue(s);
  render(<ScimCard />);
  await screen.findByRole("button", { name: /^SCIM provisioning\s*(Not set up|\d+ deactivated)/ });
  await expandCard("SCIM provisioning");
  return screen.getByTestId("scim-card");
}

beforeEach(() => getStatusMock.mockReset());

describe("ScimCard", () => {
  it("not set up: says so, names the fix, links the runbook, and still shows the endpoint", async () => {
    const card = await open(status({ configured: false, last_token_slot: "", purge_after_seconds: 0 }));
    expect(screen.getByRole("button", { name: /^SCIM provisioning\s*Not set up/ })).toBeInTheDocument();
    expect(within(card).getByText(/^Your identity provider tells Wardyn when someone leaves or changes groups\./)).toBeInTheDocument();
    expect(
      within(card).getByText("Not set up. Someone removed in your identity provider keeps their sessions, tokens and runs here until an admin revokes them."),
    ).toBeInTheDocument();
    expect(
      within(card).getByText(/^Set WARDYN_SCIM_TOKEN on wardynd, then point your Entra ID provisioning at the endpoint below\./),
    ).toBeInTheDocument();
    expect(within(card).getByRole("link", { name: "Leaver runbook" })).toHaveAttribute("href", expect.stringContaining("OPERATIONS.md#leavers-and-scim"));
    expect(within(card).getByText("Endpoint")).toBeInTheDocument();
    expect(within(card).getByText(/\/scim\/v2$/)).toBeInTheDocument();
    expect(within(card).queryByText("Token last matched")).toBeNull();
    expect(within(card).queryByText("No one is deactivated.")).toBeNull();
  });

  it("set up and quiet: the facts, and an empty line under each list", async () => {
    const card = await open(status());
    expect(screen.getByRole("button", { name: /^SCIM provisioning\s*0 deactivated · 0 unfinished/ })).toBeInTheDocument();
    expect(within(card).getByText("Endpoint")).toBeInTheDocument();
    expect(within(card).getByText("Token last matched")).toBeInTheDocument();
    expect(within(card).getByText("Primary token")).toBeInTheDocument();
    expect(within(card).queryByText(/Your identity provider is using the next token/)).toBeNull();
    expect(within(card).getByText("Purge delay")).toBeInTheDocument();
    expect(within(card).getByText("30 days")).toBeInTheDocument();
    expect(within(card).getByText("Workspaces on purge")).toBeInTheDocument();
    expect(within(card).getByText("Handed to the operator")).toBeInTheDocument();
    expect(within(card).getByText("No one is deactivated.")).toBeInTheDocument();
    expect(within(card).getByText("Nothing unfinished.")).toBeInTheDocument();
    expect(within(card).getByText("No drives to reclaim.")).toBeInTheDocument();
    expect(within(card).queryByText(/^Wardyn resumes from the failed step/)).toBeNull();
    expect(within(card).queryByText(/^A purge lists these/)).toBeNull();
  });

  it("set up with work to show: the three lists, their hints, the summary counts", async () => {
    const card = await open(
      status({
        last_token_slot: "next",
        keep_workspaces: true,
        deactivated: [
          { person: "ada@example.com", deactivated_at: ago(2 * DAY), purge_after: new Date(Date.now() + 28 * DAY).toISOString() },
          { person: "bob@example.com", deactivated_at: ago(DAY) },
        ],
        pending: [
          { person: "ada@example.com", step: "kill_run", last_error: "runner: teardown timed out" },
          { person: "ada@example.com", step: "sweep", last_error: "store: busy" },
          { person: "bob@example.com", step: "audit_deprovision", last_error: "audit sink unavailable" },
        ],
        drives: [{ person: "ada@example.com", drive: "Team share", purged_at: ago(DAY) }],
      }),
    );
    // Two people are unfinished, not three rows.
    expect(screen.getByRole("button", { name: /^SCIM provisioning\s*2 deactivated · 2 unfinished/ })).toBeInTheDocument();
    expect(within(card).getByText("Next token")).toBeInTheDocument();
    expect(
      within(card).getByText(
        "Your identity provider is using the next token. Move it to WARDYN_SCIM_TOKEN and unset WARDYN_SCIM_TOKEN_NEXT to finish the rotation.",
      ),
    ).toBeInTheDocument();
    expect(within(card).getByText("Left as they are")).toBeInTheDocument();

    const tables = within(card).getAllByRole("table");
    expect(tables).toHaveLength(3);
    expect(within(tables[0]).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Person", "Deactivated", "Purge scheduled"]);
    expect(within(tables[0]).getByText("ada@example.com")).toBeInTheDocument();
    expect(within(tables[0]).getByText("Not scheduled")).toBeInTheDocument();

    expect(within(tables[1]).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Person", "Step", "Last error"]);
    expect(within(tables[1]).getByText("Stop runs")).toBeInTheDocument();
    expect(within(tables[1]).getByText("Revoke tokens and SSH keys")).toBeInTheDocument();
    // A step the copy does not name shows as its own key.
    expect(within(tables[1]).getByText("audit_deprovision")).toBeInTheDocument();
    expect(within(tables[1]).getByText("runner: teardown timed out")).toBeInTheDocument();
    expect(
      within(card).getByText(
        "Wardyn resumes from the failed step each time your identity provider retries. To cut someone off now, erase their credentials under Credentials and kill their runs.",
      ),
    ).toBeInTheDocument();

    expect(within(tables[2]).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Person", "Drive", "Purged"]);
    expect(within(tables[2]).getByText("Team share")).toBeInTheDocument();
    expect(
      within(card).getByText(
        `A purge lists these and leaves them in place. Reclaim each one as its drive's "When a person leaves" setting says.`,
      ),
    ).toBeInTheDocument();
  });

  it("purge delay off: says only a delete purges, and nothing is scheduled", async () => {
    const card = await open(
      status({ purge_after_seconds: 0, deactivated: [{ person: "ada@example.com", deactivated_at: ago(DAY) }] }),
    );
    expect(within(card).getByText("Off — only a delete from your identity provider purges")).toBeInTheDocument();
    expect(within(card).getByText("Not scheduled")).toBeInTheDocument();
  });

  it("one day reads as a day, not days", async () => {
    const card = await open(status({ purge_after_seconds: 86400 }));
    expect(within(card).getByText("1 day")).toBeInTheDocument();
  });

  it("a failed read shows the error and a retry, with no summary", async () => {
    getStatusMock.mockReset().mockRejectedValueOnce(new Error("down")).mockResolvedValueOnce(status());
    render(<ScimCard />);
    await expandCard("SCIM provisioning");
    expect(await screen.findByText("Something went wrong")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^SCIM provisioning$/ })).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Token last matched")).toBeInTheDocument();
  });
});
