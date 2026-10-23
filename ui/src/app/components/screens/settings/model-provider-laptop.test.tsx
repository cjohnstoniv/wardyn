/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// r12 (#2957): the local model server's laptop-runner switch (local.allow_on_laptop_runners): off by
// default, the disclosure when on, the Base URL line, the admin-only control, and a save that keeps it.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ModelProvidersList as Snapshot } from "../../../lib/api/model-providers";
import { LOCAL_MODEL_EDITOR as L } from "../../../lib/local-model-copy";
import { MODEL_PROVIDERS as M, PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { setField } from "../../../../test/set-field";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelProviderEditor } from "./model-provider-editor";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const putMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a), getModelProviders: vi.fn() },
}));
vi.mock("../../../lib/api/agent-providers", () => ({
  agentProviders: { getAgentProviders: () => Promise.resolve({ providers: { agents: [] }, etag: '"r"' }) },
}));

const HARNESSES = [{ id: "claude-code", display: "Claude Code" }];
const LAPTOP: ModelProvider = {
  id: "laptop-gpu",
  name: "Laptop GPU",
  kind: "local_model",
  base_url: "http://host.docker.internal:11434",
  local: { models: ["qwen2.5-coder:7b"] },
  harnesses: [{ harness: "claude-code", model: "qwen2.5-coder:7b" }],
};
const FLAGGED: ModelProvider = { ...LAPTOP, local: { ...LAPTOP.local!, allow_on_laptop_runners: true } };

const snapshot = (providers: ModelProvider[] = []): Snapshot => ({ providers: { providers }, etag: '"e1"', connected: {} });
const putBody = () => putMock.mock.calls[0][0] as { providers: ModelProvider[] };
const laptopSwitch = () => screen.getByRole("switch", { name: L.LAPTOP_SWITCH });

function renderEditor(editing: ModelProvider | null, operator = true) {
  const user = userEvent.setup();
  const onSaved = vi.fn();
  render(
    <OperatorProvider operator={operator} principal="alice">
      <ModelProviderEditor list={snapshot(editing ? [editing] : [])} editing={editing} harnesses={HARNESSES} onClose={vi.fn()} onSaved={onSaved} />
    </OperatorProvider>,
  );
  return { user, onSaved };
}

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
});

describe("the laptop-runner switch", () => {
  it("is off for a new local model server, says so, and shows no disclosure", async () => {
    const { user } = renderEditor(null);
    await user.click(screen.getByRole("button", { name: M.KIND.local_model }));
    expect(laptopSwitch()).toHaveAttribute("aria-checked", "false");
    expect(laptopSwitch()).toBeEnabled();
    expect(screen.getByText(L.LAPTOP_OFF_HINT)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: L.LAPTOP_DISCLOSURE })).toBeNull();
  });

  it("on: the hint, the disclosure word for word, and a save that sends the flag; off again drops it", async () => {
    const { user, onSaved } = renderEditor(LAPTOP);
    await user.click(laptopSwitch());
    expect(laptopSwitch()).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(L.LAPTOP_ON_HINT)).toBeInTheDocument();
    const disclosure = screen.getByRole("region", { name: L.LAPTOP_DISCLOSURE });
    expect(within(disclosure).getAllByRole("listitem").map((li) => li.textContent)).toEqual([...L.LAPTOP_BULLETS]);
    expect(within(disclosure).getByText("The person also has to allow that port on their own runner. Until they do, their runs can't use this server.")).toBeInTheDocument();
    expect(within(disclosure).getByText(/Other containers on that computer can't use it,/)).toBeInTheDocument();
    expect(within(disclosure).getByText(L.LAPTOP_FOOT)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putBody().providers[0].local).toEqual({ models: ["qwen2.5-coder:7b"], allow_on_laptop_runners: true });
  });

  it("a console save keeps an admin's laptop-runner flag (#2957)", async () => {
    const { user, onSaved } = renderEditor(FLAGGED);
    expect(laptopSwitch()).toHaveAttribute("aria-checked", "true");
    setField(screen.getByLabelText(L.REPLY), "2048");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putBody().providers[0].local).toEqual({ models: ["qwen2.5-coder:7b"], max_output: 2048, allow_on_laptop_runners: true });
  });

  it("turning it off on a flagged server saves it off", async () => {
    const { user, onSaved } = renderEditor(FLAGGED);
    await user.click(laptopSwitch());
    expect(screen.getByText(L.LAPTOP_OFF_HINT)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putBody().providers[0].local).toEqual({ models: ["qwen2.5-coder:7b"] });
  });

  it("on with a Base URL off host.docker.internal: the line under Base URL and its outline", () => {
    renderEditor({ ...FLAGGED, base_url: "http://192.168.1.20:11434" });
    expect(screen.getByText(L.LAPTOP_BASE_URL)).toBeInTheDocument();
    expect(screen.getByLabelText(E.BASE_URL)).toHaveAttribute("aria-invalid", "true");
    setField(screen.getByLabelText(E.BASE_URL), "http://host.docker.internal:11434");
    expect(screen.queryByText(L.LAPTOP_BASE_URL)).toBeNull();
    expect(screen.getByLabelText(E.BASE_URL)).not.toHaveAttribute("aria-invalid");
  });

  it("someone who is not an admin sees the value, dimmed, with the reason", () => {
    renderEditor(FLAGGED, false);
    expect(laptopSwitch()).toBeDisabled();
    expect(laptopSwitch()).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(OPERATOR_ONLY_REASON)).toBeInTheDocument();
    expect(screen.getByRole("region", { name: L.LAPTOP_DISCLOSURE })).toBeInTheDocument();
  });
});
