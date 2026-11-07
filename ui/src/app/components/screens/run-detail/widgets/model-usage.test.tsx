/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import type { ModelUsage, ModelUsageModel, ModelUsageTokens } from "../../../../lib/types";

const getModelUsageMock = vi.fn();
vi.mock("../../../../lib/api/runs", () => ({
  runs: { getModelUsage: (...a: unknown[]) => getModelUsageMock(...a) },
}));

import { HttpError } from "../../../../lib/api/core";
import { MODEL_USAGE } from "../../../wardyn/copy";
import { ModelUsageWidget } from "./model-usage";

const tokens = (o: Partial<ModelUsageTokens> = {}): ModelUsageTokens => ({
  input_tokens: 0,
  output_tokens: 0,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  reasoning_tokens: 0,
  calls: 0,
  uncounted_calls: 0,
  partial_calls: 0,
  ...o,
});
const model = (o: Partial<ModelUsageModel>): ModelUsageModel => ({
  provider: "p",
  vendor: "anthropic",
  host: "api.anthropic.com",
  model: "claude-sonnet-4-5",
  uncounted_reasons: {},
  ...tokens(),
  ...o,
});
const usage = (by_model: ModelUsageModel[], extra: Partial<ModelUsage> = {}): ModelUsage => {
  const totals = tokens();
  for (const m of by_model) {
    for (const k of Object.keys(totals) as (keyof ModelUsageTokens)[]) totals[k] += m[k];
  }
  return { totals, by_model, opaque_tunnels: 0, ...extra };
};

const claude = model({ input_tokens: 1200, output_tokens: 300, cache_read_tokens: 900, calls: 4 });
const claudeHaiku = model({ model: "claude-haiku-4-5", input_tokens: 80, output_tokens: 20, cache_read_tokens: 0, calls: 2 });
const gpt = model({ vendor: "openai", host: "api.openai.com", model: "gpt-5.1", input_tokens: 500, output_tokens: 50, cache_read_tokens: 400, calls: 3 });

async function mount(live: boolean, runId = "r1") {
  const view = render(<ModelUsageWidget runId={runId} live={live} />);
  await act(async () => {});
  return view;
}

beforeEach(() => {
  getModelUsageMock.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("ModelUsageWidget totals", () => {
  it("one vendor, several models: the totals and a row per model, figures as reported", async () => {
    getModelUsageMock.mockResolvedValue(usage([claude, claudeHaiku]));
    await mount(true);
    expect(screen.getByRole("heading", { name: MODEL_USAGE.title })).toBeInTheDocument();
    expect(screen.getByText("1,280")).toBeInTheDocument(); // Input total: 1,200 + 80
    expect(screen.getByText("320")).toBeInTheDocument(); // Output total
    expect(screen.getByText("claude-haiku-4-5")).toBeInTheDocument();
    expect(screen.getByText("2 calls")).toBeInTheDocument();
    expect(screen.getByText(MODEL_USAGE.footnote(["anthropic"]))).toBeInTheDocument();
    // Cache read is not folded into Input anywhere.
    expect(screen.queryByText("2,180")).toBeNull();
  });

  it("one model: a single line above the totals and no per-model block", async () => {
    getModelUsageMock.mockResolvedValue(usage([claude]));
    await mount(true);
    expect(screen.getByText("claude-sonnet-4-5 · Anthropic · 4 calls")).toBeInTheDocument();
    expect(screen.queryByRole("list")).toBeNull();
    expect(screen.getByText("1,200")).toBeInTheDocument();
  });

  it("more than one vendor: the totals block is Calls only, the rows carry the figures", async () => {
    getModelUsageMock.mockResolvedValue(usage([claude, gpt]));
    await mount(true);
    const card = screen.getByRole("heading", { name: MODEL_USAGE.title }).closest("section") as HTMLElement;
    // The summed Input (1,700) and Output (350) are never shown; only Calls is a total.
    expect(within(card).queryByText("1,700")).toBeNull();
    expect(within(card).queryByText("350")).toBeNull();
    expect(within(card).getByText("7")).toBeInTheDocument();
    const rows = within(card).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText("1,200")).toBeInTheDocument();
    expect(within(rows[1]).getByText("400")).toBeInTheDocument();
    // Each vendor's own rule, from the wire vendor, and only for vendors present.
    expect(screen.getByText(/Anthropic: Cache read is not part of Input\./)).toBeInTheDocument();
    expect(screen.getByText(/OpenAI: Input includes Cache read\./)).toBeInTheDocument();
  });

  it("names the Bedrock vendor beside Anthropic in the cache note and omits an absent vendor's rule", async () => {
    const bedrock = model({ vendor: "bedrock", host: "bedrock-runtime.us-east-1.amazonaws.com", model: "anthropic.claude-haiku-4-5", input_tokens: 10, output_tokens: 5, calls: 1 });
    getModelUsageMock.mockResolvedValue(usage([claude, bedrock]));
    await mount(true);
    expect(screen.getByText(/Anthropic and Amazon Bedrock: Cache read is not part of Input\./)).toBeInTheDocument();
    expect(screen.queryByText(/OpenAI/)).toBeNull();
  });

  it("the same vendor and model on two hosts: each row names its endpoint, other rows do not", async () => {
    const east = model({ model: "claude-sonnet-4-5", provider: "corp-gateway", host: "llm.east.example.com", input_tokens: 100, output_tokens: 10, calls: 1 });
    const direct = model({ model: "claude-sonnet-4-5", provider: "api.anthropic.com", host: "api.anthropic.com", input_tokens: 200, output_tokens: 20, calls: 2 });
    getModelUsageMock.mockResolvedValue(usage([east, direct, claudeHaiku]));
    await mount(true);
    const rows = screen.getAllByRole("listitem");
    expect(within(rows[0]).getByText("corp-gateway · llm.east.example.com")).toBeInTheDocument();
    expect(within(rows[1]).getByText("api.anthropic.com")).toBeInTheDocument();
    expect(within(rows[2]).queryByText(/example\.com|anthropic\.com/)).toBeNull();
  });

  it("a long model name wraps and stays on screen in full", async () => {
    const long = "anthropic.claude-sonnet-4-5-20250929-v1:0-with-a-very-long-inference-profile-suffix";
    getModelUsageMock.mockResolvedValue(usage([model({ model: long, calls: 1, input_tokens: 1, output_tokens: 1 }), claudeHaiku]));
    await mount(true);
    expect(screen.getByText(long)).toHaveClass("break-all");
  });
});

describe("ModelUsageWidget what is not counted", () => {
  it("shows no honest line when there is nothing to say", async () => {
    getModelUsageMock.mockResolvedValue(usage([claude]));
    await mount(true);
    expect(screen.queryByText(/not counted/)).toBeNull();
    expect(screen.queryByText(/cut off/)).toBeNull();
    expect(screen.queryByText(/encrypted connection/)).toBeNull();
  });

  it("states uncounted calls with their reasons, the cut-off calls and the encrypted connections", async () => {
    getModelUsageMock.mockResolvedValue(
      usage(
        [
          model({ ...tokens({ calls: 9, uncounted_calls: 5, partial_calls: 2, input_tokens: 40, output_tokens: 4 }), uncounted_reasons: { stream_without_usage: 2, no_usage: 1, undecodable: 2 } }),
        ],
        { opaque_tunnels: 2 },
      ),
    );
    await mount(true);
    expect(screen.getByText(MODEL_USAGE.uncountedHead(5))).toBeInTheDocument();
    expect(screen.getByText(/3 sent no usage figures, 2 arrived compressed in a way Wardyn can't read/)).toBeInTheDocument();
    expect(screen.getByText(MODEL_USAGE.cutOffHead(2))).toBeInTheDocument();
    expect(screen.getByText(MODEL_USAGE.encryptedHead(2))).toBeInTheDocument();
    expect(screen.getByText(MODEL_USAGE.encryptedTail(2))).toBeInTheDocument();
  });

  it("uses the singular for one call and one connection", async () => {
    getModelUsageMock.mockResolvedValue(
      usage([model({ ...tokens({ calls: 3, uncounted_calls: 1, input_tokens: 5, output_tokens: 1 }), uncounted_reasons: { body_oversize: 1 } })], { opaque_tunnels: 1 }),
    );
    await mount(true);
    expect(screen.getByText("1 call not counted")).toBeInTheDocument();
    expect(screen.getByText(/\(had a response larger than Wardyn reads\)/)).toBeInTheDocument();
    expect(screen.getByText("1 encrypted connection")).toBeInTheDocument();
    expect(screen.getByText("to a model host was not opened; its tokens aren't counted")).toBeInTheDocument();
  });

  it("an empty run says so by calls, never a zero-token total", async () => {
    getModelUsageMock.mockResolvedValue(usage([]));
    await mount(true);
    expect(screen.getByText(MODEL_USAGE.emptyLive)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_USAGE.labelInput)).toBeNull();
  });

  it("calls seen but none counted: no totals, the lines carry the card", async () => {
    getModelUsageMock.mockResolvedValue(
      usage([model({ ...tokens({ calls: 4, uncounted_calls: 4 }), uncounted_reasons: { undecodable: 4 } })], { opaque_tunnels: 2 }),
    );
    await mount(false);
    expect(screen.getByText(MODEL_USAGE.noTokensFinished)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_USAGE.labelInput)).toBeNull();
    expect(screen.getByText("4 calls not counted")).toBeInTheDocument();
    expect(screen.getByText(/all arrived compressed/)).toBeInTheDocument();
    expect(screen.getByText(MODEL_USAGE.encryptedHead(2))).toBeInTheDocument();
    // No cache note when no tokens were counted.
    expect(screen.queryByText(/Cache read is not part of Input/)).toBeNull();
  });
});

describe("ModelUsageWidget polling and the end of the run", () => {
  it("polls every 10 s while live and marks the card Live", async () => {
    vi.useFakeTimers();
    getModelUsageMock.mockResolvedValue(usage([claude]));
    await mount(true);
    expect(getModelUsageMock).toHaveBeenCalledTimes(1);
    expect(screen.getByText(MODEL_USAGE.live)).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(getModelUsageMock).toHaveBeenCalledTimes(2);
  });

  it("reads once for a finished run, never polls, and says the run finished", async () => {
    vi.useFakeTimers();
    getModelUsageMock.mockResolvedValue(usage([claude]));
    await mount(false);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(getModelUsageMock).toHaveBeenCalledTimes(1);
    expect(screen.getByText(MODEL_USAGE.finished)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_USAGE.live)).toBeNull();
  });

  it("reads again when the run goes from live to finished, then stops polling", async () => {
    vi.useFakeTimers();
    getModelUsageMock.mockResolvedValueOnce(usage([claude])).mockResolvedValue(usage([model({ ...claude, input_tokens: 2000, calls: 5 })]));
    const view = await mount(true);
    expect(screen.getByText("1,200")).toBeInTheDocument();
    view.rerender(<ModelUsageWidget runId="r1" live={false} />);
    await act(async () => {});
    expect(getModelUsageMock).toHaveBeenCalledTimes(2);
    expect(screen.getByText("2,000")).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(getModelUsageMock).toHaveBeenCalledTimes(2);
  });

  it("a finished card with a reading offers Refresh, which shows usage that landed after the run ended and keeps the numbers while it reads", async () => {
    let finish: (u: ModelUsage) => void = () => {};
    getModelUsageMock
      .mockResolvedValueOnce(usage([claude]))
      .mockReturnValueOnce(new Promise<ModelUsage>((r) => { finish = r; }));
    await mount(false);
    expect(screen.getByText("1,200")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: MODEL_USAGE.refresh }));
    await act(async () => {});
    // Still on screen while the read is out.
    expect(screen.getByText("1,200")).toBeInTheDocument();
    await act(async () => { finish(usage([model({ ...claude, input_tokens: 1700, calls: 6 })])); });
    expect(screen.getByText("1,700")).toBeInTheDocument();
    expect(getModelUsageMock).toHaveBeenCalledTimes(2);
  });

  it("a live card has no Refresh: the poll does it", async () => {
    getModelUsageMock.mockResolvedValue(usage([claude]));
    await mount(true);
    expect(screen.queryByRole("button", { name: MODEL_USAGE.refresh })).toBeNull();
    expect(screen.queryByRole("button", { name: MODEL_USAGE.retry })).toBeNull();
  });

  it("another run starts from the skeleton, not the last run's numbers", async () => {
    getModelUsageMock.mockResolvedValueOnce(usage([claude])).mockReturnValue(new Promise(() => {}));
    const view = await mount(true, "r1");
    expect(screen.getByText("1,200")).toBeInTheDocument();
    view.rerender(<ModelUsageWidget runId="r2" live={true} />);
    await act(async () => {});
    expect(screen.queryByText("1,200")).toBeNull();
  });
});

describe("ModelUsageWidget failures", () => {
  it("a later failed read keeps the numbers, drops the Live marker and says so", async () => {
    vi.useFakeTimers();
    getModelUsageMock.mockResolvedValueOnce(usage([claude])).mockRejectedValue(new HttpError(500, "boom"));
    await mount(true);
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.getByText("1,200")).toBeInTheDocument();
    expect(screen.getByText(MODEL_USAGE.stale)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_USAGE.live)).toBeNull();
  });

  it("live success, then the final read fails: Retry is offered, keeps the numbers, and brings the current reading", async () => {
    getModelUsageMock
      .mockResolvedValueOnce(usage([claude]))
      .mockRejectedValueOnce(new HttpError(500, "boom"))
      .mockResolvedValue(usage([model({ ...claude, input_tokens: 2000, calls: 5 })]));
    const view = await mount(true);
    view.rerender(<ModelUsageWidget runId="r1" live={false} />);
    await act(async () => {});
    expect(screen.getByText(MODEL_USAGE.stale)).toBeInTheDocument();
    expect(screen.getByText("1,200")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: MODEL_USAGE.retry }));
    await act(async () => {});
    expect(screen.getByText("2,000")).toBeInTheDocument();
    expect(screen.queryByText(MODEL_USAGE.stale)).toBeNull();
    expect(screen.queryByRole("button", { name: MODEL_USAGE.retry })).toBeNull();
    expect(screen.getByRole("button", { name: MODEL_USAGE.refresh })).toBeInTheDocument();
  });

  it("a read that works again clears the cue and brings the Live marker back", async () => {
    vi.useFakeTimers();
    getModelUsageMock.mockResolvedValueOnce(usage([claude])).mockRejectedValueOnce(new HttpError(500, "boom")).mockResolvedValue(usage([claude]));
    await mount(true);
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.getByText(MODEL_USAGE.stale)).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.queryByText(MODEL_USAGE.stale)).toBeNull();
    expect(screen.getByText(MODEL_USAGE.live)).toBeInTheDocument();
  });

  it("a first-load failure on a finished run offers Retry, which reads again", async () => {
    getModelUsageMock.mockRejectedValueOnce(new HttpError(500, "boom")).mockResolvedValue(usage([claude]));
    await mount(false);
    expect(screen.getByText(MODEL_USAGE.loadError)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: MODEL_USAGE.retry }));
    await act(async () => {});
    expect(screen.getByText("1,200")).toBeInTheDocument();
    expect(getModelUsageMock).toHaveBeenCalledTimes(2);
  });

  it("a first-load failure on a live run has no Retry: the poll tries again", async () => {
    getModelUsageMock.mockRejectedValue(new HttpError(500, "boom"));
    await mount(true);
    expect(screen.getByText(MODEL_USAGE.loadError)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: MODEL_USAGE.retry })).toBeNull();
  });

  it.each([
    ["a bare 404 (no such route)", 404, "", MODEL_USAGE.unsupported],
    ["a 501 (no store for it)", 501, "model_usage_unsupported", MODEL_USAGE.unsupported],
    ["a 404 with a reason (this run is not readable)", 404, "run_not_found", MODEL_USAGE.unavailable],
    ["a 403", 403, "forbidden", MODEL_USAGE.unavailable],
  ])("%s answers its own line and the tile stays", async (_n, status, reason, line) => {
    getModelUsageMock.mockRejectedValue(new HttpError(status, "refused", reason));
    await mount(true);
    expect(screen.getByText(line)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: MODEL_USAGE.title })).toBeInTheDocument();
  });

  it("a refusal after a reading clears the old numbers and stops polling", async () => {
    vi.useFakeTimers();
    getModelUsageMock.mockResolvedValueOnce(usage([claude])).mockRejectedValue(new HttpError(404, "run not found", "run_not_found"));
    await mount(true);
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.queryByText("1,200")).toBeNull();
    expect(screen.getByText(MODEL_USAGE.unavailable)).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(getModelUsageMock).toHaveBeenCalledTimes(2);
  });
});
