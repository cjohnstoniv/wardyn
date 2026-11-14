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
import * as React from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { ReauthLayer } from "./reauth-layer";
import { OperatorProvider } from "./operator-context";
import { ReauthContext, useReauthController, type Reauth, type Renewal } from "../../lib/reauth";
import { REAUTH_DIALOG, REAUTH_EXTRA, REAUTH_RENEW } from "../../lib/reauth-copy";
import { RENEW_STRIP_SLOT } from "../../lib/use-session-renew";
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
// What GET /me answers next: a body (`raw` for one that is not JSON), or a status with none; status 0 is no answer
// at all (wfetch's deadline).
let answer: { body?: object; raw?: string; status: number } = { body: meResponse, status: 200 };
vi.mock("../../lib/api/core", async (importOriginal) => {
  const core = await importOriginal<typeof import("../../lib/api/core")>();
  return {
    ...core,
    wfetch: vi.fn(() =>
      answer.status === core.TIMEOUT_STATUS
        ? Promise.reject(new core.HttpError(core.TIMEOUT_STATUS, "Could not reach the control plane."))
        : Promise.resolve(new Response(answer.raw ?? JSON.stringify(answer.body ?? {}), { status: answer.status })),
    ),
    setToken: vi.fn(),
  };
});
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

  it("a token check Wardyn answered with a 5xx says the check failed, never that Wardyn isn't answering", async () => {
    const user = userEvent.setup();
    answer = { status: 503 };
    renderDialog();
    setField(screen.getByLabelText(TOKEN_LABEL), "a-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText(REAUTH_DIALOG.CHECK_FAILED)).toBeInTheDocument();
    expect(screen.queryByText(REAUTH_DIALOG.UNREACHABLE)).toBeNull();
    answer = { body: meResponse, status: 200 };
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

  // The page under the dialog: its h1 names the route, so a test can see which page took focus.
  // `lateRunsHeading` holds the Runs heading back until "reveal heading" is pressed, as a lazy chunk would.
  function PageHeading({ lateRunsHeading }: { lateRunsHeading?: boolean }) {
    const { pathname } = useLocation();
    const navigate = useNavigate();
    const [revealed, setRevealed] = React.useState(!lateRunsHeading);
    // MemoryRouter never moves window.location; BrowserRouter does (before the page draws), and the heading watch reads it.
    React.useLayoutEffect(() => window.history.replaceState(null, "", pathname), [pathname]);
    return (
      <>
        {pathname === "/runs" ? revealed && <h1 key="runs">Runs</h1> : pathname === "/elsewhere" ? <h1 key="elsewhere">Elsewhere</h1> : <h1 key="settings">Settings</h1>}
        <button type="button" onClick={() => setRevealed(true)}>reveal heading</button>
        <button type="button" onClick={() => void navigate("/elsewhere")}>go elsewhere</button>
        <button type="button">other control</button>
      </>
    );
  }

  function Renewing({ from, onResumed, reloadAs, withHeading, lateRunsHeading }: { from: Renewal; onResumed: (me: Me) => void; reloadAs: (path: string) => void; withHeading?: boolean; lateRunsHeading?: boolean }) {
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
        {withHeading && <button type="button">outside control</button>}
        <main id="main-content" tabIndex={-1}>
          {withHeading && <PageHeading lateRunsHeading={lateRunsHeading} />}
        </main>
      </ReauthContext.Provider>
    );
  }

  /** Alice, an admin on /admin/settings, has clicked the banner's button. */
  function startRenewal(over: Partial<Renewal> = {}, withHeading = false, lateRunsHeading = false) {
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
          <Renewing from={from} onResumed={onResumed} reloadAs={reloadAs} withHeading={withHeading} lateRunsHeading={lateRunsHeading} />
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

  // goToRuns replaces the page, the dialog's return target goes with it, and the shell does not move focus for a
  // replace: without help focus is left on the body and nothing says the person is now on Runs.
  it("T2b: Go to Runs ends with focus on the Runs heading, not on the body", async () => {
    answer = { body: alice({ role: "user", operator: false, security_operator: false }), status: 200 };
    startRenewal({}, true);
    await poll();
    expect(screen.getByRole("heading", { name: "Settings", hidden: true })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: REAUTH_EXTRA.GO_TO_RUNS }));
    await act(() => vi.advanceTimersByTimeAsync(0));
    const runs = screen.getByRole("heading", { name: "Runs" });
    expect(runs).toHaveFocus();
    expect(document.activeElement).not.toBe(document.body);
  });

  // The Runs heading can arrive seconds later (a lazy chunk). What the person did meanwhile outranks it.
  async function goToRunsWithLateHeading() {
    answer = { body: alice({ role: "user", operator: false, security_operator: false }), status: 200 };
    startRenewal({}, true, true);
    await poll();
    fireEvent.click(screen.getByRole("button", { name: REAUTH_EXTRA.GO_TO_RUNS }));
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.queryByRole("heading", { name: "Runs" })).toBeNull();
  }
  const reveal = () => act(() => void fireEvent.click(screen.getByRole("button", { name: "reveal heading" })));

  it("T2c: a key pressed and focus moved before the Runs heading arrives keep that focus", async () => {
    await goToRunsWithLateHeading();
    fireEvent.keyDown(document.body, { key: "Tab" });
    screen.getByRole("button", { name: "outside control" }).focus();
    await reveal();
    expect(screen.getByRole("heading", { name: "Runs" })).not.toHaveFocus();
    expect(screen.getByRole("button", { name: "outside control" })).toHaveFocus();
  });

  it("T2c2: focus the page placed inside itself before its heading arrives is kept", async () => {
    await goToRunsWithLateHeading();
    screen.getByRole("button", { name: "other control" }).focus();
    await reveal();
    expect(screen.getByRole("button", { name: "other control" })).toHaveFocus();
  });

  it("T2d: navigating elsewhere before the Runs heading arrives, the late Runs heading takes no focus", async () => {
    await goToRunsWithLateHeading();
    await act(async () => {
      screen.getByRole("button", { name: "go elsewhere" }).click();
    });
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByRole("heading", { name: "Elsewhere" })).toBeInTheDocument();
    // Runs is no longer the page; its heading cannot arrive, and nothing is pulled to it.
    expect(document.activeElement).not.toBe(screen.getByRole("heading", { name: "Elsewhere" }));
  });

  it("T2e: a Runs heading that arrives late with the person idle still takes focus", async () => {
    await goToRunsWithLateHeading();
    await reveal();
    expect(screen.getByRole("heading", { name: "Runs" })).toHaveFocus();
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

  // The shell draws the banner while no renewal waits and the strip in its slot
  // while one does (app-shell.tsx): the same two places, with the banner's real
  // button text, so the layer finds its way back to it.
  function BannerAndStrip({ from, lapseButton }: { from: Renewal; lapseButton?: boolean }) {
    const { reauth, lapse } = useReauthController(vi.fn());
    return (
      <ReauthContext.Provider value={reauth}>
        {reauth.phase !== "renew" && (
          <div role="status">
            <span>Your session is expiring soon.</span>
            <button type="button" onClick={() => reauth.startRenew(from)}>
              {REAUTH_RENEW.CTA}
            </button>
          </div>
        )}
        <div id={RENEW_STRIP_SLOT} />
        {lapseButton && <button type="button" onClick={() => lapse({ write: true })}>a 401 elsewhere</button>}
        {(reauth.phase !== "none" || reauth.watch) && <ReauthLayer onResumed={vi.fn()} />}
        <main id="main-content" tabIndex={-1} />
      </ReauthContext.Provider>
    );
  }
  function bannerShown(lapseButton = false) {
    const from: Renewal = {
      principal: "alice", role: "admin", operator: true, securityOperator: true, expiresAt: UNTIL,
      popup: signInWindow() as unknown as Window,
    };
    render(
      <MemoryRouter initialEntries={["/admin/settings"]}>
        <OperatorProvider operator operatorResolved principal="alice">
          <BannerAndStrip from={from} lapseButton={lapseButton} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    return screen.getByRole("button", { name: REAUTH_RENEW.CTA });
  }

  // M-F F6: the strip, and the Cancel that held focus, are gone after Cancel or
  // Escape; focus goes to the banner's own button that comes back, not to the document.
  it("Cancel returns focus to the banner's Sign in again", () => {
    answer = { body: alice(), status: 200 };
    fireEvent.click(bannerShown());
    expect(screen.getByRole("button", { name: REAUTH_RENEW.CANCEL })).toHaveFocus();
    cancel();
    expect(screen.queryByRole("button", { name: REAUTH_RENEW.CANCEL })).toBeNull();
    expect(screen.getByRole("button", { name: REAUTH_RENEW.CTA })).toHaveFocus();
  });

  it("Escape in the strip is consumed and returns focus to the banner's Sign in again", () => {
    answer = { body: alice(), status: 200 };
    fireEvent.click(bannerShown());
    const cancelButton = screen.getByRole("button", { name: REAUTH_RENEW.CANCEL });
    expect(fireEvent.keyDown(cancelButton, { key: "Escape" })).toBe(false);
    expect(screen.getByRole("button", { name: REAUTH_RENEW.CTA })).toHaveFocus();
  });

  it("Cancel after a request was refused leaves focus to the dialog, not the banner", () => {
    answer = { body: alice(), status: 200 };
    fireEvent.click(bannerShown(true));
    fireEvent.click(screen.getByRole("button", { name: "a 401 elsewhere" }));
    cancel();
    expect(screen.getByRole("dialog", { name: REAUTH_DIALOG.TITLE })).toBeInTheDocument();
    expect(document.activeElement).not.toBe(screen.queryByRole("button", { name: REAUTH_RENEW.CTA }));
  });

  it("a renewal that succeeds puts focus on the page, never on the banner", async () => {
    answer = { body: alice(), status: 200 };
    fireEvent.click(bannerShown());
    answer = { body: alice({ session_expires_at: LATER }), status: 200 };
    await poll(2);
    expect(document.getElementById("main-content")).toHaveFocus();
  });

  it("a refused window offers the sign-in in a new tab; a failed check says so and waiting resumes after it", async () => {
    answer = { status: 503 };
    startRenewal({ popup: null });
    expect(screen.getByText(REAUTH_DIALOG.POPUP_BLOCKED)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: REAUTH_DIALOG.POPUP_FALLBACK });
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("href", "/auth/login");

    fireEvent.click(link);
    await poll();
    expect(screen.getByText(REAUTH_DIALOG.CHECK_FAILED)).toBeInTheDocument();
    expect(screen.queryByText(REAUTH_DIALOG.UNREACHABLE)).toBeNull();
    answer = { body: alice(), status: 200 };
    await poll();
    expect(screen.getByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: REAUTH_RENEW.CANCEL })).toBeInTheDocument();
  });

  // Only no answer at all says Wardyn isn't answering; an answer says the check failed, in its own words if it has them.
  it.each([
    ["no answer", { status: 0 }, REAUTH_DIALOG.UNREACHABLE],
    ["a 403", { status: 403 }, REAUTH_DIALOG.CHECK_FAILED_FINAL],
    ["a 404", { status: 404 }, REAUTH_DIALOG.CHECK_FAILED_FINAL],
    ["a 2xx body that does not parse", { status: 200, raw: "<html>" }, REAUTH_DIALOG.CHECK_FAILED_FINAL],
    ["a 500 that names only its handler", { status: 500, body: { error: "read session", reason: "internal_error" } }, REAUTH_DIALOG.CHECK_FAILED],
    ["a 503 that names its cause", { status: 503, body: { error: "The session store is restarting." } }, "The session store is restarting."],
  ])("%s while waiting on the sign-in window", async (_name, failure, sentence) => {
    answer = failure;
    startRenewal({ popup: null });
    fireEvent.click(screen.getByRole("link", { name: REAUTH_DIALOG.POPUP_FALLBACK }));
    await poll();
    expect(screen.getByText(sentence)).toBeInTheDocument();
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
