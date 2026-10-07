/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SF-29: `succeed`'s "someone else signed in" branch compares the incoming
// /me against `usePrincipal()`, whose value while identity is unresolved
// (app-shell's still-loading "…", or its fail-open "unknown" once whoami()
// swallows a failure and returns null — health.ts's whoami()) is never a
// real principal. An unknown first identity cannot prove the person signing
// back in is the same one, so the rule fails closed: a fresh reload, never
// the first person's page (Q457-12).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import { ReauthLayer } from "./reauth-layer";
import { OperatorProvider } from "./operator-context";
import { ReauthContext, useReauthController, type Reauth, type Renewal } from "../../lib/reauth";
import { REAUTH_DIALOG, REAUTH_EXTRA, REAUTH_RENEW } from "../../lib/reauth-copy";
import { shortTime } from "../../lib/format";
import type { Me } from "../../lib/api/health";
import { TOKEN_LABEL } from "../screens/sign-in";
import { setField } from "../../../test/set-field";
import { aheadByHours } from "../../lib/test-clock";
import { wfetch } from "../../lib/api/core";

vi.mock("../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({}) },
}));

const meResponse = {
  principal: "gsv-member-0001",
  method: "token",
  operator: false,
  security_operator: false,
  role: "user",
  email: "",
};
// What GET /me answers next: a body, or a status with none (a 5xx reads as unreachable).
let answer: { body?: object; status: number } = { body: meResponse, status: 200 };
vi.mock("../../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../lib/api/core")>()),
  wfetch: vi.fn(() => Promise.resolve(new Response(JSON.stringify(answer.body ?? {}), { status: answer.status }))),
  setToken: vi.fn(),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), warning: vi.fn() } }));

function renderDialog(reauthOverrides: Partial<Reauth> = {}) {
  const reloadAs = vi.fn();
  const setPhase = vi.fn();
  const reauth: Reauth = {
    phase: "dialog",
    signedOut: true,
    renewal: null,
    watch: null,
    writeDropped: null,
    setPhase,
    setWatch: vi.fn(),
    startRenew: vi.fn(),
    endRenew: vi.fn(),
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
  it("mount with /me unresolved, 401, sign in: reloads fresh, since sameness cannot be proven", async () => {
    const user = userEvent.setup();
    const { reloadAs, setPhase } = renderDialog();

    setField(screen.getByLabelText(TOKEN_LABEL), "a-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() => expect(reloadAs).toHaveBeenCalledWith("/runs"));
    // Never adopted: the dialog (and the write hold it keeps) stays up.
    expect(setPhase).not.toHaveBeenCalledWith("none");
  });

  it("still reloads as a stranger once the ORIGINAL principal was actually confirmed", async () => {
    const user = userEvent.setup();
    const reloadAs = vi.fn();
    const setPhase = vi.fn();
    const reauth: Reauth = {
      phase: "dialog",
      signedOut: true,
      renewal: null,
      watch: null,
      writeDropped: null,
      setPhase,
      setWatch: vi.fn(),
      startRenew: vi.fn(),
      endRenew: vi.fn(),
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
    setField(screen.getByLabelText(TOKEN_LABEL), "a-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() => expect(reloadAs).toHaveBeenCalled());
  });
});

// The expiry banner's "Sign in again", renewing in place. The session is still
// live while the sign-in window is open, so every /me answers 200: who it is
// and with what authority is read first, and only then whether the expiry moved.
describe("ReauthLayer — a renewal from the expiry banner", () => {
  const POLL_MS = 1500;
  // The session the renewal started from, and what a replacement can carry.
  const UNTIL = Date.parse(aheadByHours(1));
  const at = (ms: number) => new Date(ms).toISOString();
  const EQUAL = at(UNTIL);
  const EARLIER = at(UNTIL - 10 * 60 * 1000);
  const LATER = at(UNTIL + 60 * 60 * 1000);
  const alice = (over: Partial<Me> = {}): Me => ({
    principal: "alice",
    method: "sso",
    operator: true,
    security_operator: true,
    role: "admin",
    email: "",
    session_expires_at: EQUAL,
    ...over,
  });

  function signInWindow() {
    const popup = { closed: false, close: vi.fn(() => void (popup.closed = true)) };
    return popup;
  }

  function Renewing({ from, onResumed, reloadAs }: { from: Renewal; onResumed: (me: Me) => void; reloadAs: (path: string) => void }) {
    const { reauth, lapse } = useReauthController(reloadAs);
    return (
      <ReauthContext.Provider value={reauth}>
        <button type="button" onClick={() => reauth.startRenew(from)}>
          banner
        </button>
        <button type="button" onClick={() => lapse({ write: true })}>
          a 401 elsewhere
        </button>
        <output data-testid="phase">{reauth.phase}</output>
        <output data-testid="signed-out">{String(reauth.signedOut)}</output>
        {/* The shell's own rule (app-shell.tsx): a watched sign-in keeps the layer. */}
        {(reauth.phase !== "none" || reauth.watch) && <ReauthLayer onResumed={onResumed} />}
        <main id="main-content" tabIndex={-1} />
      </ReauthContext.Provider>
    );
  }

  /** Alice, an admin on /admin/settings, has clicked the banner's button. */
  function startRenewal(over: Partial<Renewal> = {}) {
    const popup = signInWindow();
    const onResumed = vi.fn();
    const reloadAs = vi.fn();
    const from: Renewal = {
      principal: "alice",
      role: "admin",
      operator: true,
      securityOperator: true,
      expiresAt: UNTIL,
      popup: popup as unknown as Window,
      ...over,
    };
    render(
      <MemoryRouter initialEntries={["/admin/settings"]}>
        <OperatorProvider operator operatorResolved principal="alice">
          <Renewing from={from} onResumed={onResumed} reloadAs={reloadAs} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    fireEvent.click(screen.getByRole("button", { name: "banner" }));
    return { popup, onResumed, reloadAs };
  }
  const poll = (times = 1) => act(() => vi.advanceTimersByTimeAsync(POLL_MS * times));
  const phase = () => screen.getByTestId("phase").textContent;
  const reads = () => vi.mocked(wfetch).mock.calls.length;
  const cancel = () => fireEvent.click(screen.getByRole("button", { name: REAUTH_RENEW.CANCEL }));
  const bob = (over: Partial<Me> = {}) => alice({ principal: "bob", ...over });
  // What the browser answers the fallback link's own window.open: refused
  // again, unless a test hands it a tab.
  const open = vi.fn((): unknown => null);

  beforeEach(() => {
    vi.useFakeTimers();
    vi.mocked(toast.success).mockClear();
    vi.mocked(toast.warning).mockClear();
    open.mockReset().mockReturnValue(null);
    vi.stubGlobal("open", open);
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    answer = { body: meResponse, status: 200 };
  });

  it("T1: a different principal, at an equal and at an earlier expiry, each reloads the page as that person", async () => {
    for (const session_expires_at of [EQUAL, EARLIER]) {
      answer = { body: alice({ principal: "bob", session_expires_at }), status: 200 };
      const { onResumed, reloadAs } = startRenewal();
      await poll();
      expect(reloadAs).toHaveBeenCalledWith("/admin/settings");
      // Nothing of alice's page is adopted under bob.
      expect(onResumed).not.toHaveBeenCalled();
      expect(toast.success).not.toHaveBeenCalled();
      cleanup();
    }
  });

  it("T2: a narrowed role with an equal expiry is applied as narrowed", async () => {
    const narrowed = alice({ role: "user", operator: false, security_operator: false });
    answer = { body: narrowed, status: 200 };
    const { onResumed, reloadAs } = startRenewal();
    await poll();
    // /admin/settings is no longer hers: the role-changed dialog says so.
    expect(phase()).toBe("dialog");
    expect(screen.getByText(REAUTH_DIALOG.ROLE_CHANGED_BODY)).toBeInTheDocument();
    expect(toast.success).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: REAUTH_EXTRA.GO_TO_RUNS }));
    expect(onResumed).toHaveBeenCalledWith(narrowed);
    expect(reloadAs).not.toHaveBeenCalled();
  });

  it("T3: the same person, expiry unchanged, keeps waiting until the window closes", async () => {
    answer = { body: alice(), status: 200 };
    const { popup, onResumed, reloadAs } = startRenewal();
    await poll(3);
    expect(screen.getByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();
    expect(phase()).toBe("renew");
    expect(popup.close).not.toHaveBeenCalled();

    popup.closed = true;
    await poll();
    expect(screen.getByText(REAUTH_DIALOG.CLOSED_WITHOUT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: REAUTH_RENEW.CTA })).toBeInTheDocument();
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice());
    expect(reloadAs).not.toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("T4: the same person, later expiry, is renewed", async () => {
    const renewed = alice({ session_expires_at: LATER });
    answer = { body: alice(), status: 200 };
    const { popup, onResumed, reloadAs } = startRenewal();
    await poll();
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice());

    answer = { body: renewed, status: 200 };
    await poll();
    expect(onResumed).toHaveBeenCalledWith(renewed);
    expect(toast.success).toHaveBeenCalledWith(REAUTH_RENEW.RENEWED(shortTime(LATER)));
    expect(phase()).toBe("none");
    expect(screen.queryByText(REAUTH_DIALOG.WAITING)).toBeNull();
    expect(popup.close).toHaveBeenCalled();
    expect(document.getElementById("main-content")).toHaveFocus();
    expect(reloadAs).not.toHaveBeenCalled();
  });

  // A browser clock ahead of the server's reads "expired" on the banner while
  // the session is still live: the old session answering is not a renewal.
  it("a banner that already read expired keeps waiting while /me answers the old expiry", async () => {
    vi.setSystemTime(UNTIL + 60_000);
    answer = { body: alice(), status: 200 };
    const { onResumed, reloadAs } = startRenewal();
    await poll(3);
    expect(screen.getByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();
    expect(phase()).toBe("renew");
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice());
    expect(reloadAs).not.toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalled();

    const renewed = alice({ session_expires_at: LATER });
    answer = { body: renewed, status: 200 };
    await poll();
    expect(onResumed).toHaveBeenCalledWith(renewed);
    expect(phase()).toBe("none");
  });

  it("a request refused while renewing keeps the strip and holds the page; Cancel then asks through the dialog", async () => {
    answer = { status: 401 };
    startRenewal();
    // A renewal alone leaves the page working.
    expect(screen.getByTestId("signed-out")).toHaveTextContent("false");

    fireEvent.click(screen.getByRole("button", { name: "a 401 elsewhere" }));
    expect(phase()).toBe("renew");
    expect(screen.getByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();
    expect(screen.getByTestId("signed-out")).toHaveTextContent("true");

    // The session is over: backing out cannot land on a working page.
    fireEvent.click(screen.getByRole("button", { name: REAUTH_RENEW.CANCEL }));
    expect(phase()).toBe("dialog");
    expect(screen.getByRole("dialog", { name: REAUTH_DIALOG.TITLE })).toBeInTheDocument();
    expect(screen.getByTestId("signed-out")).toHaveTextContent("true");
  });

  it("focus moves to Cancel, which names the status; Escape cancels, closes the window and stops the wait", async () => {
    answer = { body: alice(), status: 200 };
    const { popup, onResumed } = startRenewal();
    const cancel = screen.getByRole("button", { name: REAUTH_RENEW.CANCEL });
    expect(cancel).toHaveFocus();
    expect(document.getElementById(cancel.getAttribute("aria-describedby") ?? "")).toHaveTextContent(
      REAUTH_DIALOG.WAITING,
    );

    // fireEvent returns false when the handler prevented the default.
    expect(fireEvent.keyDown(cancel, { key: "Escape" })).toBe(false);
    expect(phase()).toBe("none");
    expect(popup.close).toHaveBeenCalled();

    answer = { body: alice({ session_expires_at: LATER }), status: 200 };
    await poll(2);
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice({ session_expires_at: LATER }));
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("a refused window offers the sign-in in a new tab; an outage reads as one and waiting resumes after it", async () => {
    answer = { status: 503 };
    startRenewal({ popup: null });
    expect(screen.getByText(REAUTH_DIALOG.POPUP_BLOCKED)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: REAUTH_DIALOG.POPUP_FALLBACK });
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("href", "/auth/login");

    fireEvent.click(link);
    await poll();
    expect(screen.getByText(REAUTH_DIALOG.UNREACHABLE)).toBeInTheDocument();
    answer = { body: alice(), status: 200 };
    await poll();
    expect(screen.getByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: REAUTH_RENEW.CANCEL })).toBeInTheDocument();
  });

  // A sign-in this flow started can still complete after the page stops
  // waiting for it: the server honours a started login for ten minutes, twice
  // over when it restarts one (internal/auth/oidc), plus the callback's own work.
  const WATCH_MS = 21 * 60 * 1000;

  it("Cancel after the new-tab fallback: someone else finishing that sign-in reloads the page as them", async () => {
    answer = { body: alice(), status: 200 };
    const { onResumed, reloadAs } = startRenewal({ popup: null });
    // The browser refuses this window too, so the link opens a tab of its own:
    // one this page holds no handle on, and Cancel cannot close.
    fireEvent.click(screen.getByRole("link", { name: REAUTH_DIALOG.POPUP_FALLBACK }));
    await poll();
    cancel();
    expect(phase()).toBe("none");
    expect(screen.queryByText(REAUTH_DIALOG.WAITING)).toBeNull();

    answer = { body: bob({ session_expires_at: LATER }), status: 200 };
    await poll();
    expect(reloadAs).toHaveBeenCalledWith("/admin/settings");
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice());
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("the fallback's tab is opened from the click, so Cancel closes it, and who signs in is still watched", async () => {
    answer = { body: alice(), status: 200 };
    const tab = { closed: false, close: vi.fn(() => void (tab.closed = true)), opener: window as unknown, location: { href: "" } };
    open.mockReturnValue(tab);
    const { reloadAs } = startRenewal({ popup: null });
    // Prevented: the click opened the tab, so the link must not open a second.
    expect(fireEvent.click(screen.getByRole("link", { name: REAUTH_DIALOG.POPUP_FALLBACK }))).toBe(false);
    expect(open).toHaveBeenCalledWith("about:blank", "_blank");
    expect(tab.opener).toBeNull();
    expect(tab.location.href).toBe("/auth/login");
    expect(screen.getByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();

    cancel();
    expect(tab.close).toHaveBeenCalled();
    answer = { body: bob(), status: 200 };
    await poll();
    expect(reloadAs).toHaveBeenCalledWith("/admin/settings");
  });

  it("Cancel with the window open: a sign-in already on its way that lands as someone else reloads the page as them", async () => {
    answer = { body: alice(), status: 200 };
    const { popup, onResumed, reloadAs } = startRenewal();
    await poll();
    cancel();
    // Closing the window does not recall a callback the server already has.
    expect(popup.close).toHaveBeenCalled();
    expect(phase()).toBe("none");
    expect(reloadAs).not.toHaveBeenCalled();

    answer = { body: bob(), status: 200 };
    await poll();
    expect(reloadAs).toHaveBeenCalledWith("/admin/settings");
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice());
  });

  it("Cancel, then the same person with a narrowed role: the role is applied", async () => {
    answer = { body: alice(), status: 200 };
    const { onResumed, reloadAs } = startRenewal();
    cancel();
    const narrowed = alice({ role: "user", operator: false, security_operator: false });
    answer = { body: narrowed, status: 200 };
    await poll();
    expect(phase()).toBe("dialog");
    expect(screen.getByTestId("signed-out")).toHaveTextContent("true");
    expect(screen.getByText(REAUTH_DIALOG.ROLE_CHANGED_BODY)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: REAUTH_EXTRA.GO_TO_RUNS }));
    expect(onResumed).toHaveBeenCalledWith(narrowed);
    expect(reloadAs).not.toHaveBeenCalled();
  });

  it("Cancel, then the same person: nothing changes, and the watch stops at its bound", async () => {
    // Even renewed: they backed out of this renewal, so it says nothing.
    answer = { body: alice({ session_expires_at: LATER }), status: 200 };
    const { onResumed, reloadAs } = startRenewal();
    cancel();
    const atCancel = reads();

    // Still asking right up to the bound…
    await act(() => vi.advanceTimersByTimeAsync(WATCH_MS - POLL_MS));
    const nearBound = reads();
    expect(nearBound).toBeGreaterThan(atCancel + 800);
    await poll();
    expect(reads()).toBeGreaterThan(nearBound);
    // …and not after it.
    await poll(2);
    const atBound = reads();
    await poll(20);
    expect(reads()).toBe(atBound);

    expect(phase()).toBe("none");
    expect(screen.getByTestId("signed-out")).toHaveTextContent("false");
    expect(screen.queryByRole("button", { name: REAUTH_RENEW.CANCEL })).toBeNull();
    expect(screen.queryByText(REAUTH_DIALOG.WAITING)).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice({ session_expires_at: LATER }));
    expect(reloadAs).not.toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalled();
    expect(toast.warning).not.toHaveBeenCalled();
  });

  it("Cancel with the window refused and fallback unused confirms once, then stops watching", async () => {
    answer = { body: alice(), status: 200 };
    const { onResumed, reloadAs } = startRenewal({ popup: null });
    const before = reads();
    cancel();
    await poll(4);
    expect(reads()).toBe(before + 1);
    expect(onResumed).toHaveBeenCalledExactlyOnceWith(alice());
    expect(reloadAs).not.toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("a window that closed is not proof: someone else's sign-in landing afterwards still reloads the page", async () => {
    answer = { body: alice(), status: 200 };
    const { popup, reloadAs } = startRenewal();
    popup.closed = true;
    await poll();
    expect(screen.getByText(REAUTH_DIALOG.CLOSED_WITHOUT)).toBeInTheDocument();

    answer = { body: bob(), status: 200 };
    await poll();
    expect(reloadAs).toHaveBeenCalledWith("/admin/settings");
  });

  it("a second window the browser refuses does not end it: the first one's sign-in can still land", async () => {
    answer = { body: alice(), status: 200 };
    const { popup, reloadAs } = startRenewal();
    popup.closed = true;
    await poll();
    fireEvent.click(screen.getByRole("button", { name: REAUTH_RENEW.CTA }));
    expect(screen.getByText(REAUTH_DIALOG.POPUP_BLOCKED)).toBeInTheDocument();

    answer = { body: bob(), status: 200 };
    await poll();
    expect(reloadAs).toHaveBeenCalledWith("/admin/settings");
  });

  it.each(["open", "closed", "fallback"])("a visible %s renewal keeps reconciling beyond the quiet-watch bound", async (kind) => {
    answer = { body: alice(), status: 200 };
    const { popup, onResumed } = startRenewal(kind === "fallback" ? { popup: null } : {});
    if (kind === "closed") popup.closed = true;
    if (kind === "fallback") fireEvent.click(screen.getByRole("link", { name: REAUTH_DIALOG.POPUP_FALLBACK }));
    await act(() => vi.advanceTimersByTimeAsync(WATCH_MS + 6 * 60 * 1000));
    const before = reads();
    const cancelButton = screen.getByRole("button", { name: REAUTH_RENEW.CANCEL });
    expect(cancelButton).toHaveFocus();
    answer = { body: alice({ session_expires_at: LATER }), status: 200 };
    await poll();
    expect(reads()).toBeGreaterThan(before);
    expect(onResumed).toHaveBeenCalledTimes(2);
    expect(onResumed).toHaveBeenLastCalledWith(alice({ session_expires_at: LATER }));
    expect(phase()).toBe("none");
    expect(document.getElementById("main-content")).toHaveFocus();
  });
});
