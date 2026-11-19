/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "How people connect" in the provider editor: the choice and its selected state, the sign-in fields, the
// callback address under a base path, the server's refusals as sent with the named field outlined, and the
// confirmation that names what a change deletes and fires before any request.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS as M, PROVIDER_EDITOR as E, PROVIDERS } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelProviderEditor } from "./model-provider-editor";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const putMock = vi.fn();
vi.mock("../../wardyn/availability-control", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../wardyn/availability-control")>()),
  writeAvailability: vi.fn().mockResolvedValue(undefined),
}));
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: () => Promise.resolve(["corp-sso-client-secret", "other"]) },
}));

const O = E.OIDC;
const HARNESSES = [{ id: "claude-code", display: "Claude Code" }];
const SERVER_ISSUER =
  'invalid model providers: model_providers: "corp-sso": oidc.issuer is required — sign-in is discovered from it and the identity token is verified against it';

const PASTE: ModelProvider = {
  id: "corp-sso",
  uid: "u-1",
  name: "Corp SSO",
  kind: "custom_endpoint",
  base_url: "https://llm.contoso.example",
  auth: { header: "Authorization", format: "Bearer %s" },
  harnesses: [{ harness: "claude-code", path: "/anthropic" }],
};
const SIGN_IN: ModelProvider = {
  ...PASTE,
  oidc: {
    issuer: "https://sso.contoso.example",
    authorize_url: "https://sso.contoso.example/authorize",
    token_url: "https://sso.contoso.example/token",
    client_id: "wardyn-llm-client",
    client_secret_ref: "corp-sso-client-secret",
    scopes: ["llm.invoke"],
  },
};

const list = (p: ModelProvider | null, people = 0): ModelProvidersList => ({
  providers: { providers: p ? [p] : [] },
  etag: '"e1"',
  connected: p ? { [p.id]: people } : {},
});
const putBody = () => JSON.parse(JSON.stringify(putMock.mock.calls[0][0])) as { providers: ModelProvider[] };
const choice = (name: string) => screen.getByRole("radio", { name: new RegExp(`^${name}`) });

function renderEditor(editing: ModelProvider | null, people = 0, operator = true) {
  const user = userEvent.setup();
  render(
    <OperatorProvider operator={operator} principal="alice">
      <ModelProviderEditor list={list(editing, people)} editing={editing} harnesses={HARNESSES} onClose={vi.fn()} onSaved={vi.fn()} />
    </OperatorProvider>,
  );
  return { user };
}

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
});
afterEach(() => {
  delete document.documentElement.dataset.wardynBase;
});

describe("the choice", () => {
  it("opens on Paste a token, with no sign-in fields", async () => {
    renderEditor(PASTE);
    await screen.findByRole("radio", { name: new RegExp(`^${O.PASTE}`) });
    expect(choice(O.PASTE)).toHaveAttribute("aria-checked", "true");
    expect(choice(O.SIGN_IN)).toHaveAttribute("aria-checked", "false");
    expect(screen.queryByLabelText(new RegExp(`^${O.ISSUER}`))).not.toBeInTheDocument();
  });

  it("marks the chosen card with the word Selected, and only that one", async () => {
    const { user } = renderEditor(PASTE);
    expect(within(choice(O.PASTE)).getByText(O.SELECTED)).toBeInTheDocument();
    expect(within(choice(O.SIGN_IN)).queryByText(O.SELECTED)).not.toBeInTheDocument();
    await user.click(choice(O.SIGN_IN));
    expect(within(choice(O.SIGN_IN)).getByText(O.SELECTED)).toBeInTheDocument();
    expect(within(choice(O.PASTE)).queryByText(O.SELECTED)).not.toBeInTheDocument();
    expect(choice(O.SIGN_IN)).toHaveClass("border-primary");
    expect(choice(O.PASTE)).not.toHaveClass("border-primary");
  });

  it("Sign in shows the group, the fields, the hint and the new description", async () => {
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    expect(screen.getByText(O.PROVIDES)).toBeInTheDocument();
    expect(screen.getByText(O.CHOICE_HINT)).toBeInTheDocument();
    expect(screen.getByText(O.GROUP)).toBeInTheDocument();
    for (const label of [O.ISSUER, O.CLIENT_ID, O.CLIENT_SECRET, O.SCOPES]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(screen.getByText(O.SCOPES_HINT)).toBeInTheDocument();
    expect(O.SCOPES_HINT).toBe("openid and offline_access are always added. Add at least one scope your endpoint needs.");
    expect(screen.getByRole("switch", { name: O.BIND })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(O.BIND_ON)).toBeInTheDocument();
    // Auth header and Value format stay in both choices.
    expect(screen.getByLabelText(E.AUTH_HEADER)).toBeInTheDocument();
  });

  it("an admin-only control: another person sees it dimmed with the reason", () => {
    renderEditor(PASTE, 0, false);
    expect(choice(O.PASTE)).toBeDisabled();
    expect(choice(O.SIGN_IN)).toBeDisabled();
    const reason = screen.getByRole("radiogroup", { name: O.CHOICE }).getAttribute("aria-describedby");
    expect(document.getElementById(reason!)).toHaveTextContent("Requires the admin role.");
  });

  it("switching the binding off replaces the hint with the warning", async () => {
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    await user.click(screen.getByRole("switch", { name: O.BIND }));
    expect(screen.getByText(O.BIND_OFF)).toBeInTheDocument();
    expect(screen.queryByText(O.BIND_ON)).not.toBeInTheDocument();
  });

  it("warns, and does not refuse, when the client secret names a secret that is not stored", async () => {
    renderEditor({ ...SIGN_IN, oidc: { ...SIGN_IN.oidc!, client_secret_ref: "gone" } });
    expect(await screen.findByText(O.CLIENT_SECRET_MISSING("gone"))).toBeInTheDocument();
  });

  it("does not warn about a stored secret", async () => {
    renderEditor(SIGN_IN);
    await screen.findByRole("option", { name: "other" });
    expect(screen.queryByText(/is stored yet/)).not.toBeInTheDocument();
  });
});

describe("the callback address", () => {
  it("is this console's origin, the API path and the callback route", async () => {
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    expect(screen.getByText(`${window.location.origin}/api/v1/auth/provider-oidc/callback`)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: O.COPY_CALLBACK })).toBeInTheDocument();
  });

  it("carries the base path of an install served under one", async () => {
    document.documentElement.dataset.wardynBase = "/wardyn";
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    expect(screen.getByText(`${window.location.origin}/wardyn/api/v1/auth/provider-oidc/callback`)).toBeInTheDocument();
  });
});

describe("saving a sign-in provider", () => {
  it("writes the block with the scopes as typed, binding absent while on", async () => {
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    setField(screen.getByLabelText(new RegExp(`^${O.ISSUER}`)), "https://sso.contoso.example");
    setField(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`)), "wardyn-llm-client");
    setField(screen.getByPlaceholderText(O.SCOPES_PLACEHOLDER), "llm.invoke ");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0].oidc).toEqual({
      issuer: "https://sso.contoso.example",
      client_id: "wardyn-llm-client",
      scopes: ["llm.invoke"],
    });
  });

  it("a scope with a comma in it stays one scope, typed or pasted, and reaches the PUT whole", async () => {
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    setField(screen.getByLabelText(new RegExp(`^${O.ISSUER}`)), "https://sso.contoso.example");
    setField(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`)), "c");
    const input = screen.getByPlaceholderText(O.SCOPES_PLACEHOLDER);
    await user.click(input);
    await user.paste("read,write");
    expect(screen.getByRole("button", { name: O.SCOPE_REMOVE("read,write") })).toBeInTheDocument();
    setField(input, "other ");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0].oidc?.scopes).toEqual(["read,write", "other"]);
  });

  it("a stored authorize_url and token_url go back unchanged", async () => {
    const { user } = renderEditor(SIGN_IN);
    setField(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`)), "wardyn-llm-client");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0].oidc).toEqual(SIGN_IN.oidc);
  });

  it("shows the server's sentence raw and outlines the field it names", async () => {
    putMock.mockRejectedValue(new HttpError(400, SERVER_ISSUER, "", "", "", "", undefined, "", "", "oidc.issuer"));
    const { user } = renderEditor(SIGN_IN);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByText(SERVER_ISSUER)).toBeInTheDocument();
    expect(screen.getByLabelText(new RegExp(`^${O.ISSUER}`))).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`))).not.toHaveAttribute("aria-invalid");
  });

  it("outlines the scopes for a scopes refusal", async () => {
    putMock.mockRejectedValue(new HttpError(400, "oidc.scopes must name 1 to 8 scopes", "", "", "", "", undefined, "", "", "oidc.scopes"));
    const { user } = renderEditor(SIGN_IN);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await screen.findByText("oidc.scopes must name 1 to 8 scopes");
    expect(document.querySelector("[data-invalid]")).not.toBeNull();
  });
});

describe("the confirmation", () => {
  it("names the tokens deleted by a switch to Sign in, under the choice and before any request", async () => {
    const { user } = renderEditor(PASTE, 14);
    await user.click(choice(O.SIGN_IN));
    expect(screen.getByRole("status")).toHaveTextContent(O.TO_SIGN_IN(14));
    setField(screen.getByLabelText(new RegExp(`^${O.ISSUER}`)), "https://sso.contoso.example");
    setField(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`)), "c");
    setField(screen.getByPlaceholderText(O.SCOPES_PLACEHOLDER), "llm.invoke ");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(O.CONFIRM_TITLE("Corp SSO"))).toBeInTheDocument();
    expect(within(dialog).getByText(O.TO_SIGN_IN(14))).toBeInTheDocument();
    expect(putMock).not.toHaveBeenCalled();
    await user.click(within(dialog).getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledTimes(1));
  });

  it("uses the singular for one person", async () => {
    const { user } = renderEditor(PASTE, 1);
    await user.click(choice(O.SIGN_IN));
    expect(screen.getByRole("status")).toHaveTextContent(O.TO_SIGN_IN_ONE);
  });

  it("the reverse switch confirms too, naming the sign-ins", async () => {
    const { user } = renderEditor(SIGN_IN, 12);
    await user.click(choice(O.PASTE));
    expect(screen.getByRole("status")).toHaveTextContent(O.TO_PASTE(12));
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(O.TO_PASTE(12))).toBeInTheDocument();
    expect(putMock).not.toHaveBeenCalled();
  });

  it("an edit to the issuer, client, scopes or binding names account matching too", async () => {
    const { user } = renderEditor(SIGN_IN, 12);
    await user.click(screen.getByRole("switch", { name: O.BIND }));
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(O.CONFIRM_BODY(12))).toBeInTheDocument();
    expect(O.CONFIRM_BODY(12)).toContain("issuer, client, scopes or account matching");
    expect(putMock).not.toHaveBeenCalled();
  });

  it("Cancel keeps the draft and writes nothing", async () => {
    const { user } = renderEditor(SIGN_IN, 12);
    setField(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`)), "another-client");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: E.CANCEL }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(putMock).not.toHaveBeenCalled();
    expect(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`))).toHaveValue("another-client");
  });

  it("asks nothing when nobody is connected", async () => {
    const { user } = renderEditor(SIGN_IN, 0);
    setField(screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`)), "another-client");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });

  it("asks nothing for a change that is not in the address: the client secret's name", async () => {
    const { user } = renderEditor(SIGN_IN, 12);
    await screen.findByRole("option", { name: "other" });
    await user.selectOptions(screen.getByLabelText(O.CLIENT_SECRET), "other");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });
});

describe("the list's wording", () => {
  it("has the row line and chip words", () => {
    expect(M.PROVIDES.SIGN_IN).toBe("Each person signs in with their own account");
    expect(M.CHIP_UNBOUND).toBe("Not bound to sign-in");
  });
});

describe("keyboard and form behaviour", () => {
  it("the choice is one tab stop moved by the arrow keys, and the fields follow in reading order", async () => {
    const { user } = renderEditor(PASTE);
    const paste = choice(O.PASTE);
    paste.focus();
    await user.keyboard("{ArrowRight}");
    expect(choice(O.SIGN_IN)).toHaveAttribute("aria-checked", "true");
    expect(choice(O.SIGN_IN)).toHaveFocus();
    // Only the chosen radio is a tab stop.
    expect(choice(O.PASTE)).toHaveAttribute("tabindex", "-1");
    await user.keyboard("{ArrowLeft}");
    expect(choice(O.PASTE)).toHaveAttribute("aria-checked", "true");
    await user.keyboard("{ArrowRight}");
    const order = [
      screen.getByLabelText(new RegExp(`^${O.ISSUER}`)),
      screen.getByLabelText(new RegExp(`^${O.CLIENT_ID}`)),
      screen.getByPlaceholderText(O.SCOPES_PLACEHOLDER),
      screen.getByRole("button", { name: O.COPY_CALLBACK }),
      screen.getByRole("switch", { name: O.BIND }),
    ];
    // Tab through the form: the named controls come up in this order (the client secret's chooser and the scopes'
    // Add button sit between them).
    const seen: Element[] = [];
    for (let i = 0; i < 12; i++) {
      await user.tab();
      if (document.activeElement) seen.push(document.activeElement);
    }
    const at = order.map((el) => seen.indexOf(el));
    expect(at.every((n) => n >= 0)).toBe(true);
    expect(at).toEqual([...at].sort((x, y) => x - y));
  });

  it("Save with the Issuer empty is the browser's own message: the field is required and the form is not novalidate", async () => {
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    const issuer = screen.getByLabelText(new RegExp(`^${O.ISSUER}`)) as HTMLInputElement;
    expect(issuer).toBeRequired();
    expect(issuer.closest("form")).not.toHaveAttribute("novalidate");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(issuer.validity.valueMissing).toBe(true);
    expect(putMock).not.toHaveBeenCalled();
  });

  it("refusals the console cannot send are shown whole under the title, as the server worded them", async () => {
    const sentence = 'model_providers: "claude-keys": oidc: is only for your own endpoint providers';
    putMock.mockRejectedValue(new HttpError(400, sentence, "", "", "", "", undefined, "", "", "oidc"));
    const { user } = renderEditor(SIGN_IN);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(PROVIDERS.SAVE_REFUSED_TITLE_ONE)).toBeInTheDocument();
    expect(within(alert).getByText(sentence)).toBeInTheDocument();
  });

  it("copying the callback URL puts it on the clipboard and the button says Copied", async () => {
    const write = vi.fn().mockResolvedValue(undefined);
    const { user } = renderEditor(PASTE);
    Object.defineProperty(navigator, "clipboard", { value: { writeText: write }, configurable: true });
    await user.click(choice(O.SIGN_IN));
    await user.click(screen.getByRole("button", { name: O.COPY_CALLBACK }));
    expect(write).toHaveBeenCalledWith(`${window.location.origin}/api/v1/auth/provider-oidc/callback`);
    expect(await screen.findByText(E.GATEWAY.COPIED, { selector: "button" })).toBeInTheDocument();
    // The gateway identity switch stays beside it, as it was.
    expect(screen.getByRole("switch", { name: E.GATEWAY.SWITCH })).toBeInTheDocument();
  });
});

describe("on a phone", () => {
  it("the footer stays at the bottom of the dialog's scroll, with Save in it, while the fields scroll", async () => {
    const { user } = renderEditor(PASTE);
    await user.click(choice(O.SIGN_IN));
    const footer = screen.getByTestId("provider-editor-footer");
    expect(footer).toHaveClass("sticky", "bottom-0", "bg-background");
    expect(within(footer).getByRole("button", { name: E.SAVE })).toBeInTheDocument();
    expect(within(footer).getByRole("button", { name: E.CANCEL })).toBeInTheDocument();
    // It is a child of the element that scrolls, and the fields are above it in that same element.
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveClass("overflow-y-auto", "max-h-[calc(100dvh-2rem)]");
    expect(footer.closest("[role=dialog]")).toBe(dialog);
    const issuer = screen.getByLabelText(new RegExp(`^${O.ISSUER}`));
    expect(issuer.compareDocumentPosition(footer) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
