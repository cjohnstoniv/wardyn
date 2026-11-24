/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The provider editor's Azure Foundry arms (011-foundry-m7 F1, F2, F7 to F9): the kind step offers the row only
// when the server lists it, the disabled rows dim their label and keep their reason, the sign-in confirms, and a
// save that comes back with advisories stays open.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS, PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelProviderEditor } from "./model-provider-editor";

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
const ALL = ["anthropic_subscription", "bedrock_sso", "anthropic_api_key", "openai_api_key", "custom_endpoint", "local_model"] as const;
const AZURE: ModelProvider = {
  id: "azure-responses",
  uid: "u-az",
  name: "Azure (responses)",
  kind: "azure_foundry",
  azure: { endpoint: "https://my-resource.openai.azure.com", route: "openai_v1" },
  harnesses: [{ harness: "codex-cli", model: "team-prod" }],
};

function list(over: Partial<ModelProvidersList> = {}, providers: ModelProvider[] = [], connected: Record<string, number> = {}): ModelProvidersList {
  return { providers: { providers }, etag: '"e1"', connected, ...over };
}
function renderEditor(l: ModelProvidersList, extra: { editing?: ModelProvider | null; subscriptionAvailable?: boolean } = {}) {
  const onClose = vi.fn();
  const onSaved = vi.fn();
  render(
    <OperatorProvider operator principal="alice">
      <ModelProviderEditor list={l} editing={extra.editing ?? null} harnesses={HARNESSES} subscriptionAvailable={extra.subscriptionAvailable} onClose={onClose} onSaved={onSaved} />
    </OperatorProvider>,
  );
  return { onClose, onSaved, user: userEvent.setup() };
}
const kindRows = () => within(screen.getByRole("group", { name: E.KIND_TITLE })).getAllByRole("button").map((b) => b.textContent);

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
});

describe("the kind step and Azure Foundry", () => {
  it("offers no Azure row when the server's list lacks the kind", () => {
    renderEditor(list({ kindsAvailable: [...ALL], entraLoginConfigured: true }));
    expect(kindRows()).not.toContain(MODEL_PROVIDERS.KIND.azure_foundry);
    expect(kindRows()).toHaveLength(6);
  });

  it("an older read with no list offers every other kind and no Azure row", () => {
    renderEditor(list());
    expect(kindRows()).toEqual(ALL.map((k) => MODEL_PROVIDERS.KIND[k]));
  });

  it("lists the kind without Entra sign-in as a disabled row with ENTRA_MISSING", () => {
    renderEditor(list({ kindsAvailable: [...ALL, "azure_foundry"], entraLoginConfigured: false }));
    const row = screen.getByText(E.ENTRA_MISSING).closest("button")!;
    expect(row).toBeDisabled();
    expect(row).toHaveTextContent(MODEL_PROVIDERS.KIND.azure_foundry);
  });

  it("lists the kind with Entra sign-in as an enabled row, last", async () => {
    const { user } = renderEditor(list({ kindsAvailable: [...ALL, "azure_foundry"], entraLoginConfigured: true }));
    const rows = kindRows();
    expect(rows.at(-1)).toBe(MODEL_PROVIDERS.KIND.azure_foundry);
    const row = screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.azure_foundry });
    expect(row).toBeEnabled();
    await user.click(row);
    expect(screen.getByLabelText(E.ENDPOINT)).toBeInTheDocument();
  });

  it("every disabled kind row dims its label only; its reason keeps full opacity", () => {
    renderEditor(list({ kindsAvailable: [...ALL, "azure_foundry"], entraLoginConfigured: false }), { subscriptionAvailable: false });
    for (const reason of [E.CLAUDE_IMAGE_MISSING, E.ENTRA_MISSING]) {
      const row = screen.getByText(reason).closest("button")!;
      expect(row).toBeDisabled();
      expect(row.className).not.toMatch(/opacity/);
      expect(row.className).toMatch(/hatch-unavailable/);
      expect(screen.getByText(reason).className).not.toMatch(/opacity/);
      expect(row.querySelector("span")).toHaveClass("opacity-60");
    }
    // An enabled row has no dimmed part.
    expect(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.bedrock_sso }).querySelector(".opacity-60")).toBeNull();
  });
});

describe("saving an Azure Foundry provider", () => {
  it("keeps the dialog open on the saved row with the advisory and one Done", async () => {
    const warning = 'model_providers: "azure-responses": azure.endpoint host my-resource.openai.azure.com resolves to a private address';
    putMock.mockImplementationOnce((doc) => Promise.resolve({ providers: doc, etag: '"e2"', model_provider_warnings: [warning] }));
    const { onSaved, user } = renderEditor(list({ kindsAvailable: ["azure_foundry"], entraLoginConfigured: true }));
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.azure_foundry }));
    setField(screen.getByLabelText(E.NAME), "Azure (responses)");
    setField(screen.getByLabelText(E.ENDPOINT), "https://my-resource.openai.azure.com");
    setField(screen.getByLabelText(E.DEPLOYMENT), "team-prod");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByText(E.SAVED_WARNING_TITLE)).toBeInTheDocument();
    expect(screen.getByText(warning)).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: "Azure (responses)" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: E.SAVE })).toBeNull();
    expect(screen.queryByRole("button", { name: E.CANCEL })).toBeNull();
    await user.click(screen.getByRole("button", { name: E.DONE }));
    expect(onSaved).toHaveBeenCalledTimes(1);
  });

  it("a save with no advisory closes as before", async () => {
    const { onSaved, user } = renderEditor(list({ kindsAvailable: ["azure_foundry"], entraLoginConfigured: true }));
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.azure_foundry }));
    setField(screen.getByLabelText(E.ENDPOINT), "https://my-resource.openai.azure.com");
    setField(screen.getByLabelText(E.DEPLOYMENT), "team-prod");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await vi.waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(screen.queryByText(E.SAVED_WARNING_TITLE)).toBeNull();
  });
});

describe("the confirms for a provider people sign in to", () => {
  const edit = (n: number) => renderEditor(list({}, [AZURE], { [AZURE.id]: n }), { editing: AZURE });

  it("changing the endpoint asks with the sign-in wording", async () => {
    const { user } = edit(3);
    setField(screen.getByLabelText(E.ENDPOINT), "https://other-resource.openai.azure.com");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByRole("alertdialog")).toHaveTextContent(E.ADDRESS_BODY_SIGNIN(3));
    expect(putMock).not.toHaveBeenCalled();
  });

  it("one person has the singular wording", async () => {
    const { user } = edit(1);
    setField(screen.getByLabelText(E.ENDPOINT), "https://other-resource.openai.azure.com");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByRole("alertdialog")).toHaveTextContent(E.ADDRESS_BODY_SIGNIN_ONE);
  });

  it("changing only the route also counts as a new address", async () => {
    const { user } = edit(2);
    await user.click(screen.getByRole("radio", { name: "/anthropic" }));
    setField(screen.getByLabelText(E.DEPLOYMENT), "team-messages");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByRole("alertdialog")).toHaveTextContent(E.ADDRESS_BODY_SIGNIN(2));
  });

  it("Remove says everyone's sign-ins are deleted", async () => {
    const { user } = edit(3);
    await user.click(screen.getByRole("button", { name: E.REMOVE }));
    expect(await screen.findByRole("alertdialog")).toHaveTextContent(E.DELETE_BODY_SIGNIN);
  });

  it("the title of a saved row is its name, with the kind beside it", () => {
    edit(0);
    expect(screen.getByRole("heading", { name: "Azure (responses)" })).toBeInTheDocument();
    expect(screen.getByText(MODEL_PROVIDERS.KIND.azure_foundry)).toBeInTheDocument();
  });
});
