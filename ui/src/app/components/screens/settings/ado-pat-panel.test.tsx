/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The person's Azure DevOps card on a row that creates tokens or takes a pasted
// one (#1428, #1430): mock states 4, 8b, 9, 10 and 11, through the card's own
// shell so the deep link and the collapse behave as they do for the sign-in lane.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { HttpError } from "../../../lib/api/core";
import { ADO } from "../../../lib/ado-entra-copy";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { expandCard } from "../../../lib/test-dom";

const connectMock = vi.fn();
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: connectMock, connectFallback: connectMock, blockedUrl: null }),
}));

const disconnectMock = vi.fn();
const storeMock = vi.fn();
vi.mock("../../../lib/api/ado-pat", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/ado-pat")>("../../../lib/api/ado-pat");
  return { ...actual, adoPat: { ...actual.adoPat, disconnect: () => disconnectMock(), storeOwnToken: (b: unknown) => storeMock(b) } };
});

import { AdoConnectionCard } from "./ado-connection";

const ORG = "https://dev.azure.com/wardyn-live-test";
const at = (h: number, m: number) => new Date(2026, 8, 29, h, m).toISOString();

function status(access: Partial<SCMAccessPAT>): SetupStatus {
  return {
    ready: true,
    checks: [],
    auth: { mode: "local" },
    runner: { driver: "docker", confinement_classes: [] },
    providers: [],
    secrets: { present: [] },
    age_key: { durable: true },
    has_runs: false,
    scm_access: { org: ORG, kind: "azure_devops", ...access },
  } as unknown as SetupStatus;
}

function renderCard(access: Partial<SCMAccessPAT>, onChanged = vi.fn()) {
  render(
    <MemoryRouter initialEntries={["/account"]}>
      <AdoConnectionCard status={status(access)} onChanged={onChanged} />
    </MemoryRouter>,
  );
  return onChanged;
}

beforeEach(() => {
  connectMock.mockReset();
  disconnectMock.mockReset();
  storeMock.mockReset();
});

describe("a row that creates a token for each run", () => {
  const minted = { token_mode: "minted_pat" } as const;

  it("4a: not connected explains the connection and offers Connect Azure DevOps, which reloads on success", async () => {
    connectMock.mockResolvedValueOnce(true);
    const onChanged = renderCard({ ...minted, state: "not_configured" });
    expect(screen.getByText(ADO_PAT.CHIP_NOT_CONNECTED)).toBeInTheDocument();
    await expandCard("Azure DevOps");
    expect(screen.getByText(ADO_PAT.MEMBER_NOT_CONNECTED)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(connectMock).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
  });

  it("4c: connected says how the tokens are made and names the last one", async () => {
    renderCard({ ...minted, state: "live", source: "org", last_token: { created_at: at(9, 2), revoked_at: at(9, 41) } });
    expect(screen.getByText(ADO_PAT.CHIP_CONNECTED)).toBeInTheDocument();
    await expandCard("Azure DevOps");
    expect(screen.getByText(ADO_PAT.MEMBER_CONNECTED)).toBeInTheDocument();
    expect(screen.getByText("Last token: created 09:02, revoked 09:41.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).not.toBeInTheDocument();
  });

  it("4d: Disconnect asks first, says what it revokes, and only then disconnects", async () => {
    disconnectMock.mockResolvedValue(undefined);
    const onChanged = renderCard({ ...minted, state: "live", source: "org" });
    await expandCard("Azure DevOps");
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(ADO_PAT.DISCONNECT_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(ADO_PAT.DISCONNECT_BODY)).toBeInTheDocument();
    expect(disconnectMock).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }));
    await waitFor(() => expect(disconnectMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it("4d: Cancel leaves the connection alone", async () => {
    renderCard({ ...minted, state: "live", source: "org" });
    await expandCard("Azure DevOps");
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: ADO_PAT.DISCONNECT_CANCEL }));
    expect(disconnectMock).not.toHaveBeenCalled();
  });

  it("9: sign in again says the organisation asked, and offers Connect", async () => {
    renderCard({ ...minted, state: "expired_signin", cause: "ended", source: "org" });
    expect(screen.getByText(ADO_PAT.CHIP_SIGN_IN_AGAIN)).toBeInTheDocument();
    await expandCard("Azure DevOps");
    expect(screen.getByText(ADO_PAT.SIGN_IN_AGAIN_BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).toBeInTheDocument();
  });

  it("9: a stored sign-in that lacks the token permissions asks the same, so Microsoft can ask for consent", async () => {
    renderCard({ ...minted, state: "expired_signin", cause: "permissions_missing", source: "org" });
    await expandCard("Azure DevOps");
    expect(screen.getByText(ADO_PAT.SIGN_IN_AGAIN_BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).toBeInTheDocument();
  });

  it("a row the console cannot redeem gives a member the chip and a member sentence, never the admin's, and no Connect", async () => {
    renderCard({ ...minted, state: "expired_signin", cause: "ado_pat_needs_console_app" });
    expect(screen.getByText(ADO_PAT.CHIP_NOT_CONNECTED)).toBeInTheDocument();
    await expandCard("Azure DevOps");
    expect(screen.getByText(ADO_PAT.MEMBER_NEEDS_ADMIN)).toBeInTheDocument();
    expect(screen.queryByText(ADO_PAT.NO_CLIENT_SECRET)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).not.toBeInTheDocument();
  });

  it("Disconnect that the daemon fails to do says so and leaves the card connected (a missing route is an error, not success)", async () => {
    disconnectMock.mockRejectedValue(new HttpError(404, "not found"));
    const onChanged = renderCard({ ...minted, state: "live", source: "org" });
    await expandCard("Azure DevOps");
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }));
    await waitFor(() => expect(disconnectMock).toHaveBeenCalledTimes(1));
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("8b: blocked by the organisation (the blocked cause) names the fix and offers no button", async () => {
    renderCard({ ...minted, state: "expired_signin", cause: "blocked" });
    expect(screen.getByText(ADO_PAT.CHIP_BLOCKED)).toBeInTheDocument();
    await expandCard("Azure DevOps");
    expect(screen.getByText(ADO_PAT.BLOCKED_BODY)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).not.toBeInTheDocument();
  });
});

describe("a row where each person adds their own token", () => {
  const own: Partial<SCMAccessPAT> = {
    token_mode: "own_pat",
    max_days: 30,
    token_scopes: ["Code (Read & write)", "Project and Team (Read)", "Work Items (Read)"],
  };

  it("10b: no token yet offers Add your personal access token, and the dialog says what to create", async () => {
    renderCard({ ...own, state: "not_configured" });
    await expandCard("Azure DevOps");
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA }));
    const dialog = await screen.findByRole("dialog", { name: ADO_PAT.OWN_DIALOG_TITLE });
    expect(within(dialog).getByText(ADO_PAT.OWN_DIALOG_LEAD("wardyn-live-test", 30))).toBeInTheDocument();
    expect(within(dialog).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Code (Read & write)",
      "Project and Team (Read)",
      "Work Items (Read)",
    ]);
    expect(within(dialog).getByRole("link", { name: ADO_PAT.OWN_OPEN_TOKENS })).toHaveAttribute(
      "href",
      "https://dev.azure.com/wardyn-live-test/_usersSettings/tokens",
    );
    expect(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_TOKEN)).toBeInTheDocument();
    expect(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_EXPIRES)).toBeInTheDocument();
  });

  it("10c: Add token sends the token and the expiry, then closes and reloads", async () => {
    storeMock.mockResolvedValue(undefined);
    const onChanged = renderCard({ ...own, state: "not_configured" });
    await expandCard("Azure DevOps");
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA }));
    const dialog = await screen.findByRole("dialog");
    const add = within(dialog).getByRole("button", { name: ADO_PAT.OWN_DIALOG_ADD });
    expect(add).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_TOKEN), "pasted-secret");
    await userEvent.type(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_EXPIRES), "2026-10-27");
    await userEvent.click(add);
    await waitFor(() => expect(storeMock).toHaveBeenCalledWith({ org: ORG, token: "pasted-secret", expires_on: "2026-10-27" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  async function refuse(reason: string) {
    storeMock.mockImplementation(() => Promise.reject(new HttpError(422, "refused", reason)));
    renderCard({ ...own, state: "not_configured" });
    await expandCard("Azure DevOps");
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_TOKEN), "x");
    await userEvent.type(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_EXPIRES), "2027-03-01");
    await userEvent.click(within(dialog).getByRole("button", { name: ADO_PAT.OWN_DIALOG_ADD }));
    return dialog;
  }

  it("11: another account's token is refused under the Token field, never naming the account", async () => {
    const dialog = await refuse("ado_own_pat_identity_mismatch");
    expect(await within(dialog).findByText(ADO_PAT.OWN_MISMATCH)).toBeInTheDocument();
    expect(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_TOKEN)).toHaveAttribute("aria-invalid", "true");
  });

  it("11: an expiry past the administrator's limit is refused under Expires on", async () => {
    const dialog = await refuse("ado_own_pat_expiry_too_long");
    expect(await within(dialog).findByText(ADO_PAT.OWN_TOO_LONG(30))).toBeInTheDocument();
    expect(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_EXPIRES)).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_TOKEN)).not.toHaveAttribute("aria-invalid", "true");
  });

  it("11: a token Azure DevOps did not accept is refused under the Token field", async () => {
    const dialog = await refuse("ado_own_pat_rejected");
    expect(await within(dialog).findByText(ADO_PAT.OWN_REJECTED)).toBeInTheDocument();
  });

  it("Cancel clears the pasted token: reopening the dialog starts empty", async () => {
    renderCard({ ...own, state: "not_configured" });
    await expandCard("Azure DevOps");
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA }));
    let dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_TOKEN), "pasted-secret");
    await userEvent.type(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_EXPIRES), "2026-10-27");
    await userEvent.click(within(dialog).getByRole("button", { name: ADO_PAT.OWN_DIALOG_CANCEL }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA }));
    dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_TOKEN)).toHaveValue("");
    expect(within(dialog).getByLabelText(ADO_PAT.OWN_FIELD_EXPIRES)).toHaveValue("");
  });

  it("10: expiring counts the days and offers Replace token", async () => {
    const soon = new Date(Date.now() + 3 * 24 * 60 * 60 * 1000);
    const day = `${soon.getFullYear()}-${String(soon.getMonth() + 1).padStart(2, "0")}-${String(soon.getDate()).padStart(2, "0")}`;
    renderCard({ ...own, state: "expiring", expires_on: day });
    expect(screen.getByText(ADO_PAT.OWN_CHIP_EXPIRING(3))).toBeInTheDocument();
    await expandCard("Azure DevOps");
    expect(screen.getByText(/^Your token for wardyn-live-test expires on /)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO_PAT.OWN_REPLACE })).toBeInTheDocument();
  });

  it("10: expired says runs cannot reach Azure DevOps, and offers Add", async () => {
    renderCard({ ...own, state: "expired_signin", cause: "token_expired" });
    expect(screen.getByText(ADO_PAT.OWN_CHIP_EXPIRED)).toBeInTheDocument();
    await expandCard("Azure DevOps");
    expect(screen.getByText(ADO_PAT.OWN_EXPIRED_BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA })).toBeInTheDocument();
  });

  it("10: an Azure DevOps Server row is titled as one and says git only", async () => {
    renderCard({ ...own, org: "https://tfs.example.com/collection", state: "live", expires_on: "2026-10-27" });
    await expandCard(ADO_PAT.OWN_SERVER_TITLE);
    expect(screen.getByText(ADO_PAT.OWN_SERVER_NOTE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO_PAT.OWN_REPLACE })).toBeInTheDocument();
  });
});

it("the legacy card offers no Connect for a cause signing in cannot fix (no token_mode on the wire)", async () => {
  renderCard({ state: "expired_signin", cause: "blocked" });
  await expandCard("Azure DevOps");
  expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
});

it("a row on the Entra sign-in lane keeps its own card", async () => {
  renderCard({ token_mode: "bearer", state: "not_configured", cause: "row_is_newer" });
  await expandCard("Azure DevOps");
  expect(screen.queryByTestId("ado-pat-card")).not.toBeInTheDocument();
});
