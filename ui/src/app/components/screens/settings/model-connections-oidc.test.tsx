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
import { CONNECTIONS } from "../../wardyn/copy/door";
import { OIDC_RETURN, OIDC_SIGN_IN as T } from "../../wardyn/copy/oidc-signin";
import type { SetupModelProvider, SetupStatus } from "../../../lib/types";

const SSO: SetupModelProvider = {
  id: "corp-sso",
  name: "Corp SSO",
  kind: "custom_endpoint",
  harnesses: ["claude-code"],
  host: "llm.contoso.example",
  sign_in: "oidc",
  identity_binding: "console",
};

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
  const s = providerStatus([{ provider: { ...SSO, identity_binding: "none" }, state: "live", addedAt: "2026-11-02T10:00:00Z" }]);
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
  expect(open).toHaveBeenCalledWith("/api/v1/model-providers/corp-sso/oidc/sign-in", expect.any(String), expect.any(String));
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
    "/api/v1/model-providers/corp-sso/oidc/sign-in",
  );
});

it("the window's failure word is shown as its own sentence, with one retry", async () => {
  vi.spyOn(window, "open").mockReturnValue({ closed: false, close: vi.fn() } as unknown as Window);
  renderCard(providerStatus([{ provider: SSO, state: "not_configured" }]));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.WAITING);
  act(() => {
    window.dispatchEvent(
      new MessageEvent("message", { origin: window.location.origin, data: { type: OIDC_RETURN.MESSAGE, ok: false, reason: "identity_binding" } }),
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

it("closes the door with a toast once the person's own state shows a new sign-in", async () => {
  vi.spyOn(window, "open").mockReturnValue({ closed: false, close: vi.fn() } as unknown as Window);
  const before = providerStatus([{ provider: SSO, state: "live", addedAt: "2026-11-01T10:00:00Z" }]);
  const after = providerStatus([{ provider: SSO, state: "live", addedAt: "2026-11-03T10:00:00Z" }]);
  const view = (s: SetupStatus) => (
    <WithDoor status={s} path="/account" operator={false}>
      <ModelConnectionsCard status={s} onChanged={vi.fn()} />
    </WithDoor>
  );
  const { rerender } = render(view(before));
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: T.SIGN_IN_AGAIN }));
  await userEvent.click(await screen.findByRole("button", { name: T.CONTINUE }));
  await screen.findByText(T.WAITING);
  // The sign-in it started from does not end the wait: only a newer one does.
  rerender(view(after));
  await waitFor(() => expect(toast.success).toHaveBeenCalledWith(T.DONE_TOAST("Corp SSO")), { timeout: 4000 });
  await waitFor(() => expect(screen.queryByText(T.WAITING)).toBeNull());
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
