/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account > Signed-in CLIs (boards MATT3-*): one row per computer, a Sign out that asks first and ends that
// one session, the file-store note, the admin step-up chip, the empty state with the command, and the card
// absent where CLI sign-in is not served.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import { aheadByHours } from "../../../lib/test-clock";
import type { CLISession } from "../../../lib/types/cli-sign-in";
import { SIGNED_IN_CLIS as T } from "../../wardyn/copy/cli-sign-in";
import { MINTED } from "../../wardyn/copy/credentials";
import { SignedInClisCard } from "./signed-in-clis-card";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const session = (id: string, machine: string, over: Partial<CLISession> = {}): CLISession => ({
  id,
  machine,
  store: "keychain",
  created_at: aheadByHours(-3 * 24),
  last_refresh_at: aheadByHours(-0.2),
  expires_at: aheadByHours(27 * 24),
  idle_expires_at: aheadByHours(7 * 24),
  ...over,
});

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let live: CLISession[];
let route: (method: string, path: string) => Response;
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  live = [
    session("s1", "mbp-dev"),
    session("s2", "wsl-dev", { store: "file", created_at: aheadByHours(-48), expires_at: aheadByHours(5 * 24), idle_expires_at: aheadByHours(6 * 24) }),
  ];
  route = (method, path) => {
    if (method === "GET" && path === "/api/v1/me/cli-sessions") return json(live);
    const del = /^\/api\/v1\/me\/cli-sessions\/(.+)$/.exec(path);
    if (method === "DELETE" && del) {
      live = live.filter((s) => s.id !== del[1]);
      return new Response(null, { status: 204 });
    }
    return json({ error: "unexpected " + method + " " + path }, 500);
  };
  fetchMock = vi.fn(async (path: string, init?: RequestInit) => route(init?.method ?? "GET", String(path).split("?")[0]));
  vi.stubGlobal("fetch", fetchMock);
  vi.mocked(toast.success).mockClear();
  vi.mocked(toast.error).mockClear();
});
afterEach(() => vi.unstubAllGlobals());

const user = userEvent.setup({ pointerEventsCheck: 0 });
const openCard = async () => {
  await user.click(await screen.findByRole("button", { name: new RegExp(`^${T.TITLE}`) }));
};
const row = (id: string) => within(document.querySelector<HTMLElement>(`[data-session="${id}"]`)!);

describe("the card", () => {
  it("summarises the computers and lists each with its store, times and a Sign out", async () => {
    render(<SignedInClisCard />);
    expect(await screen.findByRole("button", { name: /^Signed-in CLIs\s*2 computers/ })).toHaveAttribute("aria-expanded", "false");
    await openCard();
    expect(screen.getByText(T.LEDE)).toBeVisible();
    const table = screen.getByRole("table", { name: T.TABLE_LABEL });
    for (const col of ["Computer", "Store", "Signed in", "Last used", "Ends", "Idle end"]) {
      expect(within(table).getByRole("columnheader", { name: col })).toBeVisible();
    }
    const mbp = row("s1");
    expect(mbp.getByText("mbp-dev")).toBeVisible();
    expect(mbp.getByText(T.CHIP)).toBeVisible();
    expect(mbp.getByText(T.STORE_KEYCHAIN)).toBeVisible();
    expect(mbp.getByText("3 days ago")).toBeVisible();
    expect(mbp.getByText("in 27 days")).toBeVisible();
    expect(mbp.getByText("in 7 days")).toBeVisible();
    expect(mbp.queryByText(T.FILE_NOTE)).toBeNull();
    expect(mbp.getByRole("button", { name: "Sign out mbp-dev" })).toHaveTextContent(T.SIGN_OUT);
  });

  it("explains the file store and shows it as a 0600 file", async () => {
    render(<SignedInClisCard />);
    await openCard();
    const wsl = row("s2");
    expect(wsl.getByText(T.STORE_FILE)).toBeVisible();
    expect(wsl.getByText(T.FILE_NOTE)).toBeVisible();
    expect(wsl.getByText("in 5 days")).toBeVisible();
    expect(wsl.getByText("in 6 days")).toBeVisible();
  });

  it("shows the live admin step-up on its computer's row", async () => {
    live = [session("s1", "mbp-dev", { admin_expires_at: new Date(Date.now() + 41.5 * 60_000).toISOString() })];
    render(<SignedInClisCard />);
    await openCard();
    expect(row("s1").getByText("Admin access, ends in 42 min")).toBeVisible();
  });

  it("stays when empty, says so and shows the command for this console's address", async () => {
    live = [];
    render(<SignedInClisCard />);
    expect(await screen.findByRole("button", { name: /^Signed-in CLIs\s*No computers signed in/ })).toBeVisible();
    await openCard();
    expect(screen.getByText(T.EMPTY)).toBeVisible();
    expect(screen.getByText(T.RUN_LEAD, { exact: false })).toBeVisible();
    expect(screen.getByText(T.RUN_COMMAND(window.location.origin))).toBeVisible();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it.each<[string, Response]>([
    ["the routes are not mounted (CLI sign-in off)", json({ error: "not found" }, 404)],
    ["the caller is a token, not a person", json({ error: "x", reason: "api_token_no_human" }, 403)],
  ])("is absent when %s", async (_n, res) => {
    route = () => res.clone();
    const { container } = render(<SignedInClisCard />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
    expect(container).toBeEmptyDOMElement();
  });

  it("offers a retry on any other failure", async () => {
    route = () => json({ error: "boom" }, 500);
    render(<SignedInClisCard />);
    await openCard();
    expect(await screen.findByRole("button", { name: /retry/i })).toBeVisible();
  });
});

describe("Sign out", () => {
  it("asks first, names the computer, and Cancel ends nothing", async () => {
    render(<SignedInClisCard />);
    await openCard();
    await user.click(row("s1").getByRole("button", { name: "Sign out mbp-dev" }));
    const dlg = await screen.findByRole("alertdialog");
    expect(within(dlg).getByText(T.DIALOG_TITLE("mbp-dev"))).toBeVisible();
    expect(within(dlg).getByText(T.DIALOG_BODY("mbp-dev"))).toBeVisible();
    await user.click(within(dlg).getByRole("button", { name: MINTED.REVOKE_CANCEL }));
    expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === "DELETE")).toBe(false);
    expect(row("s1").getByText("mbp-dev")).toBeVisible();
  });

  it("ends only that computer's session, says so, and drops the row", async () => {
    render(<SignedInClisCard />);
    await openCard();
    await user.click(row("s1").getByRole("button", { name: "Sign out mbp-dev" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.SIGN_OUT }));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Signed out mbp-dev."));
    const deletes = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === "DELETE");
    expect(deletes.map(([p]) => String(p))).toEqual(["/api/v1/me/cli-sessions/s1"]);
    await waitFor(() => expect(document.querySelector('[data-session="s1"]')).toBeNull());
    expect(document.querySelector('[data-session="s2"]')).not.toBeNull();
    expect(screen.getByRole("button", { name: /^Signed-in CLIs\s*1 computer/ })).toBeVisible();
  });

  it("reports a refusal and keeps the list as the server has it", async () => {
    const del = route;
    route = (m, p) => (m === "DELETE" ? json({ error: "could not end it" }, 500) : del(m, p));
    render(<SignedInClisCard />);
    await openCard();
    await user.click(row("s1").getByRole("button", { name: "Sign out mbp-dev" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.SIGN_OUT }));
    await waitFor(() => expect(toast.error).toHaveBeenCalled());
    expect(toast.success).not.toHaveBeenCalled();
    expect(await screen.findByRole("table")).toBeVisible();
    expect(document.querySelector('[data-session="s1"]')).not.toBeNull();
  });
});
