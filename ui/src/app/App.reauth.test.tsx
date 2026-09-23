/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #483 — a session that ends mid-page. Full App mount over a mocked fetch,
// driving the REAL wfetch -> onUnauthorized -> reauth layer wiring. The page
// behind it is a stub screen with a typed-but-unsaved draft, a Save that
// PUTs, and a poll — the three things the dialog must leave alone.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import App from "./App";
import { SESSION_ENDED_REASON, wfetch } from "./lib/api/core";
import { REAUTH_BAR, REAUTH_DIALOG, REAUTH_DRAFT } from "./lib/reauth-copy";
import { PROVIDERS_DRAFT } from "./lib/workspace-providers-copy";

const mockState = vi.hoisted(() => ({ claim: true }));

// One stub for /runs and /providers: a draft, a Save, a poll, and where it is.
vi.mock("./components/screens/runs", async () => {
  const React = await import("react");
  const { useLocation } = await import("react-router-dom");
  const core = await import("./lib/api/core");
  const { usePoll } = await import("./lib/use-poll");
  const { useRegisterUnsaved } = await import("./lib/unsaved-registry");
  const { useWriteDropped } = await import("./lib/use-write-dropped");
  const copy = await import("./lib/reauth-copy");
  function Dropped() {
    const [dropped] = useWriteDropped("note");
    return dropped ? <p>{copy.REAUTH_DIALOG.WRITE_DROPPED}</p> : null;
  }
  function RunsScreen() {
    const [text, setText] = React.useState("");
    const { pathname } = useLocation();
    useRegisterUnsaved("note", text !== "", () => `note: ${text}`);
    usePoll(() => core.wfetch("/stub-poll").catch(() => {}), 1000, false);
    return (
      <div>
        <p>at {pathname}</p>
        <input aria-label="Note" value={text} onChange={(e) => setText(e.target.value)} />
        <button
          type="button"
          onClick={() => void core.wfetch("/stub-save", { method: "PUT", body: "{}", save: "note" }).catch(() => {})}
        >
          Save note
        </button>
        {/* A write that is not a Save (a background grade, say). */}
        <button type="button" onClick={() => void core.wfetch("/stub-grade", { method: "POST", body: "{}" }).catch(() => {})}>
          Grade note
        </button>
        {mockState.claim && <Dropped />}
      </div>
    );
  }
  return { RunsScreen };
});
vi.mock("./components/screens/providers/providers-screen", async () => {
  const { RunsScreen } = await import("./components/screens/runs");
  return { ProvidersScreen: RunsScreen };
});

const ME = { principal: "cj", method: "token", operator: true, security_operator: true, role: "admin", email: "" };
const SETUP_STATUS_READY = {
  ready: true,
  has_runs: true,
  checks: [],
  auth: { mode: "token", local_loopback: false },
  runner: { driver: "docker", confinement_classes: ["CC1"] },
  providers: [],
  secrets: { present: [], github_app: false },
  age_key: { durable: true },
  platform: { os: "linux", wsl: false },
};

function json(status: number, body: unknown): Response {
  return { ok: status >= 200 && status < 300, status, json: async () => body, text: async () => JSON.stringify(body) } as Response;
}

// The daemon: `dead` 401s every /api/v1 call; `down` refuses the connection;
// /healthz answers regardless, with `sso` deciding the dialog's doors.
const daemon = { dead: false, down: false, sso: false, me: ME as Record<string, unknown> };
const calls: string[] = [];
const fetchMock = vi.fn((url: RequestInfo | URL, init?: RequestInit) => {
  const u = String(url);
  calls.push(`${init?.method ?? "GET"} ${u}`);
  if (u.includes("/healthz")) return Promise.resolve(json(200, { status: "ok", sso: daemon.sso, token_login: true }));
  if (u.includes("/readyz")) return Promise.resolve(json(200, { status: "ok" }));
  if (daemon.down) return Promise.reject(new TypeError("Failed to fetch"));
  if (daemon.dead) return Promise.resolve(json(401, { error: "unauthorized" }));
  if (u.includes("/setup/status")) return Promise.resolve(json(200, SETUP_STATUS_READY));
  if (u.includes("/me")) return Promise.resolve(json(200, daemon.me));
  if (u.includes("/approvals") || u.includes("/runs")) return Promise.resolve(json(200, []));
  return Promise.resolve(json(200, {}));
});
const count = (needle: string) => calls.filter((c) => c.includes(needle)).length;

let user: ReturnType<typeof userEvent.setup>;
const realLocation = window.location;
const assign = vi.fn();

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
  Object.assign(daemon, { dead: false, down: false, sso: false, me: ME });
  mockState.claim = true;
  calls.length = 0;
  sessionStorage.setItem("wardyn_admin_token", "good-token");
  vi.stubGlobal("fetch", fetchMock);
  Object.defineProperty(window, "location", { value: { ...realLocation, assign }, configurable: true });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  assign.mockReset();
  sessionStorage.clear();
  Object.defineProperty(window, "location", { value: realLocation, configurable: true });
});

/** Mounted at `path`, typed "hello" into the draft, then the session died
 *  under a Save — the dialog is up over the page. */
async function lapseMidPage(path = "/runs") {
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  await user.type(await screen.findByLabelText("Note"), "hello");
  daemon.dead = true;
  await user.click(screen.getByRole("button", { name: "Save note" }));
  await screen.findByRole("dialog");
}

async function signInWithToken() {
  daemon.dead = false;
  const dialog = screen.getByRole("dialog");
  await user.type(within(dialog).getByLabelText("Admin token"), "good-token");
  await user.click(within(dialog).getByRole("button", { name: REAUTH_BAR.CTA }));
}

describe("App — a session that ends mid-page (#483)", () => {
  it("opens the dialog over the page and keeps the tree mounted", async () => {
    await lapseMidPage();
    expect(within(screen.getByRole("dialog")).getByText(REAUTH_DIALOG.TITLE)).toBeInTheDocument();
    expect(screen.getByText(REAUTH_DIALOG.BODY)).toBeInTheDocument();
    expect(screen.getByLabelText("Note")).toHaveValue("hello");
    expect(screen.queryByText(SESSION_ENDED_REASON)).toBeNull();
  });

  it("opens ONE dialog however many requests 401", async () => {
    await lapseMidPage();
    await act(async () => {
      await Promise.allSettled([wfetch("/a"), wfetch("/b", { method: "POST" }), wfetch("/c")]);
    });
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
  });

  it("pauses the page's polls while the dialog is up", async () => {
    await lapseMidPage();
    const before = count("/stub-poll");
    await act(async () => {
      vi.advanceTimersByTime(10_000);
    });
    expect(count("/stub-poll")).toBe(before);
  });

  it("same person back: closes, carries on, never re-sends the refused save, and says so beside Save", async () => {
    await lapseMidPage();
    expect(count("PUT /api/v1/stub-save")).toBe(1);
    await signInWithToken();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByLabelText("Note")).toHaveValue("hello");
    expect(screen.getByText(REAUTH_DIALOG.WRITE_DROPPED)).toBeInTheDocument();
    await act(async () => {
      vi.advanceTimersByTime(5_000);
    });
    expect(count("PUT /api/v1/stub-save")).toBe(1);
    expect(assign).not.toHaveBeenCalled();
    // …and the polls are running again.
    const before = count("/stub-poll");
    await act(async () => {
      vi.advanceTimersByTime(3_000);
    });
    expect(count("/stub-poll")).toBeGreaterThan(before);
  });

  it("a screen that does not show it inline gets the write-dropped sentence as a toast", async () => {
    mockState.claim = false;
    const warning = vi.spyOn(toast, "warning").mockImplementation(() => "id");
    await lapseMidPage();
    await signInWithToken();
    await waitFor(() => expect(warning).toHaveBeenCalledWith(REAUTH_DIALOG.WRITE_DROPPED));
  });

  it("someone else signs in: the page reloads fresh as them — no dialog, no copy offer", async () => {
    await lapseMidPage("/providers");
    daemon.me = { ...ME, principal: "someone-else" };
    await signInWithToken();
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/providers"));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByLabelText("Note")).toBeNull();
    expect(screen.queryByText(PROVIDERS_DRAFT.CONFLICT_COPY)).toBeNull();
  });

  it("someone else who cannot open this page reloads onto Runs", async () => {
    await lapseMidPage("/providers");
    daemon.me = { ...ME, principal: "someone-else", role: "member", operator: false, security_operator: false };
    await signInWithToken();
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
  });

  it("same person, narrower role: says so, offers the copy, and continues to Runs", async () => {
    await lapseMidPage("/providers");
    daemon.me = { ...ME, role: "member", operator: false, security_operator: false };
    await signInWithToken();
    const dialog = screen.getByRole("dialog");
    expect(await within(dialog).findByText(REAUTH_DIALOG.ROLE_CHANGED_BODY)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: PROVIDERS_DRAFT.CONFLICT_COPY })).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: REAUTH_DRAFT.GO_TO_RUNS }));
    await screen.findByText("at /runs");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(assign).not.toHaveBeenCalled();
  });

  it("Not now: a read-only bar with Copy and Sign in; a later read's 401 keeps the bar, not the dialog", async () => {
    await lapseMidPage();
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Not now" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText(REAUTH_BAR.BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: PROVIDERS_DRAFT.CONFLICT_COPY })).toBeInTheDocument();
    await act(async () => {
      await wfetch("/again").catch(() => {});
    });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText(REAUTH_BAR.BODY)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: REAUTH_BAR.CTA }));
    await screen.findByRole("dialog");
  });

  it("under the bar a Save sends nothing and asks again", async () => {
    await lapseMidPage();
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Not now" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const before = count("PUT /api/v1/stub-save");
    await user.click(screen.getByRole("button", { name: "Save note" }));
    await screen.findByRole("dialog");
    expect(count("PUT /api/v1/stub-save")).toBe(before);
  });

  it("another tab signed in as someone else: the bar's Save submits nothing, and signing in reloads", async () => {
    await lapseMidPage();
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Not now" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    // The shared cookie / remembered token now answers as B.
    Object.assign(daemon, { dead: false, me: { ...ME, principal: "someone-else" } });
    const puts = count("PUT /api/v1/stub-save");
    await user.click(screen.getByRole("button", { name: "Save note" }));
    await screen.findByRole("dialog");
    expect(count("PUT /api/v1/stub-save")).toBe(puts);
    // A read that answers 200 as B does not resume the page either.
    await act(async () => {
      await wfetch("/runs");
    });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await signInWithToken();
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    expect(count("PUT /api/v1/stub-save")).toBe(puts);
  });

  it("a refused write that was not a Save never claims one was dropped", async () => {
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <App />
      </MemoryRouter>,
    );
    await user.type(await screen.findByLabelText("Note"), "hello");
    daemon.dead = true;
    await user.click(screen.getByRole("button", { name: "Grade note" }));
    await screen.findByRole("dialog");
    const warning = vi.spyOn(toast, "warning").mockImplementation(() => "id");
    await signInWithToken();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByText(REAUTH_DIALOG.WRITE_DROPPED)).toBeNull();
    expect(warning).not.toHaveBeenCalled();
  });

  it("a dropped save is forgotten when its screen goes away — no other screen shows it", async () => {
    const warning = vi.spyOn(toast, "warning").mockImplementation(() => "id");
    await lapseMidPage();
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Not now" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await user.click(screen.getByRole("link", { name: /^Approvals/ }));
    await waitFor(() => expect(screen.queryByLabelText("Note")).toBeNull());
    await user.click(screen.getByRole("button", { name: REAUTH_BAR.CTA }));
    await screen.findByRole("dialog");
    await signInWithToken();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await user.click(screen.getByRole("link", { name: /^Runs/ }));
    await screen.findByLabelText("Note");
    expect(screen.queryByText(REAUTH_DIALOG.WRITE_DROPPED)).toBeNull();
    expect(warning).not.toHaveBeenCalled();
  });

  it("an outage while signing in is not a sign-out", async () => {
    await lapseMidPage();
    const dialog = screen.getByRole("dialog");
    daemon.down = true;
    await user.type(within(dialog).getByLabelText("Admin token"), "good-token");
    await user.click(within(dialog).getByRole("button", { name: REAUTH_BAR.CTA }));
    expect(await within(dialog).findByText(REAUTH_DIALOG.UNREACHABLE)).toBeInTheDocument();
    expect(screen.getByLabelText("Note")).toHaveValue("hello");
  });
});

describe("App — the SSO door in the dialog (#483)", () => {
  beforeEach(() => {
    daemon.sso = true;
  });

  it("a blocked popup offers the sign-in page in a new tab", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);
    await lapseMidPage();
    const dialog = screen.getByRole("dialog");
    await user.click(await within(dialog).findByRole("button", { name: "Sign in with SSO" }));
    expect(await within(dialog).findByText(REAUTH_DIALOG.POPUP_BLOCKED)).toBeInTheDocument();
    const link = within(dialog).getByRole("link", { name: REAUTH_DIALOG.POPUP_FALLBACK });
    expect(link).toHaveAttribute("href", "/auth/login");
    expect(link).toHaveAttribute("target", "_blank");
  });

  it("a popup closed before signing in says so", async () => {
    const popup = { closed: false, opener: {} as unknown, location: { href: "" }, close: vi.fn() };
    const open = vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    await lapseMidPage();
    const dialog = screen.getByRole("dialog");
    await user.click(await within(dialog).findByRole("button", { name: "Sign in with SSO" }));
    expect(open).toHaveBeenCalledWith("about:blank", expect.any(String), expect.any(String));
    expect(popup.opener).toBeNull();
    expect(popup.location.href).toBe("/auth/login");
    expect(await within(dialog).findByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();
    popup.closed = true;
    await act(async () => {
      vi.advanceTimersByTime(1_600);
    });
    expect(await within(dialog).findByText(REAUTH_DIALOG.CLOSED_WITHOUT)).toBeInTheDocument();
  });

  it("signing in through the popup closes it and carries on — writing nothing to browser storage", async () => {
    const popup = { closed: false, opener: {} as unknown, location: { href: "" }, close: vi.fn() };
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    await lapseMidPage();
    await user.click(await within(screen.getByRole("dialog")).findByRole("button", { name: "Sign in with SSO" }));
    daemon.dead = false;
    await act(async () => {
      vi.advanceTimersByTime(1_600);
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(popup.close).toHaveBeenCalled();
    expect(screen.getByLabelText("Note")).toHaveValue("hello");
    // The theme toggle's own preference is the one write the console makes on
    // mount; nothing about the page, its draft or the lapse may follow it.
    expect(setItem.mock.calls.filter(([key]) => key !== "wardyn-theme")).toEqual([]);
  });
});

describe("App — a deliberate sign-out (#483)", () => {
  it("shows no notice and no dialog, even when the logout itself 401s", async () => {
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <App />
      </MemoryRouter>,
    );
    await screen.findByLabelText("Note");
    const header = screen.getByRole("banner");
    await waitFor(() => expect(within(header).getByText("cj")).toBeInTheDocument());
    daemon.dead = true;
    const headerButtons = within(header).getAllByRole("button");
    await user.click(headerButtons[headerButtons.length - 1]);
    await user.click(within(await screen.findByRole("menu")).getByText("Sign out"));
    await screen.findByText("Admin token", { exact: true });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByText(SESSION_ENDED_REASON)).toBeNull();
  });
});
