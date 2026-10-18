/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { credentialDelivery } from "./credential-delivery";

const respond = (body: unknown) =>
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json", ETag: '"v1"' } }),
    ),
  );

// The PUT replaces the whole document, so whatever get() drops the editor's next save erases: a per-trust
// override refusing a class on personal runners would silently fall back to the global row.
describe("credentialDelivery.get", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("keeps by_trust as stored", async () => {
    const by_trust = { personal: { classes: { env_secret: { mode: "refuse" as const } } } };
    respond({
      policy: { classes: { env_secret: { mode: "runner_resident" } }, by_trust },
      updated_at: "",
      updated_by: "admin@wardyn.test",
    });
    const snap = await credentialDelivery.get();
    expect(snap.policy.by_trust).toEqual(by_trust);
    expect(snap.etag).toBe('"v1"');
  });

  it("reads a never-written policy as empty classes", async () => {
    respond({ updated_at: "", updated_by: "" });
    const snap = await credentialDelivery.get();
    expect(snap.policy).toEqual({ classes: {} });
  });
});
