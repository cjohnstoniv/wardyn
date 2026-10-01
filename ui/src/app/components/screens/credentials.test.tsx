/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// CredentialsScreen — the admin "Stored credentials" inventory (design F-1/
// F-2, packet F §4) and its erase flow (F-5/F-6, packet F §5). Every
// assertion reads its expected string from the copy modules (INVENTORY/ERASE,
// wardyn/copy/credentials.ts).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const listInventoryMock = vi.fn();
const eraseMock = vi.fn();
const listMintedMock = vi.fn();
const revokeTokenMock = vi.fn();
vi.mock("../../lib/api/credentials", () => ({
  credentials: {
    listInventory: (...a: unknown[]) => listInventoryMock(...a),
    erase: (...a: unknown[]) => eraseMock(...a),
    listAdminMintedTokens: (...a: unknown[]) => listMintedMock(...a),
    revokeToken: (...a: unknown[]) => revokeTokenMock(...a),
  },
}));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) },
}));

import { HttpError } from "../../lib/api/core";
import type { CredentialInventory } from "../../lib/api/credentials";
import type { SetupStatus } from "../../lib/types";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { INVENTORY, ERASE, MINTED } from "../wardyn/copy/credentials";
import { CredentialsScreen } from "./credentials";

function renderScreen(opts: { status?: SetupStatus | null; operator?: boolean; securityOperator?: boolean } = {}) {
  const { status = null, operator = true, securityOperator = true } = opts;
  return render(
    <MemoryRouter>
      <ModelAccessProvider status={status} onRefresh={() => {}}>
        <OperatorProvider operator={operator} securityOperator={securityOperator} operatorResolved principal="admin@corp.example">
          <CredentialsScreen />
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}

function inventory(rows: CredentialInventory["credentials"], byProvider: Record<string, number> = {}): CredentialInventory {
  const counts = { people: new Set(rows.map((r) => r.person)).size, credentials: rows.length, by_provider: byProvider };
  return { credentials: rows, counts };
}

const aliceRow = {
  person: "sub-alice",
  email: "alice@corp.example",
  provider: "corp-gw",
  provider_name: "Corp gateway",
  state: "stored" as const,
  store: "pg" as const,
  added_at: "2026-09-03T00:00:00Z",
  last_used_at: "2026-09-25T00:00:00Z",
};

beforeEach(() => {
  listInventoryMock.mockReset();
  eraseMock.mockReset();
  listMintedMock.mockReset();
  listMintedMock.mockResolvedValue([]);
  revokeTokenMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
});

describe("the inventory", () => {
  it("lists a person's row with their email and the provider's name (design F-2)", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    renderScreen();
    expect(await screen.findByText("Stored credentials")).toBeInTheDocument();
    const row = (await screen.findByText("alice@corp.example")).closest("tr")!;
    expect(within(row).getByText("sub-alice")).toBeInTheDocument();
    expect(within(row).getByText("Corp gateway")).toBeInTheDocument();
    expect(screen.getByText(INVENTORY.SUMMARY(1, 1))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Corp gateway · 1" })).toBeInTheDocument();
  });

  it("no one has stored a credential yet", async () => {
    listInventoryMock.mockResolvedValue(inventory([], { "corp-gw": 0 }));
    renderScreen();
    expect(await screen.findByText(INVENTORY.EMPTY_TITLE)).toBeInTheDocument();
  });

  it("no model providers yet: an operator gets Open Settings, a security admin does not", async () => {
    listInventoryMock.mockResolvedValue(inventory([], {}));
    renderScreen({ operator: false, securityOperator: true });
    await screen.findByText("No model providers yet");
    expect(screen.queryByRole("link", { name: INVENTORY.OPEN_SETTINGS })).toBeNull();
  });

  it("the store's own 503 (no credential metadata) shows the server's sentence, with no Retry", async () => {
    const msg = "This deployment's secret store keeps no credential metadata, so there is nothing to list.";
    listInventoryMock.mockRejectedValue(new HttpError(503, msg));
    renderScreen();
    expect(await screen.findByText(msg)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("a read failure gets the generic error state, with Retry", async () => {
    listInventoryMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    renderScreen();
    const retry = await screen.findByRole("button", { name: "Retry" });
    listInventoryMock.mockResolvedValueOnce(inventory([aliceRow], { "corp-gw": 1 }));
    await userEvent.click(retry);
    expect(await screen.findByText("alice@corp.example")).toBeInTheDocument();
  });

  it("groups a person's rows: one Erase per person, not per credential", async () => {
    const second = { ...aliceRow, provider: "bedrock-prod", provider_name: "Bedrock (prod)" };
    listInventoryMock.mockResolvedValue(inventory([aliceRow, second], { "corp-gw": 1, "bedrock-prod": 1 }));
    renderScreen();
    await screen.findByText("alice@corp.example");
    // Both providers show, but the person's identity and the Erase action
    // appear exactly once — the table is grouped by person because erase
    // works per person (packet F §4 a colfoot), never per credential.
    expect(screen.getAllByText("alice@corp.example")).toHaveLength(1);
    expect(screen.getByText("Corp gateway")).toBeInTheDocument();
    expect(screen.getByText("Bedrock (prod)")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: INVENTORY.ERASE_ROW })).toHaveLength(1);
  });

  it("a zero-credential provider chip falls back to the raw id when no row named it", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1, "unnamed-prov": 0 }));
    renderScreen();
    await screen.findByText("alice@corp.example");
    expect(screen.getByRole("button", { name: "unnamed-prov · 0" })).toBeInTheDocument();
  });

  it("the footer names the store and links to Audit, even when rows mix more than one store", async () => {
    const vaultRow = { ...aliceRow, person: "sub-bob", email: "bob@corp.example", store: "azurekv" as const };
    listInventoryMock.mockResolvedValue(inventory([aliceRow, vaultRow], { "corp-gw": 2 }));
    renderScreen();
    await screen.findByText("alice@corp.example");
    // Mixed stores (pg + azurekv here): the "Stored in" column carries the
    // per-row distinction, but the footer still names ONE store's own audit
    // trail — Key Vault's, since it's the external one — never null.
    expect(screen.getByRole("columnheader", { name: INVENTORY.COL_STORE })).toBeInTheDocument();
    expect(screen.getByText(/Stored in Key Vault\. Every use is in the Audit log/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: INVENTORY.OPEN_AUDIT })).toHaveAttribute("href", "/admin/audit");
  });
});

describe("erasing a listed person (design F-5)", () => {
  it("stays disabled until the typed email matches, then shows the result", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    eraseMock.mockResolvedValue({ count: 4 });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_ROW }));
    const confirmBtn = screen.getByRole("button", { name: ERASE.CONFIRM });
    expect(confirmBtn).toBeDisabled();
    const field = screen.getByLabelText(ERASE.CONFIRM_LABEL("alice@corp.example"));
    await userEvent.type(field, "ALICE@corp.example");
    expect(confirmBtn).toBeEnabled();
    await userEvent.click(confirmBtn);
    expect(eraseMock).toHaveBeenCalledWith("sub-alice");
    expect(await screen.findByText("Erased 4 credentials for alice@corp.example.")).toBeInTheDocument();
    expect(screen.getByText(ERASE.DONE_AUDIT)).toBeInTheDocument();
  });

  it("count 0: nothing was stored, so nothing was erased", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    eraseMock.mockResolvedValue({ count: 0 });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_ROW }));
    await userEvent.type(screen.getByLabelText(ERASE.CONFIRM_LABEL("alice@corp.example")), "alice@corp.example");
    await userEvent.click(screen.getByRole("button", { name: ERASE.CONFIRM }));
    expect(await screen.findByText("Nothing was stored for alice@corp.example, so nothing was erased.")).toBeInTheDocument();
  });

  it("Key Vault, not purged: the recoverable-days line shows the server's own number", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    eraseMock.mockResolvedValue({ count: 2, store: "azurekv", purged: false, recoverable_days: 90 });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_ROW }));
    await userEvent.type(screen.getByLabelText(ERASE.CONFIRM_LABEL("alice@corp.example")), "alice@corp.example");
    await userEvent.click(screen.getByRole("button", { name: ERASE.CONFIRM }));
    expect(await screen.findByText(ERASE.KEY_VAULT_RECOVERABLE(90))).toBeInTheDocument();
  });

  it("the retention line names the deployment's own store (design F-6)", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    renderScreen({ status: { credential_storage: "key_vault" } as SetupStatus });
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_ROW }));
    expect(screen.getByText(ERASE.RETENTION_KEY_VAULT)).toBeInTheDocument();
  });

  it("§5 (d)'s order: the recoverable-days line comes before the audit line", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    eraseMock.mockResolvedValue({ count: 2, store: "azurekv", purged: false, recoverable_days: 90 });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_ROW }));
    await userEvent.type(screen.getByLabelText(ERASE.CONFIRM_LABEL("alice@corp.example")), "alice@corp.example");
    await userEvent.click(screen.getByRole("button", { name: ERASE.CONFIRM }));
    await screen.findByText(ERASE.KEY_VAULT_RECOVERABLE(90));
    const dialog = screen.getByRole("dialog");
    const order = Array.from(dialog.querySelectorAll("p")).map((el) => el.textContent);
    const recoverableIdx = order.indexOf(ERASE.KEY_VAULT_RECOVERABLE(90));
    const auditIdx = order.indexOf(ERASE.DONE_AUDIT);
    expect(recoverableIdx).toBeGreaterThanOrEqual(0);
    expect(auditIdx).toBeGreaterThan(recoverableIdx);
  });

  it("a >=500 erase failure shows ERASE.FAILED, never the server's bare 500 text", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    eraseMock.mockRejectedValue(new HttpError(500, "erase credentials"));
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_ROW }));
    await userEvent.type(screen.getByLabelText(ERASE.CONFIRM_LABEL("alice@corp.example")), "alice@corp.example");
    await userEvent.click(screen.getByRole("button", { name: ERASE.CONFIRM }));
    expect(await screen.findByRole("alert")).toHaveTextContent(ERASE.FAILED("alice@corp.example"));
    expect(screen.queryByText("erase credentials")).toBeNull();
  });
});

describe("erasing someone not listed (design F-5)", () => {
  it("by email or subject: Erase is enabled once something is typed, no second field", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    eraseMock.mockResolvedValue({ count: 1 });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_BY_EMAIL }));
    const confirmBtn = screen.getByRole("button", { name: ERASE.CONFIRM });
    expect(confirmBtn).toBeDisabled();
    await userEvent.type(screen.getByLabelText(ERASE.FIELD), "eli@example.com");
    expect(confirmBtn).toBeEnabled();
    expect(screen.queryByLabelText(/type .* to confirm/i)).toBeNull();
    await userEvent.click(confirmBtn);
    expect(eraseMock).toHaveBeenCalledWith("eli@example.com");
    expect(await screen.findByText("Erased 1 credential for eli@example.com.")).toBeInTheDocument();
  });

  it("an unresolved address: the server's own sentence, and the dialog stays open to retry", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    const msg =
      "That email address doesn't match anyone this deployment knows, so nothing was erased. Use the person's subject, as the Audit log shows it.";
    eraseMock.mockRejectedValue(new HttpError(422, msg));
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: INVENTORY.ERASE_BY_EMAIL }));
    await userEvent.type(screen.getByLabelText(ERASE.FIELD), "former@example.com");
    await userEvent.click(screen.getByRole("button", { name: ERASE.CONFIRM }));
    expect(await screen.findByRole("alert")).toHaveTextContent(msg);
    expect(screen.getByRole("button", { name: ERASE.CONFIRM })).toBeInTheDocument();
  });
});

// #1477: no one can create a token that acts as another person, so the console
// shows what an admin already created — metadata only, with Revoke — under the
// inventory. Strings are console-085-packet's, character for character.
describe("tokens an admin created for someone else (#1477)", () => {
  const dana = { id: "t-1", principal: "sub-dana", email: "dana@acme.io", name: "ci", minted_by: "sam@acme.io", created_at: "2026-09-14T00:00:00Z", last_used_at: "2026-09-29T00:00:00Z" };
  const lee = { id: "t-2", principal: "sub-lee", email: "lee@acme.io", name: "nightly", minted_by: "sam@acme.io", created_at: "2026-09-02T00:00:00Z" };
  const bare = { id: "t-3", principal: "sub-kim", name: "deploy", minted_by: "kim@acme.io", created_at: "2026-08-28T00:00:00Z" };

  async function section() {
    return (await screen.findByRole("heading", { name: "Tokens an admin created for someone else" })).closest("section")!;
  }

  it("canon strings", () => {
    expect(MINTED.TITLE).toBe("Tokens an admin created for someone else");
    expect(MINTED.CHIP(3)).toBe("3 still work");
    expect(MINTED.COUNT(3)).toBe(
      "3 tokens were created by an admin for another person. They keep working until revoked. Values are never shown.",
    );
    expect(MINTED.NOTE).toBe("No one can create a token that acts as another person. They sign in and create their own.");
    expect(MINTED.EMPTY).toBe("No admin has created a token for someone else.");
    expect([MINTED.COL_PERSON, MINTED.COL_TOKEN, MINTED.COL_CREATED_BY, MINTED.COL_ADDED, MINTED.COL_LAST_USED]).toEqual([
      "Person",
      "Token",
      "Created by",
      "Added",
      "Last used",
    ]);
    expect(MINTED.NEVER).toBe("Never");
    expect(MINTED.REVOKE).toBe("Revoke");
    expect(MINTED.REVOKE_TITLE("ci", "dana@acme.io")).toBe("Revoke ci for dana@acme.io?");
    expect(MINTED.REVOKE_BODY("dana@acme.io")).toBe("It stops working now. dana@acme.io can create their own after signing in.");
    expect([MINTED.REVOKE_CANCEL, MINTED.REVOKE_CONFIRM, MINTED.REVOKED_TOAST]).toEqual(["Cancel", "Revoke token", "Token revoked."]);
  });

  it("lists each token with its person, name, minter and times, a count, the note and a Revoke per row", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    listMintedMock.mockResolvedValue([dana, lee, bare]);
    renderScreen();
    const sec = within(await section());
    await sec.findByText("dana@acme.io");
    expect(sec.getByText("3 still work")).toBeInTheDocument();
    expect(sec.getByText(MINTED.COUNT(3))).toBeInTheDocument();
    expect(sec.getByText(MINTED.NOTE)).toBeInTheDocument();
    for (const h of ["Person", "Token", "Created by", "Added", "Last used"]) {
      expect(sec.getByRole("columnheader", { name: h })).toBeInTheDocument();
    }
    const row = sec.getByText("dana@acme.io").closest("tr")!;
    expect(within(row).getByText("ci")).toBeInTheDocument();
    expect(within(row).getByText("sam@acme.io")).toBeInTheDocument();
    // A person with no known email reads as their principal; a token never used reads Never.
    expect(sec.getByText("sub-kim")).toBeInTheDocument();
    expect(within(sec.getByText("lee@acme.io").closest("tr")!).getByText("Never")).toBeInTheDocument();
    expect(sec.getAllByRole("button", { name: "Revoke" })).toHaveLength(3);
    // The request is the inventory filter, and never a value.
    expect(listMintedMock).toHaveBeenCalledTimes(1);
    expect(document.body.textContent).not.toMatch(/wdn_/);
  });

  it("one token reads in the singular", async () => {
    listMintedMock.mockResolvedValue([dana]);
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    renderScreen();
    const sec = within(await section());
    expect(await sec.findByText("1 still works")).toBeInTheDocument();
    expect(sec.getByText(MINTED.COUNT(1))).toBeInTheDocument();
  });

  it("nothing to list: the empty line and the note, with no table", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    renderScreen();
    const sec = within(await section());
    expect(await sec.findByText("No admin has created a token for someone else.")).toBeInTheDocument();
    expect(sec.getByText(MINTED.NOTE)).toBeInTheDocument();
    expect(sec.queryByRole("table")).toBeNull();
    expect(sec.queryByText(/still work/)).toBeNull();
  });

  it("Revoke asks first, in the approved words, then revokes that token and reloads", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    listMintedMock.mockResolvedValueOnce([dana, lee]).mockResolvedValue([lee]);
    revokeTokenMock.mockResolvedValue(undefined);
    renderScreen();
    const sec = within(await section());
    await userEvent.click(within((await sec.findByText("dana@acme.io")).closest("tr")!).getByRole("button", { name: "Revoke" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Revoke ci for dana@acme.io?")).toBeInTheDocument();
    expect(within(dialog).getByText("It stops working now. dana@acme.io can create their own after signing in.")).toBeInTheDocument();
    expect(revokeTokenMock).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke token" }));
    await waitFor(() => expect(revokeTokenMock).toHaveBeenCalledWith("t-1"));
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith("Token revoked."));
    await waitFor(() => expect(sec.queryByText("dana@acme.io")).toBeNull());
    expect(sec.getByText("lee@acme.io")).toBeInTheDocument();
    expect(revokeTokenMock).toHaveBeenCalledTimes(1);
  });

  it("Cancel revokes nothing", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    listMintedMock.mockResolvedValue([dana]);
    renderScreen();
    const sec = within(await section());
    await userEvent.click(await sec.findByRole("button", { name: "Revoke" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Cancel" }));
    expect(revokeTokenMock).not.toHaveBeenCalled();
    expect(sec.getByText("dana@acme.io")).toBeInTheDocument();
  });

  it("a failed revoke says why and leaves the token listed", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    listMintedMock.mockResolvedValue([dana]);
    revokeTokenMock.mockRejectedValue(new HttpError(500, "boom"));
    renderScreen();
    const sec = within(await section());
    await userEvent.click(await sec.findByRole("button", { name: "Revoke" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Revoke token" }));
    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(sec.getByText("dana@acme.io")).toBeInTheDocument();
  });

  it("a failed read of the list has its own error state and Retry, and does not take the inventory down", async () => {
    listInventoryMock.mockResolvedValue(inventory([aliceRow], { "corp-gw": 1 }));
    listMintedMock.mockRejectedValueOnce(new HttpError(500, "boom")).mockResolvedValue([dana]);
    renderScreen();
    expect(await screen.findByText("alice@corp.example")).toBeInTheDocument();
    const sec = within(await section());
    await userEvent.click(await sec.findByRole("button", { name: "Retry" }));
    expect(await sec.findByText("dana@acme.io")).toBeInTheDocument();
  });
});
