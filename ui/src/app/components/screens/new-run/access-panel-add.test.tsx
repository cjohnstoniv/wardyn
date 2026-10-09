/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ComponentRef } from "../../../lib/types";
import type { ComponentFact } from "../../../lib/types/components";
import { AccessPanel } from "./access-panel";
import { accessRows } from "./access-rows-model";

const ID = "11111111-1111-1111-1111-111111111111";

// The panel the way New Run wires it: the run's component list is state the
// panel changes, and the rows come from the facts the server would return for it.
function Harness({ initial = [], facts = [] }: { initial?: ComponentRef[]; facts?: ComponentFact[] }) {
  const [refs, setRefs] = React.useState<ComponentRef[]>(initial);
  const [open, setOpen] = React.useState<string | undefined>(undefined);
  return (
    <MemoryRouter>
      <AccessPanel
        adoAccess={undefined}
        savedMode={false}
        savedCaps={undefined}
        adoRefusal={undefined}
        adoConnecting={false}
        onAdoConnect={() => {}}
        rows={accessRows(facts, undefined)}
        openRowId={open}
        onOpenRow={setOpen}
        secretsPath="/secrets"
        guardLink={() => () => {}}
        refs={refs}
        onRefsChange={setRefs}
      />
      <output data-testid="refs">{JSON.stringify(refs)}</output>
    </MemoryRouter>
  );
}

const fact = (id: string, name: string): ComponentFact => ({
  kind: "custom", id, name, reason: "self", status: "ready", requirements: [],
});

let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  fetchMock = vi.fn().mockResolvedValue(
    new Response(
      JSON.stringify({ may_define: true, resident_delivery_allowed: true, autonomy_cap: "", mine: [], org: [{ id: ID, name: "Billing API", hosts: ["billing.corp.test"], secrets: [], config_keys: [] }] }),
      { status: 200, headers: { "content-type": "application/json" } },
    ),
  );
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

describe("Access panel Add control", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  it("reads nothing until Add access is pressed, then adds the picked component to the run", async () => {
    render(<Harness />);
    expect(fetchMock).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Add access" }));
    await user.click(await screen.findByRole("button", { name: "Add Billing API" }));
    expect(JSON.parse(screen.getByTestId("refs").textContent!)).toEqual([{ id: ID }]);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("has no Add control when the screen gives no way to change the list", () => {
    render(
      <MemoryRouter>
        <AccessPanel
          adoAccess={undefined} savedMode={false} savedCaps={undefined} adoRefusal={undefined}
          adoConnecting={false} onAdoConnect={() => {}} rows={[]} openRowId={undefined}
          onOpenRow={() => {}} secretsPath="/secrets" guardLink={() => () => {}}
        />
      </MemoryRouter>,
    );
    expect(screen.queryByRole("button", { name: "Add access" })).not.toBeInTheDocument();
  });

  it("removes an inline component by its position and a stored one by its id", async () => {
    const initial: ComponentRef[] = [{ inline: { hosts: ["a.test"] }, name: "First" }, { id: ID }];
    render(<Harness initial={initial} facts={[fact("inline:0", "First"), fact(ID, "Billing API")]} />);
    await user.click(screen.getByRole("button", { name: /First/ }));
    await user.click(screen.getByRole("button", { name: "Remove First from this run" }));
    expect(JSON.parse(screen.getByTestId("refs").textContent!)).toEqual([{ id: ID }]);
  });

  it("offers Remove only on a row the person added", async () => {
    render(<Harness initial={[{ id: ID }]} facts={[fact(ID, "Billing API"), fact("c9", "Provided")]} />);
    await user.click(screen.getByRole("button", { name: /Provided/ }));
    expect(screen.queryByRole("button", { name: /Remove Provided/ })).not.toBeInTheDocument();
  });
});
