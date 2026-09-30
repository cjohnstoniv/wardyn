/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";

const getSetupStatus = vi.fn();
vi.mock("./api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatus(...a) } }));

import { useK8sRunner } from "./use-k8s-runner";

describe("useK8sRunner", () => {
  it("reads true from an operator's driver", async () => {
    getSetupStatus.mockResolvedValue({ runner: { driver: "k8s" } });
    const { result } = renderHook(() => useK8sRunner());
    await waitFor(() => expect(result.current).toBe(true));
  });

  // #1416 — a member's /setup/status blanks driver and keeps only the
  // member-safe kubernetes bit.
  it("reads true from a member's redacted status (driver blank, kubernetes bit)", async () => {
    getSetupStatus.mockResolvedValue({ runner: { driver: "", kubernetes: true } });
    const { result } = renderHook(() => useK8sRunner());
    await waitFor(() => expect(result.current).toBe(true));
  });

  it("stays false on docker", async () => {
    getSetupStatus.mockResolvedValue({ runner: { driver: "docker" } });
    const { result } = renderHook(() => useK8sRunner());
    await waitFor(() => expect(getSetupStatus).toHaveBeenCalled());
    expect(result.current).toBe(false);
  });
});
