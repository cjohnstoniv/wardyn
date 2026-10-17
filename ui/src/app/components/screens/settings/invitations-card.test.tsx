/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account > Invitations (#1508, mock M-SVC v2): the person's own pending invitations, and Claim through the
// shared dialog. The token is shown once and held nowhere after the dialog closes.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import { holdsText } from "../../../../test/holds-text";
import { aheadByHours } from "../../../lib/test-clock";
import type { ApiToken, Invitation } from "../../../lib/types/service-identities";
import { INVITATIONS as T, TOKEN_DIALOG as D } from "../../wardyn/copy/service-tokens";
import { InvitationsCard } from "./invitations-card";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const SECRET = "wdn_mockonly0001_Qm3xV7pLzK9aRt2WbN8yHc5dJf4sUe6gEXAMPLE";
const OLD = "c7d1b093-2e5a-4f68-9b1c-a4e80d3f6725";
const withTTL: Invitation = {
  id: "3f6c1a90-5b7e-4d21-8a43-c0e9d2b71f05",
  target: "dana-sub",
  created_by: "security-lee@example.com",
  created_at: aheadByHours(-24),
  expires_at: aheadByHours(48),
  token_name: "ci-laptop",
  token_ttl_seconds: 30 * 86400,
  replaces_token_id: OLD,
  state: "pending",
};
const plain: Invitation = {
  id: "8a21e4d7-90c3-47b6-b1f8-52d7a60c3e19",
  target: "dana-sub",
  created_by: "ops-admin@example.com",
  created_at: aheadByHours(-1),
  expires_at: aheadByHours(12),
  state: "pending",
};
const minted = (over: Partial<ApiToken> = {}): ApiToken => ({
  id: "n1",
  principal: "dana-sub",
  role: "user",
  user_type: "standard",
  name: "ci-laptop",
  created_at: new Date().toISOString(),
  expires_at: aheadByHours(30 * 24),
  token: SECRET,
  ...over,
});

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let invitations: Invitation[];
let route: (method: string, path: string, body: string | undefined) => Response;
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  invitations = [withTTL, plain];
  route = (method, path) => {
    if (method === "GET" && path.startsWith("/api/v1/me/invitations")) return json(invitations);
    if (method === "GET" && path.startsWith("/api/v1/me/tokens")) return json([{ id: OLD, name: "ci-old-key" }]);
    return json({ error: "unexpected " + method + " " + path }, 500);
  };
  fetchMock = vi.fn(async (path: string, init?: RequestInit) => route(init?.method ?? "GET", String(path), init?.body as string | undefined));
  vi.stubGlobal("fetch", fetchMock);
  vi.mocked(toast.success).mockClear();
});
afterEach(() => vi.unstubAllGlobals());

const user = userEvent.setup({ pointerEventsCheck: 0 });
const mount = (url = "/account") =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <InvitationsCard />
    </MemoryRouter>,
  );
const sent = () => fetchMock.mock.calls.map(([p, i]) => `${i?.method ?? "GET"} ${String(p).split("?")[0]}`);
const openCard = async () => {
  await user.click(await screen.findByRole("button", { name: new RegExp(`^${T.TITLE}`) }));
};

describe("InvitationsCard", () => {
  it("shows nothing when no invitation is waiting", async () => {
    invitations = [];
    const { container } = mount();
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("shows nothing where there is no person to invite (the admin token, local mode)", async () => {
    route = () => json({ error: "an API token belongs to a signed-in human", reason: "api_token_no_human" }, 403);
    const { container } = mount();
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("counts what is waiting, then lists each invitation as the mock drew it", async () => {
    mount();
    expect(await screen.findByText(T.SUMMARY(2))).toBeInTheDocument();
    await openCard();
    expect(screen.getByText(T.LEDE)).toBeInTheDocument();
    expect(screen.getByText(T.FROM("security-lee@example.com"))).toBeInTheDocument();
    expect(screen.getByText("Expires in 2 days")).toBeInTheDocument();
    expect(screen.getByText("ci-laptop")).toBeInTheDocument();
    expect(screen.getByText(T.LASTS("30 days"))).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(/Claiming it also revokes/)).toHaveTextContent("Claiming it also revokes ci-old-key, a token an admin created for you."));
    expect(screen.getByText(T.FROM("ops-admin@example.com"))).toBeInTheDocument();
    expect(screen.getByText("Expires in 12 hours")).toBeInTheDocument();
    expect(sent()).toEqual(["GET /api/v1/me/invitations", "GET /api/v1/me/tokens"]);
  });

  it("says 'a token' when the replaced token's name cannot be read", async () => {
    const base = route;
    route = (m, p, b) => (p.startsWith("/api/v1/me/tokens") ? json({ error: "x" }, 500) : base(m, p, b));
    mount();
    await openCard();
    await waitFor(() => expect(screen.getByText(/Claiming it also revokes/)).toHaveTextContent("Claiming it also revokes a token, a token an admin created for you."));
  });

  it("opens the card from the link an invitation's console path names", async () => {
    mount(`/account?invitation=${withTTL.id}`);
    expect(await screen.findByText(T.LEDE)).toBeInTheDocument();
  });

  it("claims: shows the token once, refreshes the list, and holds the token nowhere after Done", async () => {
    let claimBody = "";
    route = (m, p, b) => {
      if (m === "POST" && p === `/api/v1/me/invitations/${withTTL.id}/claim`) {
        claimBody = b ?? "";
        invitations = [plain];
        return json(minted(), 201);
      }
      return m === "GET" && p.startsWith("/api/v1/me/tokens") ? json([{ id: OLD, name: "ci-old-key" }]) : json(invitations);
    };
    const { container } = mount();
    await openCard();
    await user.click(screen.getByRole("button", { name: T.CLAIM_LABEL("security-lee@example.com") }));
    expect(screen.getByLabelText(/Token name/)).toHaveValue("ci-laptop");
    await user.click(screen.getByRole("button", { name: D.CLAIM_SUBMIT }));
    expect(await screen.findByTestId("show-once-token")).toHaveTextContent(SECRET);
    expect(JSON.parse(claimBody)).toEqual({ name: "ci-laptop", ttl_seconds: 30 * 86400 });
    // the card already dropped the claimed invitation behind the dialog
    await waitFor(() => expect(screen.getByText(T.SUMMARY(1))).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: D.COPY }));
    await user.click(screen.getByRole("button", { name: D.DONE }));

    await waitFor(() => expect(screen.queryByTestId("show-once-token")).not.toBeInTheDocument());
    expect(toast.success).toHaveBeenCalledWith(T.CLAIMED_TOAST);
    expect(document.body.textContent).not.toContain(SECRET);
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(holdsText(container, SECRET)).toBe(false);
    // the opener is gone with the claimed row: focus lands on the card's heading
    await waitFor(() => expect(screen.getByRole("button", { name: new RegExp(`^${T.TITLE}`) })).toHaveFocus());
  });

  it("claiming the last waiting invitation moves focus to the main region, not <body> (FUG-L356)", async () => {
    invitations = [withTTL];
    route = (m, p) => {
      if (m === "POST" && p === `/api/v1/me/invitations/${withTTL.id}/claim`) {
        invitations = [];
        return json(minted(), 201);
      }
      return m === "GET" && p.startsWith("/api/v1/me/tokens") ? json([{ id: OLD, name: "ci-old-key" }]) : json(invitations);
    };
    const main = document.createElement("main");
    main.id = "main-content";
    main.tabIndex = -1;
    document.body.appendChild(main);
    try {
      mount();
      await openCard();
      await user.click(screen.getByRole("button", { name: T.CLAIM_LABEL("security-lee@example.com") }));
      await user.click(screen.getByRole("button", { name: D.CLAIM_SUBMIT }));
      await screen.findByTestId("show-once-token");
      await user.click(screen.getByRole("button", { name: D.COPY }));
      await user.click(screen.getByRole("button", { name: D.DONE }));
      await waitFor(() => expect(main).toHaveFocus());
    } finally {
      main.remove();
    }
  });

  it("a second claim never shows the first token again", async () => {
    const SECOND = SECRET.replace("0001", "0002");
    let n = 0;
    route = (m, p) => (m === "POST" ? json(minted({ token: n++ === 0 ? SECRET : SECOND }), 201) : p.startsWith("/api/v1/me/tokens") ? json([]) : json(invitations));
    const { container } = mount();
    await openCard();
    for (const secret of [SECRET, SECOND]) {
      await user.click(screen.getAllByRole("button", { name: /^Claim invitation from/ })[0]);
      await user.click(screen.getByRole("button", { name: D.CLAIM_SUBMIT }));
      expect(await screen.findByTestId("show-once-token")).toHaveTextContent(secret);
      await user.click(screen.getByRole("button", { name: D.COPY }));
      await user.click(screen.getByRole("button", { name: D.DONE }));
      await waitFor(() => expect(screen.queryByTestId("show-once-token")).not.toBeInTheDocument());
      expect(holdsText(container, secret)).toBe(false);
    }
    expect(holdsText(container, SECRET)).toBe(false);
  });

  it("shows a refusal in the dialog with the server's sentence, and refreshes the list behind it", async () => {
    route = (m, p) =>
      m === "POST"
        ? json({ error: "this invitation is claimed", reason: "invitation_not_pending", state: "claimed" }, 409)
        : p.startsWith("/api/v1/me/tokens")
          ? json([])
          : json(invitations);
    mount();
    await openCard();
    await user.click(screen.getAllByRole("button", { name: /^Claim invitation from/ })[0]);
    await user.click(screen.getByRole("button", { name: D.CLAIM_SUBMIT }));
    expect(await screen.findByRole("alert")).toHaveTextContent("this invitation is claimed");
    expect(screen.queryByRole("button", { name: D.CLAIM_SUBMIT })).not.toBeInTheDocument();
    await waitFor(() => expect(sent().filter((s) => s === "GET /api/v1/me/invitations").length).toBe(2));
  });

  it("says the server does not manage invitations when the store is missing", async () => {
    route = () => json({ error: "invitations require the Postgres store backend", reason: "people_store_unavailable" }, 501);
    mount();
    await openCard();
    expect(await screen.findByText(T.UNAVAILABLE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(T.UNAVAILABLE_BODY)).toBeInTheDocument();
  });

  it("offers Retry when the list cannot be read", async () => {
    let fail = true;
    route = () => (fail ? json({ error: "x" }, 500) : json(invitations));
    mount();
    await openCard();
    fail = false;
    await user.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByText(T.LEDE)).toBeInTheDocument();
  });
});
