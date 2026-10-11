/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { previewRunPolicy } from "./policy-preview";
import { runs } from "./runs";
import type { ComponentFact } from "../types/components";

// This fixture is pinned to actual server responses by the Go door tests.
const golden = JSON.parse(readFileSync(resolve(process.cwd(), "../internal/api/testdata/new_run_component_facts.json"), "utf8")) as Record<string, ComponentFact>;
afterEach(() => vi.unstubAllGlobals());

describe("resolved component facts at the dry-run clients", () => {
  for (const door of ["preflight", "preview"] as const) {
    for (const [key, fact] of Object.entries(golden)) {
      it(`${door} preserves ${key} and omitted/empty fields`, async () => {
        vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ components: [fact] }), {
          status: 200, headers: { "content-type": "application/json" },
        })));
        const input = { agent: fact.agent?.agent ?? "claude-code", task: "facts" };
        const result = door === "preflight" ? await runs.preflightRun(input) : await previewRunPolicy(input);
        expect(result.components).toEqual([fact]);
        const received = result.components![0];
        expect(received.requirements).toBeInstanceOf(Array);
        if (received.agent) {
          expect(received.reason).toBe("agent");
          expect(received.agent.hosts).toEqual([]);
          expect(received.agent.secrets).toEqual([]);
          expect(received.agent).not.toHaveProperty("model_provider");
          if (received.agent.agent !== "claude-code") expect(received.agent).not.toHaveProperty("managed_settings");
        } else {
          expect(received.org).toBe(key.split("_")[1]);
          expect(received.repo_access?.[0].can_write).toBe(false);
          expect("install_url" in received).toBe(key.endsWith("_true"));
        }
      });
    }
  }
});
