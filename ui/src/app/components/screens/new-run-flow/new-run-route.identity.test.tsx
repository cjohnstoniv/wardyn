/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Another person signing in never sees, sends or reloads the previous one's
// New Run: the draft resets and the URL loses the template or run it named.
import type * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation, useNavigationType } from "react-router-dom";
import { ReauthLayer } from "../../wardyn/reauth-layer";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ReauthContext, type Reauth } from "../../../lib/reauth";
import { TOKEN_LABEL } from "../sign-in";
import { setField } from "../../../../test/set-field";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDraft, type NewRunScreenName, type NewRunScreenProps } from "./new-run-route";

vi.mock("../../../lib/api/health", () => ({ health: { health: () => Promise.resolve({}) } }));
vi.mock("../../../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/core")>()),
  wfetch: vi.fn(() => Promise.resolve(new Response(JSON.stringify({
    principal: "bob", method: "token", operator: false, security_operator: false, role: "user", email: "",
  }), { status: 200 }))),
  setToken: vi.fn(),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), warning: vi.fn() } }));

const R = "e1111111-1111-4111-8111-111111111111";

afterEach(cleanup);

describe("New Run and a different identity (U10)", () => {
  it("the sign-in reload goes to bare /runs/new, never replaying the previous person's ?again=", async () => {
    const reloadAs = vi.fn();
    const reauth: Reauth = {
      phase: "dialog", signedOut: true, renewal: null, watch: null, writeDropped: null,
      setPhase: vi.fn(), setWatch: vi.fn(), startRenew: vi.fn(), endRenew: vi.fn(), reloadAs,
      clearWriteDropped: vi.fn(), writeDroppedClaimed: () => false, claimWriteDropped: () => vi.fn(),
    };
    render(
      <MemoryRouter initialEntries={[`/runs/new?again=${R}&step=review`]}>
        <OperatorProvider operator={false} operatorResolved={true} principal="alice">
          <ReauthContext.Provider value={reauth}>
            <ReauthLayer onResumed={vi.fn()} />
          </ReauthContext.Provider>
        </OperatorProvider>
      </MemoryRouter>,
    );
    setField(screen.getByLabelText(TOKEN_LABEL), "a-token");
    await userEvent.setup().click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(reloadAs).toHaveBeenCalled());
    expect(reloadAs).toHaveBeenCalledWith("/runs/new");
  });

  it("a different resolved principal resets the draft and replaces the URL with bare /runs/new", async () => {
    function Probe({ name }: { name: NewRunScreenName }) {
      const draft = useNewRunDraft();
      return <p data-testid="draft">{`${name} ${draft.owner} ${draft.origin.kind}`}</p>;
    }
    const probes = Object.fromEntries((Object.keys(NEW_RUN_SCREENS) as NewRunScreenName[])
      .map((n) => [n, (_: NewRunScreenProps) => <Probe name={n} />])) as Record<NewRunScreenName, React.ComponentType<NewRunScreenProps>>;
    function Where() {
      const l = useLocation();
      return <p data-testid="where">{`${useNavigationType()} ${l.pathname}${l.search}`}</p>;
    }
    const again = vi.fn(() => Promise.resolve({ origin: { kind: "again" as const, runId: R, title: "Nightly" }, intent: { shape: "environment" as const } }));
    const tree = (principal: string) => (
      <MemoryRouter initialEntries={[`/runs/new?again=${R}&step=review`]}>
        <OperatorProvider operator={false} principal={principal}>
          <Routes>
            <Route path="/runs/new" element={<><NewRunRoute loaders={{ template: vi.fn(), again }} screens={probes} /><Where /></>} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>
    );
    const view = render(tree("alice"));
    expect(await screen.findByText("review alice again")).toBeInTheDocument();
    view.rerender(tree("bob"));
    expect(await screen.findByText("start bob scratch")).toBeInTheDocument();
    expect(screen.getByTestId("where").textContent).toBe("REPLACE /runs/new");
    expect(again).toHaveBeenCalledTimes(1);
  });
});
