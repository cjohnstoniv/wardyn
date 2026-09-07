/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R4-F079: every console request must be BOUNDED. A daemon that accepts a
// connection and never answers (internal/db/db.go:110-121 describes reaching
// that with no Wardyn bug at all) used to freeze the whole console: the shell
// gates every route behind health()+whoami() settling, so an un-bounded fetch
// meant a permanent spinner — no error, no retry, and a reload re-entered the
// same wait. These cases fail by TIMING OUT on the un-bounded code, which is
// precisely the defect.
import { describe, it, expect, vi, afterEach } from "vitest";
import { HttpError, WFETCH_TIMEOUT_MS, wfetch } from "./core";
import { health as healthApi } from "./health";

afterEach(() => vi.unstubAllGlobals());

/** A server that ACCEPTS and never writes: it settles only if it is aborted. */
function stubHangingFetch() {
  const seen: RequestInit[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((_url: string, init: RequestInit = {}) => {
      seen.push(init);
      return new Promise<Response>((_resolve, reject) => {
        init.signal?.addEventListener("abort", () =>
          reject((init.signal as AbortSignal & { reason?: unknown }).reason),
        );
      });
    }),
  );
  return seen;
}

describe("wfetch — a request that is never answered still ends", () => {
  it("rejects on the deadline instead of waiting forever", async () => {
    stubHangingFetch();
    // A short deadline for the test; WFETCH_TIMEOUT_MS is the shipped one.
    await expect(wfetch("/runs", { method: "GET" }, 20)).rejects.toBeInstanceOf(HttpError);
  });

  it("reports it as the unreachable-daemon case, with no HTTP status to claim", async () => {
    stubHangingFetch();
    const err = await wfetch("/runs", { method: "GET" }, 20).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(HttpError);
    // Not 401 — sign-in must not read a hang as "that admin token was rejected".
    expect((err as HttpError).status).toBe(0);
  });

  it("bounds a request the caller said nothing about — the default is a real deadline", () => {
    const seen = stubHangingFetch();
    void wfetch("/runs", { method: "GET" }).catch(() => {});
    expect(seen[0]?.signal).toBeInstanceOf(AbortSignal);
    expect(WFETCH_TIMEOUT_MS).toBeGreaterThan(0);
    expect(Number.isFinite(WFETCH_TIMEOUT_MS)).toBe(true);
  });
});

describe("health() — /healthz is the one call that bypasses wfetch, so it carries its own deadline", () => {
  it("hands the raw fetch an abort signal", () => {
    const seen = stubHangingFetch();
    void healthApi.health();
    expect(seen[0]?.signal).toBeInstanceOf(AbortSignal);
  });
});
