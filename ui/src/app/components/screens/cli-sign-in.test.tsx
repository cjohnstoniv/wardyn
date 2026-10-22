/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// /cli/sign-in (boards MATT2-*): the approve page. The endpoints are stubbed from the server's wire types
// (internal/types/cli_sign_in.go, lane ATT-LOGIN-SRV); this proves what the page does with each answer.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { CLISignInView } from "../../lib/types/cli-sign-in";
import { SIGN_IN as T } from "../wardyn/copy/cli-sign-in";
import { CliSignInScreen, isLoopbackCallback } from "./cli-sign-in";

// Every time in this file is an offset in seconds from one start instant, 2026-10-19 18:42:00 UTC as epoch
// milliseconds, so no fixture is a calendar literal. The clock is faked, so the start can be a fixed instant.
const START = 1792435320000;
const at = (seconds: number) => new Date(START + seconds * 1000).toISOString();

const ID = "5e1d9b0c-0000-4000-8000-000000000001";
const CALLBACK = `http://127.0.0.1:53124/callback?id=${ID}&code=abc123`;
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const refuse = (status: number, reason: string, error = "server text") => json({ error, reason }, status);

const view = (over: Partial<CLISignInView> = {}): CLISignInView => ({
  id: ID,
  machine: "mbp-dev",
  user_code: "KQ7M-2XJD",
  mode: "loopback",
  store: "keychain",
  admin: false,
  request_address: "203.0.113.24",
  approver_address: "203.0.113.24",
  address_match: true,
  created_at: at(0),
  expires_at: at(600),
  session_ttl_seconds: 30 * 86400,
  session_idle_seconds: 7 * 86400,
  access_ttl_seconds: 3600,
  ...over,
});

let answers: Record<string, () => Response>;
let calls: string[];
let assign: ReturnType<typeof vi.fn>;
let me: Record<string, unknown>;
const realLocation = window.location;
beforeEach(() => {
  // 30 s into a request that lives 10 minutes (created at 0 s, expires at 600 s).
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(Date.parse(at(30)));
  me = { principal: "sub-1", email: "you@example.com", role: "user", method: "sso" };
  calls = [];
  answers = { [`GET /api/v1/me/cli-sign-ins/${ID}`]: () => json(view()) };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init?: RequestInit) => {
      const key = `${init?.method ?? "GET"} ${String(path).split("?")[0]}`;
      calls.push(key);
      if (key === "GET /api/v1/me") return json(me);
      return (answers[key] ?? (() => json({ error: "unexpected " + key }, 500)))();
    }),
  );
  assign = vi.fn();
  Object.defineProperty(window, "location", { value: { ...realLocation, assign }, configurable: true });
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  Object.defineProperty(window, "location", { value: realLocation, configurable: true });
});

const user = userEvent.setup({ pointerEventsCheck: 0 });
const mount = (search = `?id=${ID}`) =>
  render(
    <MemoryRouter initialEntries={[`/cli/sign-in${search}`]}>
      <CliSignInScreen />
    </MemoryRouter>,
  );
const approvePath = `POST /api/v1/me/cli-sign-ins/${ID}/approve`;
const denyPath = `POST /api/v1/me/cli-sign-ins/${ID}/deny`;

describe("the request", () => {
  it("shows the computer, address, time, expiry, code and the operator's own lifetimes", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: T.TITLE })).toBeVisible();
    expect(screen.getByText(T.LEAD("you@example.com"))).toBeVisible();
    expect(screen.getByText("mbp-dev")).toBeVisible();
    expect(screen.getByText("203.0.113.24")).toBeVisible();
    expect(screen.getByText("19 Oct 2026, 18:42 UTC")).toBeVisible();
    expect(screen.getByText("Expires in 10 minutes")).toBeVisible();
    expect(screen.getByTestId("cli-user-code")).toHaveTextContent("KQ7M-2XJD");
    expect(screen.getByText(T.CODE_NOTE)).toBeVisible();
    expect(screen.getByText(T.GRANT(3600, 30 * 86400, 7 * 86400))).toBeVisible();
    expect(screen.queryByTestId("cli-address-mismatch")).toBeNull();
  });

  it("words a shorter file-store session from the server's numbers", async () => {
    answers[`GET /api/v1/me/cli-sign-ins/${ID}`] = () => json(view({ store: "file", session_ttl_seconds: 7 * 86400 }));
    mount();
    expect(await screen.findByText(T.GRANT(3600, 7 * 86400, 7 * 86400))).toBeVisible();
  });

  it("warns above Approve, naming both addresses, when the server says they differ, and still lets Approve through", async () => {
    answers[`GET /api/v1/me/cli-sign-ins/${ID}`] = () =>
      json(view({ request_address: "198.51.100.7", approver_address: "203.0.113.24", address_match: false }));
    answers[approvePath] = () => json({ code: "abc123", code_expires_in: 60, redirect_url: CALLBACK });
    mount();
    const warning = await screen.findByTestId("cli-address-mismatch");
    expect(warning).toHaveTextContent(T.MISMATCH_TITLE);
    expect(warning).toHaveTextContent(T.MISMATCH_BODY);
    expect(warning).toHaveTextContent("203.0.113.24");
    expect(screen.getByText("198.51.100.7")).toBeVisible();
    expect(screen.getByText(T.NOT_THIS_BROWSER)).toBeVisible();
    // Above Approve in document order.
    expect(warning.compareDocumentPosition(screen.getByRole("button", { name: T.APPROVE })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await user.click(screen.getByRole("button", { name: T.APPROVE }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(CALLBACK));
  });

  it("is the admin step-up when the request asks for one", async () => {
    answers[`GET /api/v1/me/cli-sign-ins/${ID}`] = () => json(view({ admin: true }));
    mount();
    expect(await screen.findByRole("heading", { name: "Admin access for 1 hour" })).toBeVisible();
    expect(screen.getByText(T.ADMIN_LEAD(3600))).toBeVisible();
    expect(screen.getByText(T.ADMIN_GRANT(3600))).toBeVisible();
    expect(screen.queryByText(T.TITLE)).toBeNull();
  });
});

describe("the lead and the clock", () => {
  it("says 'you' when the session carries no name, not an empty 'sign in as .'", async () => {
    me = { role: "user", method: "sso" };
    mount();
    expect(await screen.findByText(T.LEAD("you"))).toBeVisible();
    expect(screen.queryByText(/sign in as \./)).toBeNull();
  });

  it("shows the time left, not the whole lifetime: 8 minutes into 10 says 2 minutes", async () => {
    vi.setSystemTime(Date.parse(at(480)));
    mount();
    expect(await screen.findByText("Expires in 2 minutes")).toBeVisible();
    expect(screen.queryByText("Expires in 10 minutes")).toBeNull();
  });

  it("rounds the last seconds up to a minute rather than saying 0", async () => {
    vi.setSystemTime(Date.parse(at(580)));
    mount();
    expect(await screen.findByText("Expires in 1 minute")).toBeVisible();
  });

  it("is the expired page once the time is up, and offers no Approve", async () => {
    vi.setSystemTime(Date.parse(at(600)));
    mount();
    expect(await screen.findByRole("heading", { name: T.EXPIRED_TITLE })).toBeVisible();
    expect(screen.queryByRole("button", { name: T.APPROVE })).toBeNull();
  });

  it("turns into the expired page while it is open when the clock runs out", async () => {
    vi.setSystemTime(Date.parse(at(599)));
    mount();
    expect(await screen.findByText("Expires in 1 minute")).toBeVisible();
    vi.setSystemTime(Date.parse(at(601)));
    expect(await screen.findByRole("heading", { name: T.EXPIRED_TITLE }, { timeout: 3000 })).toBeVisible();
  });
});

describe("Approve", () => {
  it("is a fetch POST, then the page navigates the browser to the loopback URL it was given", async () => {
    answers[approvePath] = () => json({ code: "abc123", code_expires_in: 60, redirect_url: CALLBACK });
    mount();
    await user.click(await screen.findByRole("button", { name: T.APPROVE }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(CALLBACK));
    expect(calls).toContain(approvePath);
  });

  it("does not move the browser anywhere but the CLI's own loopback callback", async () => {
    answers[approvePath] = () => json({ code: "abc123", code_expires_in: 60, redirect_url: "https://evil.example/callback?code=abc123" });
    mount();
    await user.click(await screen.findByRole("button", { name: T.APPROVE }));
    expect(await screen.findByText(T.NO_CALLBACK)).toBeVisible();
    expect(assign).not.toHaveBeenCalled();
    expect(isLoopbackCallback(CALLBACK)).toBe(true);
    expect(isLoopbackCallback("http://127.0.0.1:1/callback?id=x&code=y")).toBe(true);
    expect(isLoopbackCallback("http://127.0.0.1:65535/callback")).toBe(true);
    const bad = [
      "http://127.0.0.1.evil.example/callback",
      "https://127.0.0.1:1/callback",
      "http://127.0.0.1:1/other",
      "javascript:alert(1)",
      "",
      "http://evil@127.0.0.1:53124/callback", // userinfo
      "http://:pw@127.0.0.1:53124/callback", // password only
      "http://127.0.0.1/callback", // no explicit port
      "http://127.0.0.1:80/callback", // the default port parses as none
      "http://127.0.0.1:0/callback", // port 0
      "http://127.0.0.1:65536/callback", // out of range
      "http://[::1]:53124/callback", // not the literal address
      "http://localhost:53124/callback",
    ];
    for (const b of bad) {
      expect(isLoopbackCallback(b)).toBe(false);
    }
  });

  it("shows the code once, with the paste warning and a copy button, for a paste-back request", async () => {
    answers[`GET /api/v1/me/cli-sign-ins/${ID}`] = () => json(view({ mode: "paste" }));
    answers[approvePath] = () => json({ code: "7H4N-Q2VC", code_expires_in: 300 });
    mount();
    await user.click(await screen.findByRole("button", { name: T.APPROVE }));
    expect(await screen.findByRole("heading", { name: T.PASTE_TITLE })).toBeVisible();
    expect(screen.getByTestId("cli-paste-code")).toHaveTextContent("7H4N-Q2VC");
    expect(screen.getByText(T.PASTE_WARN)).toBeVisible();
    expect(screen.getByText(T.PASTE_ONCE(300))).toBeVisible();
    expect(screen.getByRole("button", { name: T.PASTE_COPY })).toBeVisible();
    expect(assign).not.toHaveBeenCalled();
  });

  it.each<[string, number, string, string]>([
    ["the user view (409)", 409, "api_token_member_mode_mint", T.REFUSED_USER_VIEW],
    ["no API-token permission (403)", 403, "capability_feature", T.REFUSED_NOT_ALLOWED],
  ])("words a refusal for %s as a sign-in and keeps the request on screen", async (_n, status, reason, sentence) => {
    answers[approvePath] = () => refuse(status, reason, "the server's own sentence");
    mount();
    await user.click(await screen.findByRole("button", { name: T.APPROVE }));
    expect(await screen.findByRole("alert")).toHaveTextContent(sentence);
    expect(screen.queryByText("the server's own sentence")).toBeNull();
    expect(screen.getByTestId("cli-user-code")).toHaveTextContent("KQ7M-2XJD");
    expect(screen.getByRole("button", { name: T.APPROVE })).toBeEnabled();
    expect(assign).not.toHaveBeenCalled();
  });

  it("shows the server's own text for any other refusal", async () => {
    answers[approvePath] = () => refuse(409, "cli_admin_needs_session", "admin access needs a live CLI sign-in of yours on that computer");
    mount();
    await user.click(await screen.findByRole("button", { name: T.APPROVE }));
    expect(await screen.findByRole("alert")).toHaveTextContent("admin access needs a live CLI sign-in of yours on that computer");
  });
});

describe("Deny", () => {
  it("tells the CLI through its callback in loopback mode", async () => {
    const back = `http://127.0.0.1:53124/callback?id=${ID}&error=denied`;
    answers[denyPath] = () => json({ redirect_url: back });
    mount();
    await user.click(await screen.findByRole("button", { name: T.DENY }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(back));
    expect(calls).not.toContain(approvePath);
  });

  it("shows the Denied page in paste mode, which has no callback", async () => {
    answers[`GET /api/v1/me/cli-sign-ins/${ID}`] = () => json(view({ mode: "paste" }));
    answers[denyPath] = () => json({});
    mount();
    await user.click(await screen.findByRole("button", { name: T.DENY }));
    expect(await screen.findByRole("heading", { name: T.DENIED_TITLE })).toBeVisible();
    expect(screen.getByText(T.DENIED_BODY)).toBeVisible();
    expect(assign).not.toHaveBeenCalled();
  });
});

describe("a link that is over", () => {
  it.each<[string, string, string, string]>([
    ["expired", "cli_sign_in_expired", T.EXPIRED_TITLE, T.EXPIRED_BODY],
    ["already used", "cli_sign_in_used", T.USED_TITLE, T.USED_BODY],
  ])("opens a page for a request that is %s (410)", async (_n, reason, title, body) => {
    answers[`GET /api/v1/me/cli-sign-ins/${ID}`] = () => refuse(410, reason);
    mount();
    expect(await screen.findByRole("heading", { name: title })).toBeVisible();
    expect(screen.getByText(body)).toBeVisible();
    expect(screen.queryByRole("button", { name: T.APPROVE })).toBeNull();
  });

  it("swaps the request for the used page when Approve finds it already decided", async () => {
    answers[approvePath] = () => refuse(410, "cli_sign_in_used");
    mount();
    await user.click(await screen.findByRole("button", { name: T.APPROVE }));
    expect(await screen.findByRole("heading", { name: T.USED_TITLE })).toBeVisible();
  });

  it("offers a retry when the request cannot be read", async () => {
    answers[`GET /api/v1/me/cli-sign-ins/${ID}`] = () => refuse(404, "cli_sign_in_not_found", "no such sign-in: run wardyn login again");
    mount();
    expect(await screen.findByText("no such sign-in: run wardyn login again")).toBeVisible();
  });
});
