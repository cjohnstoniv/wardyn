/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// An endpoint's sign-in on Your model connections: the row's buttons and lines, Disconnect asking first, and
// Sign in opening the door that waits for the window it opened.
import { it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const deleteMock = vi.fn();
vi.mock("../../../lib/api/model-provider-credentials", () => ({
  modelProviderCredentials: { deleteCredential: (...a: unknown[]) => deleteMock(...a) },
}));

import { ModelConnectionsCard } from "./model-connections-card";
import { WithDoor } from "../../../../test/door-harness";
import { providerStatus } from "../../../lib/test-fixtures";
import { expandCard } from "../../../lib/test-dom";
import { aheadByHours } from "../../../lib/test-clock";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { OIDC_RETURN, OIDC_SIGN_IN as T } from "../../wardyn/copy/oidc-signin";
import type { SetupModelProvider, SetupStatus } from "../../../lib/types";

const SSO: SetupModelProvider = {
  id: "corp-sso",
  uid: "uid-7f3a",
  name: "Corp SSO",
  kind: "custom_endpoint",
  harnesses: ["claude-code"],
  host: "llm.contoso.example",
  sign_in: "oidc",
  identity_binding: "console",
};

/** A real Window to stand for the sign-in window: a message's source must be one. */
function fakeWindow(): Window {
  const frame = document.createElement("iframe");
  document.body.appendChild(frame);
  const w = frame.contentWindow as Window;
  w.close = vi.fn();
  return w;
}

function renderCard(status: SetupStatus, onChanged = vi.fn(), onRefresh: () => void | Promise<unknown> = () => {}) {
  return render(
    <WithDoor status={status} path="/account" operator={false} onRefresh={onRefresh}>
      <ModelConnectionsCard status={status} onChanged={onChanged} />
    </WithDoor>,
  );
}

beforeEach(() => {
  deleteMock.mockReset().mockResolvedValue(undefined);
  vi.mocked(toast.success).mockReset();
  vi.mocked(toast.error).mockReset();
});
afterEach(() => vi.restoreAllMocks());

it("a signed-in row that is not bound says so, with what it means, and offers Sign in again and Disconnect", async () => {
  const s = providerStatus([{ provider: { ...SSO, identity_binding: "none" }, state: "live", addedAt: aheadByHours(-30) }]);
  renderCard(s);
  await expandCard(CONNECTIONS.TITLE);
  const card = screen.getByTestId("model-connections-card");
  expect(within(card).getByText(T.NOT_BOUND)).toBeInTheDocument();
  expect(within(card).getByText(T.NOT_BOUND_EXPLAIN)).toBeInTheDocument();
  expect(within(card).getByRole("button", { name: T.SIGN_IN_AGAIN })).toBeInTheDocument();
  expect(within(card).getByRole("button", { name: T.DISCONNECT })).toBeInTheDocument();
});

it("Disconnect asks first, and Cancel deletes nothing", async () => {
  const s = providerStatus([{ provider: SSO, state: "live" }]);
  renderCard(s);
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.DISCONNECT }));
  const dialog = await screen.findByRole("alertdialog");
  expect(within(dialog).getByText(T.DISCONNECT_TITLE("Corp SSO"))).toBeInTheDocument();
  expect(within(dialog).getByText(T.DISCONNECT_BODY("Corp SSO"))).toBeInTheDocument();
  expect(deleteMock).not.toHaveBeenCalled();
  await userEvent.click(within(dialog).getByRole("button", { name: T.CANCEL }));
  expect(deleteMock).not.toHaveBeenCalled();
});

it("confirming deletes this provider's credential, says so, and re-reads", async () => {
  const s = providerStatus([{ provider: SSO, state: "live" }]);
  const changed = vi.fn();
  renderCard(s, changed);
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.DISCONNECT }));
  const dialog = await screen.findByRole("alertdialog");
  await userEvent.click(within(dialog).getByRole("button", { name: T.DISCONNECT }));
  await waitFor(() => expect(deleteMock).toHaveBeenCalledWith("corp-sso"));
  await waitFor(() => expect(changed).toHaveBeenCalledOnce());
  expect(toast.success).toHaveBeenCalledWith(T.DISCONNECT_TOAST("Corp SSO"));
});

it("a refused Disconnect says why and leaves the row alone", async () => {
  deleteMock.mockRejectedValue(new Error("the secret store is unavailable"));
  const s = providerStatus([{ provider: SSO, state: "live" }]);
  const changed = vi.fn();
  renderCard(s, changed);
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.DISCONNECT }));
  await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DISCONNECT }));
  await waitFor(() => expect(toast.error).toHaveBeenCalled());
  expect(changed).not.toHaveBeenCalled();
});

it("Sign in opens the door; Continue opens the authority's sign-in window at the API path", async () => {
  const popup = { closed: false, close: vi.fn() } as unknown as Window;
  const open = vi.spyOn(window, "open").mockReturnValue(popup);
  const s = providerStatus([{ provider: SSO, state: "not_configured" }]);
  renderCard(s);
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(T.DOOR_TITLE("Corp SSO"))).toBeInTheDocument();
  expect(within(dialog).getByText(/^For /)).toBeInTheDocument();
  expect(within(dialog).getByText(T.INTRO)).toBeInTheDocument();
  expect(within(dialog).getByText(CONNECTIONS.SENT_TO("llm.contoso.example"))).toBeInTheDocument();
  await userEvent.click(within(dialog).getByRole("button", { name: T.CONTINUE }));
  expect(open).toHaveBeenCalledWith("/api/v1/model-providers/uid-7f3a/oidc/sign-in", expect.any(String), expect.any(String));
  expect(await within(dialog).findByText(T.WAITING)).toBeInTheDocument();
});

it("a blocked window gives a link to open it", async () => {
  vi.spyOn(window, "open").mockReturnValue(null);
  renderCard(providerStatus([{ provider: SSO, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  expect(screen.getByText(T.BLOCKED)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: T.BLOCKED_LINK("Corp SSO") })).toHaveAttribute(
    "href",
    "/api/v1/model-providers/uid-7f3a/oidc/sign-in",
  );
});

it("the window's failure word is shown as its own sentence, with one retry", async () => {
  const win = fakeWindow();
  vi.spyOn(window, "open").mockReturnValue(win);
  renderCard(providerStatus([{ provider: SSO, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.WAITING);
  act(() => {
    window.dispatchEvent(
      new MessageEvent("message", { origin: window.location.origin, source: win, data: { type: OIDC_RETURN.MESSAGE, ok: false, reason: "identity_binding" } }),
    );
  });
  const alert = await screen.findByRole("alert");
  expect(within(alert).getByText(T.FAIL_TITLE("Corp SSO"))).toBeInTheDocument();
  expect(within(alert).getByText(T.FAIL.identity_binding("Corp SSO"))).toBeInTheDocument();
  expect(screen.getByRole("button", { name: T.RETRY })).toBeInTheDocument();
});

it("a message from another origin is ignored", async () => {
  vi.spyOn(window, "open").mockReturnValue({ closed: false, close: vi.fn() } as unknown as Window);
  renderCard(providerStatus([{ provider: SSO, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.WAITING);
  act(() => {
    window.dispatchEvent(
      new MessageEvent("message", { origin: "https://evil.example", data: { type: OIDC_RETURN.MESSAGE, ok: false, reason: "x" } }),
    );
  });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.getByText(T.WAITING)).toBeInTheDocument();
});

async function openDoor() {
  const wins: Window[] = [];
  const names: string[] = [];
  vi.spyOn(window, "open").mockImplementation((_u, name) => {
    const w = fakeWindow();
    wins.push(w);
    names.push(String(name));
    return w;
  });
  renderCard(providerStatus([{ provider: SSO, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.WAITING);
  return { wins, names };
}
async function openAgain(status: SetupStatus) {
  const wins: Window[] = [];
  vi.spyOn(window, "open").mockImplementation(() => {
    const w = fakeWindow();
    wins.push(w);
    return w;
  });
  renderCard(status);
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN_AGAIN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.WAITING);
  return { wins };
}
const word = (source: Window | null, extra: Record<string, unknown> = {}) =>
  act(() => {
    window.dispatchEvent(
      new MessageEvent("message", {
        origin: window.location.origin,
        source,
        data: { type: OIDC_RETURN.MESSAGE, ok: false, reason: "identity_binding", ...extra },
      }),
    );
  });

it("each attempt names its window with its own nonce, and only that window's word counts", async () => {
  const { wins, names } = await openDoor();
  expect(names[0]).toMatch(/^wardyn-provider-oidc-[0-9a-f]{16}$/);
  // Another window of this origin, and a message with no window at all: ignored.
  word(fakeWindow());
  word(null);
  expect(screen.queryByRole("alert")).toBeNull();
  // The right window echoing another attempt's name: ignored. Echoing its own, or none (a cleared name): heard.
  word(wins[0], { attempt: "wardyn-provider-oidc-0000000000000000" });
  expect(screen.queryByRole("alert")).toBeNull();
  word(wins[0], { attempt: names[0] });
  expect(await screen.findByRole("alert")).toBeInTheDocument();
});

it("a window cleared of its name still speaks for its own attempt", async () => {
  const { wins } = await openDoor();
  word(wins[0]);
  expect(await screen.findByRole("alert")).toBeInTheDocument();
});

it("a retry closes the earlier window and a late word from it is ignored", async () => {
  const { wins, names } = await openDoor();
  word(wins[0]);
  await userEvent.click(await screen.findByRole("button", { name: T.RETRY }));
  await screen.findByText(T.WAITING);
  expect(wins[0].close).toHaveBeenCalled();
  expect(names[1]).not.toBe(names[0]);
  word(wins[0], { attempt: names[0] });
  expect(screen.queryByRole("alert")).toBeNull();
  word(wins[1], { attempt: names[1] });
  expect(await screen.findByRole("alert")).toBeInTheDocument();
});

it("the sign-in URL carries the row's uid, never the admin's slug", async () => {
  const open = vi.spyOn(window, "open").mockReturnValue(fakeWindow());
  renderCard(providerStatus([{ provider: SSO, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  const url = String(open.mock.calls[0][0]);
  expect(url).toContain(`/model-providers/${SSO.uid}/oidc/sign-in`);
  expect(url).not.toContain(`/${SSO.id}/`);
});

it("a provider with no uid in the status offers no Continue", async () => {
  const { uid: _uid, ...noUid } = SSO;
  renderCard(providerStatus([{ provider: noUid, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(T.NOT_READY)).toBeInTheDocument();
  expect(within(dialog).queryByRole("button", { name: T.CONTINUE })).toBeNull();
});

// The credential's first-stored time, which a re-sign-in keeps.
const FIXED_ADDED_AT = aheadByHours(-48);
const live = (signedInAt: string) =>
  providerStatus([{ provider: SSO, state: "live" }], {
    provider_access: [{ provider: "corp-sso", state: "live", added_at: FIXED_ADDED_AT, signed_in_at: signedInAt }],
  });

it("Sign in again ends when this attempt's own window says it worked and a re-read shows it live, with added_at unchanged", async () => {
  const { wins } = await openAgain(live(aheadByHours(-30)));
  word(wins[0], { ok: true, reason: "" });
  await waitFor(() => expect(toast.success).toHaveBeenCalledWith(T.DONE_TOAST("Corp SSO")), { timeout: 4000 });
  await waitFor(() => expect(screen.queryByText(T.WAITING)).toBeNull());
  expect(screen.queryByRole("alert")).toBeNull();
});

it("without a word (the blocked-link path) Sign in again ends when signed_in_at moves, with added_at unchanged", async () => {
  const before = live(aheadByHours(-30));
  const after = live(aheadByHours(-1));
  const view = (s: SetupStatus) => (
    <WithDoor status={s} path="/account" operator={false}>
      <ModelConnectionsCard status={s} onChanged={vi.fn()} />
    </WithDoor>
  );
  vi.spyOn(window, "open").mockReturnValue(null);
  const { rerender } = render(view(before));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN_AGAIN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.BLOCKED);
  // The sign-in it started from does not end the wait: only a newer one does.
  rerender(view(before));
  expect(toast.success).not.toHaveBeenCalled();
  rerender(view(after));
  await waitFor(() => expect(toast.success).toHaveBeenCalledWith(T.DONE_TOAST("Corp SSO")), { timeout: 4000 });
  // The row after it: signed in, with Sign in again and Disconnect beside it.
  const card = screen.getByTestId("model-connections-card");
  expect(within(card).getByText(CONNECTIONS.SIGNED_IN)).toBeInTheDocument();
  expect(within(card).getByRole("button", { name: T.SIGN_IN_AGAIN })).toBeInTheDocument();
  expect(within(card).getByRole("button", { name: T.DISCONNECT })).toBeInTheDocument();
});

it("a window closed with no sign-in ends the wait as too long or closed", async () => {
  const popup = { closed: false, close: vi.fn() };
  vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
  renderCard(providerStatus([{ provider: SSO, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.WAITING);
  popup.closed = true;
  expect(await screen.findByText(T.FAIL.timeout(), undefined, { timeout: 9000 })).toBeInTheDocument();
}, 15000);
