/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The model-provider editor registers with the shared unsaved guard while it holds edits.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS, PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import { unsavedSnapshot } from "../../../lib/unsaved-registry";
import type { ModelProvider } from "../../../lib/types/site";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelProviderEditor } from "./model-provider-editor";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../../../lib/api/health", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/health")>("../../../lib/api/health");
  return { ...actual, health: { ...actual.health, whoami: vi.fn() } };
});
vi.mock("../../../lib/api/model-providers", () => ({ modelProviders: { putModelProviders: vi.fn() } }));

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

function renderEditor(editing: ModelProvider | null) {
  const user = userEvent.setup();
  const view = render(
    <OperatorProvider operator principal="alice">
      <ModelProviderEditor list={list(editing ? [editing] : [])} editing={editing} harnesses={HARNESSES} onClose={vi.fn()} onSaved={vi.fn()} />
    </OperatorProvider>,
  );
  return { ...view, user };
}

afterEach(() => cleanup());

describe("ModelProviderEditor unsaved guard", () => {
  it("a new provider: picking a kind is not an edit; typing is, and typing the name back clears it", async () => {
    const { user } = renderEditor(null);
    expect(unsavedSnapshot()).toBeNull();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.custom_endpoint }));
    expect(unsavedSnapshot()).toBeNull();
    setField(screen.getByLabelText(E.BASE_URL), "https://llm.corp.example");
    const text = unsavedSnapshot();
    expect(text).toContain("https://llm.corp.example");
    expect(text).toContain("authHeader");
    setField(screen.getByLabelText(E.BASE_URL), "");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("an existing provider is compared to its own values", () => {
    renderEditor(CORP);
    expect(unsavedSnapshot()).toBeNull();
    setField(screen.getByLabelText(E.NAME), "Corp gateway 2");
    expect(unsavedSnapshot()).toContain("Corp gateway 2");
    setField(screen.getByLabelText(E.NAME), "Corp gateway");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("unregisters on unmount", () => {
    const { unmount } = renderEditor(CORP);
    setField(screen.getByLabelText(E.NAME), "Corp gateway 2");
    expect(unsavedSnapshot()).not.toBeNull();
    unmount();
    expect(unsavedSnapshot()).toBeNull();
  });
});
