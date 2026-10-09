/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The organisation's component catalog and editor against the real routes' shapes
// (the api modules are stubbed at their edge; the request bodies are asserted).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));

const listMock = vi.fn();
const putMock = vi.fn();
const removeMock = vi.fn();
vi.mock("../../../lib/api/components", () => ({
  components: { list: () => listMock(), put: (...a: unknown[]) => putMock(...a), remove: (...a: unknown[]) => removeMock(...a) },
}));

const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      listUserTypes: () => Promise.resolve([]),
    },
  };
});

import { HttpError } from "../../../lib/api/core";
import type { Component } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ComponentsScreen } from "./components-screen";

const ID1 = "11111111-1111-4111-8111-111111111111";
const ID2 = "22222222-2222-4222-8222-222222222222";
const NEW_ID = "99999999-9999-4999-8999-999999999999";
const NOBODY_YET = "Nobody yet. Add a person, group or user type below to let them use this component.";

const comp = (over: Partial<Component> = {}): Component => ({
  id: ID1,
  name: "Payments API",
  version: 1,
  created_at: "2026-10-08T00:00:00Z",
  updated_at: "2026-10-08T00:00:00Z",
  definition: {
    hosts: ["api.pay.example", "files.pay.example"],
    secrets: [{ secret_name: "pay-key", delivery: { mode: "header", host: "api.pay.example" } }],
  },
  ...over,
});
const avail = (id: string, restricted: boolean, n: number) => ({
  kind: "component",
  value: id,
  restricted,
  allowed_by: Array.from({ length: n }, (_, i) => ({
    id: `g${i}`,
    subject_type: "group",
    subject: `team-${i}`,
    capability: "component",
    value: id,
    effect: "allow",
    created_at: "2026-10-08T00:00:00Z",
  })),
});

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator operatorResolved>
        <ComponentsScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  toastSuccess.mockClear();
  toastError.mockClear();
  listMock.mockReset().mockResolvedValue([comp()]);
  putMock.mockReset();
  removeMock.mockReset();
  getAvailabilityMock.mockReset().mockImplementation((_k: string, id: string) => Promise.resolve(avail(id, true, 0)));
  putAvailabilityMock.mockReset();
  vi.spyOn(crypto, "randomUUID").mockReturnValue(NEW_ID);
});

describe("catalog", () => {
  it("lists each component with what it reaches, its secrets and who may use it: nobody yet by default", async () => {
    listMock.mockResolvedValue([
      comp(),
      comp({ id: ID2, name: "Search", definition: { hosts: ["s.example"] } }),
    ]);
    getAvailabilityMock.mockImplementation((_k: string, id: string) =>
      Promise.resolve(id === ID1 ? avail(id, true, 0) : avail(id, true, 2)),
    );
    renderScreen();
    const row1 = (await screen.findByText("Payments API")).closest("tr")!;
    expect(within(row1).getByText("2 hosts")).toBeInTheDocument();
    expect(within(row1).getByText("1 secret")).toBeInTheDocument();
    expect(await within(row1).findByText("Nobody yet")).toBeInTheDocument();
    const row2 = screen.getByText("Search").closest("tr")!;
    expect(within(row2).getByText("1 host")).toBeInTheDocument();
    expect(within(row2).getByText("None")).toBeInTheDocument();
    expect(await within(row2).findByText("Only 2 listed")).toBeInTheDocument();
    expect(getAvailabilityMock).toHaveBeenCalledWith("component", ID1);
  });

  it("an unrestricted component reads Everyone", async () => {
    getAvailabilityMock.mockResolvedValue(avail(ID1, false, 0));
    renderScreen();
    expect(await screen.findByText("Everyone")).toBeInTheDocument();
  });

  it("with none, the empty state carries the Add action, and Add opens the editor", async () => {
    listMock.mockResolvedValue([]);
    renderScreen();
    expect(await screen.findByText("No components yet")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Add component" }));
    expect(await screen.findByRole("dialog")).toHaveTextContent("Add a component");
  });

  it("a 403 is the tier, not an outage: no Retry, no Add", async () => {
    listMock.mockRejectedValue(new HttpError(403, "forbidden"));
    renderScreen();
    expect(await screen.findByText("Requires the admin role.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add component" })).toBeNull();
  });

  it("any other failure offers Retry", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom")).mockResolvedValue([comp()]);
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Payments API")).toBeInTheDocument();
  });

  it("Choose who opens the Available to control with Nobody yet, one row at a time, and never sends a restriction", async () => {
    listMock.mockResolvedValue([comp(), comp({ id: ID2, name: "Search" })]);
    renderScreen();
    await screen.findByText("Search");
    await userEvent.click(screen.getByRole("button", { name: "Choose who may use Payments API" }));
    expect(await screen.findByTestId("availability-nobody")).toHaveTextContent(NOBODY_YET);
    await userEvent.click(screen.getByRole("button", { name: "Choose who may use Search" }));
    await waitFor(() => expect(screen.getAllByTestId("availability-nobody")).toHaveLength(1));
    expect(putAvailabilityMock).not.toHaveBeenCalled();
  });
});

describe("editor", () => {
  async function openNew() {
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "Add component" }));
    return screen.getByRole("dialog");
  }

  it("creating sends the request to a new id, then opens Choose who on the new row", async () => {
    putMock.mockResolvedValue({ ...comp({ id: NEW_ID, name: "Billing" }), requirements: [] });
    const dialog = await openNew();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Billing");
    await userEvent.type(within(dialog).getByLabelText("Hosts"), "billing.example");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a secret" }));
    await userEvent.type(within(dialog).getByLabelText("Secret name"), "billing-key");
    listMock.mockResolvedValue([comp(), comp({ id: NEW_ID, name: "Billing" })]);
    await userEvent.click(within(dialog).getByRole("button", { name: "Save component" }));

    await waitFor(() =>
      expect(putMock).toHaveBeenCalledWith(NEW_ID, {
        name: "Billing",
        definition: {
          hosts: ["billing.example"],
          secrets: [{ secret_name: "billing-key", delivery: { mode: "header", host: "billing.example" } }],
        },
      }),
    );
    expect(await screen.findByTestId("availability-nobody")).toHaveTextContent(NOBODY_YET);
    expect(toastSuccess).toHaveBeenCalledWith("Component saved.");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("Save waits for a name", async () => {
    const dialog = await openNew();
    expect(within(dialog).getByRole("button", { name: "Save component" })).toBeDisabled();
  });

  it("editing a stored row keeps its id and shows its values", async () => {
    putMock.mockResolvedValue({ ...comp({ name: "Payments v2" }), requirements: [] });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "Edit Payments API" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByLabelText("Name")).toHaveValue("Payments API");
    expect(within(dialog).getByLabelText("Hosts")).toHaveValue("api.pay.example\nfiles.pay.example");
    expect(within(dialog).getByLabelText("Secret name")).toHaveValue("pay-key");
    await userEvent.clear(within(dialog).getByLabelText("Name"));
    await userEvent.type(within(dialog).getByLabelText("Name"), "Payments v2");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save component" }));
    await waitFor(() => expect(putMock).toHaveBeenCalledTimes(1));
    expect(putMock.mock.calls[0][0]).toBe(ID1);
    expect(putMock.mock.calls[0][1].name).toBe("Payments v2");
    expect(putMock.mock.calls[0][1].definition.secrets).toEqual([
      { secret_name: "pay-key", delivery: { mode: "header", host: "api.pay.example" } },
    ]);
  });

  it("a refusal shows the server's sentence as sent and keeps the dialog", async () => {
    const msg = 'invalid component: definition.secrets[0]: the organisation has no secret named "x"';
    putMock.mockRejectedValue(new HttpError(422, msg));
    const dialog = await openNew();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Billing");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save component" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(msg);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("a secret you provide is header-only; other modes disable it and send no shared flag", async () => {
    putMock.mockResolvedValue({ ...comp(), requirements: [] });
    const dialog = await openNew();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Billing");
    await userEvent.type(within(dialog).getByLabelText("Hosts"), "billing.example");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a secret" }));
    await userEvent.type(within(dialog).getByLabelText("Secret name"), "billing-key");
    await userEvent.click(within(dialog).getByRole("button", { name: "Provided by your organisation" }));
    expect(within(dialog).getByText("Uses the secret of this name that you store. People who use the component never see the value.")).toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: "Environment variable" }));
    expect(within(dialog).getByRole("button", { name: "Provided by your organisation" })).toBeDisabled();
    expect(within(dialog).getByText("Only a request header can use a secret you provide.")).toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("Variable name"), "BILLING_KEY");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save component" }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putMock.mock.calls[0][1].definition.secrets).toEqual([
      { secret_name: "billing-key", delivery: { mode: "env", var: "BILLING_KEY" } },
    ]);
  });

  it("a header secret can be provided by the org, sent over plain HTTP, and bound to a chosen host", async () => {
    putMock.mockResolvedValue({ ...comp(), requirements: [] });
    const dialog = await openNew();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Billing");
    await userEvent.type(within(dialog).getByLabelText("Hosts"), "a.example{Enter}b.example{Enter}*.c.example");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a secret" }));
    await userEvent.type(within(dialog).getByLabelText("Secret name"), "billing-key");
    await userEvent.click(within(dialog).getByRole("combobox", { name: "Host" }));
    await userEvent.click(await screen.findByRole("option", { name: "b.example" }));
    await userEvent.type(within(dialog).getByLabelText("Header"), "X-Api-Key");
    await userEvent.type(within(dialog).getByLabelText("Value"), "%s");
    await userEvent.click(within(dialog).getByRole("button", { name: "Provided by your organisation" }));
    await userEvent.click(within(dialog).getByRole("checkbox", { name: /Also send over plain HTTP/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Save component" }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putMock.mock.calls[0][1].definition.secrets).toEqual([
      {
        secret_name: "billing-key",
        shared: true,
        delivery: { mode: "header", host: "b.example", header: "X-Api-Key", format: "%s", plain_http: true },
      },
    ]);
  });

  it("settings and secrets can be removed again", async () => {
    const dialog = await openNew();
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a secret" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a setting" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove secret 1" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove setting 1" }));
    expect(within(dialog).queryByTestId("component-secret-row")).toBeNull();
    expect(within(dialog).queryByLabelText("Name", { selector: "input[id^=component-setting]" })).toBeNull();
  });
});

describe("delete", () => {
  it("confirms, deletes by id and reloads", async () => {
    removeMock.mockResolvedValue(undefined);
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "Delete Payments API" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete Payments API?")).toBeInTheDocument();
    expect(removeMock).not.toHaveBeenCalled();
    listMock.mockResolvedValue([]);
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete component" }));
    await waitFor(() => expect(removeMock).toHaveBeenCalledWith(ID1));
    expect(await screen.findByText("No components yet")).toBeInTheDocument();
    expect(toastSuccess).toHaveBeenCalledWith("Component deleted.");
  });

  it("a failed delete says so and leaves the row", async () => {
    removeMock.mockRejectedValue(new HttpError(500, "boom"));
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "Delete Payments API" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete component" }));
    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(screen.getByText("Payments API")).toBeInTheDocument();
  });
});
