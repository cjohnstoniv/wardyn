/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure Foundry form (011-foundry-m7 frames F3 to F7): what a save sends on each route, the agent each route
// can serve, the Fast deployment, a refused endpoint, and the sign-in prerequisite per route.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS, PROVIDER_EDITOR as E, PROVIDERS } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelProviderEditor } from "./model-provider-editor";
import { AZURE_RESOURCE } from "./model-provider-azure";
import { azureRefusedField, incompatibleReason, refusedField } from "./model-provider-draft";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../../../lib/use-unsaved-guard", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/use-unsaved-guard")>("../../../lib/use-unsaved-guard");
  return { ...actual, releaseUnloadGuard: vi.fn() };
});
const putMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));

const HARNESSES = [
  { id: "claude-code", display: "Claude Code" },
  { id: "codex-cli", display: "Codex CLI" },
];
const LIST: ModelProvidersList = {
  providers: { providers: [] },
  etag: '"e1"',
  connected: {},
  kindsAvailable: ["anthropic_api_key", "azure_foundry"],
  entraLoginConfigured: true,
};

async function openForm() {
  const user = userEvent.setup();
  render(
    <OperatorProvider operator principal="alice">
      <ModelProviderEditor list={LIST} editing={null} harnesses={HARNESSES} onClose={vi.fn()} onSaved={vi.fn()} />
    </OperatorProvider>,
  );
  await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.azure_foundry }));
  return user;
}
const sent = () => (putMock.mock.calls[0][0] as { providers: ModelProvider[] }).providers[0];

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
});

describe("the Azure Foundry form", () => {
  it("saves on /openai/v1 with azure {endpoint, route}, the deployment on Codex CLI, and no base_url", async () => {
    const user = await openForm();
    expect(screen.getByRole("heading", { name: MODEL_PROVIDERS.ADD_CTA })).toBeInTheDocument();
    expect(screen.getByText(E.PROVIDES_ENTRA)).toBeInTheDocument();
    expect(screen.queryByLabelText(E.BASE_URL)).toBeNull();
    setField(screen.getByLabelText(E.ENDPOINT), "https://my-resource.openai.azure.com");
    setField(screen.getByLabelText(E.DEPLOYMENT), "team-prod");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const p = sent();
    expect(p.kind).toBe("azure_foundry");
    expect(p.azure).toEqual({ endpoint: "https://my-resource.openai.azure.com", route: "openai_v1" });
    expect(p.harnesses).toEqual([{ harness: "codex-cli", model: "team-prod" }]);
    for (const absent of ["base_url", "auth", "bedrock", "gateway_identity", "oidc", "local"] as const) expect(p[absent]).toBeUndefined();
  });

  it("saves on /anthropic with the Messages harness, its deployment and its Fast deployment", async () => {
    const user = await openForm();
    await user.click(screen.getByRole("radio", { name: "/anthropic" }));
    setField(screen.getByLabelText(E.ENDPOINT), "https://my-resource.openai.azure.com");
    setField(screen.getByLabelText(E.DEPLOYMENT), "team-prod");
    setField(screen.getByLabelText(E.FAST_DEPLOYMENT), "team-fast");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const p = sent();
    expect(p.azure).toEqual({ endpoint: "https://my-resource.openai.azure.com", route: "anthropic" });
    expect(p.harnesses).toEqual([{ harness: "claude-code", model: "team-prod", fast_model: "team-fast" }]);
  });

  it("disables the agent a route cannot serve, with its reason and never hidden", async () => {
    const user = await openForm();
    const claude = screen.getByRole("checkbox", { name: "Claude Code" });
    expect(claude).toBeDisabled();
    expect(claude).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Codex CLI" })).toBeChecked();
    expect(screen.getByText(/Claude Code needs the/).textContent).toBe(E.ROUTE_INCOMPATIBLE("Claude Code", "/anthropic"));
    await user.click(screen.getByRole("radio", { name: "/anthropic" }));
    expect(screen.getByRole("checkbox", { name: "Codex CLI" })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: "Codex CLI" })).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Claude Code" })).toBeChecked();
    expect(screen.getByText(/Codex CLI needs the/).textContent).toBe(E.ROUTE_INCOMPATIBLE("Codex CLI", "/openai/v1"));
  });

  it("shows Fast deployment only for Claude Code on /anthropic, never for Codex CLI", async () => {
    const user = await openForm();
    expect(screen.queryByLabelText(E.FAST_DEPLOYMENT)).toBeNull();
    await user.click(screen.getByRole("radio", { name: "/anthropic" }));
    expect(screen.getByLabelText(E.FAST_DEPLOYMENT)).toBeInTheDocument();
    expect(screen.getAllByLabelText(E.DEPLOYMENT)).toHaveLength(1);
    await user.click(screen.getByRole("radio", { name: "/openai/v1" }));
    expect(screen.queryByLabelText(E.FAST_DEPLOYMENT)).toBeNull();
  });

  it("outlines Endpoint on a 400 naming azure.endpoint, by the server's field and by the sentence", async () => {
    const sentence = 'model_providers: "azure-responses": azure.endpoint host "x.example.com" is not an Azure Foundry host';
    putMock.mockRejectedValueOnce(Object.assign(new HttpError(400, sentence), { field: "azure.endpoint" }));
    const user = await openForm();
    setField(screen.getByLabelText(E.ENDPOINT), "https://x.example.com");
    setField(screen.getByLabelText(E.DEPLOYMENT), "team-prod");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByText(PROVIDERS.SAVE_REFUSED_TITLE_ONE)).toBeInTheDocument();
    expect(screen.getByLabelText(E.ENDPOINT)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("radio", { name: "/openai/v1" })).not.toHaveAttribute("aria-invalid");
    expect(azureRefusedField(sentence, null)).toBe("azure_endpoint");
    expect(azureRefusedField("x", "azure.route")).toBe("azure_route");
    expect(refusedField(sentence)).toBe("azure_endpoint");
    expect(refusedField('model_providers: "a": azure.route "x" must be "anthropic"')).toBe("azure_route");
  });

  it("/openai/v1 names the Cognitive Services resource in the sign-in prerequisite", async () => {
    await openForm();
    const note = screen.getByText(/People sign in through the app registration/);
    expect(note.textContent).toBe(E.PREREQ("https://cognitiveservices.azure.com"));
    expect(within(note).getByText("https://cognitiveservices.azure.com").tagName).toBe("CODE");
  });

  it("/anthropic names the Foundry resource in the sign-in prerequisite", async () => {
    const user = await openForm();
    await user.click(screen.getByRole("radio", { name: "/anthropic" }));
    const note = screen.getByText(/People sign in through the app registration/);
    expect(note.textContent).toBe(E.PREREQ("https://ai.azure.com"));
    expect(AZURE_RESOURCE).toEqual({ openai_v1: "https://cognitiveservices.azure.com", anthropic: "https://ai.azure.com" });
  });

  it("keeps the private-endpoint note collapsed until asked, with the config names in code type", async () => {
    const user = await openForm();
    expect(screen.queryByText(/internal_hosts/)).toBeNull();
    await user.click(screen.getByRole("button", { name: E.PRIVATE_TITLE }));
    const body = screen.getByText(/Add its host to/);
    expect(body.textContent).toBe(E.PRIVATE_BODY);
    expect(within(body).getByText("internal_hosts").tagName).toBe("CODE");
    expect(within(body).getByText("upstream_proxy_no_proxy").tagName).toBe("CODE");
  });
});

describe("incompatibleReason for azure_foundry", () => {
  const cc = { route: "openai_v1" as const, display: "Claude Code" };
  it("names the route a harness needs, for every harness and route", () => {
    expect(incompatibleReason("azure_foundry", "claude-code", cc)).toBe(E.ROUTE_INCOMPATIBLE("Claude Code", "/anthropic"));
    expect(incompatibleReason("azure_foundry", "claude-code", { ...cc, route: "anthropic" })).toBeUndefined();
    expect(incompatibleReason("azure_foundry", "codex-cli", { route: "anthropic", display: "Codex CLI" })).toBe(
      E.ROUTE_INCOMPATIBLE("Codex CLI", "/openai/v1"),
    );
    expect(incompatibleReason("azure_foundry", "codex-cli", { route: "openai_v1", display: "Codex CLI" })).toBeUndefined();
    expect(incompatibleReason("azure_foundry", "codex-cli")).toBeUndefined();
    expect(incompatibleReason("azure_foundry", "some-other", cc)).toBeUndefined();
    expect(incompatibleReason("custom_endpoint", "claude-code", cc)).toBeUndefined();
  });
});
