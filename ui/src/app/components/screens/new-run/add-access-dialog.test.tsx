/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ComponentRef, MyComponents } from "../../../lib/types";
import { AddAccessDialog, MAX_COMPONENT_REFS } from "./add-access-dialog";

const MINE_ID = "11111111-1111-1111-1111-111111111111";
const ORG_ID = "22222222-2222-2222-2222-222222222222";

const reply = (over: Partial<MyComponents> = {}): MyComponents => ({
  may_define: true,
  resident_delivery_allowed: true,
  autonomy_cap: "",
  mine: [
    {
      id: MINE_ID,
      owner: "me",
      name: "My Acme",
      definition: { hosts: ["api.acme.test", "cdn.acme.test"] },
      version: 1,
      created_at: "2026-10-01T00:00:00Z",
      updated_at: "2026-10-01T00:00:00Z",
    },
  ],
  org: [{ id: ORG_ID, name: "Billing API", hosts: ["billing.corp.test"], secrets: [{ delivery: { mode: "header" } }], config_keys: [] }],
  ...over,
});

let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

const serve = (body: MyComponents | Response) =>
  fetchMock.mockResolvedValue(
    body instanceof Response ? body : new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } }),
  );

function open(refs: ComponentRef[] = []) {
  const onAdd = vi.fn();
  const onClose = vi.fn();
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  render(
    <MemoryRouter>
      <AddAccessDialog refs={refs} secretsPath="/secrets" onAdd={onAdd} onClose={onClose} />
    </MemoryRouter>,
  );
  return { onAdd, onClose, user };
}

describe("AddAccessDialog", () => {
  it("reads GET /me/components once it opens", async () => {
    serve(reply());
    open();
    await screen.findByText("Billing API");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0][0])).toBe("/api/v1/me/components");
    expect(fetchMock.mock.calls[0][1]?.method).toBe("GET");
  });

  it("lists Custom, the person's saved components and the organisation's, with hosts", async () => {
    serve(reply());
    open();
    expect(await screen.findByRole("button", { name: /Custom component/ })).toBeEnabled();
    const mine = screen.getByRole("region", { name: "Your saved components" });
    expect(within(mine).getByText("My Acme")).toBeInTheDocument();
    expect(within(mine).getByText("api.acme.test, cdn.acme.test")).toBeInTheDocument();
    const org = screen.getByRole("region", { name: "Provided by your organisation" });
    expect(within(org).getByText("Billing API")).toBeInTheDocument();
    expect(within(org).getByText("billing.corp.test")).toBeInTheDocument();
  });

  it("does not list Custom, or the person's saved ones, when they may not define their own", async () => {
    serve(reply({ may_define: false }));
    open();
    await screen.findByText("Billing API");
    expect(screen.queryByRole("button", { name: /Custom component/ })).not.toBeInTheDocument();
    expect(screen.queryByText("My Acme")).not.toBeInTheDocument();
    expect(screen.getByText("Your organisation doesn't let you define your own components.")).toBeInTheDocument();
  });

  it("adds a saved or organisation component by id and closes", async () => {
    serve(reply());
    const { onAdd, onClose, user } = open();
    await user.click(await screen.findByRole("button", { name: "Add My Acme" }));
    expect(onAdd).toHaveBeenCalledWith({ id: MINE_ID });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("adds an organisation component by id", async () => {
    serve(reply());
    const { onAdd, user } = open();
    await user.click(await screen.findByRole("button", { name: "Add Billing API" }));
    expect(onAdd).toHaveBeenCalledWith({ id: ORG_ID });
  });

  it("shows what the run already carries as Added and does not add it twice", async () => {
    serve(reply());
    const { onAdd, user } = open([{ id: MINE_ID }]);
    const added = await screen.findByRole("button", { name: "Added My Acme" });
    expect(added).toBeDisabled();
    await user.click(added);
    expect(onAdd).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Add Billing API" })).toBeEnabled();
  });

  it("stops at the most a run carries", async () => {
    serve(reply());
    open(Array.from({ length: MAX_COMPONENT_REFS }, () => ({ inline: { hosts: [] } })));
    await screen.findByText("Billing API");
    expect(screen.getByText(`A run can carry at most ${MAX_COMPONENT_REFS} components.`)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add Billing API" })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Custom component/ })).toBeDisabled();
  });

  it("opens the custom form from Custom, with env and file as the deployment allows", async () => {
    serve(reply({ resident_delivery_allowed: false }));
    const { user } = open();
    await user.click(await screen.findByRole("button", { name: /Custom component/ }));
    expect(await screen.findByRole("button", { name: "Add to this run" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    expect(screen.queryByRole("button", { name: "Environment variable" })).not.toBeInTheDocument();
  });

  it("says so, and offers a retry, when the list cannot be read", async () => {
    serve(new Response(JSON.stringify({ error: "boom" }), { status: 500, headers: { "content-type": "application/json" } }));
    const { user } = open();
    expect(await screen.findByText("Couldn't load what you can add.")).toBeInTheDocument();
    serve(reply());
    await user.click(screen.getByRole("button", { name: /retry/i }));
    expect(await screen.findByText("Billing API")).toBeInTheDocument();
  });
});
