/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import path from "node:path";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { parse } from "yaml";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

import { HttpError } from "../../../../../lib/api/core";
import { LEGACY_RUN_MODE_FIELDS } from "../../../../../lib/run-mode-map";
import { intentToWire } from "../../draft/intent-to-wire";
import { setTask } from "../../lenses/harness-lens";
import { specifies } from "../../review/blockers";
import { seedLoaders } from "./seed-loaders";

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
const signal = new AbortController().signal;
const T = "d1111111-1111-4111-8111-111111111111";
const R = "e1111111-1111-4111-8111-111111111111";
const CREATED = "2026-10-11T08:00:00Z"; // passthrough: a launch intent's created_at, never compared to the clock
const document = { api_version: "wardyn/v1", kind: "RunTemplate", intent: { shape: "application", workload: { kind: "command", command: "make test" } } };

beforeEach(() => wfetchMock.mockReset());

describe("seed loaders", () => {
  it("reads a pinned template revision and seeds the draft with its document intent", async () => {
    wfetchMock.mockResolvedValue(json({ id: T, kind: "policy", name: "Nightly", revision: 4, document }));
    const seed = await seedLoaders.template({ id: T, revision: 4 }, signal);
    expect(wfetchMock.mock.calls[0][0]).toBe(`/templates/${T}/revisions/4`);
    expect(seed).toEqual({ origin: { kind: "template", id: T, revision: 4, name: "Nightly", templateKind: "policy" }, intent: document.intent });
  });

  it("reads the current revision when the link names none, and pins what it read", async () => {
    wfetchMock.mockResolvedValue(json({ id: T, kind: "template", name: "Nightly", revision: 7, document }));
    const seed = await seedLoaders.template({ id: T }, signal);
    expect(wfetchMock.mock.calls[0][0]).toBe(`/templates/${T}`);
    expect(seed.origin).toMatchObject({ kind: "template", revision: 7 });
  });

  it("reads a run's launch intent and seeds an again draft", async () => {
    wfetchMock.mockResolvedValue(json({ run_id: R, created_at: CREATED, document }));
    const seed = await seedLoaders.again(R, signal);
    expect(wfetchMock.mock.calls[0][0]).toBe(`/runs/${R}/launch-intent`);
    expect(seed).toMatchObject({ origin: { kind: "again", runId: R }, intent: document.intent });
  });

  // A saved run or template in the older run-mode fields is seeded with them in the carriers, as a committed run
  // document is: beside the contract marker every console request sends, the server refuses the older fields.
  it("seeds Run again from a launch intent in the older fields, kept as the SDK wrote it, in the carriers", async () => {
    const legacy = { ...document, kind: "Run", intent: { agent: "codex-cli", task: "fix the flaky test" } };
    wfetchMock.mockResolvedValue(json({ run_id: R, created_at: CREATED, document: legacy }));
    const seed = await seedLoaders.again(R, signal);
    expect(seed.intent).toEqual({
      shape: "agent_task", experience: "background", workload: { kind: "agent_task", agent: "codex-cli", task: "fix the flaky test" },
      tools: [{ id: "codex-cli", kind: "harness" }],
    });
  });

  it.each([
    ["converts a template that states its run mode", { agent: "claude-code", task: "review the diff", interactive: true, interactive_start: "agent" },
      { shape: "agent_task", experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }], startup: { kind: "harness", tool: "claude-code", prompt: "review the diff" } }],
    // A template's absent field is left for the person: no interactive key chooses no run mode there.
    // The repository's example (examples/templates/ado-contribute-partial.yaml): the task is written when it is used.
    ["converts a background template that leaves its task for the person", { interactive: false, task_mode: "harness", agent: "claude-code" },
      { shape: "agent_task", experience: "background", workload: { kind: "agent_task", agent: "claude-code", task: "" }, tools: [{ id: "claude-code", kind: "harness" }] }],
    // With no agent either: the task_mode the server refuses beside the carriers goes, and Guided setup asks for both.
    ["converts a background template that leaves its agent and task for the person", { interactive: false, task_mode: "harness" },
      { shape: "agent_task", experience: "background", workload: { kind: "agent_task", task: "" } }],
    ["keeps a template with no interactive key as written", { agent: "codex-cli", task: "fix the flaky test" }, { agent: "codex-cli", task: "fix the flaky test" }],
  ])("%s", async (_name, intent, seeded) => {
    wfetchMock.mockResolvedValue(json({ id: T, kind: "template", name: "Nightly", revision: 4, document: { ...document, intent } }));
    expect((await seedLoaders.template({ id: T, revision: 4 }, signal)).intent).toEqual(seeded);
  });

  // The repository's shipped template, quoted in docs/AZURE-DEVOPS.md: seeded, its task written in Guided setup, sent.
  it("seeds the shipped Azure DevOps template into the carriers and sends it with no older field", async () => {
    const shipped = parse(readFileSync(path.resolve(__dirname, "../../../../../../../../examples/templates/ado-contribute-partial.yaml"), "utf8"));
    wfetchMock.mockResolvedValue(json({ id: T, kind: "template", name: shipped.name, revision: 1, document: shipped }));
    const seeded = (await seedLoaders.template({ id: T, revision: 1 }, signal)).intent;
    expect(seeded).toEqual({
      shape: "agent_task", experience: "background", workload: { kind: "agent_task", agent: "claude-code", task: "" },
      tools: [{ id: "claude-code", kind: "harness" }], inline_policy: shipped.intent.inline_policy,
    });
    expect(specifies(seeded, "task")).toBe(false);
    const intent = setTask(seeded, "fix the bug in the work item");
    expect(specifies(intent, "task")).toBe(true);
    const { body, unsupported } = intentToWire(intent);
    expect(unsupported).toEqual([]);
    expect(body).toMatchObject({ experience: "background", workload: { kind: "agent_task", agent: "claude-code", task: "fix the bug in the work item" } });
    for (const field of LEGACY_RUN_MODE_FIELDS) expect(body).not.toHaveProperty(field);
  });

  // Only a template leaves its task for later: a kept launch intent with no task is not a whole background run.
  it("keeps a Run again intent with no task as written", async () => {
    wfetchMock.mockResolvedValue(json({ run_id: R, created_at: CREATED, document: { ...document, kind: "Run", intent: { agent: "codex-cli" } } }));
    expect((await seedLoaders.again(R, signal)).intent).toEqual({ agent: "codex-cli" });
  });

  it("lets a refused read reach the seed-failed screen with the server's sentence", async () => {
    wfetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "That template does not exist.", reason: "template_not_found" }), { status: 404, headers: { "content-type": "application/json" } }));
    await expect(seedLoaders.template({ id: T, revision: 1 }, signal)).rejects.toBeInstanceOf(HttpError);
  });
});
