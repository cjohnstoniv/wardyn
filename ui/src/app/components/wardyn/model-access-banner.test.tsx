/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The strip's shell behaviour — the door it opens, where focus goes when that
// door closes, ownership, and where the strip says nothing — over a provider
// block (#548: every install has one). The per-provider kind/state table is
// provider-strip.test.tsx's.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";

// The strip is the unit under test; the login pane is a terminal screen with
// its own suite. Faked to ONE button, so "the door opened and the sign-in
// completed" is a fact this file can state.
vi.mock("../screens/settings/harness-login-pane", () => ({
  HarnessLoginPane: ({ modelProvider, onDone }: { modelProvider: string; onDone: () => void }) => (
    <button type="button" onClick={onDone}>{`fake pane ${modelProvider}`}</button>
  ),
}));

import { ModelAccessBanner } from "./model-access-banner";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { BANNER, CONNECTIONS } from "./copy/door";
import { ModelAccessProvider, useClaimModelAccessDoor, useModelAccessDoor } from "./model-access-context";
import { OperatorProvider } from "./operator-context";
import { ViewAccessProvider, type ViewAccess } from "./console-view";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { MODEL_PROVIDERS, providerStatus } from "../../lib/test-fixtures";
import type { SetupStatus } from "../../lib/types";

const { bedrock } = MODEL_PROVIDERS;
const B1 = BANNER.B1("Claude Code", bedrock.name);

function statusFor(state: string, cause?: string): SetupStatus {
  const s = providerStatus([{ provider: bedrock, defaultFor: ["claude-code"], state }], {
    harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: false, has_login: true }],
  });
  if (cause) s.provider_access![0].cause = cause;
  return s;
}

/** A page surface that owns the door (the New Run rail, a failure block). */
function Claimer() {
  useClaimModelAccessDoor(true);
  return null;
}

function renderStrip({
  state = "not_configured",
  cause,
  operator = false,
  principal = "member@corp.example",
  resolved = true,
  path = "/runs",
  onRefresh = vi.fn(),
  claimed = false,
  page,
  access,
}: {
  state?: string;
  cause?: string;
  operator?: boolean;
  principal?: string;
  resolved?: boolean;
  path?: string;
  onRefresh?: () => void | Promise<unknown>;
  claimed?: boolean;
  page?: React.ReactNode;
  /** The view access the shell provides; absent is the one-operator default, where the URL decides. */
  access?: ViewAccess;
}) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ModelAccessProvider status={statusFor(state, cause)} onRefresh={onRefresh}>
        <OperatorProvider operator={operator} securityOperator={operator} operatorResolved={resolved} principal={principal}>
          {claimed && <Claimer />}
          {/* The shell's own two pieces of context: the live region it mounts
              EAGERLY around this lazy chunk, and the skip-to-main target focus
              lands on when the strip that opened the door is gone. */}
          <ViewAccessProvider value={access ?? "url"}>
            <div role="status">
              <ModelAccessBanner />
            </div>
          </ViewAccessProvider>
          <main id="main-content" tabIndex={-1}>
            {page ?? "screen"}
          </main>
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
  it("a connected default renders no sentence and no button", () => {
    renderStrip({ state: "live" });
    expect(screen.queryByText(B1)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });

  it("adds no live region of its own", () => {
    renderStrip({});
    expect(screen.getAllByRole("status")).toHaveLength(1);
  });
});

describe("the door itself", () => {
  it("opens the provider's pane in place, and a completed sign-in refreshes the status and says so", async () => {
    const onRefresh = vi.fn();
    const success = vi.spyOn(toast, "success").mockImplementation(() => "id");
    renderStrip({ onRefresh });

    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    expect(screen.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("button", { name: `fake pane ${bedrock.id}` }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(success).toHaveBeenCalledWith(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST);
    expect(screen.queryByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeNull();
  });
});

describe("the strip's Re-check on an unreadable store", () => {
  it("is disabled while the status read is out, then live again", async () => {
    let finish: () => void = () => {};
    const onRefresh = vi.fn(() => new Promise<void>((resolve) => { finish = resolve; }));
    renderStrip({ cause: "store_unreadable", onRefresh });
    await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.RECHECK }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: CONNECTIONS.RECHECK })).toBeDisabled();
    await act(async () => finish());
    expect(screen.getByRole("button", { name: CONNECTIONS.RECHECK })).toBeEnabled();
  });
});

describe("door ownership — one primary recovery action per state per screen", () => {
  it("a claiming page takes the button and the strip keeps its sentence", () => {
    renderStrip({ claimed: true });
    expect(screen.getByText(B1)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });
});

// S1: focus after the door closes. Radix's FocusScope is still mounted while
// onDone/onCancel run, so anything focused there is taken back and lands on
// <body>; onCloseAutoFocus is the callback that fires after the trap releases,
// and every assertion below has to wait a macrotask for it.
const afterFocusSettles = () => act(() => new Promise((r) => setTimeout(r, 0)));

describe("where focus goes when the door closes", () => {
  it("a completed sign-in moves it to the main region — the strip it came from is gone", async () => {
    renderStrip({});
    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    await userEvent.click(screen.getByRole("button", { name: `fake pane ${bedrock.id}` }));
    await afterFocusSettles();
    expect(document.activeElement).toBe(document.getElementById("main-content"));
  });

  it("Escape returns it to the control that opened the door", async () => {
    renderStrip({});
    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    await userEvent.keyboard("{Escape}");
    await afterFocusSettles();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
  });

  it("a door opened by a PAGE control leaves focus to that page", async () => {
    function PageDoor() {
      const door = useModelAccessDoor();
      return (
        <button type="button" onClick={() => door.openDoor({ for: { provider: bedrock.id } })}>
          page door
        </button>
      );
    }
    renderStrip({ path: "/runs/new", page: <PageDoor /> });
    const page = screen.getByRole("button", { name: "page door" });
    await userEvent.click(page);
    await userEvent.keyboard("{Escape}");
    await afterFocusSettles();
    expect(document.activeElement).toBe(page);
  });
});

describe("where the strip says nothing", () => {
  it("before /me answers: no sentence and no button", () => {
    renderStrip({ resolved: false, principal: "" });
    expect(screen.queryByText(B1)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });

  it("never on /setup — the page is the door", () => {
    renderStrip({ path: "/setup" });
    expect(screen.queryByText(B1)).toBeNull();
  });
});

describe("the strip follows the session's view (FUG-L772)", () => {
  it("is not drawn on /account when the session holds the Admin view", () => {
    renderStrip({ path: "/account", access: "session-admin" });
    expect(screen.queryByRole("button", { name: /sign in/i })).not.toBeInTheDocument();
  });

  it("is drawn on /account for a one-operator install, where the path decides", () => {
    renderStrip({ path: "/account" });
    expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
  });

  it("is not drawn above a switch page: an SSO admin in member mode on /admin/runs (F5)", () => {
    renderStrip({ path: "/admin/runs", access: "session-user" });
    expect(screen.queryByRole("button", { name: /sign in/i })).not.toBeInTheDocument();
  });

  it("is drawn for that same member-mode session on a User page that renders", () => {
    renderStrip({ path: "/runs", access: "session-user" });
    expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
  });

  it("is not drawn for a one-view admin token on a User page", () => {
    renderStrip({ path: "/runs", access: "admin-only" });
    expect(screen.queryByRole("button", { name: /sign in/i })).not.toBeInTheDocument();
  });
});
