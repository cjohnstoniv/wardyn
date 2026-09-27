/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// CredentialsScreen — the admin "Stored credentials" inventory (design F-1/
// F-2, packet F §4) and its erase flow (F-5/F-6, packet F §5). Every
// assertion reads its expected string from the copy modules (INVENTORY/ERASE,
// wardyn/copy/credentials.ts).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listInventoryMock = vi.fn();
const eraseMock = vi.fn();
vi.mock("../../lib/api/credentials", () => ({
  credentials: {
    listInventory: (...a: unknown[]) => listInventoryMock(...a),
    erase: (...a: unknown[]) => eraseMock(...a),
  },
}));

import { HttpError } from "../../lib/api/core";
import type { CredentialInventory } from "../../lib/api/credentials";
import type { SetupStatus } from "../../lib/types";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { INVENTORY, ERASE } from "../wardyn/copy/credentials";
import { CredentialsScreen } from "./credentials";

function renderScreen(opts: { status?: SetupStatus | null; operator?: boolean; securityOperator?: boolean } = {}) {
  const { status = null, operator = true, securityOperator = true } = opts;
  return render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <OperatorProvider operator={operator} securityOperator={securityOperator} operatorResolved principal="admin@corp.example">
        <CredentialsScreen />
      </OperatorProvider>
    </ModelAccessProvider>,
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
