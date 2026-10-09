/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { Component, MyComponents } from "../../../lib/types";
import { setField } from "../../../../test/set-field";
import { MyComponentsCard } from "./my-components-card";

const row = (id: string, name: string, secrets = 0): Component => ({
  id,
  owner: "me",
  name,
  definition: {
    hosts: ["api.acme.test", "cdn.acme.test"],
    ...(secrets > 0 && { secrets: Array.from({ length: secrets }, (_, i) => ({ secret_name: `k${i}`, delivery: { mode: "header" as const, host: "api.acme.test" } })) }),
  },
  version: 1,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
});
const A = row("11111111-1111-1111-1111-111111111111", "Acme", 1);
const B = row("22222222-2222-2222-2222-222222222222", "Beta");

const reply = (over: Partial<MyComponents> = {}): MyComponents => ({
  may_define: true, resident_delivery_allowed: true, autonomy_cap: "", mine: [A, B], org: [], ...over,
});

let mine: MyComponents;
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  mine = reply();
  fetchMock = vi.fn(async (_path: string, init?: RequestInit) => {
    const method = init?.method;
    if (method === "DELETE") return new Response(null, { status: 204 });
    if (method === "PUT") return new Response(JSON.stringify({ ...A, name: "Acme v2", requirements: [] }), { status: 200, headers: { "content-type": "application/json" } });
    return new Response(JSON.stringify(mine), { status: 200, headers: { "content-type": "application/json" } });
  });
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

const user = userEvent.setup({ pointerEventsCheck: 0 });
const mount = () =>
  render(
    <MemoryRouter>
      <MyComponentsCard />
    </MemoryRouter>,
  );
const openCard = async () => {
  await screen.findByText("2 saved");
  await user.click(screen.getByRole("button", { name: /Your components/ }));
};
const sent = () => fetchMock.mock.calls.map(([p, i]) => `${i?.method} ${p}`);

describe("MyComponentsCard", () => {
  it("summarises the saved count, then lists each row with its hosts and secrets", async () => {
    mount();
    await openCard();
    expect(screen.getByText("2 hosts · 1 secret")).toBeInTheDocument();
    expect(screen.getByText("2 hosts · no secrets")).toBeInTheDocument();
    expect(sent()).toEqual(["GET /api/v1/me/components"]);
  });

  it("deletes a row through DELETE /me/components/{id} after confirmation, then reloads", async () => {
    mount();
    await openCard();
    await user.click(screen.getByRole("button", { name: "Delete Acme" }));
    const confirm = await screen.findByRole("button", { name: "Delete component" });
    mine = reply({ mine: [B] });
    await user.click(confirm);
    await waitFor(() => expect(sent()).toContain(`DELETE /api/v1/me/components/${A.id}`));
    await screen.findByText("1 saved");
  });

  it("edits a row in the same form New Run uses, saving with PUT", async () => {
    mount();
    await openCard();
    await user.click(screen.getByRole("button", { name: "Edit Acme" }));
    const name = await screen.findByLabelText(/^Name/);
    expect(name).toHaveValue("Acme");
    setField(name, "Acme v2");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(sent()).toContain(`PUT /api/v1/me/components/${A.id}`));
    const put = fetchMock.mock.calls.find(([, i]) => i?.method === "PUT")!;
    expect(JSON.parse(String(put[1].body)).name).toBe("Acme v2");
  });

  it("creates a new one with POST", async () => {
    mount();
    await openCard();
    await user.click(screen.getByRole("button", { name: "New component" }));
    setField(await screen.findByLabelText(/^Name/), "Gamma");
    setField(screen.getByLabelText(/^Hosts it reaches/), "gamma.test");
    fetchMock.mockImplementationOnce(async () => new Response(JSON.stringify(mine), { status: 200, headers: { "content-type": "application/json" } }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(sent().some((s) => s === "POST /api/v1/me/components")).toBe(true));
  });

  it("when they may no longer define components: no New, no Edit, Delete stays, and it says why", async () => {
    mine = reply({ may_define: false });
    mount();
    await openCard();
    expect(screen.queryByRole("button", { name: "New component" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit Acme" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete Acme" })).toBeInTheDocument();
    expect(screen.getByText(/doesn't let you define your own components/)).toBeInTheDocument();
  });

  it("renders nothing when they may not define any and have none", async () => {
    mine = reply({ may_define: false, mine: [] });
    const { container } = mount();
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("says so, and retries, when the list cannot be read", async () => {
    fetchMock.mockImplementationOnce(async () => new Response(JSON.stringify({ error: "boom" }), { status: 500, headers: { "content-type": "application/json" } }));
    mount();
    await user.click(screen.getByRole("button", { name: /Your components/ }));
    expect(await screen.findByText("Couldn't load your components.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByText("2 saved");
    expect(within(screen.getByTestId("my-components-card")).getByText("Acme")).toBeInTheDocument();
  });
});
