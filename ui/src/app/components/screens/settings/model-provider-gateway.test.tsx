/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run identity on a gateway in the provider editor (gateway-identity boards M1 and M2, with their
// verdicts): the switch and its sentence per kind, the audience, issuer and key address, the re-entry on
// a new address, and the server's refusals in their place.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS as M, PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AVAILABILITY } from "../../../lib/availability-copy";
import { draftFrom, providerFrom, refusedField } from "./model-provider-draft";
import { ModelProviderEditor } from "./model-provider-editor";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const putMock = vi.fn();
const availabilityMock = vi.fn();
vi.mock("../../wardyn/availability-control", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../wardyn/availability-control")>()),
  writeAvailability: (...a: unknown[]) => availabilityMock(...a),
}));
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));

const G = E.GATEWAY;
const MOVED = (id: string) =>
  `model_providers: "${id}": moved to a new address, so gateway_identity.audience can't stay the same: use a different one, or save with identity off first`;
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
// The same answer from an install that does, or does not, sign AWS requests at its proxy.
const ssoList = (providers: ModelProvider[], signer: boolean | undefined): ModelProvidersList => ({
  ...list(providers),
  gatewayIdentityBedrockSso: signer,
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
  availabilityMock.mockReset().mockResolvedValue(undefined);
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

  it("links the claim table, which the doc has under that heading", async () => {
    await addKind(M.KIND.custom_endpoint);
    expect(screen.getByRole("link", { name: G.CLAIMS_LINK })).toHaveAttribute(
      "href",
      "https://github.com/cjohnstoniv/wardyn/blob/main/docs/operations/ai-gateway.md#the-claims-a-gateway-receives",
    );
    // process.cwd() is the vitest root, ui/.
    const doc = readFileSync(resolve(process.cwd(), "../docs/operations/ai-gateway.md"), "utf8");
    expect(doc).toMatch(/^## The claims a gateway receives$/m);
  });

  it("does not offer the link where the switch is unavailable", async () => {
    await addKind(M.KIND.local_model);
    expect(screen.queryByRole("link", { name: G.CLAIMS_LINK })).not.toBeInTheDocument();
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

  it("a refusal for a moved address with the same audience outlines the audience, not Base URL", async () => {
    putMock.mockRejectedValue(new HttpError(400, MOVED("corp-gateway")));
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

describe("a create whose list write is refused", () => {
  it("keeps the editor open on the saved provider, with identity read as saved", async () => {
    availabilityMock.mockRejectedValue(new Error("grant refused"));
    const { user } = await addKind(M.KIND.custom_endpoint);
    setField(screen.getByLabelText(/^Base URL/), "https://gateway.example.com");
    for (const path of screen.getAllByLabelText(E.PATH)) setField(path, "/v1");
    await user.click(sw());
    setField(audience(), "wardyn-corp-gateway");
    expect(screen.getByText(new RegExp(G.KEY_ADDRESS_UNSAVED))).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByText(AVAILABILITY.CREATE_PARTIAL_TITLE)).toBeInTheDocument();
    // Saved with identity on: the key address no longer waits, and turning it off is a change from saved.
    expect(screen.queryByText(new RegExp(G.KEY_ADDRESS_UNSAVED))).not.toBeInTheDocument();
    setField(screen.getByLabelText(/^Base URL/), "https://new-gateway.example.com");
    expect(audience()).toHaveValue("");
    await user.click(sw());
    expect(screen.getByText(G.OFF_NOTE)).toBeInTheDocument();
  });
});

describe("which field a refusal is about", () => {
  // The provider id is quoted first and may spell any field's name.
  it.each([
    ['model_providers: "audience": gateway_identity needs a base_url, or the token would go to the vendor', "base_url"],
    ['model_providers: "gateway_identity.audience": base_url: must be https:// (got "http://x")', "base_url"],
    ['model_providers: "base_url": gateway_identity: audience is required', "gateway_audience"],
    ['model_providers: "base_url": gateway_identity is not available for anthropic_subscription providers', null],
    ['model_providers: "p": gateway_identity needs a bedrock.base_url, or the token would go to AWS', "base_url"],
    ['model_providers: "p": gateway_identity on a bedrock_sso provider needs WARDYN_AWS_SIGV4_AT_PROXY on, so Wardyn signs each AWS request at its proxy', null],
    ['model_providers: "local.models": local.max_output: must be between 1 and 99', "max_output"],
    ['model_providers: "local.models": gateway_identity is not available for anthropic_subscription providers', null],
    [MOVED("audience"), "gateway_audience"],
    [MOVED("base_url"), "gateway_audience"],
    // The duplicate refusal quotes what the admin typed, which may spell base_url.
    ['model_providers: "q": gateway_identity.audience "base_url" is already used by provider "p" for a different address; use a different one for each', "gateway_audience"],
    ['invalid model providers: model_providers: "q": gateway_identity: audience must be at most 256 bytes, with no control characters', "gateway_audience"],
  ])("%s", (message, field) => {
    expect(refusedField(message)).toBe(field);
  });
});

describe("a Bedrock key row", () => {
  const HOST = "bedrock-runtime.us-east-1.amazonaws.com";
  const BEDROCK_ON: ModelProvider = {
    id: "bedrock-keys",
    uid: "u-3",
    name: "Bedrock keys",
    kind: "bedrock_bearer",
    bedrock: { region: "us-east-1", base_url: "https://gateway.example.com/bedrock" },
    gateway_identity: { audience: "wardyn-bedrock" },
    harnesses: [{ harness: "claude-code", model: "us.anthropic.profile" }],
  };
  const bearer = () => screen.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_BEARER });
  const sso = () => screen.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO });

  it("asks for a gateway address first, naming the regional host, and labels the region for what it is", async () => {
    const { user } = await addKind(M.KIND.bedrock_bearer);
    await user.click(bearer());
    expect(sw()).toBeDisabled();
    expect(screen.getByText(G.NEEDS_ADDRESS(HOST))).toBeInTheDocument();
    expect(screen.getByLabelText(E.REGION_BEARER)).toBeInTheDocument();
    expect(screen.queryByText(E.IDC_GROUP)).not.toBeInTheDocument();
    setField(screen.getByLabelText(E.REGION_BEARER), "eu-west-1");
    expect(screen.getByText(G.NEEDS_ADDRESS("bedrock-runtime.eu-west-1.amazonaws.com"))).toBeInTheDocument();
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "https://gateway.example.com/bedrock");
    expect(sw()).toBeEnabled();
    expect(screen.getByText(G.DISCLOSURE)).toBeInTheDocument();
  });

  it("saves the address under bedrock and the audience beside it, and no base_url of its own", async () => {
    const { user } = await addKind(M.KIND.bedrock_bearer);
    await user.click(bearer());
    setField(screen.getByLabelText(E.REGION_BEARER), "us-east-1");
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "https://gateway.example.com/bedrock");
    for (const model of screen.getAllByLabelText(new RegExp(`^${E.MODEL}`))) setField(model, "us.anthropic.profile");
    await user.click(sw());
    setField(audience(), "  wardyn-bedrock ");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    const saved = putBody().providers[0];
    expect(saved).toMatchObject({
      kind: "bedrock_bearer",
      bedrock: { region: "us-east-1", base_url: "https://gateway.example.com/bedrock" },
      gateway_identity: { audience: "wardyn-bedrock" },
    });
    expect(saved).not.toHaveProperty("base_url");
  });

  it("a new address clears the audience, and clearing it while on says it needs one", async () => {
    renderEditor(BEDROCK_ON);
    expect(audience()).toHaveValue("wardyn-bedrock");
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "https://other.example.com/bedrock");
    expect(audience()).toHaveValue("");
    expect(screen.getByText(G.ADDRESS_CHANGED)).toBeInTheDocument();
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "");
    expect(screen.getByText(G.ADDRESS_CLEARED)).toBeInTheDocument();
    expect(screen.getByLabelText(E.ROUTE_THROUGH)).toBeInvalid();
  });

  it("switching to SSO where the install cannot sign keeps the switch on, still able to turn off, and says why it is unavailable", async () => {
    const { user } = renderEditor(BEDROCK_ON);
    expect(sw()).toHaveAttribute("aria-checked", "true");
    await user.click(sso());
    // Already on, so it can still be turned off; only turning it on is refused here.
    expect(sw()).toBeEnabled();
    expect(sw()).toHaveAttribute("aria-checked", "true");
    expect(document.getElementById(sw().getAttribute("aria-describedby")!)).toHaveTextContent(G.NOT_AVAILABLE.bedrock_sso);
    expect(screen.queryByText(G.CHIP)).not.toBeInTheDocument();
    await user.click(bearer());
    expect(sw()).toBeEnabled();
    expect(audience()).toHaveValue("wardyn-bedrock");
  });

  it("a draft switched to SSO keeps its identity and its gateway address", () => {
    const draft = { ...draftFrom(BEDROCK_ON, HARNESSES), kind: "bedrock_sso" as const };
    const out = providerFrom(draft, BEDROCK_ON, []);
    expect(out.gateway_identity).toEqual({ audience: "wardyn-bedrock" });
    expect(out.bedrock?.base_url).toBe("https://gateway.example.com/bedrock");
  });
});

describe("a Bedrock sign-in row", () => {
  const SSO_OFF: ModelProvider = {
    id: "bedrock-sso",
    uid: "u-4",
    name: "Bedrock sign-in",
    kind: "bedrock_sso",
    bedrock: { region: "us-east-1", sso_start_url: "https://acme.awsapps.com/start" },
    harnesses: [{ harness: "claude-code", model: "us.anthropic.profile" }],
  };
  const SSO_ON: ModelProvider = {
    ...SSO_OFF,
    bedrock: { ...SSO_OFF.bedrock, base_url: "https://gateway.example.com/bedrock" },
    gateway_identity: { audience: "wardyn-sso" },
  };
  const VAR = "WARDYN_AWS_SIGV4_AT_PROXY";
  const UNAVAILABLE = `Needs Wardyn to sign AWS requests at its proxy. Ask your operator to turn on ${VAR}.`;
  const DISCLOSURE =
    "When on, each Bedrock call carries a token naming the person, run and runner, plus an AWS signature. The gateway can resend the signed request while AWS still accepts it, but cannot change it.";
  const HOST = "bedrock-runtime.us-east-1.amazonaws.com";

  it.each([false, undefined])("the switch is unavailable, with the operator setting named, when the install's answer is %s", (signer) => {
    renderEditor(SSO_OFF, ssoList([SSO_OFF], signer));
    expect(G.NOT_AVAILABLE.bedrock_sso).toBe(UNAVAILABLE);
    expect(sw()).toBeDisabled();
    const reason = document.getElementById(sw().getAttribute("aria-describedby")!)!;
    expect(reason).toHaveTextContent(UNAVAILABLE);
    // The variable is shown as code.
    expect(within(reason).getByText(VAR).tagName).toBe("CODE");
    expect(screen.queryByText(G.DISCLOSURE)).not.toBeInTheDocument();
    expect(screen.queryByText(DISCLOSURE)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: G.CLAIMS_LINK })).not.toBeInTheDocument();
    expect(screen.queryByText(G.CHIP)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(E.ROUTE_THROUGH)).not.toBeInTheDocument();
  });

  it("asks for a gateway address first when the install can sign", () => {
    renderEditor(SSO_OFF, ssoList([SSO_OFF], true));
    expect(sw()).toBeDisabled();
    expect(screen.getByText(G.NEEDS_ADDRESS(HOST))).toBeInTheDocument();
    expect(screen.queryByText(DISCLOSURE)).not.toBeInTheDocument();
    expect(screen.queryByText(G.CHIP)).not.toBeInTheDocument();
  });

  it("with an address, turns on with the Bedrock disclosure, the chip and the link, and saves the address and audience", async () => {
    const { user } = renderEditor(SSO_OFF, ssoList([SSO_OFF], true));
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "https://gateway.example.com/bedrock");
    expect(sw()).toBeEnabled();
    expect(G.DISCLOSURE_BEDROCK_SSO).toBe(DISCLOSURE);
    expect(screen.getByText(DISCLOSURE)).toBeInTheDocument();
    expect(screen.queryByText(G.DISCLOSURE)).not.toBeInTheDocument();
    expect(document.getElementById(sw().getAttribute("aria-describedby")!)).toHaveTextContent(DISCLOSURE);
    expect(screen.getByRole("link", { name: G.CLAIMS_LINK })).toBeInTheDocument();
    expect(screen.queryByText(G.CHIP)).not.toBeInTheDocument();
    await user.click(sw());
    expect(screen.getByText(G.CHIP)).toBeInTheDocument();
    setField(audience(), " wardyn-sso ");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0]).toMatchObject({
      kind: "bedrock_sso",
      bedrock: { region: "us-east-1", base_url: "https://gateway.example.com/bedrock", sso_start_url: "https://acme.awsapps.com/start" },
      gateway_identity: { audience: "wardyn-sso" },
    });
  });

  it("shows a stored row's identity on, and clearing the address says it needs one", () => {
    renderEditor(SSO_ON, ssoList([SSO_ON], true));
    expect(sw()).toHaveAttribute("aria-checked", "true");
    expect(audience()).toHaveValue("wardyn-sso");
    setField(screen.getByLabelText(E.ROUTE_THROUGH), "");
    expect(screen.getByText(G.ADDRESS_CLEARED)).toBeInTheDocument();
  });

  it("a stored-on row on an install that cannot sign can be switched off and saved, but not back on", async () => {
    const { user } = renderEditor(SSO_ON, ssoList([SSO_ON], false));
    expect(sw()).toHaveAttribute("aria-checked", "true");
    expect(sw()).toBeEnabled();
    expect(document.getElementById(sw().getAttribute("aria-describedby")!)).toHaveTextContent(UNAVAILABLE);
    await user.click(sw());
    expect(sw()).toHaveAttribute("aria-checked", "false");
    expect(sw()).toBeDisabled();
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0]).not.toHaveProperty("gateway_identity");
  });

  it("another person still sees a stored-on row's switch dimmed when the install cannot sign", () => {
    renderEditor(SSO_ON, ssoList([SSO_ON], false), false);
    expect(sw()).toBeDisabled();
  });

  it("another person sees the switch dimmed, with the admin-only reason", () => {
    renderEditor(SSO_ON, ssoList([SSO_ON], true), false);
    expect(sw()).toBeDisabled();
    expect(screen.getByText(DISCLOSURE)).toBeInTheDocument();
  });
});
