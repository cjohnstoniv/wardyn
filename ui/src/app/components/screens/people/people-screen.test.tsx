/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.8.6 ppl-p2: the admin People page (mock M12). The strings are written out, not read from the copy
// module, so a drift in people.ts fails this suite.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { PersonSummary } from "../../../lib/types";

const listMock = vi.fn();
const createMock = vi.fn();
const tokensMock = vi.fn();
const signOutMock = vi.fn();
const removeKeysMock = vi.fn();
const eraseMock = vi.fn();
const getAccessMock = vi.fn();
vi.mock("../../../lib/api/people", () => ({
  people: {
    list: (...a: unknown[]) => listMock(...a),
    create: (...a: unknown[]) => createMock(...a),
    tokens: (...a: unknown[]) => tokensMock(...a),
    signOutEverywhere: (...a: unknown[]) => signOutMock(...a),
    removeSSHKeys: (...a: unknown[]) => removeKeysMock(...a),
  },
}));
vi.mock("../../../lib/api/credentials", () => ({ credentials: { erase: (...a: unknown[]) => eraseMock(...a) } }));
vi.mock("../../../lib/api/access", () => ({
  access: { getAccess: (...a: unknown[]) => getAccessMock(...a) },
  AccessCollisionError: class extends Error {},
  AccessPostureFlipRequiredError: class extends Error {},
}));
vi.mock("../../../lib/api/scim", () => ({
  scim: { getStatus: () => Promise.resolve({ configured: false, last_token_slot: "", purge_after_seconds: 0, keep_workspaces: false, deactivated: [], pending: [], drives: [] }) },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError } from "../../../lib/api/core";
import { OperatorProvider } from "../../wardyn/operator-context";
import { PeopleScreen } from "./people-screen";

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

function person(over: Partial<PersonSummary> = {}): PersonSummary {
  return {
    principal: "sub-ana",
    email: "ana@example.com",
    issuer_kind: "oidc",
    pre_created: false,
    last_signed_in_at: ago(2 * HOUR),
    role: "user",
    active_sessions: 1,
    api_tokens: 2,
    ssh_keys: 1,
    credentials: 0,
    active_runs: 0,
    ...over,
  };
}

const ROWS = [
  person(),
  person({ principal: "sub-new", email: "new@example.com", last_signed_in_at: undefined, pre_created: true, active_sessions: 0, api_tokens: 0, ssh_keys: 0 }),
  person({ principal: "sub-left", email: "left@example.com", last_signed_in_at: ago(12 * DAY), deactivated_at: ago(DAY), active_sessions: 0, api_tokens: 0, ssh_keys: 0, credentials: 3 }),
];

function renderScreen(opts: { operator?: boolean } = {}) {
  const { operator = true } = opts;
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} securityOperator operatorResolved principal="admin@corp.example">
        <PeopleScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  for (const m of [listMock, createMock, tokensMock, signOutMock, removeKeysMock, eraseMock, getAccessMock]) m.mockReset();
  listMock.mockResolvedValue({ people: ROWS });
  tokensMock.mockResolvedValue([{ id: "t1", name: "ci-bot", created_at: ago(DAY) }]);
  signOutMock.mockResolvedValue(undefined);
  removeKeysMock.mockResolvedValue(1);
  eraseMock.mockResolvedValue({ count: 3 });
  getAccessMock.mockRejectedValue(new HttpError(503, "sso off"));
});

describe("the people table", () => {
  it("shows the heading, lead and each person's state and holdings", async () => {
    renderScreen();
    expect(await screen.findByRole("heading", { name: "People", level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Everyone who can reach this deployment, and what each person holds.")).toBeInTheDocument();
    const ana = (await screen.findByText("ana@example.com")).closest("tr")!;
    expect(within(ana).getByText("User")).toBeInTheDocument();
    expect(within(ana).getByText("Active")).toBeInTheDocument();
    expect(within(ana).getByText("1 session · 2 tokens · 1 key")).toBeInTheDocument();
    const fresh = screen.getByText("new@example.com").closest("tr")!;
    expect(within(fresh).getByText("Never signed in")).toBeInTheDocument();
    expect(within(fresh).getAllByText("—")).toHaveLength(2);
    const left = screen.getByText("left@example.com").closest("tr")!;
    expect(within(left).getByText("Deactivated")).toBeInTheDocument();
    expect(within(left).getByText("3 credentials")).toBeInTheDocument();
  });

  it("mounts the SCIM card under the table", async () => {
    renderScreen({ operator: false });
    expect(await screen.findByRole("button", { name: /^SCIM provisioning/ })).toBeInTheDocument();
  });

  it("says what is empty and where people come from", async () => {
    listMock.mockResolvedValue({ people: [] });
    renderScreen();
    expect(await screen.findByText("Nobody has signed in yet. People appear here after their first sign-in, or when you add them.")).toBeInTheDocument();
  });

  it("searches (debounced) and filters by state through the server", async () => {
    const user = userEvent.setup();
    renderScreen();
    await screen.findByText("ana@example.com");
    await user.type(screen.getByRole("textbox", { name: "Search people" }), "ana");
    await waitFor(() => expect(listMock).toHaveBeenLastCalledWith(expect.objectContaining({ q: "ana" })));
    await user.click(screen.getByRole("button", { name: "Deactivated" }));
    await waitFor(() => expect(listMock).toHaveBeenLastCalledWith(expect.objectContaining({ q: "ana", state: "deactivated" })));
  });

  it("pages: Load more appends the next page by cursor", async () => {
    listMock.mockResolvedValueOnce({ people: [ROWS[0]], next_cursor: "c1" }).mockResolvedValueOnce({ people: [ROWS[1]] });
    const user = userEvent.setup();
    renderScreen();
    await screen.findByText("ana@example.com");
    await user.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("new@example.com")).toBeInTheDocument();
    expect(screen.getByText("ana@example.com")).toBeInTheDocument();
    expect(listMock).toHaveBeenLastCalledWith(expect.objectContaining({ cursor: "c1" }));
  });
});

describe("who can sign in", () => {
  it("the super admin gets the access panel, fetched once", async () => {
    renderScreen();
    expect(await screen.findByRole("heading", { name: "Who can sign in" })).toBeInTheDocument();
    await waitFor(() => expect(getAccessMock).toHaveBeenCalledTimes(1));
  });

  it("a security admin sees the super-admin-only line and never calls GET /access", async () => {
    renderScreen({ operator: false });
    expect(await screen.findByText("Only the super admin can see and change who can sign in.")).toBeInTheDocument();
    await screen.findByText("ana@example.com");
    expect(getAccessMock).not.toHaveBeenCalled();
  });
});

describe("the person drawer", () => {
  async function openAna() {
    const user = userEvent.setup();
    renderScreen({ operator: false });
    await user.click(await screen.findByRole("button", { name: "ana@example.com" }));
    return { user, drawer: await screen.findByRole("dialog") };
  }

  it("shows the sections with their counts, the token list and the runs link", async () => {
    const { drawer } = await openAna();
    expect(within(drawer).getByText("ana@example.com · User")).toBeInTheDocument();
    expect(within(drawer).getByText("1 active")).toBeInTheDocument();
    expect(within(drawer).getByText("API tokens")).toBeInTheDocument();
    expect(await within(drawer).findByText("ci-bot")).toBeInTheDocument();
    // #1477 refuses every admin-for-other mint, so the drawer offers none.
    expect(within(drawer).queryByRole("button", { name: /mint/i })).toBeNull();
    expect(within(drawer).getByText("0 running")).toBeInTheDocument();
    expect(within(drawer).getByRole("link", { name: /View their runs/ })).toHaveAttribute("href", "/admin/runs?q=sub-ana");
    expect(within(drawer).getByRole("button", { name: "Erase" })).toBeDisabled();
  });

  it.each([
    ["Sign out everywhere", "Sign ana@example.com out of every browser and the CLI? They can sign in again unless their access is removed.", () => signOutMock],
    ["Remove all", "Remove every SSH key ana@example.com has added? Their open SSH sessions end.", () => removeKeysMock],
  ])("%s asks first, naming the person, and only then acts", async (button, sentence, mock) => {
    const { user, drawer } = await openAna();
    await user.click(within(drawer).getByRole("button", { name: button }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText(sentence)).toBeInTheDocument();
    expect(mock()).not.toHaveBeenCalled();
    await user.click(within(confirm).getByRole("button", { name: button }));
    await waitFor(() => expect(mock()).toHaveBeenCalledWith("sub-ana"));
  });

  it("Cancel on the confirm acts on nothing", async () => {
    const { user, drawer } = await openAna();
    await user.click(within(drawer).getByRole("button", { name: "Remove all" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Cancel" }));
    expect(removeKeysMock).not.toHaveBeenCalled();
  });

  it("erases a person's stored credentials after the confirm", async () => {
    const user = userEvent.setup();
    renderScreen({ operator: false });
    await user.click(await screen.findByRole("button", { name: "left@example.com" }));
    const drawer = await screen.findByRole("dialog");
    await user.click(within(drawer).getByRole("button", { name: "Erase" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Erase every credential left@example.com has stored? This can't be undone.")).toBeInTheDocument();
    await user.click(within(confirm).getByRole("button", { name: "Erase" }));
    await waitFor(() => expect(eraseMock).toHaveBeenCalledWith("sub-left"));
  });
});

describe("Add a person", () => {
  it("creates the person and re-reads the list", async () => {
    createMock.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderScreen();
    await screen.findByText("ana@example.com");
    await user.click(screen.getByRole("button", { name: "Add a person" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Add someone before their first sign-in, so you can assign them a profile or key domain ahead of time. They mint their own API tokens after they sign in.")).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText("Subject"), "sub-zed");
    await user.type(within(dialog).getByLabelText("Email"), "zed@example.com");
    const before = listMock.mock.calls.length;
    await user.click(within(dialog).getByRole("button", { name: "Add a person" }));
    await waitFor(() => expect(createMock).toHaveBeenCalledWith("sub-zed", "zed@example.com"));
    await waitFor(() => expect(listMock.mock.calls.length).toBeGreaterThan(before));
  });

  it("shows the server's refusal in the dialog", async () => {
    createMock.mockRejectedValue(new HttpError(409, "email: another subject is already known by this email"));
    const user = userEvent.setup();
    renderScreen();
    await screen.findByText("ana@example.com");
    await user.click(screen.getByRole("button", { name: "Add a person" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("Subject"), "sub-zed");
    await user.click(within(dialog).getByRole("button", { name: "Add a person" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/already known by this email/);
  });
});
