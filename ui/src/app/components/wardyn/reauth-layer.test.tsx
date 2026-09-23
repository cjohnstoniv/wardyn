/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SF-29: `succeed`'s "someone else signed in" branch compared the incoming
// /me against `usePrincipal()`, whose value while identity is unresolved
// (app-shell's still-loading "…", or its fail-open "unknown" once whoami()
// swallows a failure and returns null — health.ts's whoami()) is never a
// real principal. Comparing against either placeholder always differs from
// a genuine signed-in principal, reloading the page and losing the draft
// this dialog just promised nothing here had lost.
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ReauthLayer } from "./reauth-layer";
import { OperatorProvider } from "./operator-context";
import { ReauthContext, type Reauth } from "../../lib/reauth";
import { TOKEN_LABEL } from "../screens/sign-in";

vi.mock("../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({}) },
}));

const meResponse = {
  principal: "gsv-member-0001",
  method: "token",
  operator: false,
  security_operator: false,
  role: "member",
  email: "",
};
vi.mock("../../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../lib/api/core")>()),
  wfetch: vi.fn(() => Promise.resolve(new Response(JSON.stringify(meResponse), { status: 200 }))),
  setToken: vi.fn(),
}));

function renderDialog(reauthOverrides: Partial<Reauth> = {}) {
  const reloadAs = vi.fn();
  const setPhase = vi.fn();
  const reauth: Reauth = {
    phase: "dialog",
    writeDropped: null,
    setPhase,
    reloadAs,
    clearWriteDropped: vi.fn(),
    writeDroppedClaimed: () => false,
    claimWriteDropped: () => vi.fn(),
    ...reauthOverrides,
  };
  render(
    // meResponse's role (member) can reach /runs — the path the succeed()
    // path this test exercises (onResumed, not the role-narrowed dialog)
    // needs to fall through to.
    <MemoryRouter initialEntries={["/runs"]}>
      {/* operator=false/operatorResolved=false + principal="unknown": app-shell's
          own shape for a /me that never resolved (or resolved to a failure)
          before this page's session lapsed. */}
      <OperatorProvider operator={false} operatorResolved={false} principal="unknown">
        <ReauthContext.Provider value={reauth}>
          <ReauthLayer onResumed={vi.fn()} />
        </ReauthContext.Provider>
      </OperatorProvider>
    </MemoryRouter>,
  );
  return { reloadAs, setPhase };
}

describe("ReauthLayer — succeed() and an unresolved identity (SF-29)", () => {
  it("mount with /me unresolved, 401, sign in: adopts the session instead of reloading as a stranger", async () => {
    const user = userEvent.setup();
    const { reloadAs, setPhase } = renderDialog();

    await user.type(screen.getByLabelText(TOKEN_LABEL), "a-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    // The dialog closes (reauth.setPhase("none")) once succeed() adopts —
    // waiting for that settles the same promise chain reloadAs would ride.
    await waitFor(() => expect(setPhase).toHaveBeenCalledWith("none"));
    expect(reloadAs).not.toHaveBeenCalled();
  });

  it("still reloads as a stranger once the ORIGINAL principal was actually confirmed", async () => {
    const user = userEvent.setup();
    const reloadAs = vi.fn();
    const setPhase = vi.fn();
    const reauth: Reauth = {
      phase: "dialog",
      writeDropped: null,
      setPhase,
      reloadAs,
      clearWriteDropped: vi.fn(),
      writeDroppedClaimed: () => false,
      claimWriteDropped: () => vi.fn(),
    };
    render(
      <MemoryRouter>
        {/* A settled, DIFFERENT principal — the real "someone else" case. */}
        <OperatorProvider operator={false} operatorResolved={true} principal="alice">
          <ReauthContext.Provider value={reauth}>
            <ReauthLayer onResumed={vi.fn()} />
          </ReauthContext.Provider>
        </OperatorProvider>
      </MemoryRouter>,
    );
    await user.type(screen.getByLabelText(TOKEN_LABEL), "a-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() => expect(reloadAs).toHaveBeenCalled());
  });
});
