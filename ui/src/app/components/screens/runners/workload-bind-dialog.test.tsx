/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Bind namespace dialog offers an organization pool only: a person's own pool is refused by the server
// (workload_binding_pool_scope), so it is never listed.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BindNamespaceDialog } from "./workload-bind-dialog";

vi.mock("../../../lib/api/workloads", () => ({ workloadBindings: { create: vi.fn() } }));

afterEach(cleanup);

describe("Bind namespace dialog", () => {
  it("lists no user pool", async () => {
    render(
      <BindNamespaceDialog
        pools={[
          { id: "o1", name: "Platform services", scope: "organization" },
          { id: "u1", name: "My pool", scope: "user" },
        ]}
        services={[]}
        onClose={() => {}}
        onBound={() => {}}
      />,
    );
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("combobox", { name: /Pool/ }));
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Platform services"]);
  });
});
