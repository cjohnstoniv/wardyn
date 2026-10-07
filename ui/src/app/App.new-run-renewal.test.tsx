/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import App from "./App";
import { SHELL } from "./components/wardyn/copy";
import { isSwitching, switchView } from "./components/wardyn/console-view";
import { CONSOLE_VIEW } from "./components/wardyn/copy/console-view";
import { getAuthGeneration, getToken, isSignedOutHold, setSignedOutHold, setToken } from "./lib/api/core";
import type { Me } from "./lib/api/health";
import { REAUTH_DIALOG, REAUTH_RENEW } from "./lib/reauth-copy";
import { SIGN_IN_AGAIN } from "./lib/session-renew-copy";
import { baseStatus } from "./lib/test-fixtures";
import { setField } from "../test/set-field";

// The next route does not participate in confirming the draft owner's identity.
vi.mock("./components/screens/run-detail", () => ({ RunDetailScreen: () => <p>Created run</p> }));

const spec = { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC1" };
const setup = baseStatus({ ready: true, has_runs: true });
const tick = (ms = 800) => act(() => vi.advanceTimersByTimeAsync(ms));
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const calls: { path: string; method: string; body?: BodyInit | null }[] = [];
const count = (path: string, method = "POST") => calls.filter((c) => c.path === path && c.method === method).length;
let me: Me;
let meStatus = 200;
let viewFailsOffline = false;
let viewReply: Response | undefined;
let logoutReply: Promise<Response> | undefined;
function pendingView(status = 503) {
  let finish!: () => void;
  viewReply = new Response(new ReadableStream<Uint8Array>({ start(controller) {
    finish = () => {
      controller.enqueue(new TextEncoder().encode(JSON.stringify(status === 200 ? { user_view: false } : { error: "view switch unavailable" })));
      controller.close();
    };
  } }), { status, headers: { "Content-Type": "application/json" } });
  return finish;
}
function pendingMe() {
  let resolve!: (answer: Response) => void;
  const promise = new Promise<Response>((done) => { resolve = done; });
  const read = { promise, resolve, signal: undefined as AbortSignal | undefined };
  heldMe.push(read);
  return read;
}
let heldMe: ReturnType<typeof pendingMe>[] = [];
const fetchMock = vi.fn((url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
  const path = new URL(String(url), "http://localhost").pathname;
  const method = init?.method ?? "GET";
  calls.push({ path, method, body: init?.body });
  if (path === "/healthz") return Promise.resolve(json(200, { status: "ok", sso: true, token_login: true, runner: "docker" }));
  if (path === "/readyz") return Promise.resolve(json(200, { status: "ok" }));
  if (path === "/api/v1/me") {
    const held = heldMe.shift();
    if (held) { held.signal = init?.signal ?? undefined; return held.promise; }
    return Promise.resolve(json(meStatus, meStatus === 200 ? me : { error: "unavailable" }));
  }
  if (path === "/api/v1/me/view") return viewFailsOffline ? Promise.reject(new TypeError("Failed to fetch")) : Promise.resolve(viewReply ?? json(503, { error: "view switch unavailable" }));
  if (path === "/api/v1/auth/logout") return logoutReply ?? Promise.resolve(json(200, {}));
  if (path === "/api/v1/me/capabilities") return Promise.resolve(json(200, { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false }));
  if (path === "/api/v1/setup/status") return Promise.resolve(json(200, setup));
  if (path === "/api/v1/policies/default") return Promise.resolve(json(200, spec));
  if (path === "/api/v1/runs/preflight") return Promise.resolve(json(200, { enforced_confinement_class: "CC1", setup_items: [] }));
  if (path === "/api/v1/runs/policy-preview") return Promise.resolve(json(200, { spec, source: { kind: "inline" }, warnings: [], pending: ["credential_liveness"], repository_access: [] }));
  if (path === "/api/v1/runs" && method === "POST") return Promise.resolve(json(201, { id: "created" }));
  if (["/api/v1/runs", "/api/v1/policies", "/api/v1/workspaces", "/api/v1/approvals"].includes(path)) return Promise.resolve(json(200, []));
  if (path === "/api/v1/policies/grade") return Promise.resolve(json(200, { risk_assessment: [], overall_risk: "low" }));
  return Promise.resolve(json(404, { error: "not found" }));
});
const realLocation = window.location;
const assign = vi.fn();
let popup: { closed: boolean; close: ReturnType<typeof vi.fn>; opener: unknown; location: { href: string } };

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  me = { principal: "alice", method: "sso", role: "user", operator: false, security_operator: false, email: "", session_expires_at: new Date(Date.now() + 120_000).toISOString() };
  meStatus = 200;
  viewFailsOffline = false;
  viewReply = undefined;
  logoutReply = undefined;
  heldMe = [];
  calls.length = 0;
  popup = { closed: false, close: vi.fn(() => { popup.closed = true; }), opener: {}, location: { href: "" } };
  vi.spyOn(window, "open").mockImplementation(() => popup as unknown as Window);
  vi.spyOn(toast, "success").mockImplementation(() => "toast");
  vi.stubGlobal("fetch", fetchMock);
  Object.defineProperty(window, "location", { value: { ...realLocation, assign }, configurable: true });
  sessionStorage.setItem("wardyn_admin_token", "good-token");
  setSignedOutHold(false);
});
afterEach(async () => {
  cleanup();
  // A mocked successful document reload leaves the module alive in this test process.
  if (isSwitching()) {
    viewReply = undefined;
    await switchView("user", "/runs").catch(() => {});
  }
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  assign.mockReset();
  sessionStorage.clear();
  setSignedOutHold(false);
  Object.defineProperty(window, "location", { value: realLocation, configurable: true });
});

async function draft() {
  const view = render(<MemoryRouter initialEntries={["/runs/new"]}><App /></MemoryRouter>);
  setField(await screen.findByLabelText("Title"), "Keep this draft");
  await tick();
  await waitFor(() => expect(count("/api/v1/runs/preflight")).toBeGreaterThan(0));
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(0);
  expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
  return view;
}
async function startRenew() {
  fireEvent.click(await screen.findByRole("button", { name: SIGN_IN_AGAIN }));
  await screen.findByRole("button", { name: REAUTH_RENEW.CANCEL });
}
const cancelRenew = () => fireEvent.click(screen.getByRole("button", { name: REAUTH_RENEW.CANCEL }));

it("the real renewal start/cancel confirms an unchanged owner and resumes the retained New Run draft", async () => {
  await draft();
  const originalChecks = count("/api/v1/runs/policy-preview");
  const originalPreflights = count("/api/v1/runs/preflight");
  await startRenew();
  await tick(1500);
  await tick(1000);
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(originalChecks);
  expect(screen.getByText(REAUTH_DIALOG.WAITING)).toBeInTheDocument();
  const beforeCancel = count("/api/v1/runs/policy-preview");
  cancelRenew();
  await tick(0);
  await tick(1000);
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(beforeCancel);
  expect(count("/api/v1/runs/preflight")).toBeGreaterThan(originalPreflights);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
  expect(popup.close).toHaveBeenCalled();
  expect(toast.success).not.toHaveBeenCalled();
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs")).toBe(0);

  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  await screen.findByText("Created run");
  expect(count("/api/v1/runs")).toBe(1);
  expect(JSON.parse(String(calls.find((c) => c.path === "/api/v1/runs" && c.method === "POST")?.body))).toMatchObject({ title: "Keep this draft" });
});

it.each(["open", "blocked"])("canceling the %s popup discards a late old identity and waits for the current /me", async (kind) => {
  await draft();
  if (kind === "blocked") vi.mocked(window.open).mockReturnValue(null);
  const old = pendingMe();
  await startRenew();
  await tick(1500);
  expect(old.signal).toBeDefined();
  const current = pendingMe();
  const checked = count("/api/v1/runs/policy-preview");
  cancelRenew();
  expect(old.signal?.aborted).toBe(true);
  await act(async () => old.resolve(json(200, { ...me, principal: "late-old-owner" })));
  await tick(1000);
  expect(current.signal).toBeDefined();
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);

  await act(async () => current.resolve(json(200, me)));
  await tick(1000);
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(checked);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
  expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
  expect(count("/api/v1/runs")).toBe(0);
  expect(toast.success).not.toHaveBeenCalled();
});

it.each([401, 503])("a current /me rejected with %s does not release the canceled renewal's request fence", async (status) => {
  await draft();
  await startRenew();
  const checked = count("/api/v1/runs/policy-preview");
  meStatus = status;
  cancelRenew();
  await tick(3000);
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
  expect(assign).not.toHaveBeenCalled();
  if (status === 401) expect(isSignedOutHold()).toBe(true);
  expect(toast.success).not.toHaveBeenCalled();
});

it("a different current identity unloads the draft before reload without resuming or creating", async () => {
  await draft();
  await startRenew();
  const checked = count("/api/v1/runs/policy-preview");
  me = { ...me, principal: "bob" };
  cancelRenew();
  await tick(1000);
  expect(assign).toHaveBeenCalledWith("/runs/new");
  expect(screen.queryByLabelText("Title")).toBeNull();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);
  expect(toast.success).not.toHaveBeenCalled();
});

it("unmount retires a cancellation confirmation even when the response ignores abort", async () => {
  const view = await draft();
  await startRenew();
  const current = pendingMe();
  cancelRenew();
  await tick(0);
  expect(current.signal).toBeDefined();
  const checked = count("/api/v1/runs/policy-preview");
  view.unmount();
  expect(current.signal?.aborted).toBe(true);
  await act(async () => current.resolve(json(200, { ...me, principal: "bob" })));
  await tick(3000);
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);
});

it("reviewer: failed view switch retains a launchable same-owner draft", async () => {
  me = { ...me, user_view: true, user_view_super_admin: true };
  await draft();
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  await screen.findByText(CONSOLE_VIEW.SWITCH_FAILED);
  await tick(1000);
  act(() => window.dispatchEvent(new Event("focus")));
  await tick(1000);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs")).toBe(0);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  await tick(0);
  expect(count("/api/v1/runs")).toBe(1);
});


it.each([false, true])("a failed view switch (offline=%s) confirms automatically without focus or write replay", async (offline) => {
  me = { ...me, user_view: true, user_view_super_admin: true };
  viewFailsOffline = offline;
  await draft();
  const checked = count("/api/v1/runs/policy-preview");
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  await screen.findByText(CONSOLE_VIEW.SWITCH_FAILED);
  await tick(0);
  await tick(1000);
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(checked);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
  expect(count("/api/v1/me/view")).toBe(1);
  expect(count("/api/v1/runs")).toBe(0);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  await screen.findByText("Created run");
  expect(count("/api/v1/runs")).toBe(1);
});

it.each(["quiet", "renewal", "watch"])("a delayed failed view body resumes %s confirmation only after settlement", async (phase) => {
  me = { ...me, user_view: true, user_view_super_admin: true };
  await draft();
  if (phase !== "quiet") {
    await startRenew();
    await tick(1500);
    if (phase === "watch") cancelRenew();
    await tick(1000);
  }
  const finish = pendingView();
  const checked = count("/api/v1/runs/policy-preview");
  const before = count("/api/v1/me", "GET");
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  await tick(1000);
  expect(count("/api/v1/me", "GET")).toBeGreaterThan(before);
  expect(screen.queryByText(CONSOLE_VIEW.SWITCH_FAILED)).toBeNull();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  expect(count("/api/v1/runs")).toBe(0);
  const reads = count("/api/v1/me", "GET");
  if (phase === "quiet") {
    await tick(4500);
    expect(count("/api/v1/me", "GET")).toBe(reads);
  }
  await act(async () => finish());
  await screen.findByText(CONSOLE_VIEW.SWITCH_FAILED);
  await tick(1000);
  expect(count("/api/v1/me", "GET")).toBeGreaterThan(reads);
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(checked);
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs")).toBe(0);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  await screen.findByText("Created run");
  expect(count("/api/v1/runs")).toBe(1);
  expect(count("/api/v1/me/view")).toBe(1);
});

it("an explicit token check waits for the pending view body without losing its confirmation", async () => {
  me = { ...me, user_view: true, user_view_super_admin: true };
  await draft();
  const finish = pendingView();
  const checked = count("/api/v1/runs/policy-preview");
  meStatus = 401;
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  const dialog = await screen.findByRole("dialog");
  meStatus = 200;
  setField(within(dialog).getByLabelText("Admin token"), "confirmed-token");
  fireEvent.click(within(dialog).getByRole("button", { name: "Sign in", exact: true }));
  await tick(1000);
  expect(isSignedOutHold()).toBe(true);
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  const reads = count("/api/v1/me", "GET");
  await tick(4500);
  expect(count("/api/v1/me", "GET")).toBe(reads);
  await act(async () => finish());
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await tick(1000);
  expect(isSignedOutHold()).toBe(false);
  expect(count("/api/v1/me", "GET")).toBeGreaterThan(reads);
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(checked);
  expect(count("/api/v1/runs")).toBe(0);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  await screen.findByText("Created run");
  expect(count("/api/v1/runs")).toBe(1);
});

it.each(["same", "other", "401", "unmount"])("view settlement discards an older pending answer before a %s outcome", async (outcome) => {
  me = { ...me, user_view: true, user_view_super_admin: true };
  const view = await draft();
  const finish = pendingView();
  const old = pendingMe();
  const checked = count("/api/v1/runs/policy-preview");
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  await tick(0);
  expect(old.signal).toBeDefined();
  const current = pendingMe();
  act(() => setToken("replacement-during-view-switch"));
  await act(async () => finish());
  await screen.findByText(CONSOLE_VIEW.SWITCH_FAILED);
  expect(old.signal?.aborted).toBe(true);
  await act(async () => old.resolve(json(200, { ...me, principal: "obsolete-owner" })));
  await tick(0);
  expect(current.signal).toBeDefined();
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  if (outcome === "unmount") view.unmount();
  await act(async () => current.resolve(outcome === "401" ? json(401, {}) : json(200, outcome === "same" ? me : { ...me, principal: "bob" })));
  await tick(1000);
  expect(count("/api/v1/runs")).toBe(0);
  if (outcome === "same") {
    expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(checked);
    expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
    expect(assign).not.toHaveBeenCalled();
  } else {
    expect(count("/api/v1/runs/policy-preview")).toBe(checked);
    if (outcome === "other") expect(assign).toHaveBeenCalledWith("/runs/new");
    else expect(assign).not.toHaveBeenCalled();
    if (outcome === "401") {
      expect(isSignedOutHold()).toBe(true);
      const reads = count("/api/v1/me", "GET");
      await tick(4500);
      expect(count("/api/v1/me", "GET")).toBe(reads);
    } else expect(screen.queryByLabelText("Title")).toBeNull();
  }
});

it("a delayed successful view switch keeps its reload guard and never adopts the pending draft", async () => {
  me = { ...me, user_view: true, user_view_super_admin: true };
  await draft();
  const finish = pendingView(200);
  const checked = count("/api/v1/runs/policy-preview");
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  await tick(1000);
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  await act(async () => finish());
  expect(assign).toHaveBeenCalledExactlyOnceWith("/admin");
  expect(isSwitching()).toBe(true);
  act(() => window.dispatchEvent(new Event("focus")));
  await tick(1000);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);
  expect(assign).toHaveBeenCalledTimes(1);
});

it("a delayed view 401 ends reconciliation in the explicit sign-in hold", async () => {
  me = { ...me, user_view: true, user_view_super_admin: true };
  await draft();
  const finish = pendingView(401);
  const checked = count("/api/v1/runs/policy-preview");
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  await tick(1000);
  expect(screen.queryByRole("dialog")).toBeNull();
  await act(async () => finish());
  await screen.findByRole("dialog");
  expect(isSignedOutHold()).toBe(true);
  expect(getToken()).toBeNull();
  const reads = count("/api/v1/me", "GET");
  act(() => setToken("later-unconfirmed-token"));
  act(() => window.dispatchEvent(new Event("focus")));
  await tick(4500);
  expect(count("/api/v1/me", "GET")).toBe(reads);
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);
  expect(assign).not.toHaveBeenCalled();
});

it("a normal confirmation outage remains fenced without polling and recovers on focus", async () => {
  await draft();
  meStatus = 503;
  const checked = count("/api/v1/runs/policy-preview");
  act(() => setToken("replacement-token"));
  await tick(0);
  const reads = count("/api/v1/me", "GET");
  await tick(4500);
  expect(count("/api/v1/me", "GET")).toBe(reads);
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  expect(count("/api/v1/runs")).toBe(0);
  meStatus = 200;
  act(() => window.dispatchEvent(new Event("focus")));
  await tick(0);
  await tick(1000);
  expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(checked);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
  expect(count("/api/v1/runs")).toBe(0);
});

it("a rejected current token holds New Run without a /me loop until explicit sign-in", async () => {
  await draft();
  const before = count("/api/v1/me", "GET");
  meStatus = 401;
  act(() => setToken("rejected-token"));
  await tick(0);
  await screen.findByRole("dialog");
  expect(getToken()).toBeNull();
  expect(isSignedOutHold()).toBe(true);
  expect(count("/api/v1/me", "GET")).toBe(before + 1);
  const reads = count("/api/v1/me", "GET");
  meStatus = 200;
  act(() => setToken("later-token"));
  act(() => window.dispatchEvent(new Event("focus")));
  await tick(4500);
  expect(count("/api/v1/me", "GET")).toBe(reads);
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  expect(count("/api/v1/runs")).toBe(0);

  const dialog = screen.getByRole("dialog");
  setField(within(dialog).getByLabelText("Admin token"), "confirmed-token");
  fireEvent.click(within(dialog).getByRole("button", { name: "Sign in", exact: true }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await tick(1000);
  expect(isSignedOutHold()).toBe(false);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
  expect(count("/api/v1/runs")).toBe(0);
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  await screen.findByText("Created run");
  expect(count("/api/v1/runs")).toBe(1);
});

it.each(["same", "other", "authority", "unmount"])("local token changes retire old normal-phase reads before a %s outcome", async (outcome) => {
  const view = await draft();
  const old = pendingMe();
  act(() => setToken("first-replacement"));
  await tick(0);
  expect(old.signal).toBeDefined();
  const current = pendingMe();
  act(() => setToken("second-replacement"));
  expect(old.signal?.aborted).toBe(true);
  const checked = count("/api/v1/runs/policy-preview");
  await act(async () => old.resolve(json(200, { ...me, principal: "obsolete-owner" })));
  await tick(0);
  expect(current.signal).toBeDefined();
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);
  if (outcome === "unmount") {
    view.unmount();
    expect(current.signal?.aborted).toBe(true);
  }
  const answer = outcome === "other" || outcome === "unmount" ? { ...me, principal: "bob" }
    : outcome === "authority" ? { ...me, role: "admin", operator: true, security_operator: true } : me;
  await act(async () => current.resolve(json(200, answer)));
  await tick(1000);
  expect(count("/api/v1/runs")).toBe(0);
  if (outcome === "same") {
    expect(assign).not.toHaveBeenCalled();
    expect(count("/api/v1/runs/policy-preview")).toBeGreaterThan(checked);
    expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
    const confirmedReads = count("/api/v1/me", "GET");
    act(() => window.dispatchEvent(new Event("focus")));
    await tick(1000);
    expect(count("/api/v1/me", "GET")).toBe(confirmedReads);
  } else {
    expect(count("/api/v1/runs/policy-preview")).toBe(checked);
    expect(screen.queryByLabelText("Title")).toBeNull();
    if (outcome === "unmount") expect(assign).not.toHaveBeenCalled();
    else expect(assign).toHaveBeenCalledWith("/runs/new");
  }
});

it.each([["quiet", 200], ["quiet", 401], ["quiet", 503], ["renewal", 200]] as const)("deliberate logout from %s with status %s retires the pending confirmation", async (phase, status) => {
  const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
  await draft();
  const current = pendingMe();
  if (phase === "quiet") act(() => setToken("replacement-before-logout"));
  else await startRenew();
  await tick(phase === "quiet" ? 0 : 1500);
  expect(current.signal).toBeDefined();
  let finishLogout!: (reply: Response) => void;
  logoutReply = new Promise<Response>((done) => { finishLogout = done; });
  const buttons = within(screen.getByRole("banner")).getAllByRole("button");
  await user.click(buttons[buttons.length - 1]);
  await user.click(within(await screen.findByRole("menu")).getByText("Sign out"));
  expect(isSignedOutHold()).toBe(true);
  expect(count("/api/v1/auth/logout")).toBe(1);
  const checked = count("/api/v1/runs/policy-preview");
  await act(async () => current.resolve(json(200, { ...me, principal: "bob" })));
  fireEvent.click(screen.getByRole("button", { name: /Launch run/ }));
  expect(assign).not.toHaveBeenCalled();
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(count("/api/v1/runs")).toBe(0);
  await act(async () => finishLogout(json(status, {})));
  await screen.findByText("Admin token", { exact: true });
  expect(isSignedOutHold()).toBe(false);
  act(() => window.dispatchEvent(new Event("focus")));
  await tick(3000);
  expect(getToken()).toBeNull();
  expect(screen.queryByLabelText("Title")).toBeNull();
  expect(assign).not.toHaveBeenCalled();
  expect(count("/api/v1/runs/policy-preview")).toBe(checked);
  expect(count("/api/v1/runs")).toBe(0);
});


it("a normal auth change cannot adopt an identity the shell never resolved", async () => {
  meStatus = 503;
  render(<MemoryRouter initialEntries={["/runs/new"]}><App /></MemoryRouter>);
  await screen.findByText(SHELL.UNKNOWN_BODY);
  meStatus = 200;
  act(() => setToken("new-token"));
  await tick(0);
  expect(assign).toHaveBeenCalledWith("/runs/new");
  expect(screen.queryByLabelText("Title")).toBeNull();
  expect(count("/api/v1/runs")).toBe(0);
});


it("a cookie-only 401 keeps the normal confirmation held without advancing auth or polling", async () => {
  sessionStorage.clear();
  me = { ...me, user_view: true, user_view_super_admin: true };
  await draft();
  meStatus = 401;
  fireEvent.click(screen.getByRole("button", { name: CONSOLE_VIEW.ADMIN, exact: true }));
  await screen.findByText(CONSOLE_VIEW.SWITCH_FAILED);
  await tick(0);
  await screen.findByRole("dialog");
  expect(getToken()).toBeNull();
  expect(isSignedOutHold()).toBe(true);
  const generation = getAuthGeneration();
  const reads = count("/api/v1/me", "GET");
  await tick(4500);
  expect(getAuthGeneration()).toBe(generation);
  expect(count("/api/v1/me", "GET")).toBe(reads);
  expect(count("/api/v1/runs")).toBe(0);
  expect(screen.getByLabelText("Title")).toHaveValue("Keep this draft");
});
