/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import App from "./App";
import { isSignedOutHold, setSignedOutHold } from "./lib/api/core";
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
afterEach(() => {
  cleanup();
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
