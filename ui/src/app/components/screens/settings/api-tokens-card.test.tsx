/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account > Your API tokens (r15 T2-d): a person's own tokens, listed with the Inbound only chip, minted
// through the shared dialog (kind "inbound" only when Calling inbound ports is chosen) and revoked with a confirm.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import { setField } from "../../../../test/set-field";
import { aheadByHours } from "../../../lib/test-clock";
import type { ApiToken } from "../../../lib/types/service-identities";
import { MINTED } from "../../wardyn/copy/credentials";
import { OWN_TOKENS as O, SERVICES as T, TOKEN_DIALOG as D } from "../../wardyn/copy/service-tokens";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ApiTokensCard } from "./api-tokens-card";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const SECRET = "wdn_mockonly0001_Qm3xV7pLzK9aRt2WbN8yHc5dJf4sUe6gEXAMPLE";
const tok = (over: Partial<ApiToken>): ApiToken => ({
  id: "t1",
  principal: "dana-sub",
  role: "user",
  user_type: "standard",
  name: "laptop-cli",
  created_at: aheadByHours(-24),
  expires_at: aheadByHours(30 * 24),
  ...over,
});
const api = tok({});
const inbound = tok({ id: "t2", name: "ci-hook", inbound: true });

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let tokens: ApiToken[];
let route: (method: string, path: string, body: string | undefined) => Response;
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  tokens = [api, inbound];
  route = (method, path) => {
    if (method === "GET" && path.startsWith("/api/v1/me/tokens")) return json(tokens);
    return json({ error: "unexpected " + method + " " + path }, 500);
  };
  fetchMock = vi.fn(async (path: string, init?: RequestInit) => route(init?.method ?? "GET", String(path), init?.body as string | undefined));
  vi.stubGlobal("fetch", fetchMock);
  vi.mocked(toast.success).mockClear();
  vi.mocked(toast.error).mockClear();
});
afterEach(() => vi.unstubAllGlobals());

const user = userEvent.setup({ pointerEventsCheck: 0 });
const mount = (operator = true) =>
  render(
    <OperatorProvider operator={operator}>
      <ApiTokensCard />
    </OperatorProvider>,
  );
const sent = () => fetchMock.mock.calls.map(([p, i]) => `${i?.method ?? "GET"} ${String(p).split("?")[0]}`);
const openCard = async () => {
  await user.click(await screen.findByRole("button", { name: new RegExp(`^${O.TITLE}`) }));
};

describe("ApiTokensCard", () => {
  it("counts the live tokens, and says 'No tokens' when there are none", async () => {
    mount();
    expect(await screen.findByText(O.SUMMARY(2))).toBeInTheDocument();
    tokens = [];
    mount();
    expect(await screen.findByText("No tokens")).toBeInTheDocument();
  });

  it("lists the tokens, with the Inbound only chip on the inbound one alone", async () => {
    mount();
    await openCard();
    const table = await screen.findByRole("table", { name: O.TABLE_LABEL });
    const rowOf = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;
    expect(table).toBeInTheDocument();
    expect(rowOf("laptop-cli")).not.toHaveTextContent(T.INBOUND_ONLY);
    expect(rowOf("ci-hook")).toHaveTextContent(T.INBOUND_ONLY);
    expect(screen.getAllByText(T.INBOUND_ONLY)).toHaveLength(1);
  });

  it("shows nothing where there is no person to own a token (the admin token, local mode)", async () => {
    route = () => json({ error: "An API token belongs to a signed-in human", reason: "api_token_no_human" }, 403);
    const { container } = mount();
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("shows nothing where the api_token feature is not granted", async () => {
    const base = route;
    route = (m, p, b) =>
      p.startsWith("/api/v1/me/capabilities")
        ? json({ grants: [], enforcement: { feature: true }, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 })
        : base(m, p, b);
    const { container } = mount(false);
    await waitFor(() => expect(sent()).toContain("GET /api/v1/me/capabilities"));
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  async function mintWith(use: string | null) {
    let body = "";
    const base = route;
    route = (m, p, b) => {
      if (m === "POST" && p === "/api/v1/me/tokens") {
        body = b ?? "";
        return json(tok({ id: "t3", name: "new-one", token: SECRET }), 201);
      }
      return base(m, p, b);
    };
    mount();
    await openCard();
    await user.click(await screen.findByRole("button", { name: T.MINT }));
    expect(screen.getByRole("heading", { name: D.OWN_MINT_TITLE })).toBeInTheDocument();
    expect(screen.getByText(D.OWN_MINT_LEDE)).toBeInTheDocument();
    if (use) await user.click(screen.getByRole("radio", { name: use }));
    setField(screen.getByLabelText(/Token name/), "new-one");
    await user.click(screen.getByRole("button", { name: D.MINT_SUBMIT }));
    expect(await screen.findByTestId("show-once-token")).toHaveTextContent(SECRET);
    return JSON.parse(body) as Record<string, unknown>;
  }

  it("mints for the Wardyn API with no kind", async () => {
    const body = await mintWith(null);
    expect(body).toEqual({ name: "new-one", ttl_seconds: 90 * 86400 });
    expect(body).not.toHaveProperty("kind");
    expect(screen.getByText(D.ACTS_AS_YOU("User"))).toBeInTheDocument();
  });

  it("mints kind inbound when Calling inbound ports is chosen", async () => {
    const body = await mintWith(D.USE_INBOUND);
    expect(body).toEqual({ name: "new-one", ttl_seconds: 90 * 86400, kind: "inbound" });
    await user.click(screen.getByRole("button", { name: D.COPY }));
    await user.click(screen.getByRole("button", { name: D.DONE }));
    await waitFor(() => expect(screen.queryByTestId("show-once-token")).not.toBeInTheDocument());
    expect(toast.success).toHaveBeenCalledWith(T.MINTED_TOAST);
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it("revokes after a confirm, then refreshes the list", async () => {
    const base = route;
    route = (m, p, b) => {
      if (m === "DELETE" && p === "/api/v1/me/tokens/t1") {
        tokens = [inbound];
        return new Response(null, { status: 204 });
      }
      return base(m, p, b);
    };
    mount();
    await openCard();
    await user.click(await screen.findByRole("button", { name: O.REVOKE_LABEL("laptop-cli") }));
    expect(screen.getByText(O.REVOKE_TITLE("laptop-cli"))).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: MINTED.REVOKE_CONFIRM }));
    await waitFor(() => expect(screen.queryByRole("button", { name: O.REVOKE_LABEL("laptop-cli") })).not.toBeInTheDocument());
    expect(sent().filter((s) => s.startsWith("DELETE"))).toEqual(["DELETE /api/v1/me/tokens/t1"]);
    expect(sent().filter((s) => s === "GET /api/v1/me/tokens")).toHaveLength(2);
    expect(toast.success).toHaveBeenCalledWith(MINTED.REVOKED_TOAST);
    expect(screen.queryByText("laptop-cli")).not.toBeInTheDocument();
  });
});
