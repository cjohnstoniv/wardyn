/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";

// The strip is the unit under test; the login pane is a 941-line terminal
// screen with its own suite. Faked to ONE button, so "the door opened and the
// sign-in completed" is a fact this file can state.
vi.mock("../screens/settings/harness-login-pane", () => ({
  HarnessLoginPane: ({ startURLManaged, onDone }: { startURLManaged?: boolean; onDone: () => void }) => (
    <button type="button" onClick={onDone}>{`fake pane managed=${String(!!startURLManaged)}`}</button>
  ),
}));

import { ModelAccessBanner, modelAccessStripCopy } from "./model-access-banner";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { ModelAccessProvider, useClaimModelAccessDoor } from "./model-access-context";
import { OperatorProvider } from "./operator-context";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { absoluteTime } from "../../lib/format";
import { baseStatus } from "../../lib/test-fixtures";
import type { SetupHarnessTool, SetupModelAccess, SetupStatus } from "../../lib/types";

const PER_USER_ROW: SetupHarnessTool = {
  id: "claude-code",
  display: "claude-code",
  has_gateway: false,
  has_login: true,
  enabled: true,
  mechanism: "bedrock_sso",
  credential_source: "per_user",
};
const SHARED_ROW: SetupHarnessTool = { ...PER_USER_ROW, credential_source: "shared" };

const SHARED_EXPIRED_ACTION = "Your admin's model credential expired — ask them to reconnect it";
const PIN_ACTION =
  "Your stored AWS session is for account 111111111111 / role Old; this row now allows 222222222222 / New — sign in again.";

function statusFor(access: SetupModelAccess | undefined, row = PER_USER_ROW): SetupStatus {
  return baseStatus({ model_access: access, harnesses: [row] });
}

/** A page surface that owns the door (the New Run rail, a failure block). */
function Claimer() {
  useClaimModelAccessDoor(true);
  return null;
}

function renderStrip({
  access,
  row = PER_USER_ROW,
  operator = false,
  principal = "member@corp.example",
  path = "/runs",
  onRefresh = vi.fn(),
  claimed = false,
}: {
  access?: SetupModelAccess;
  row?: SetupHarnessTool;
  operator?: boolean;
  principal?: string;
  path?: string;
  onRefresh?: () => void;
  claimed?: boolean;
}) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ModelAccessProvider status={statusFor(access, row)} onRefresh={onRefresh}>
        <OperatorProvider operator={operator} securityOperator={operator} principal={principal}>
          {claimed && <Claimer />}
          <ModelAccessBanner />
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  try {
    window.sessionStorage.clear();
  } catch {
    /* jsdom always has it; the guard mirrors the component's */
  }
});
afterEach(() => vi.restoreAllMocks());

describe("the strip says nothing when there is nothing to say", () => {
  it.each([["live"], ["not_applicable"]])("state %s renders no sentence", (state) => {
    renderStrip({ access: { state } });
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });

  it("an absent model_access (a legacy install) renders no sentence", () => {
    renderStrip({ access: undefined });
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });

  it("keeps its live region MOUNTED so a later state change is announced, not mounted", () => {
    renderStrip({ access: { state: "live" } });
    expect(screen.getByRole("status")).toBeInTheDocument();
  });
});

describe("the per-person states", () => {
  it("not_configured: the sentence, the button, and a 'Not now'", async () => {
    renderStrip({ access: { state: "not_configured", action: AGENTS.SIGN_IN_AWS } });
    expect(screen.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeInTheDocument();
    // S1 / W0-mock ruling 1: the server's action for this state is BYTE-IDENTICAL
    // to the button's label, so rendering it too would print the button's label
    // as prose. Exactly one control carries that name.
    expect(screen.getAllByText(AGENTS.SIGN_IN_AWS)).toHaveLength(1);
  });

  it("expired_signin: one sentence true of a lapse AND of a pin contradiction, with the server's pair beside it", () => {
    renderStrip({ access: { state: "expired_signin", action: PIN_ACTION } });
    expect(screen.getByText(MODEL_ACCESS_BANNER.EXPIRED)).toBeInTheDocument();
    // The one action line that carries what our sentence and the button cannot.
    expect(screen.getByText(PIN_ACTION)).toBeInTheDocument();
    // A lapse of something the person already had is never dismissable.
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeNull();
  });

  it("expiring: the deadline on the reader's clock, the absolute stamp as its title, the info tone", () => {
    const deadline = "2026-09-19T14:03:22Z";
    renderStrip({
      access: { state: "expiring", action: `Sign in again before ${deadline}`, deadline },
    });
    const line = screen.getByTitle(absoluteTime(deadline));
    expect(line.textContent).toMatch(/^Your AWS sign-in lapses in /);
    // The raw UTC stamp never reaches the screen…
    expect(screen.queryByText(new RegExp(deadline))).toBeNull();
    // …and no separate action line: the deadline is IN the sentence (S1).
    expect(screen.queryByText(`Sign in again before ${deadline}`)).toBeNull();
    expect(line.closest("div")?.className).toContain("bg-info-subtle");
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeNull();
  });

  it("expiring against a daemon that sends no deadline falls back to the server's sentence", () => {
    const action = "Sign in again before 2026-09-19T14:03:22Z";
    renderStrip({ access: { state: "expiring", action } });
    expect(screen.getByText(action)).toBeInTheDocument();
  });
});

describe("the shared row — one credential, two audiences (Codex #7)", () => {
  it("a member reads the server's instruction ALONE, with no button", () => {
    renderStrip({
      access: { state: "shared_expired", action: SHARED_EXPIRED_ACTION },
      row: SHARED_ROW,
    });
    expect(screen.getByText(SHARED_EXPIRED_ACTION)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
    // …and may set it aside: it is the one state they cannot act on at all.
    expect(screen.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeInTheDocument();
  });

  it("the ADMIN reads the blast radius and gets the repair path — never 'ask your admin'", () => {
    renderStrip({
      access: { state: "shared_expired", action: SHARED_EXPIRED_ACTION },
      row: SHARED_ROW,
      operator: true,
      principal: "admin@corp.example",
    });
    expect(screen.getByText(MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED)).toBeInTheDocument();
    expect(screen.queryByText(SHARED_EXPIRED_ACTION)).toBeNull();
    expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    // Actionable, therefore undismissable (W0-mock ruling 3).
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeNull();
  });

  it("the ADMIN's expiring names the shared credential, not a personal one", () => {
    const deadline = "2026-09-19T14:03:22Z";
    renderStrip({
      access: { state: "expiring", action: `Sign in again before ${deadline}`, deadline },
      row: SHARED_ROW,
      operator: true,
    });
    expect(screen.getByTitle(absoluteTime(deadline)).textContent).toMatch(
      /^The shared AWS sign-in every Claude Code run uses lapses in /,
    );
  });
});

describe("door ownership — one primary recovery action per state per screen", () => {
  it("a claiming page takes the button and the strip keeps its sentence", () => {
    renderStrip({ access: { state: "expired_signin", action: AGENTS.SIGN_IN_AWS }, claimed: true });
    expect(screen.getByText(MODEL_ACCESS_BANNER.EXPIRED_SHORT)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_BANNER.EXPIRED)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });
});

describe("the door itself", () => {
  it("opens the pane in place, and a completed sign-in refreshes the status and says so", async () => {
    const onRefresh = vi.fn();
    const success = vi.spyOn(toast, "success").mockImplementation(() => "id");
    renderStrip({ access: { state: "not_configured", action: AGENTS.SIGN_IN_AWS }, onRefresh });

    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    // getByRole, not getByText: the dialog's TITLE and the strip's button carry
    // the same words on purpose — one spelling everywhere (W0-mock ruling 5).
    expect(screen.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeInTheDocument();
    expect(screen.getByText(MODEL_ACCESS_BANNER.DIALOG_DESCRIPTION)).toBeInTheDocument();
    // A per_user row: the org's portal is stored, so the pane never asks for one.
    const pane = screen.getByRole("button", { name: "fake pane managed=true" });

    await userEvent.click(pane);
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(success).toHaveBeenCalledWith(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST);
    expect(screen.queryByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeNull();
  });

  it("a shared row's admin signs in with the start-URL prompt (nothing is stored for it)", async () => {
    renderStrip({
      access: { state: "shared_expired", action: SHARED_EXPIRED_ACTION },
      row: SHARED_ROW,
      operator: true,
    });
    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    expect(screen.getByRole("button", { name: "fake pane managed=false" })).toBeInTheDocument();
  });
});

describe("'Not now' is per viewer, per browsing context", () => {
  it("hides the strip for this session and is keyed on the viewer's subject", async () => {
    const access: SetupModelAccess = { state: "not_configured", action: AGENTS.SIGN_IN_AWS };
    const first = renderStrip({ access, principal: "first@corp.example" });
    await userEvent.click(screen.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW }));
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
    first.unmount();

    // sessionStorage survives a sign-out in the same tab, so an UNKEYED flag
    // would pre-dismiss the strip for the next person on this machine.
    renderStrip({ access, principal: "second@corp.example" });
    expect(screen.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeInTheDocument();
  });
});

describe("where the strip is withheld", () => {
  it("never on /setup — the page is the door", () => {
    renderStrip({ access: { state: "not_configured" }, path: "/setup" });
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
  });

  it.each([["/settings"], ["/providers"]])("not on %s for an OPERATOR — those pages mount the pane", (path) => {
    renderStrip({ access: { state: "not_configured" }, path, operator: true });
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
  });

  it("but a MEMBER keeps it on /settings — the card's AWS button is admin-only there", () => {
    renderStrip({ access: { state: "not_configured" }, path: "/settings" });
    expect(screen.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeInTheDocument();
  });
});

// The table, read directly — a render assertion proves the wiring, this proves
// the rule for every state at once, including the ones no fixture reaches.
describe("modelAccessStripCopy", () => {
  const door = (over: Partial<Parameters<typeof modelAccessStripCopy>[0]>) => ({
    state: "",
    action: "",
    deadline: "",
    needsAttention: true,
    actionable: true,
    perUser: true,
    ...over,
  });

  it("says nothing about a state this console does not know", () => {
    expect(modelAccessStripCopy(door({ state: "expired_renewable" }), { operator: false }, false)).toMatchObject({
      sentence: "",
      action: "",
    });
  });

  it("only not_configured and a non-operator's shared_expired are dismissable", () => {
    const dismissable = (state: string, operator: boolean) =>
      modelAccessStripCopy(door({ state }), { operator }, false).dismissible;
    expect(dismissable("not_configured", false)).toBe(true);
    expect(dismissable("not_configured", true)).toBe(true);
    expect(dismissable("shared_expired", false)).toBe(true);
    expect(dismissable("shared_expired", true)).toBe(false);
    expect(dismissable("expiring", false)).toBe(false);
    expect(dismissable("expired_signin", false)).toBe(false);
  });
});
