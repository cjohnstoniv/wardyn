/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account > Service identities I sponsor (#1508, mock M-SVC v2): the identities the person sponsors, a
// mint through the shared dialog that shows the token once, and listing and revoking the identity's tokens.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import { holdsText } from "../../../../test/holds-text";
import { setField } from "../../../../test/set-field";
import { aheadByHours } from "../../../lib/test-clock";
import type { ApiToken, ServiceIdentity } from "../../../lib/types/service-identities";
import { MINTED } from "../../wardyn/copy/credentials";
import { SERVICES as T, TOKEN_DIALOG as D } from "../../wardyn/copy/service-tokens";
import { ServiceIdentitiesCard } from "./service-identities-card";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const SECRET = "wdn_mockonly0001_Qm3xV7pLzK9aRt2WbN8yHc5dJf4sUe6gEXAMPLE";
const identity = (name: string, state: ServiceIdentity["state"], description = ""): ServiceIdentity => ({
  name,
  description,
  sponsor: "dana-sub",
  sponsor_since: aheadByHours(-500),
  user_type: "standard",
  created_by: "admin",
  created_at: aheadByHours(-500),
  state,
});
const tok = (id: string, name: string, over: Partial<ApiToken> = {}): ApiToken => ({
  id,
  principal: "service:ci-nightly",
  role: "user",
  user_type: "standard",
  name,
  created_at: aheadByHours(-20 * 24),
  expires_at: aheadByHours(70 * 24),
  ...over,
});

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let identities: ServiceIdentity[];
let tokens: ApiToken[];
let route: (method: string, path: string, body: string | undefined) => Response;
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  identities = [
    identity("ci-nightly", "active", "Nightly build and test runs"),
    identity("release-notes", "active"),
    identity("legacy-sync", "suspended"),
    identity("old-importer", "retired"),
  ];
  tokens = [
    tok("a1", "nightly-runner", { last_used_at: aheadByHours(-1) }),
    tok("a2", "nightly-runner-old", { created_at: aheadByHours(-80 * 24), expires_at: aheadByHours(-48) }),
    tok("a3", "trial", { created_at: aheadByHours(-40 * 24), revoked_at: aheadByHours(-38 * 24) }),
  ];
  route = (method, path) => {
    if (method === "GET" && path.startsWith("/api/v1/me/service-identities/ci-nightly/tokens")) return json(tokens);
    if (method === "GET" && path.includes("/tokens")) return json([]);
    if (method === "GET" && path.startsWith("/api/v1/me/service-identities")) return json(identities);
    return json({ error: "unexpected " + method + " " + path }, 500);
  };
  fetchMock = vi.fn(async (path: string, init?: RequestInit) => route(init?.method ?? "GET", String(path).split("?")[0], init?.body as string | undefined));
  vi.stubGlobal("fetch", fetchMock);
  vi.mocked(toast.success).mockClear();
  vi.mocked(toast.error).mockClear();
});
afterEach(() => vi.unstubAllGlobals());

const user = userEvent.setup({ pointerEventsCheck: 0 });
const row = (name: string) => within(document.querySelector<HTMLElement>(`[data-svc="${name}"]`)!);
const openCard = async () => {
  await user.click(await screen.findByRole("button", { name: new RegExp(`^${T.TITLE}`) }));
};
const sent = () => fetchMock.mock.calls.map(([p, i]) => `${i?.method ?? "GET"} ${String(p).split("?")[0]}`);

describe("ServiceIdentitiesCard", () => {
  it("shows nothing for a person who sponsors none", async () => {
    identities = [];
    const { container } = render(<ServiceIdentitiesCard />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("shows nothing where there is no person to sponsor one (local mode)", async () => {
    route = () => json({ error: "service identities need single sign-on", reason: "needs_sso" }, 409);
    const { container } = render(<ServiceIdentitiesCard />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("counts active and not active identities, then lists each with its state and what it means", async () => {
    render(<ServiceIdentitiesCard />);
    expect(await screen.findByText(T.SUMMARY(2, 4))).toBeInTheDocument();
    await openCard();
    expect(screen.getByText(T.LEDE)).toBeInTheDocument();
    expect(screen.getByText("Nightly build and test runs")).toBeInTheDocument();
    expect(row("ci-nightly").getByText(T.STATE_ACTIVE)).toBeInTheDocument();
    expect(row("legacy-sync").getByText(T.STATE_SUSPENDED)).toBeInTheDocument();
    expect(row("legacy-sync").getByText(T.HINT_SUSPENDED)).toBeInTheDocument();
    expect(row("old-importer").getByText(T.STATE_RETIRED)).toBeInTheDocument();
    expect(row("old-importer").getByText(T.HINT_RETIRED)).toBeInTheDocument();
  });

  it("an identity that is not active cannot be minted for, and the reason is beside the button", async () => {
    identities = [identity("ci-nightly", "sponsor_inactive")];
    render(<ServiceIdentitiesCard />);
    await openCard();
    const mint = row("ci-nightly").getByRole("button", { name: T.MINT_LABEL("ci-nightly") });
    expect(mint).toHaveAttribute("aria-disabled", "true");
    expect(mint).toHaveAccessibleDescription(T.HINT_SPONSOR_INACTIVE);
    await user.click(mint);
    expect(screen.queryByRole("heading", { name: D.MINT_TITLE("ci-nightly") })).not.toBeInTheDocument();
  });

  it("lists an identity's tokens on demand: expired and revoked ones marked, only live ones revocable", async () => {
    render(<ServiceIdentitiesCard />);
    await openCard();
    expect(sent()).toEqual(["GET /api/v1/me/service-identities"]);
    await user.click(row("ci-nightly").getByRole("button", { name: T.SHOW_TOKENS_LABEL("ci-nightly") }));
    const table = await screen.findByRole("table", { name: T.TABLE_LABEL("ci-nightly") });
    expect(within(table).getByText("nightly-runner")).toBeInTheDocument();
    expect(within(table).getByText(T.CREATED("20 days ago"))).toBeInTheDocument();
    expect(within(table).getByText("in 70 days")).toBeInTheDocument();
    expect(within(table).getByText(T.EXPIRED_AT("2 days ago"))).toBeInTheDocument();
    expect(within(table).getByText(T.REVOKED_AT("38 days ago"))).toBeInTheDocument();
    expect(within(table).getAllByText(T.NEVER).length).toBeGreaterThan(0);
    expect(within(table).getAllByRole("button", { name: /^Revoke / })).toHaveLength(1);
    await user.click(row("ci-nightly").getByRole("button", { name: T.HIDE_TOKENS_LABEL("ci-nightly") }));
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("says no tokens yet, and offers to mint only for an active identity", async () => {
    render(<ServiceIdentitiesCard />);
    await openCard();
    await user.click(row("release-notes").getByRole("button", { name: T.SHOW_TOKENS_LABEL("release-notes") }));
    expect(await screen.findByText(`${T.NO_TOKENS} ${T.NO_TOKENS_MINT("release-notes")}`)).toBeInTheDocument();
    await user.click(row("old-importer").getByRole("button", { name: T.SHOW_TOKENS_LABEL("old-importer") }));
    expect(await screen.findByText(T.NO_TOKENS)).toBeInTheDocument();
  });

  it("revokes one token after a confirmation that names it, then reloads the tokens", async () => {
    let revoked = "";
    const base = route;
    route = (m, p, b) => {
      if (m === "DELETE") {
        revoked = p;
        tokens = tokens.map((t) => (t.id === "a1" ? { ...t, revoked_at: new Date().toISOString() } : t));
        return new Response(null, { status: 204 });
      }
      return base(m, p, b);
    };
    render(<ServiceIdentitiesCard />);
    await openCard();
    await user.click(row("ci-nightly").getByRole("button", { name: T.SHOW_TOKENS_LABEL("ci-nightly") }));
    await user.click(await screen.findByRole("button", { name: T.REVOKE_LABEL("nightly-runner", "ci-nightly") }));
    expect(screen.getByRole("alertdialog")).toHaveTextContent(T.REVOKE_TITLE("nightly-runner", "ci-nightly"));
    expect(screen.getByText(T.REVOKE_BODY("ci-nightly"))).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: MINTED.REVOKE_CONFIRM }));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(MINTED.REVOKED_TOAST));
    expect(revoked).toBe("/api/v1/me/service-identities/ci-nightly/tokens/a1");
    await waitFor(() => expect(screen.queryByRole("button", { name: T.REVOKE_LABEL("nightly-runner", "ci-nightly") })).not.toBeInTheDocument());
    await waitFor(() => expect(row("ci-nightly").getByRole("button", { name: T.HIDE_TOKENS_LABEL("ci-nightly") })).toHaveFocus());
  });

  it("mints: shows the token once, adds it to the open list, and holds it nowhere after Done", async () => {
    let mintBody = "";
    const base = route;
    route = (m, p, b) => {
      if (m === "POST" && p === "/api/v1/me/service-identities/ci-nightly/tokens") {
        mintBody = b ?? "";
        const t = tok("n1", "deploy", { created_at: new Date().toISOString(), expires_at: aheadByHours(90 * 24), token: SECRET });
        tokens = [{ ...t, token: undefined }, ...tokens];
        return json(t, 201);
      }
      return base(m, p, b);
    };
    const { container } = render(<ServiceIdentitiesCard />);
    await openCard();
    await user.click(row("ci-nightly").getByRole("button", { name: T.MINT_LABEL("ci-nightly") }));
    expect(screen.getByText(D.MINT_WARNING("ci-nightly"))).toBeInTheDocument();
    setField(screen.getByLabelText(/Token name/), "deploy");
    await user.click(screen.getByRole("button", { name: D.MINT_SUBMIT }));
    expect(await screen.findByTestId("show-once-token")).toHaveTextContent(SECRET);
    expect(JSON.parse(mintBody)).toEqual({ name: "deploy", ttl_seconds: 90 * 86400 });
    expect(screen.getByText("service:ci-nightly")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: D.COPY }));
    await user.click(screen.getByRole("button", { name: D.DONE }));

    await waitFor(() => expect(screen.queryByTestId("show-once-token")).not.toBeInTheDocument());
    expect(toast.success).toHaveBeenCalledWith(T.MINTED_TOAST);
    expect(document.body.textContent).not.toContain(SECRET);
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(holdsText(container, SECRET)).toBe(false);
    // the new token is in the open list, without a value
    const table = await screen.findByRole("table", { name: T.TABLE_LABEL("ci-nightly") });
    expect(within(table).getByText("deploy")).toBeInTheDocument();
    // focus is back on the button that opened the dialog
    await waitFor(() => expect(row("ci-nightly").getByRole("button", { name: T.MINT_LABEL("ci-nightly") })).toHaveFocus());
  });

  it("a refusal keeps the dialog open with the server's sentence and moves the row to its new state", async () => {
    const base = route;
    route = (m, p, b) => {
      if (m === "POST") {
        identities = identities.map((s) => (s.name === "ci-nightly" ? { ...s, state: "suspended" } : s));
        return json({ error: "this service identity is suspended; its tokens cannot be minted", reason: "service_identity_inactive" }, 409);
      }
      return base(m, p, b);
    };
    render(<ServiceIdentitiesCard />);
    await openCard();
    await user.click(row("ci-nightly").getByRole("button", { name: T.MINT_LABEL("ci-nightly") }));
    setField(screen.getByLabelText(/Token name/), "deploy");
    await user.click(screen.getByRole("button", { name: D.MINT_SUBMIT }));
    expect(await screen.findByRole("alert")).toHaveTextContent("this service identity is suspended; its tokens cannot be minted");
    expect(screen.getByRole("button", { name: D.MINT_SUBMIT })).toBeEnabled();
    await waitFor(() => expect(row("ci-nightly").getByText(T.STATE_SUSPENDED)).toBeInTheDocument());
  });

  it("an identity that is no longer the person's to mint for leaves only Close", async () => {
    const base = route;
    route = (m, p, b) => {
      if (m === "POST") {
        identities = identities.filter((s) => s.name !== "ci-nightly");
        return json({ error: "this service identity is no longer active under you; its tokens cannot be minted", reason: "service_identity_inactive" }, 409);
      }
      return base(m, p, b);
    };
    render(<ServiceIdentitiesCard />);
    await openCard();
    await user.click(row("ci-nightly").getByRole("button", { name: T.MINT_LABEL("ci-nightly") }));
    setField(screen.getByLabelText(/Token name/), "deploy");
    await user.click(screen.getByRole("button", { name: D.MINT_SUBMIT }));
    expect(await screen.findByRole("alert")).toHaveTextContent("no longer active under you");
    await waitFor(() => expect(screen.queryByRole("button", { name: D.MINT_SUBMIT })).not.toBeInTheDocument());
  });

  it("says the server does not manage service identities when the store is missing", async () => {
    route = () => json({ error: "x", reason: "people_store_unavailable" }, 501);
    render(<ServiceIdentitiesCard />);
    await openCard();
    expect(await screen.findByText(T.UNAVAILABLE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(T.UNAVAILABLE_BODY)).toBeInTheDocument();
  });

  it("offers Retry when the tokens cannot be read", async () => {
    let fail = true;
    const base = route;
    route = (m, p, b) => (p.endsWith("/ci-nightly/tokens") && fail ? json({ error: "x" }, 500) : base(m, p, b));
    render(<ServiceIdentitiesCard />);
    await openCard();
    await user.click(row("ci-nightly").getByRole("button", { name: T.SHOW_TOKENS_LABEL("ci-nightly") }));
    fail = false;
    await user.click(await within(row("ci-nightly").getByRole("group", { name: T.TABLE_LABEL("ci-nightly") })).findByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table", { name: T.TABLE_LABEL("ci-nightly") })).toBeInTheDocument();
  });
});
