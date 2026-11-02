/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run identity on a gateway in the provider editor (gateway-identity boards M1 and M2, with their
// verdicts): the switch and its sentence per kind, the audience, issuer and key address, the re-entry on
// a new address, and the server's refusals in their place.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS as M, PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelProviderEditor } from "./model-provider-editor";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const putMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));

const G = E.GATEWAY;
const HARNESSES = [
  { id: "claude-code", display: "Claude Code" },
  { id: "codex-cli", display: "Codex CLI" },
];
// The token's real iss is a spiffe:// address, never /health's bare trust domain; the key set's path
// carries the deployment's base path.
const ISSUER = "spiffe://acme.example.com";
const JWKS_PATH = "/wardyn/.well-known/wardyn-experimental/gateway-jwks.json";

const CORP_ON: ModelProvider = {
  id: "corp-gateway",
  uid: "u-1",
  name: "Corp gateway",
  kind: "custom_endpoint",
  base_url: "https://gateway.example.com",
  auth: { header: "Authorization", format: "Bearer %s" },
  gateway_identity: { audience: "wardyn-corp-gateway" },
  harnesses: [{ harness: "claude-code", path: "/anthropic" }],
};
const KEYS_ON: ModelProvider = {
  id: "anthropic-keys",
  uid: "u-2",
  name: "Anthropic keys",
  kind: "anthropic_api_key",
  base_url: "https://gateway.example.com",
  gateway_identity: { audience: "wardyn-keys" },
  harnesses: [{ harness: "claude-code" }],
};

const list = (providers: ModelProvider[] = [], issuer: string | null = ISSUER): ModelProvidersList => ({
  providers: { providers },
  etag: '"e1"',
  connected: {},
  gatewayIssuer: issuer ?? undefined,
  gatewayJwksPath: JWKS_PATH,
});
// As the wire carries it: an undefined field is not sent.
const putBody = () => JSON.parse(JSON.stringify(putMock.mock.calls[0][0])) as { providers: ModelProvider[] };
const sw = () => screen.getByRole("switch", { name: G.SWITCH });
const audience = () => screen.getByLabelText(new RegExp(`^${G.AUDIENCE}`));

function renderEditor(editing: ModelProvider | null, l: ModelProvidersList = list(editing ? [editing] : []), operator = true) {
  const user = userEvent.setup();
  render(
    <OperatorProvider operator={operator} principal="alice">
      <ModelProviderEditor list={l} editing={editing} harnesses={HARNESSES} onClose={vi.fn()} onSaved={vi.fn()} />
    </OperatorProvider>,
  );
  return { user };
}

async function addKind(kind: string) {
  const r = renderEditor(null);
  await r.user.click(screen.getByRole("button", { name: kind }));
  return r;
}

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
  vi.mocked(toast.error).mockReset();
});

describe("the switch, off", () => {
  it("is off by default on a key kind with a gateway address, with the sentence beside it", async () => {
    await addKind(M.KIND.anthropic_api_key);
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "https://gateway.example.com");
    expect(sw()).toHaveAttribute("aria-checked", "false");
    expect(sw()).toBeEnabled();
    expect(screen.getByText(G.DISCLOSURE)).toBeInTheDocument();
    expect(G.DISCLOSURE).toBe(
      "When on, each model call to this gateway carries a signed token naming the person, the run and the runner.",
    );
    expect(screen.queryByText(G.CHIP)).not.toBeInTheDocument();
  });

  it("reads the sentence with the switch", async () => {
    await addKind(M.KIND.custom_endpoint);
    const id = sw().getAttribute("aria-describedby");
    expect(id).toBeTruthy();
    expect(document.getElementById(id!)).toHaveTextContent(G.DISCLOSURE);
  });

  it("is disabled with its reason on a key kind that has no gateway address, and says nothing else", async () => {
    await addKind(M.KIND.anthropic_api_key);
    expect(sw()).toBeDisabled();
    expect(screen.getByText(G.NEEDS_ADDRESS("api.anthropic.com"))).toBeInTheDocument();
    expect(screen.queryByText(G.DISCLOSURE)).not.toBeInTheDocument();
  });

  it("names api.openai.com for an OpenAI key", async () => {
    await addKind(M.KIND.openai_api_key);
    expect(screen.getByText(G.NEEDS_ADDRESS("api.openai.com"))).toBeInTheDocument();
  });

  it.each([
    [M.KIND.anthropic_subscription, G.NOT_AVAILABLE.anthropic_subscription],
    [M.KIND.bedrock_sso, G.NOT_AVAILABLE.bedrock_sso],
    [M.KIND.local_model, G.NOT_AVAILABLE.local_model],
  ])("%s: disabled with %s", async (kind, reason) => {
    await addKind(kind);
    expect(sw()).toBeDisabled();
    expect(screen.getByText(reason)).toBeInTheDocument();
    expect(screen.queryByText(G.DISCLOSURE)).not.toBeInTheDocument();
    expect(document.getElementById(sw().getAttribute("aria-describedby")!)).toHaveTextContent(reason);
  });

  it("an admin-only control: another person sees it dimmed with the reason", async () => {
    renderEditor(KEYS_ON, list([KEYS_ON]), false);
    expect(sw()).toBeDisabled();
    expect(screen.getByText("Requires the admin role.")).toBeInTheDocument();
  });

  it("changes nothing a save writes: no gateway_identity key at all", async () => {
    const { user } = await addKind(M.KIND.custom_endpoint);
    setField(screen.getByLabelText(/^Base URL/), "https://gateway.example.com");
    for (const path of screen.getAllByLabelText(E.PATH)) setField(path, "/v1");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0]).not.toHaveProperty("gateway_identity");
    expect(JSON.stringify(putBody())).not.toMatch(/issuer|jwks/);
  });
});

describe("the switch, on", () => {
  it("shows the chip, the audience, the real issuer and the key address under the base path", async () => {
    const { user } = await addKind(M.KIND.custom_endpoint);
    setField(screen.getByLabelText(/^Base URL/), "https://gateway.example.com");
    await user.click(sw());
    expect(screen.getByText(G.CHIP)).toBeInTheDocument();
    expect(audience()).toBeRequired();
    expect(screen.getByText(G.AUDIENCE_HINT)).toBeInTheDocument();
    expect(screen.getByText(ISSUER)).toBeInTheDocument();
    expect(screen.queryByText("wardyn.local")).not.toBeInTheDocument();
    expect(screen.getByText(`${window.location.origin}${JWKS_PATH}`)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: G.COPY_ISSUER })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: G.COPY_KEY_ADDRESS })).toBeInTheDocument();
    expect(screen.getByText(G.MAY_CHANGE)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(G.KEY_ADDRESS_HINT + " " + G.KEY_ADDRESS_OTHER))).toBeInTheDocument();
    // Not yet saved: the key address says when it starts answering.
    expect(screen.getByText(new RegExp(G.KEY_ADDRESS_UNSAVED))).toBeInTheDocument();
    expect(screen.queryByText(G.OFF_NOTE)).not.toBeInTheDocument();
  });

  it("warns on the install's default issuer name", async () => {
    renderEditor(CORP_ON, list([CORP_ON], "spiffe://wardyn.local"));
    expect(screen.getByText(G.ISSUER_DEFAULT)).toBeInTheDocument();
  });

  it("shows no issuer rather than a wrong one when the server sent none", async () => {
    renderEditor(CORP_ON, list([CORP_ON], null));
    expect(screen.queryByText(G.ISSUER)).not.toBeInTheDocument();
    expect(screen.getByText(G.KEY_ADDRESS)).toBeInTheDocument();
  });

  it("says another address may serve the key path only where an address is shown", async () => {
    const l = { ...list([CORP_ON]), gatewayJwksPath: undefined };
    renderEditor(CORP_ON, l);
    expect(screen.queryByText(G.KEY_ADDRESS)).not.toBeInTheDocument();
    expect(screen.queryByText(new RegExp(G.KEY_ADDRESS_OTHER))).not.toBeInTheDocument();
  });

  it("saves the audience trimmed, and a saved row's key address no longer says it is unsaved", async () => {
    const { user } = renderEditor(KEYS_ON);
    expect(screen.queryByText(new RegExp(G.KEY_ADDRESS_UNSAVED))).not.toBeInTheDocument();
    expect(audience()).toHaveValue("wardyn-keys");
    setField(audience(), "  wardyn-other  ");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0].gateway_identity).toEqual({ audience: "wardyn-other" });
    expect(JSON.stringify(putBody())).not.toMatch(/issuer|jwks/);
  });

  it("a plain edit keeps the stored identity", async () => {
    const { user } = renderEditor(CORP_ON);
    setField(screen.getByLabelText("Name"), "Corp gateway 2");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0].gateway_identity).toEqual({ audience: "wardyn-corp-gateway" });
  });

  it("turning it off says what that does to runs, and a save drops the field", async () => {
    const { user } = renderEditor(CORP_ON);
    expect(screen.queryByText(G.OFF_NOTE)).not.toBeInTheDocument();
    await user.click(sw());
    expect(screen.getByText(G.OFF_NOTE)).toBeInTheDocument();
    expect(screen.queryByLabelText(new RegExp(`^${G.AUDIENCE}`))).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0]).not.toHaveProperty("gateway_identity");
  });
});

describe("the address", () => {
  it("a new address clears the audience and asks for it again; the old address brings it back", async () => {
    renderEditor(CORP_ON);
    setField(screen.getByLabelText(/^Base URL/), "https://new-gateway.example.com");
    expect(audience()).toHaveValue("");
    expect(audience()).toBeInvalid();
    expect(screen.getByText(G.ADDRESS_CHANGED)).toBeInTheDocument();
    setField(audience(), "wardyn-new");
    expect(audience()).toHaveValue("wardyn-new");
    setField(screen.getByLabelText(/^Base URL/), "https://gateway.example.com/");
    expect(audience()).toHaveValue("wardyn-corp-gateway");
    expect(screen.queryByText(G.ADDRESS_CHANGED)).not.toBeInTheDocument();
  });

  it("a new address with the audience typed again saves both", async () => {
    const { user } = renderEditor(CORP_ON);
    setField(screen.getByLabelText(/^Base URL/), "https://new-gateway.example.com");
    setField(audience(), "wardyn-new");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0]).toMatchObject({
      base_url: "https://new-gateway.example.com",
      gateway_identity: { audience: "wardyn-new" },
    });
  });

  it("clearing a key kind's address while on says it needs one and outlines the field", async () => {
    renderEditor(KEYS_ON);
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "");
    expect(screen.getByText(G.ADDRESS_CLEARED)).toBeInTheDocument();
    expect(screen.getByLabelText(E.ROUTE_THROUGH)).toBeInvalid();
    expect(sw()).toBeEnabled();
  });
});

describe("a refused save", () => {
  it("a duplicate audience shows the server's sentence in the alert and outlines the audience", async () => {
    const msg =
      'model_providers: "anthropic-keys": gateway_identity.audience "wardyn-keys" is already used by provider "corp-gateway" for a different address; use a different one for each';
    putMock.mockRejectedValue(new HttpError(400, msg));
    const { user } = renderEditor(KEYS_ON);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(msg)).toBeInTheDocument();
    expect(audience()).toBeInvalid();
    expect(screen.getByLabelText(E.ROUTE_THROUGH)).not.toBeInvalid();
  });

  it("a refusal naming base_url and the audience outlines the audience, not Base URL", async () => {
    putMock.mockRejectedValue(
      new HttpError(400, 'model_providers: "corp-gateway": base_url changed, so gateway_identity.audience must be entered again'),
    );
    const { user } = renderEditor(CORP_ON);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await screen.findByRole("alert");
    expect(audience()).toBeInvalid();
    expect(screen.getByLabelText(/^Base URL/)).not.toBeInvalid();
  });

  it("a 503 from the signing key shows the server's words and keeps the editor open", async () => {
    putMock.mockRejectedValue(new HttpError(503, "The signing key is unavailable, so this wasn't saved."));
    const { user } = renderEditor(CORP_ON);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("The signing key is unavailable, so this wasn't saved."));
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });
});
