/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The exact lookup of a portal person by issuer and person ID, beside the ordinary search. The strings are
// written out so a drift in the copy module fails here.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../../lib/api/core";
import { setField } from "../../../../test/set-field";
import type { PersonSummary } from "../../../lib/types";

const listMock = vi.fn();
const getMock = vi.fn();
const lookupMock = vi.fn();
vi.mock("../../../lib/api/people", () => ({
  people: {
    list: (...a: unknown[]) => listMock(...a),
    get: (...a: unknown[]) => getMock(...a),
    portalPerson: (...a: unknown[]) => lookupMock(...a),
    create: vi.fn(),
    tokens: () => Promise.resolve([]),
    portals: () => Promise.resolve({ portals_used: [] }),
    signOutEverywhere: vi.fn(),
    removeSSHKeys: vi.fn(),
  },
}));
vi.mock("../../../lib/api/credentials", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/credentials")>()),
  credentials: { erase: vi.fn() },
}));
vi.mock("../../../lib/api/access", () => ({
  access: { getAccess: () => Promise.reject(new Error("sso off")) },
  AccessCollisionError: class extends Error {},
  AccessPostureFlipRequiredError: class extends Error {},
}));
vi.mock("../../../lib/api/scim", () => ({
  scim: { getStatus: () => Promise.resolve({ configured: false, last_token_slot: "", purge_after_seconds: 0, keep_workspaces: false, deactivated: [], pending: [], drives: [] }) },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { OperatorProvider } from "../../wardyn/operator-context";
import { PeopleScreen } from "./people-screen";

const ISSUER = "https://login.example.org";
const SAM: PersonSummary = {
  principal: "delegate:person:k1:sub-sam",
  email: "sam@example.com",
  issuer_kind: "oidc",
  pre_created: false,
  last_signed_in_at: new Date(Date.now() - 3_600_000).toISOString(),
  role: "user",
  issuer: ISSUER,
  active_sessions: 0,
  api_tokens: 0,
  ssh_keys: 0,
  credentials: 0,
  active_runs: 0,
};

const refused = (status: number, reason: string, message: string) => new HttpError(status, message, reason);

async function find(issuer: string, personId: string) {
  const user = userEvent.setup();
  render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator operatorResolved principal="admin@corp.example">
        <PeopleScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
  setField(await screen.findByRole("textbox", { name: "Issuer" }), issuer);
  setField(screen.getByRole("textbox", { name: "Person ID" }), personId);
  await user.click(screen.getByRole("button", { name: "Find exact" }));
  return user;
}

beforeEach(() => {
  listMock.mockReset();
  getMock.mockReset();
  lookupMock.mockReset();
  listMock.mockResolvedValue({ people: [] });
  getMock.mockImplementation((principal: string) => Promise.resolve(principal === SAM.principal ? SAM : undefined));
});

describe("finding a portal person by issuer and person ID", () => {
  it("asks for the exact issuer and ID and opens that person's drawer", async () => {
    lookupMock.mockResolvedValue({ principal: SAM.principal, state: "active", display_email: "sam@example.com", portals_used: [] });
    await find(ISSUER, "sub-sam");
    const drawer = within(await screen.findByRole("dialog"));
    expect(lookupMock).toHaveBeenCalledWith(ISSUER, "sub-sam");
    expect(drawer.getByText("Portal person")).toBeInTheDocument();
    expect(drawer.getByText(ISSUER)).toBeInTheDocument();
  });

  it("says the issuer is not registered, and to check its spelling", async () => {
    lookupMock.mockRejectedValue(refused(404, "portal_issuer_not_bound", "no issuer portal is bound to that exact issuer"));
    await find("https://login.example.org/", "sub-sam");
    expect(await screen.findByRole("alert")).toHaveTextContent("No portal is registered for that issuer. Check how the issuer is spelled: it must match exactly.");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("says no portal has introduced that person when the issuer is registered", async () => {
    lookupMock.mockRejectedValue(refused(404, "person_not_found", "that issuer is bound, and no portal has introduced a person with that id"));
    await find(ISSUER, "sub-nobody");
    expect(await screen.findByRole("alert")).toHaveTextContent("That issuer is registered, but no portal has introduced a person with that ID.");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps the ordinary search working beside it", async () => {
    listMock.mockResolvedValue({ people: [SAM] });
    render(
      <MemoryRouter>
        <OperatorProvider operator securityOperator operatorResolved principal="admin@corp.example">
          <PeopleScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    setField(await screen.findByRole("textbox", { name: "Search people" }), "sam");
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledWith(expect.objectContaining({ q: "sam" })));
    expect(lookupMock).not.toHaveBeenCalled();
  });
});

// A lookup that is still waiting when the operator opens another row, or dismisses that row's drawer, must not
// open its person over that choice, nor show its failure.
describe("a lookup that finishes after the operator chose something else", () => {
  const KIM: PersonSummary = { ...SAM, principal: "delegate:person:k1:sub-kim", email: "kim@example.com" };

  function deferred<T>() {
    let resolve!: (v: T) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }

  // Starts a lookup for Sam that stays pending, with Kim in the table.
  async function lookupSamPending() {
    const pending = deferred<{ principal: string; state: string; display_email: string; portals_used: never[] }>();
    lookupMock.mockReturnValue(pending.promise);
    listMock.mockResolvedValue({ people: [KIM] });
    getMock.mockImplementation((principal: string) => Promise.resolve([SAM, KIM].find((p) => p.principal === principal)));
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <OperatorProvider operator securityOperator operatorResolved principal="admin@corp.example">
          <PeopleScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    setField(await screen.findByRole("textbox", { name: "Issuer" }), ISSUER);
    setField(screen.getByRole("textbox", { name: "Person ID" }), "sub-sam");
    await user.click(screen.getByRole("button", { name: "Find exact" }));
    await waitFor(() => expect(lookupMock).toHaveBeenCalled());
    return { user, pending };
  }
  const samAnswer = { principal: SAM.principal, state: "active", display_email: "sam@example.com", portals_used: [] as never[] };

  it("leaves the row opened meanwhile open", async () => {
    const { user, pending } = await lookupSamPending();
    await user.click(await screen.findByRole("button", { name: "kim@example.com" }));
    const drawer = within(await screen.findByRole("dialog"));
    expect(drawer.getByText("kim@example.com", { exact: false })).toBeInTheDocument();
    await act(async () => {
      pending.resolve(samAnswer);
    });
    expect(within(screen.getByRole("dialog")).getByText("kim@example.com", { exact: false })).toBeInTheDocument();
    expect(within(screen.getByRole("dialog")).queryByText("sam@example.com", { exact: false })).toBeNull();
  });

  it("keeps the drawer closed when it was dismissed meanwhile", async () => {
    const { user, pending } = await lookupSamPending();
    await user.click(await screen.findByRole("button", { name: "kim@example.com" }));
    await screen.findByRole("dialog");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await act(async () => {
      pending.resolve(samAnswer);
    });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows no error when it fails after another row was opened", async () => {
    const { user, pending } = await lookupSamPending();
    await user.click(await screen.findByRole("button", { name: "kim@example.com" }));
    await screen.findByRole("dialog");
    await act(async () => {
      pending.reject(refused(404, "person_not_found", "no such person"));
    });
    expect(screen.queryByRole("alert", { hidden: true })).toBeNull();
    expect(within(screen.getByRole("dialog")).getByText("kim@example.com", { exact: false })).toBeInTheDocument();
  });

  it("still opens the person when nothing else was chosen", async () => {
    const { pending } = await lookupSamPending();
    await act(async () => {
      pending.resolve(samAnswer);
    });
    expect(within(await screen.findByRole("dialog")).getByText("sam@example.com", { exact: false })).toBeInTheDocument();
  });
});
