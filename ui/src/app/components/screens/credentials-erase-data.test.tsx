/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Erase someone's data (mock packet M4, approved 2026-10-03, surface C): the
// security tier's by-scope erase of a person, beside the credentials-only
// erase. Expected strings are the packet's, spelled out rather than read back
// from the copy module.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const listInventoryMock = vi.fn();
const erasePersonMock = vi.fn();
vi.mock("../../lib/api/credentials", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../lib/api/credentials")>()),
  credentials: {
    listInventory: (...a: unknown[]) => listInventoryMock(...a),
    erasePerson: (...a: unknown[]) => erasePersonMock(...a),
    listAdminMintedTokens: () => Promise.resolve([]),
    revokeToken: vi.fn(),
    erase: vi.fn(),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError } from "../../lib/api/core";
import { ErasureIncomplete } from "../../lib/api/credentials";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { ERASE, ERASE_DATA, INVENTORY } from "../wardyn/copy/credentials";
import { CredentialsScreen } from "./credentials";
import { aheadByHours } from "../../lib/test-clock";

const aliceRow = {
  person: "sub-alice",
  email: "alice@example.com",
  provider: "corp-gw",
  provider_name: "Corp gateway",
  state: "stored" as const,
  store: "pg" as const,
  added_at: aheadByHours(-300),
};

function renderScreen(opts: { operator?: boolean; securityOperator?: boolean } = {}) {
  const { operator = true, securityOperator = true } = opts;
  return render(
    <MemoryRouter>
      <ModelAccessProvider status={null} onRefresh={() => {}}>
        <OperatorProvider operator={operator} securityOperator={securityOperator} operatorResolved principal="sec@corp.example">
          <CredentialsScreen />
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}

const SCOPE_LABELS = [
  "Credentials",
  "Personal details in audit events",
  "Run tasks",
  "Run output",
  "Copies kept for masking",
  "Recordings",
];
const scopeBox = (dialog: HTMLElement, label: string) =>
  within(dialog).getByRole("checkbox", { name: new RegExp(`^${label}`) });

describe("Erase someone's data (M4)", () => {
  beforeEach(() => {
    listInventoryMock.mockReset().mockResolvedValue({
      credentials: [aliceRow],
      counts: { people: 1, credentials: 1, by_provider: { "corp-gw": 1 } },
    });
    erasePersonMock.mockReset();
  });

  async function openDialog() {
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "Erase someone's data" }));
    return screen.getByRole("dialog");
  }

  it("is a security operator's second header button, beside the credentials-only erase", async () => {
    renderScreen();
    expect(await screen.findByRole("button", { name: "Erase someone's data" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: INVENTORY.ERASE_BY_EMAIL })).toBeInTheDocument();
  });

  it("an admin who is not a security operator never gets the button", async () => {
    renderScreen({ operator: true, securityOperator: false });
    await screen.findByRole("button", { name: INVENTORY.ERASE_BY_EMAIL });
    expect(screen.queryByRole("button", { name: "Erase someone's data" })).toBeNull();
  });

  it("canon strings", () => {
    expect(ERASE_DATA.BUTTON).toBe("Erase someone's data");
    expect(ERASE_DATA.HINT).toBe("Anyone who has used Wardyn, listed here or not.");
    expect(ERASE_DATA.SCOPES).toBe("What to erase");
    expect(ERASE_DATA.SCOPE.credentials.hint).toBe("Keys, sign-ins and secrets they stored.");
    expect(ERASE_DATA.SCOPE.audit_personal_fields.hint).toBe("Task text, messages, notes and emails recorded about them.");
    expect(ERASE_DATA.SCOPE.run_tasks.hint).toBe("The task text of every run they started.");
    expect(ERASE_DATA.SCOPE.run_outputs.hint).toBe("The saved output of every run they started.");
    expect(ERASE_DATA.SCOPE.mask_copies.hint).toBe("Their secret values, kept so run output can hide them.");
    expect(ERASE_DATA.SCOPE.recordings.hint).toBe("Not erased unless you choose it.");
    expect(ERASE_DATA.SEALING_NOTE).toBe(
      "Personal details are erasable only for events recorded while audit sealing was on. Earlier events keep them until retention drops their partition.",
    );
    expect(ERASE_DATA.KEEPS).toBe("The events themselves stay, and the log still verifies.");
    expect(ERASE_DATA.CONFIRM).toBe("Erase data");
    expect([ERASE_DATA.ERASED, ERASE_DATA.NOT_DONE]).toEqual(["Erased", "Not finished"]);
    expect(ERASE_DATA.DONE("ana@example.com")).toBe("Erased the chosen data for ana@example.com.");
    expect(ERASE_DATA.PARTIAL("ana@example.com")).toBe(
      "Some of ana@example.com's data wasn't erased. Try again — erasing twice is safe.",
    );
    expect(ERASE_DATA.FAILED("ana@example.com")).toBe(
      "The erase didn't finish, so some of ana@example.com's data may still be stored. Try again — erasing twice is safe.",
    );
    expect(ERASE_DATA.DONE_AUDIT).toBe("Recorded in the Audit log as person.erasure.");
  });

  it("opens with everything ticked except Recordings, and the disclosure lines", async () => {
    const dialog = await openDialog();
    expect(within(dialog).getByText("Erase someone's data", { selector: "h2" })).toBeInTheDocument();
    for (const label of SCOPE_LABELS.slice(0, 5)) expect(scopeBox(dialog, label)).toBeChecked();
    expect(scopeBox(dialog, "Recordings")).not.toBeChecked();
    expect(within(dialog).getByText(ERASE_DATA.SEALING_NOTE)).toBeInTheDocument();
    expect(within(dialog).getByText("The events themselves stay, and the log still verifies.")).toBeInTheDocument();
    expect(within(dialog).getByText("Backups keep a copy until they expire.")).toBeInTheDocument();
  });

  it("Erase data waits for an address and at least one scope; there is no type-to-confirm", async () => {
    const dialog = await openDialog();
    const confirm = within(dialog).getByRole("button", { name: "Erase data" });
    expect(confirm).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), "ana@example.com");
    expect(confirm).toBeEnabled();
    expect(within(dialog).queryByLabelText(/type .* to confirm/i)).toBeNull();
    for (const label of SCOPE_LABELS.slice(0, 5)) await userEvent.click(scopeBox(dialog, label));
    expect(confirm).toBeDisabled();
  });

  it("without recordings: the default scopes go to the server, Recordings not among them", async () => {
    erasePersonMock.mockResolvedValue({ person: "sub-ana", scopes: [], outcome: {} });
    const dialog = await openDialog();
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), " ana@example.com ");
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase data" }));
    expect(erasePersonMock).toHaveBeenCalledWith("ana@example.com", [
      "credentials",
      "audit_personal_fields",
      "run_tasks",
      "run_outputs",
      "mask_copies",
    ]);
    expect(await within(dialog).findByText("Erased the chosen data for ana@example.com.")).toBeInTheDocument();
    expect(within(dialog).getByText("Recorded in the Audit log as person.erasure.")).toBeInTheDocument();
    expect(within(dialog).getAllByText("Erased")).toHaveLength(5);
    expect(within(dialog).queryByText("Recordings")).toBeNull();
  });

  it("with recordings: ticking it sends the scope and lists it in the result", async () => {
    erasePersonMock.mockResolvedValue({ person: "sub-ana", scopes: [], outcome: {} });
    const dialog = await openDialog();
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), "ana@example.com");
    await userEvent.click(scopeBox(dialog, "Recordings"));
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase data" }));
    expect(await within(dialog).findByText("Recordings")).toBeInTheDocument();
    expect(erasePersonMock.mock.calls[0][1]).toContain("recordings");
    expect(within(dialog).getAllByText("Erased")).toHaveLength(6);
  });

  it("a partial run: finished scopes read Erased, the rest Not finished, and the retry sentence shows", async () => {
    erasePersonMock.mockRejectedValue(
      new ErasureIncomplete("erasure is not complete", ["mask_copies", "run_tasks"], ["run_outputs", "credentials"]),
    );
    const dialog = await openDialog();
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), "ana@example.com");
    await userEvent.click(scopeBox(dialog, "Personal details in audit events"));
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase data" }));
    expect(
      await within(dialog).findByText("Some of ana@example.com's data wasn't erased. Try again — erasing twice is safe."),
    ).toBeInTheDocument();
    const rowOf = (label: string) => within(dialog).getByText(label).closest("li")!;
    expect(within(rowOf("Run tasks")).getByText("Erased")).toBeInTheDocument();
    expect(within(rowOf("Copies kept for masking")).getByText("Erased")).toBeInTheDocument();
    expect(within(rowOf("Run output")).getByText("Not finished")).toBeInTheDocument();
    expect(within(rowOf("Credentials")).getByText("Not finished")).toBeInTheDocument();
    expect(within(dialog).queryByText("Personal details in audit events")).toBeNull();
    expect(within(dialog).getByText("Recorded in the Audit log as person.erasure.")).toBeInTheDocument();
  });

  it("a failure at 500 or above that is not a partial run gets its own sentence, and the form stays", async () => {
    erasePersonMock.mockRejectedValue(new HttpError(503, "busy"));
    const dialog = await openDialog();
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), "ana@example.com");
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase data" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "The erase didn't finish, so some of ana@example.com's data may still be stored. Try again — erasing twice is safe.",
    );
    expect(within(dialog).getByRole("button", { name: "Erase data" })).toBeInTheDocument();
  });

  it("a refusal shows the server's sentence as sent", async () => {
    const msg = "you cannot erase your own records, except your credentials; ask another security admin. Nothing was erased";
    erasePersonMock.mockRejectedValue(new HttpError(403, msg));
    const dialog = await openDialog();
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), "sec@corp.example");
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase data" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(msg);
  });

  it("Close reloads the inventory", async () => {
    erasePersonMock.mockResolvedValue({ person: "sub-ana", scopes: [], outcome: {} });
    const dialog = await openDialog();
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), "ana@example.com");
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase data" }));
    await within(dialog).findByText("Erased the chosen data for ana@example.com.");
    const before = listInventoryMock.mock.calls.length;
    await userEvent.click(within(dialog).getAllByRole("button", { name: ERASE.CLOSE })[0]);
    await waitFor(() => expect(listInventoryMock.mock.calls.length).toBeGreaterThan(before));
  });
});
