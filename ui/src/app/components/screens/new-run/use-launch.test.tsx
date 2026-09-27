/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// B9 (#540, packet MP-D): a launch that fails after New Run has unmounted hands
// its failure back for the shell strip — the SERVER's sentence only, never this
// screen's own fallback.
import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const createRun = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return { ...actual, runs: { ...actual.runs, createRun: (...a: unknown[]) => createRun(...a) } };
});

import { useLaunch } from "./use-launch";
import { initialWizardState } from "./wizard-types";
import { HttpError } from "../../../lib/api/core";

function mountLaunch() {
  return renderHook(
    () =>
      useLaunch({
        state: initialWizardState("CC1", { selectedPolicyId: "p1" }),
        workspaces: [],
        useSaved: true,
        ccTouched: false,
        merged: null,
      }),
    { wrapper: ({ children }: { children: React.ReactNode }) => <MemoryRouter>{children}</MemoryRouter> },
  );
}

/** Launch, unmount the screen while the request is in flight, then fail it. */
async function failAfterUnmount(error: unknown): Promise<string | void> {
  let reject: (e: unknown) => void = () => {};
  createRun.mockReturnValue(new Promise((_, r) => (reject = r)));
  const { result, unmount } = mountLaunch();
  const pending = result.current.launch();
  unmount();
  reject(error);
  return pending;
}

beforeEach(() => {
  createRun.mockReset();
});

describe("useLaunch — a failure after the screen is gone (B9)", () => {
  it("hands back the server's sentence verbatim", async () => {
    const sentence = "This run's model provider is Corp gateway, and you have not added your token for it.";
    await expect(failAfterUnmount(new HttpError(422, sentence))).resolves.toBe(sentence);
  });

  it("hands back nothing when the server gave no sentence — never the client fallback", async () => {
    await expect(failAfterUnmount(new Error(""))).resolves.toBeUndefined();
  });

  it("while the screen is still mounted, returns nothing: the rail shows it", async () => {
    createRun.mockRejectedValue(new HttpError(422, "refused"));
    const { result } = mountLaunch();
    await expect(result.current.launch()).resolves.toBeUndefined();
  });
});
