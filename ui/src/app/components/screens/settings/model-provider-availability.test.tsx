/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #923, editors.html section 2: "Available to" in the provider editor — live on
// an existing provider, asked on a new one, and a new provider whose list the
// server refused.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { AVAILABILITY } from "../../../lib/availability-copy";
import { MODEL_PROVIDERS, PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const putMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));
const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
const upsertGrantMock = vi.fn();
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      listUserTypes: async () => [
        { id: "developer", name: "Developer", description: "", priority: 0, built_in: false },
        { id: "portfolio-manager", name: "Portfolio manager", description: "", priority: 0, built_in: false },
      ],
    },
  };
});

import { ModelProviderEditor } from "./model-provider-editor";

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
  harnesses: [{ harness: "claude-code", path: "/anthropic" }],
};
const list = (providers: ModelProvider[] = []): ModelProvidersList => ({ providers: { providers }, etag: '"e1"', connected: {} });
const UNKNOWN_TYPE = 'The user type "portfolio-mgr" doesn\'t exist. Create it under User types first.';
const grant = (subject: string) => ({
  id: `g-${subject}`,
  subject_type: "user_type" as const,
  subject,
  capability: "model_provider",
  value: "corp-gateway",
  effect: "allow" as const,
  created_at: "2026-09-01T00:00:00Z",
});

function renderEditor(editing: ModelProvider | null, providers: ModelProvider[] = []) {
  const onClose = vi.fn();
  const onSaved = vi.fn();
  render(
    <ModelProviderEditor list={list(providers)} editing={editing} harnesses={HARNESSES} onClose={onClose} onSaved={onSaved} />,
  );
  return { onClose, onSaved };
}

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
  getAvailabilityMock.mockReset();
  putAvailabilityMock.mockReset();
  upsertGrantMock.mockReset();
});

describe("the provider editor's Available to", () => {
  it("editing: the live control after Use with, above the footer, with the provider's two lines; Save leaves it alone", async () => {
    getAvailabilityMock.mockResolvedValue({
      kind: "model_provider",
      value: "corp-gateway",
      restricted: true,
      allowed_by: [grant("developer"), grant("portfolio-manager")],
    });
    const { onSaved } = renderEditor(CORP, [CORP]);

    expect(await screen.findByText("Portfolio manager")).toBeInTheDocument();
    expect(getAvailabilityMock).toHaveBeenCalledWith("model_provider", "corp-gateway");
    expect(screen.getByRole("radio", { name: AVAILABILITY.ONLY })).toBeChecked();
    expect(screen.getByTestId("availability-only").textContent).toBe(AVAILABILITY.MODEL_PROVIDER_ONLY_HINT);
    expect(screen.getByText(AVAILABILITY.MODEL_PROVIDER_NOTE)).toBeInTheDocument();
    const useWith = screen.getByText(E.USE_WITH);
    const label = screen.getByText(AVAILABILITY.LABEL);
    const save = screen.getByRole("button", { name: E.SAVE });
    expect(useWith.compareDocumentPosition(label) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(label.compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    await userEvent.click(save);
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putAvailabilityMock).not.toHaveBeenCalled();
    expect(upsertGrantMock).not.toHaveBeenCalled();
  });

  it("a new provider asks, starting at Everyone; Save creates it, then writes the list, then Only these", async () => {
    const calls: string[] = [];
    putMock.mockImplementation(async (doc: { providers: ModelProvider[] }) => {
      calls.push(`put ${doc.providers.map((p) => p.id).join(",")}`);
      return { providers: doc, etag: '"e2"' };
    });
    upsertGrantMock.mockImplementation(async (g: { subject: string; capability: string; value: string }) =>
      calls.push(`grant ${g.subject} ${g.capability} ${g.value}`),
    );
    putAvailabilityMock.mockImplementation(async (kind: string, value: string, r: boolean) =>
      calls.push(`restrict ${kind} ${value} ${r}`),
    );
    const { onSaved } = renderEditor(null);
    await userEvent.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_api_key }));

    const d = screen.getByRole("dialog");
    expect(within(d).getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    expect(within(d).getByText(AVAILABILITY.MODEL_PROVIDER_NOTE)).toBeInTheDocument();
    await userEvent.clear(within(d).getByLabelText(E.NAME));
    await userEvent.type(within(d).getByLabelText(E.NAME), "Bloomberg gateway");
    await userEvent.type(within(d).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    await userEvent.click(within(d).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    expect(await within(d).findByText("Portfolio manager")).toBeInTheDocument();
    await userEvent.click(within(d).getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.click(within(d).getByRole("button", { name: E.SAVE }));

    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(calls).toEqual([
      "put bloomberg-gateway",
      "grant portfolio-manager model_provider bloomberg-gateway",
      "restrict model_provider bloomberg-gateway true",
    ]);
  });

  it("the provider saved but the list didn't: stays open on the saved provider, with the server's sentence", async () => {
    upsertGrantMock.mockRejectedValue(new HttpError(400, UNKNOWN_TYPE));
    getAvailabilityMock.mockResolvedValue({ kind: "model_provider", value: "bloomberg-gateway", restricted: false, allowed_by: [] });
    const { onSaved, onClose } = renderEditor(null);
    await userEvent.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_api_key }));
    const d = screen.getByRole("dialog");
    await userEvent.clear(within(d).getByLabelText(E.NAME));
    await userEvent.type(within(d).getByLabelText(E.NAME), "Bloomberg gateway");
    await userEvent.type(within(d).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-mgr");
    await userEvent.click(within(d).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await userEvent.click(within(d).getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.click(within(d).getByRole("button", { name: E.SAVE }));

    expect(await within(d).findByText(AVAILABILITY.CREATE_PARTIAL_TITLE)).toBeInTheDocument();
    expect(within(d).getByText(UNKNOWN_TYPE)).toBeInTheDocument();
    expect(within(d).getByText(AVAILABILITY.CREATE_PARTIAL_EVERYONE)).toBeInTheDocument();
    // Titled as the saved provider with its kind chip, the live control, and Remove.
    expect(within(d).getByRole("heading", { name: "Bloomberg gateway" })).toBeInTheDocument();
    expect(within(d).getByText(MODEL_PROVIDERS.KIND.anthropic_api_key)).toBeInTheDocument();
    await waitFor(() => expect(getAvailabilityMock).toHaveBeenCalledWith("model_provider", "bloomberg-gateway"));
    expect(await within(d).findByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    expect(within(d).getByRole("button", { name: E.REMOVE })).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
    expect(putAvailabilityMock).not.toHaveBeenCalled();

    // A further Save is an edit of the saved provider, against the new ETag.
    await userEvent.click(within(d).getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledTimes(2));
    expect(putMock.mock.calls[1][1]).toBe('"e2"');
    expect((putMock.mock.calls[1][0] as { providers: ModelProvider[] }).providers.map((p) => p.id)).toEqual([
      "bloomberg-gateway",
    ]);
    expect(onClose).not.toHaveBeenCalled();
  });
});
