/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { expect, it, vi } from "vitest";
import { isScalar } from "yaml";
import { parsePolicySource } from ".";

// Counts key checks: the parser's own repeated-key comparisons (its default comparison, counted) and the
// reader's scalar checks. Every call still does the real work.
const compared = vi.hoisted(() => ({ n: 0 }));
vi.mock("yaml", async (importOriginal) => {
  const yaml = await importOriginal<typeof import("yaml")>();
  class Composer extends yaml.Composer {
    constructor(options?: ConstructorParameters<typeof yaml.Composer>[0]) {
      super(options?.uniqueKeys === true ? {
        ...options,
        uniqueKeys: (a, b) => {
          compared.n++;
          return a === b || (yaml.isScalar(a) && yaml.isScalar(b) && a.value === b.value);
        },
      } : options);
    }
  }
  return { ...yaml, Composer, isScalar: vi.fn(yaml.isScalar) };
});

const KEYS = 40_000;

it.each([
  ["reads", "", { ok: true }],
  ["refuses a repeated key in", "k0: 2\n", { ok: false, line: KEYS + 1, column: 1, message: "DUPLICATE_KEY: Map keys must be unique" }],
])("%s a mapping of many keys with a bounded number of key checks", (_, tail, answer) => {
  const source = Array.from({ length: KEYS }, (_, i) => `k${i}: 1\n`).join("") + tail;
  compared.n = 0;
  vi.mocked(isScalar).mockClear();
  expect(parsePolicySource(source)).toMatchObject(answer);
  expect(compared.n + vi.mocked(isScalar).mock.calls.length).toBeLessThanOrEqual(10 * KEYS);
}, 60_000);
