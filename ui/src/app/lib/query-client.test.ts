/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { queryClient } from "./query-client";

describe("queryClient (the console's one TanStack Query client)", () => {
  it("never retries, polls or re-reads on its own, and keeps nothing after the last screen leaves", () => {
    expect(queryClient.getDefaultOptions().queries).toMatchObject({
      retry: false,
      refetchOnWindowFocus: false,
      refetchOnReconnect: false,
      refetchInterval: false,
      staleTime: 0,
      gcTime: 0,
      networkMode: "always",
    });
  });
});
