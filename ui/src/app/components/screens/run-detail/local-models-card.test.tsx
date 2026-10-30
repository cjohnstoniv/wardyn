/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M9: the run Overview's "Local models" card.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { LOCAL_MODEL_CARD as T } from "../../../lib/local-model-copy";
import type { RunDetail } from "../../../lib/types";
import type { ModelProvider } from "../../../lib/types/site";
import { OperatorProvider } from "../../wardyn/operator-context";
import { LocalModelsCard } from "./local-models-card";

const getMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: () => getMock() },
}));
vi.mock("../../wardyn/model-access-context", () => ({
  useShellSetupStatus: () => ({
    status: {
      harnesses: [
        { id: "claude-code", display: "Claude Code" },
        { id: "codex-cli", display: "Codex CLI" },
      ],
    },
    refresh: () => {},
  }),
}));

const LAPTOP: ModelProvider = {
  id: "laptop-gpu",
  name: "Laptop GPU",
  kind: "local_model",
  base_url: "http://host.docker.internal:11434",
  local: { models: ["qwen2.5-coder:7b", "llama3.2:3b"] },
  harnesses: [
    { harness: "claude-code", model: "qwen2.5-coder:7b" },
    { harness: "codex-cli", model: "qwen2.5-coder:7b" },
  ],
};

// The allowlist the run launched with (#3374): not the provider's list now (LAPTOP carries two models).
const run = (patch: Partial<RunDetail> = {}) =>
  ({
    id: "r1",
    model_provider_id: "laptop-gpu",
    model_provider_name: "Laptop GPU",
    model_allowlists: { "laptop-gpu": ["qwen2.5-coder:7b", "llama3.2:3b"] },
    ...patch,
  }) as RunDetail;

function renderCard(r: RunDetail, operator = true, operatorResolved = true) {
  return render(
    <OperatorProvider operator={operator} operatorResolved={operatorResolved} principal="alice">
      <LocalModelsCard run={r} />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  getMock.mockReset().mockResolvedValue({ providers: { providers: [LAPTOP] }, connected: {}, etag: '"e"' });
});

describe("LocalModelsCard", () => {
  it("lists the allowed models, who asks for each, the server and the caps (defaults when unset)", async () => {
    renderCard(run());
    expect(await screen.findByRole("heading", { name: T.TITLE })).toBeInTheDocument();
    expect(screen.getByText("Laptop GPU")).toBeInTheDocument();
    expect(T).not.toHaveProperty("LEDE");
    expect(T).not.toHaveProperty("RUN_KEEPS");
    expect(screen.getByText("qwen2.5-coder:7b")).toBeInTheDocument();
    expect(screen.getByText(T.ASKED_FOR(["Claude Code", "Codex CLI"]))).toBeInTheDocument();
    expect(screen.getByText("llama3.2:3b")).toBeInTheDocument();
    expect(screen.getByText(T.NOT_ASKED_FOR)).toBeInTheDocument();
    expect(screen.getByText("host.docker.internal:11434")).toBeInTheDocument();
    expect(screen.getByText(T.SERVER_NOTE)).toBeInTheDocument();
    expect(screen.getAllByText("32,768 tokens")).toHaveLength(2);
    expect(screen.getByText("Up to 30 minutes")).toBeInTheDocument();
    expect(screen.getByText(T.NO_MANAGEMENT)).toBeInTheDocument();
  });

  it("lists the models the run launched with, not the provider's list now", async () => {
    renderCard(run({ model_allowlists: { "laptop-gpu": ["llama3.2:3b"] } }));
    expect(await screen.findByText("llama3.2:3b")).toBeInTheDocument();
    await waitFor(() => expect(getMock).toHaveBeenCalled());
    expect(screen.queryByText("qwen2.5-coder:7b")).toBeNull();
  });

  it("a member sees the run's own list without reading the admin-only provider record", async () => {
    renderCard(run(), false);
    expect(await screen.findByRole("heading", { name: T.TITLE })).toBeInTheDocument();
    expect(screen.getByText("qwen2.5-coder:7b")).toBeInTheDocument();
    expect(screen.getByText("llama3.2:3b")).toBeInTheDocument();
    expect(screen.getByText(T.NO_MANAGEMENT)).toBeInTheDocument();
    expect(screen.queryByText(T.SERVER)).toBeNull();
    expect(getMock).not.toHaveBeenCalled();
  });

  it("states the provider's own caps", async () => {
    getMock.mockResolvedValue({
      providers: { providers: [{ ...LAPTOP, local: { models: ["a"], max_context: 8192, max_output: 1024, keep_alive_seconds: 3600 } }] },
      connected: {},
      etag: '"e"',
    });
    renderCard(run());
    expect(await screen.findByText("8,192 tokens")).toBeInTheDocument();
    expect(screen.getByText("1,024 tokens")).toBeInTheDocument();
    expect(screen.getByText("Up to 1 hour")).toBeInTheDocument();
  });

  it("says 'asks' for one agent, and 'Up to N seconds' for an odd keep-alive", () => {
    expect(T.ASKED_FOR(["Claude Code"])).toBe("Claude Code asks for this");
    expect(T.KEEP_FOR(90)).toBe("Up to 90 seconds");
    expect(T.KEEP_FOR(60)).toBe("Up to 1 minute");
  });

  it("is absent for a run that holds no model allowlist, and reads nothing", () => {
    renderCard(run({ model_allowlists: undefined }));
    expect(screen.queryByText(T.TITLE)).toBeNull();
    expect(getMock).not.toHaveBeenCalled();
  });

  it("is absent for a run under no provider", () => {
    renderCard(run({ model_provider_id: undefined }));
    expect(getMock).not.toHaveBeenCalled();
  });

  it("fails closed while /me has not answered: an unresolved identity reads operator, and the read is operatorOnly", () => {
    renderCard(run(), true, false);
    expect(getMock).not.toHaveBeenCalled();
    expect(screen.getByText(T.TITLE)).toBeInTheDocument();
    expect(screen.queryByText(T.SERVER)).toBeNull();
  });

  it("keeps the run's own list when the provider has since been removed or the read fails", async () => {
    getMock.mockRejectedValue(new Error("down"));
    renderCard(run());
    await waitFor(() => expect(getMock).toHaveBeenCalled());
    expect(screen.getByText("qwen2.5-coder:7b")).toBeInTheDocument();
    expect(screen.queryByText(T.SERVER)).toBeNull();
  });
});
