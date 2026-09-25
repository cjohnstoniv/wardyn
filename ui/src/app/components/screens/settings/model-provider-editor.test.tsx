/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The provider editor (#537, #538): one test per state provider-editor.html /
// mp-packet-B.html draws, every visible string asserted against
// lib/model-providers-copy.ts (itself pinned to
// docs/design/model-providers-canon.md by model-providers-copy.test.ts).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { T } from "../../../lib/integrations";
import { MODEL_PROVIDERS, PROVIDER_EDITOR as E, PROVIDERS } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { AGENTS, AGENTS_DRAFT, PROVIDERS as WS_PROVIDERS } from "../../../lib/workspace-providers-copy";
import { ModelProviderEditor, type ModelProviderEditorProps } from "./model-provider-editor";
import { addressChanged, providerIdFrom } from "./model-provider-draft";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const putMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));

const HARNESSES = [
  { id: "claude-code", display: "Claude Code" },
  { id: "codex-cli", display: "Codex CLI" },
];

const CORP: ModelProvider = {
  id: "corp-gateway",
  uid: "u-1",
  name: "Corp gateway",
  kind: "custom_endpoint",
  base_url: "https://gateway.corp.example",
  auth: { header: "Authorization", format: "Bearer %s" },
  harnesses: [
    { harness: "claude-code", path: "/anthropic" },
    { harness: "codex-cli", model: "acme-codex-large", path: "/v1" },
  ],
};
const ANTHROPIC: ModelProvider = {
  id: "anthropic-api-key",
  uid: "u-2",
  name: "Anthropic API key",
  kind: "anthropic_api_key",
  harnesses: [{ harness: "claude-code" }],
};
const BEDROCK: ModelProvider = {
  id: "bedrock-prod",
  uid: "u-3",
  name: "Bedrock (prod)",
  kind: "bedrock_sso",
  bedrock: {
    region: "us-east-1",
    sso_start_url: "https://acme.awsapps.com/start",
    sso_account_id: "111122223333",
    sso_role_name: "WardynBedrockUser",
  },
  harnesses: [{ harness: "claude-code", model: "acme.claude-sonnet" }],
};
const SUBSCRIPTION: ModelProvider = {
  id: "claude-subscription",
  uid: "u-4",
  name: "Claude subscription",
  kind: "anthropic_subscription",
  harnesses: [{ harness: "claude-code" }],
};

function list(providers: ModelProvider[] = [], connected: Record<string, number> = {}): ModelProvidersList {
  return { providers: { providers }, etag: '"e1"', connected };
}

function renderEditor(props: Partial<ModelProviderEditorProps> = {}) {
  const onClose = vi.fn();
  const onSaved = vi.fn();
  const user = userEvent.setup();
  render(
    <ModelProviderEditor
      list={list()}
      editing={null}
      harnesses={HARNESSES}
      onClose={onClose}
      onSaved={onSaved}
      {...props}
    />,
  );
  return { onClose, onSaved, user };
}

const dialog = () => screen.getByRole("dialog");
const alert = () => screen.getByRole("alertdialog");
const box = (name: string) => screen.getByRole("checkbox", { name });
const putBody = () => putMock.mock.calls[0][0] as { providers: ModelProvider[] };

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
  vi.mocked(toast.success).mockReset();
  vi.mocked(toast.error).mockReset();
});

describe("the kind step", () => {
  it("lists all five kinds in order, and Cancel — no Next", async () => {
    const { onClose, user } = renderEditor();
    expect(screen.getByRole("heading", { name: MODEL_PROVIDERS.ADD_CTA })).toBeInTheDocument();
    const group = screen.getByRole("group", { name: E.KIND_TITLE });
    expect(within(group).getAllByRole("button").map((b) => b.textContent)).toEqual([
      MODEL_PROVIDERS.KIND.anthropic_subscription,
      MODEL_PROVIDERS.KIND.bedrock_sso,
      MODEL_PROVIDERS.KIND.anthropic_api_key,
      MODEL_PROVIDERS.KIND.openai_api_key,
      MODEL_PROVIDERS.KIND.custom_endpoint,
    ]);
    await user.click(screen.getByRole("button", { name: E.CANCEL }));
    expect(onClose).toHaveBeenCalled();
  });

  it("Q1/QB-5: Claude subscription is enabled by default (subscriptionAvailable defaults true)", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_subscription }));
    expect(screen.getByText(E.PROVIDES_CLAUDE)).toBeInTheDocument();
  });

  it("QB-5: disabled with its reason, word for word, when the sign-in image doesn't resolve", async () => {
    const { user } = renderEditor({ subscriptionAvailable: false });
    const subscription = screen.getByText(E.CLAUDE_IMAGE_MISSING).closest("button")!;
    expect(subscription).toBeDisabled();
    await user.click(subscription);
    expect(screen.queryByText(E.PROVIDES_CLAUDE)).toBeNull();
    // The other four options stay enabled.
    expect(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.bedrock_sso })).toBeEnabled();
  });
});

describe("E1 — a new key provider", () => {
  it("empty: named after its kind, route-through open, Claude Code ticked, Codex CLI disabled with the catalog's reason", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_api_key }));

    expect(screen.getByText(E.PROVIDES_KEY)).toBeInTheDocument();
    expect(screen.getByLabelText(E.NAME)).toHaveValue(MODEL_PROVIDERS.KIND.anthropic_api_key);
    expect(screen.getByText(E.NAME_HINT)).toBeInTheDocument();
    const gateway = screen.getByLabelText(E.ROUTE_THROUGH);
    expect(gateway).toHaveValue("");
    expect(gateway).not.toBeRequired();
    expect(document.getElementById("mp-base-url-hint")?.textContent).toBe(E.ROUTE_THROUGH_HINT("api.anthropic.com"));
    expect(screen.getByText(E.USE_WITH)).toBeInTheDocument();
    expect(box("Claude Code")).toBeChecked();
    expect(box("Codex CLI")).not.toBeChecked();
    expect(box("Codex CLI")).toBeDisabled();
    expect(screen.getByText(T.X_KEY_CODEX)).toBeInTheDocument();
    // Collapsed: no required field is empty.
    expect(screen.queryByLabelText(E.MODEL)).toBeNull();
    // Configuration only — no key field anywhere.
    expect(within(dialog()).queryByLabelText(/key$/i)).toBeNull();
    expect(screen.getByRole("button", { name: E.SAVE })).toBeEnabled();
    expect(screen.queryByRole("button", { name: E.REMOVE })).toBeNull();
  });

  it("filled: saves the whole document with If-Match, the id made from the name, then closes with the toast", async () => {
    const { onSaved, user } = renderEditor({ list: list([CORP]) });
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_api_key }));
    await user.clear(screen.getByLabelText(E.NAME));
    await user.type(screen.getByLabelText(E.NAME), "Anthropic via LLM gateway");
    await user.type(screen.getByLabelText(E.ROUTE_THROUGH), "https://llm.corp.example");
    await user.click(screen.getByRole("button", { name: "Claude Code" }));
    expect(screen.getByText(E.MODEL_HINT)).toBeInTheDocument();
    await user.type(screen.getByLabelText(E.MODEL), "acme-claude-large");
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putMock).toHaveBeenCalledWith(
      {
        providers: [
          CORP,
          {
            id: "anthropic-via-llm-gateway",
            name: "Anthropic via LLM gateway",
            kind: "anthropic_api_key",
            base_url: "https://llm.corp.example",
            auth: undefined,
            harnesses: [{ harness: "claude-code", model: "acme-claude-large", path: undefined }],
          },
        ],
      },
      '"e1"',
    );
    expect(toast.success).toHaveBeenCalledWith(E.SAVED_TOAST);
  });
});

describe("E2 — a new endpoint on both agents", () => {
  it("empty: base URL required, the default header scheme, both agents ticked and open on their required Path", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.custom_endpoint }));

    expect(screen.getByText(E.PROVIDES_TOKEN)).toBeInTheDocument();
    expect(screen.getByLabelText(E.NAME)).toHaveValue(MODEL_PROVIDERS.KIND.custom_endpoint);
    expect(screen.getByLabelText(E.BASE_URL)).toBeRequired();
    expect(screen.getByText(E.BASE_URL_HINT)).toBeInTheDocument();
    expect(screen.getByLabelText(E.AUTH_HEADER)).toHaveValue("Authorization");
    expect(screen.getByLabelText(E.VALUE_FORMAT)).toHaveValue("Bearer {token}");
    expect(box("Claude Code")).toBeChecked();
    expect(box("Codex CLI")).toBeChecked();
    const paths = screen.getAllByLabelText(E.PATH);
    expect(paths).toHaveLength(2);
    paths.forEach((p) => expect(p).toBeRequired());
    expect(screen.getAllByText(E.MODEL_HINT)).toHaveLength(2);
    expect(screen.getByText(E.PATH_HINT_CLAUDE)).toBeInTheDocument();
    expect(screen.getByText(E.PATH_HINT_CODEX)).toBeInTheDocument();
    expect(within(dialog()).queryByLabelText(/token/i)).toBeNull();
  });

  it("an agent whose required Path is empty can't be collapsed; once filled, it can", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.custom_endpoint }));
    const chevron = screen.getByRole("button", { name: "Claude Code" });
    expect(chevron).toBeDisabled();
    await user.click(chevron);
    expect(screen.getAllByLabelText(E.PATH)).toHaveLength(2);

    await user.type(screen.getAllByLabelText(E.PATH)[0], "/anthropic");
    expect(chevron).toBeEnabled();
    await user.click(chevron);
    expect(screen.getAllByLabelText(E.PATH)).toHaveLength(1);
  });

  it("filled: saves corp-gateway with its paths, and the format back in its wire form", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.custom_endpoint }));
    await user.clear(screen.getByLabelText(E.NAME));
    await user.type(screen.getByLabelText(E.NAME), "Corp gateway");
    await user.type(screen.getByLabelText(E.BASE_URL), "https://gateway.corp.example");
    const [claudePath, codexPath] = screen.getAllByLabelText(E.PATH);
    await user.type(claudePath, "/anthropic");
    await user.type(codexPath, "/v1");
    await user.type(screen.getAllByLabelText(E.MODEL)[1], "acme-codex-large");
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    await waitFor(() => expect(putMock).toHaveBeenCalled());
    const [p] = putBody().providers;
    expect(p).toMatchObject({
      id: "corp-gateway",
      name: "Corp gateway",
      kind: "custom_endpoint",
      base_url: "https://gateway.corp.example",
      auth: { header: "Authorization", format: "Bearer %s" },
      harnesses: [
        { harness: "claude-code", path: "/anthropic" },
        { harness: "codex-cli", model: "acme-codex-large", path: "/v1" },
      ],
    });
  });
});

describe("E5 — an agent that can't be ticked", () => {
  it("OpenAI: Claude Code disabled with the catalog's reason word for word, Codex CLI ticked", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.openai_api_key }));
    expect(screen.getByText(E.PROVIDES_KEY)).toBeInTheDocument();
    expect(document.getElementById("mp-base-url-hint")?.textContent).toBe(E.ROUTE_THROUGH_HINT("api.openai.com"));
    expect(box("Claude Code")).toBeDisabled();
    expect(box("Claude Code")).not.toBeChecked();
    expect(screen.getByText(T.X_OPENAI_CLAUDE)).toBeInTheDocument();
    expect(box("Codex CLI")).toBeChecked();
  });
});

describe("E3 — a new Bedrock provider (#538)", () => {
  it("empty: SSO by default, the IDC block, Model required and open, Codex CLI disabled with Bedrock's reason", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.bedrock_sso }));

    expect(screen.getByText(E.PROVIDES_SSO)).toBeInTheDocument();
    expect(screen.getByLabelText(E.NAME)).toHaveValue(MODEL_PROVIDERS.KIND.bedrock_sso);
    // No key/token/route-through field anywhere — configuration only.
    expect(screen.queryByLabelText(E.ROUTE_THROUGH)).toBeNull();
    expect(screen.queryByLabelText(E.BASE_URL)).toBeNull();

    const signIn = screen.getByRole("radiogroup", { name: E.HOW_PEOPLE_SIGN_IN });
    expect(within(signIn).getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO })).toHaveAttribute("aria-checked", "true");
    expect(within(signIn).getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_BEARER })).toHaveAttribute("aria-checked", "false");

    expect(screen.getByText(E.IDC_GROUP)).toBeInTheDocument();
    expect(screen.getByLabelText(E.REGION)).toHaveValue("");
    expect(screen.getByLabelText(AGENTS.FIELD_SSO_START_URL)).toHaveValue("");
    expect(screen.getByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID)).toHaveValue("");
    expect(screen.getByLabelText(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME)).toHaveValue("");
    expect(screen.getByText(E.SSO_SETUP_HINT)).toBeInTheDocument();

    expect(box("Claude Code")).toBeChecked();
    expect(box("Codex CLI")).not.toBeChecked();
    expect(box("Codex CLI")).toBeDisabled();
    expect(screen.getByText(T.X_BEDROCK_CODEX)).toBeInTheDocument();
    // Collapsed unless a required field is empty (QB-2) — Model is required
    // for Bedrock, so it stays open here.
    expect(screen.getByLabelText(E.MODEL)).toBeRequired();
    expect(screen.getByText(E.MODEL_HINT_BEDROCK)).toBeInTheDocument();
  });

  it("How people sign in flips to Bearer: the IDC-only fields drop, Region stays, the provides line becomes a key line", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.bedrock_sso }));
    await user.click(screen.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_BEARER }));

    expect(screen.getByText(E.PROVIDES_KEY)).toBeInTheDocument();
    expect(screen.getByLabelText(E.REGION)).toBeInTheDocument();
    expect(screen.queryByLabelText(AGENTS.FIELD_SSO_START_URL)).toBeNull();
    expect(screen.queryByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID)).toBeNull();
    expect(screen.queryByLabelText(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME)).toBeNull();
  });

  it("filled: saves the SSO fields under bedrock, and the ticked agent's model", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.bedrock_sso }));
    await user.clear(screen.getByLabelText(E.NAME));
    await user.type(screen.getByLabelText(E.NAME), "Bedrock (prod)");
    await user.type(screen.getByLabelText(E.REGION), "us-east-1");
    await user.type(screen.getByLabelText(AGENTS.FIELD_SSO_START_URL), "https://acme.awsapps.com/start");
    await user.type(screen.getByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID), "111122223333");
    await user.type(screen.getByLabelText(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME), "WardynBedrockUser");
    await user.type(screen.getByLabelText(E.MODEL), "acme.claude-sonnet");
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    await waitFor(() => expect(putMock).toHaveBeenCalled());
    const [p] = putBody().providers;
    expect(p).toMatchObject({
      id: "bedrock-prod",
      name: "Bedrock (prod)",
      kind: "bedrock_sso",
      base_url: undefined,
      auth: undefined,
      bedrock: {
        region: "us-east-1",
        sso_start_url: "https://acme.awsapps.com/start",
        sso_account_id: "111122223333",
        sso_role_name: "WardynBedrockUser",
      },
      harnesses: [{ harness: "claude-code", model: "acme.claude-sonnet", path: undefined }],
    });
  });

  it("editing a stored Bearer provider: Bearer is selected, no SSO-only fields, the id and kind stay put", async () => {
    const BEARER: ModelProvider = { ...BEDROCK, id: "bedrock-bearer", kind: "bedrock_bearer" };
    renderEditor({ editing: BEARER, list: list([BEARER]) });
    expect(within(dialog()).getByText(MODEL_PROVIDERS.KIND.bedrock_bearer)).toBeInTheDocument();
    expect(screen.getByText(E.PROVIDES_KEY)).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_BEARER })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByLabelText(E.REGION)).toHaveValue("us-east-1");
    expect(screen.queryByLabelText(AGENTS.FIELD_SSO_START_URL)).toBeNull();
  });
});

describe("E4 — a new Claude subscription provider (#538)", () => {
  it("empty: named after its kind, no key/token/route-through field, Claude Code ticked, Codex CLI disabled", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_subscription }));

    expect(screen.getByText(E.PROVIDES_CLAUDE)).toBeInTheDocument();
    expect(screen.getByLabelText(E.NAME)).toHaveValue(MODEL_PROVIDERS.KIND.anthropic_subscription);
    expect(screen.queryByLabelText(E.ROUTE_THROUGH)).toBeNull();
    expect(screen.queryByLabelText(E.BASE_URL)).toBeNull();
    expect(screen.queryByText(E.HOW_PEOPLE_SIGN_IN)).toBeNull();
    expect(screen.queryByText(E.IDC_GROUP)).toBeNull();

    expect(box("Claude Code")).toBeChecked();
    expect(box("Codex CLI")).not.toBeChecked();
    expect(box("Codex CLI")).toBeDisabled();
    expect(screen.getByText(T.X_SUB_CODEX)).toBeInTheDocument();
    // Model is optional for a subscription (Bedrock's own rule) — collapsed.
    expect(screen.queryByLabelText(E.MODEL)).toBeNull();
  });

  it("filled: saves the kind with no bedrock or auth block", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_subscription }));
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    await waitFor(() => expect(putMock).toHaveBeenCalled());
    const [p] = putBody().providers;
    expect(p).toMatchObject({
      id: "claude-subscription",
      name: "Claude subscription",
      kind: "anthropic_subscription",
      base_url: undefined,
      auth: undefined,
      bedrock: undefined,
      harnesses: [{ harness: "claude-code", model: undefined, path: undefined }],
    });
  });

  it("editing: the kind chip reads Claude subscription", async () => {
    renderEditor({ editing: SUBSCRIPTION, list: list([SUBSCRIPTION]) });
    // The name equals the kind label for this fixture, so both the title and
    // the chip render it — two occurrences, not a stray duplicate.
    expect(within(dialog()).getAllByText(MODEL_PROVIDERS.KIND.anthropic_subscription)).toHaveLength(2);
    expect(screen.getByText(E.PROVIDES_CLAUDE)).toBeInTheDocument();
  });
});

describe("saving and saved", () => {
  it("Save disables at once, shows its spinner later, and keeps its label", async () => {
    let resolve!: (v: unknown) => void;
    putMock.mockImplementation(() => new Promise((r) => (resolve = r)));
    const { onSaved, user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_api_key }));
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    const save = screen.getByRole("button", { name: E.SAVE });
    expect(save).toBeDisabled();
    expect(save.querySelector(".animate-spin")).toBeNull();
    await waitFor(() => expect(save.querySelector(".animate-spin")).not.toBeNull());
    expect(save).toHaveTextContent(E.SAVE);
    expect(onSaved).not.toHaveBeenCalled();

    resolve({ providers: {}, etag: '"e2"' });
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(toast.success).toHaveBeenCalledWith(E.SAVED_TOAST);
  });
});

describe("E6 — save refused", () => {
  // The three real 400s: handlePutModelProviders writes
  // "invalid model providers: " + the validateModelProviders error.
  const BODIES = [
    'invalid model providers: model_providers: "corp-gateway": base_url: must be https:// (got "http://gateway.corp.example")',
    'invalid model providers: model_providers: "corp-gateway": codex-cli: path "v1" must start with a single / and carry no query, fragment, backslash, whitespace or .. segment',
    'invalid model providers: model_providers: "corp-gateway": claude-code: model "acme large" is not a model id — letters, digits and ._:/@+[]- , at most 256 characters',
  ];

  it.each(BODIES)("renders the server's body verbatim under the heading, and keeps what was typed: %s", async (body) => {
    putMock.mockRejectedValue(new HttpError(400, body));
    const { onSaved, user } = renderEditor({ editing: CORP, list: list([CORP]) });
    await user.clear(screen.getByLabelText(E.BASE_URL));
    await user.type(screen.getByLabelText(E.BASE_URL), "http://gateway.corp.example");
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    const note = await screen.findByRole("alert");
    expect(within(note).getByText(PROVIDERS.SAVE_REFUSED_TITLE_ONE).textContent).toBe(PROVIDERS.SAVE_REFUSED_TITLE_ONE);
    expect(note.querySelector("p")?.textContent).toBe(body);
    expect(screen.getByLabelText(E.BASE_URL)).toHaveValue("http://gateway.corp.example");
    expect(onSaved).not.toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("a 412 gets the console's shared someone-else-saved banner, not a silent overwrite", async () => {
    putMock.mockRejectedValue(new HttpError(412, "model providers changed since you loaded them — reload and retry"));
    const { user } = renderEditor({ editing: CORP, list: list([CORP]) });
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(await screen.findByText(WS_PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeInTheDocument();
  });
});

describe("editing", () => {
  it("titles the dialog with the name and a kind chip, never shows the id, and keeps the id on save", async () => {
    const { user } = renderEditor({ editing: CORP, list: list([CORP, ANTHROPIC], { "corp-gateway": 12 }) });
    expect(screen.getByRole("heading", { name: "Corp gateway" })).toBeInTheDocument();
    expect(within(dialog()).getByText(MODEL_PROVIDERS.KIND.custom_endpoint)).toBeInTheDocument();
    expect(within(dialog()).queryByText(/corp-gateway/)).toBeNull();
    expect(screen.getByText(E.PROVIDES_TOKEN)).toBeInTheDocument();
    expect(screen.getByLabelText(E.NAME)).toHaveValue("Corp gateway");
    expect(screen.getByLabelText(E.BASE_URL)).toHaveValue("https://gateway.corp.example");
    expect(screen.getByRole("button", { name: E.REMOVE })).toBeInTheDocument();

    // A rename is no change of address: no confirm, even with 12 people connected.
    await user.clear(screen.getByLabelText(E.NAME));
    await user.type(screen.getByLabelText(E.NAME), "Corp gateway (EU)");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(putBody().providers.map((p) => [p.id, p.name, p.uid])).toEqual([
      ["corp-gateway", "Corp gateway (EU)", "u-1"],
      ["anthropic-api-key", "Anthropic API key", "u-2"],
    ]);
  });
});

describe("E7 — remove confirm", () => {
  it("token wording: Remove deletes the provider from the document", async () => {
    const { onSaved, user } = renderEditor({ editing: CORP, list: list([CORP, ANTHROPIC]) });
    await user.click(screen.getByRole("button", { name: E.REMOVE }));
    expect(within(alert()).getByText(E.DELETE_TITLE("Corp gateway"))).toBeInTheDocument();
    expect(within(alert()).getByText(E.DELETE_BODY)).toBeInTheDocument();
    await user.click(within(alert()).getByRole("button", { name: E.REMOVE }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putMock).toHaveBeenCalledWith({ providers: [ANTHROPIC] }, '"e1"');
  });

  it("key wording, and Cancel writes nothing", async () => {
    const { user } = renderEditor({ editing: ANTHROPIC, list: list([CORP, ANTHROPIC]) });
    await user.click(screen.getByRole("button", { name: E.REMOVE }));
    expect(within(alert()).getByText(E.DELETE_TITLE("Anthropic API key"))).toBeInTheDocument();
    expect(within(alert()).getByText(E.DELETE_BODY_KEY)).toBeInTheDocument();
    await user.click(within(alert()).getByRole("button", { name: E.CANCEL }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(putMock).not.toHaveBeenCalled();
  });
});

describe("E8 — remove blocked", () => {
  it("while it is an agent's default: names the agent, and Remove is disabled", async () => {
    const { user } = renderEditor({ editing: CORP, list: list([CORP]), defaultFor: ["claude-code"] });
    await user.click(screen.getByRole("button", { name: E.REMOVE }));
    expect(within(alert()).getByText(E.DELETE_TITLE("Corp gateway"))).toBeInTheDocument();
    expect(within(alert()).getByText(E.DELETE_BLOCKED("Corp gateway", "Claude Code"))).toBeInTheDocument();
    expect(within(alert()).getByRole("button", { name: E.REMOVE })).toBeDisabled();
  });
});

describe("E9 — changing where it sends requests", () => {
  const cases: [string, ModelProvider, number, string][] = [
    ["tokens, 12 people", CORP, 12, E.ADDRESS_BODY(12)],
    ["keys, 12 people", ANTHROPIC, 12, E.ADDRESS_BODY_KEY(12)],
    ["token, one person", CORP, 1, E.ADDRESS_BODY_ONE],
    ["key, one person", ANTHROPIC, 1, E.ADDRESS_BODY_KEY_ONE],
  ];

  it.each(cases)("%s: asks first, and Save in the confirm writes", async (_, provider, n, body) => {
    const { onSaved, user } = renderEditor({ editing: provider, list: list([provider], { [provider.id]: n }) });
    const url = screen.getByLabelText(provider.kind === "custom_endpoint" ? E.BASE_URL : E.ROUTE_THROUGH);
    await user.clear(url);
    await user.type(url, "https://new.corp.example");
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    expect(within(alert()).getByText(E.ADDRESS_TITLE(provider.name!))).toBeInTheDocument();
    expect(within(alert()).getByText(body)).toBeInTheDocument();
    expect(putMock).not.toHaveBeenCalled();
    await user.click(within(alert()).getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putBody().providers[0].base_url).toBe("https://new.corp.example");
  });

  it("a changed path asks too; Cancel writes nothing", async () => {
    const { user } = renderEditor({ editing: CORP, list: list([CORP], { "corp-gateway": 12 }) });
    await user.click(screen.getByRole("button", { name: "Claude Code" }));
    const path = screen.getByLabelText(E.PATH);
    await user.clear(path);
    await user.type(path, "/claude");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(within(alert()).getByText(E.ADDRESS_BODY(12))).toBeInTheDocument();
    await user.click(within(alert()).getByRole("button", { name: E.CANCEL }));
    expect(putMock).not.toHaveBeenCalled();
  });

  it("with nobody connected yet, Save saves with no confirm", async () => {
    const { onSaved, user } = renderEditor({ editing: CORP, list: list([CORP], { "corp-gateway": 0 }) });
    await user.clear(screen.getByLabelText(E.BASE_URL));
    await user.type(screen.getByLabelText(E.BASE_URL), "https://new.corp.example");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  // Bedrock's region or endpoint (E9's colfoot) — a key kind, so the key wording.
  it("a changed Bedrock region asks too, with the key wording", async () => {
    const { user } = renderEditor({ editing: BEDROCK, list: list([BEDROCK], { "bedrock-prod": 3 }) });
    await user.clear(screen.getByLabelText(E.REGION));
    await user.type(screen.getByLabelText(E.REGION), "eu-west-1");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(within(alert()).getByText(E.ADDRESS_BODY_KEY(3))).toBeInTheDocument();
    await user.click(within(alert()).getByRole("button", { name: E.CANCEL }));
    expect(putMock).not.toHaveBeenCalled();
  });
});

describe("the draft rules", () => {
  it("the id is the name's slug, suffixed when taken", () => {
    expect(providerIdFrom("Corp gateway", [])).toBe("corp-gateway");
    expect(providerIdFrom("Corp gateway", ["corp-gateway", "corp-gateway-2"])).toBe("corp-gateway-3");
    expect(providerIdFrom("  ¡Acme!  LLM ", [])).toBe("acme-llm");
    expect(providerIdFrom("***", [])).toBe("provider");
  });

  it("rule 8 reads the address as the server stores it", () => {
    expect(addressChanged(CORP, { ...CORP, base_url: "https://gateway.corp.example/" })).toBe(false);
    expect(addressChanged(CORP, { ...CORP, auth: { header: "Authorization", format: "Token %s" } })).toBe(false);
    expect(addressChanged(CORP, { ...CORP, auth: { header: "X-Api-Key", format: "%s" } })).toBe(true);
    expect(addressChanged(ANTHROPIC, { ...ANTHROPIC, base_url: "https://llm.corp.example" })).toBe(true);
  });

  it("rule 8 also reads Bedrock's region and endpoint, but not the AWS sign-in portal or its pin", () => {
    expect(addressChanged(BEDROCK, { ...BEDROCK, bedrock: { ...BEDROCK.bedrock, region: "eu-west-1" } })).toBe(true);
    expect(
      addressChanged(BEDROCK, { ...BEDROCK, bedrock: { ...BEDROCK.bedrock, base_url: "https://vpce.example" } }),
    ).toBe(true);
    expect(
      addressChanged(BEDROCK, { ...BEDROCK, bedrock: { ...BEDROCK.bedrock, sso_start_url: "https://other.awsapps.com/start" } }),
    ).toBe(false);
    expect(
      addressChanged(BEDROCK, { ...BEDROCK, bedrock: { ...BEDROCK.bedrock, sso_account_id: "999988887777" } }),
    ).toBe(false);
  });
});
