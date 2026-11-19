/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// enterUIApp's stale option, as the /open page passes it: a page left while the ticket was being bound to this
// browser gets no hand-off, so the tab never leaves for the app after the person has moved on.
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("./run-detail/desktop-files-panel", () => ({ DesktopFilesPanel: () => null }));
vi.mock("../../lib/api/runs", () => ({ runs: { attachTicket: vi.fn() } }));

import { enterUIApp } from "./run-detail-ssh";

const SANDBOX = {
  enabled: true,
  bind_url: "https://run-{run}.apps.example.com/__wardyn/bind",
  enter_post_url: "https://run-{run}.apps.example.com/__wardyn/enter",
  host_mode: true,
};

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("enterUIApp with a stale check", () => {
  it("a page left during the binding is not handed off", async () => {
    let left = false;
    vi.stubGlobal("fetch", vi.fn(async () => {
      left = true;
      return new Response(null, { status: 204 });
    }));
    const submit = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    await enterUIApp("7c41e0a9", "api", SANDBOX, { ticket: "tkt", target: "_self", stale: () => left });
    expect(submit).not.toHaveBeenCalled();
  });

  it("the live page is handed off once", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 204 })));
    const submit = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    await enterUIApp("7c41e0a9", "api", SANDBOX, { ticket: "tkt", target: "_self", stale: () => false });
    expect(submit).toHaveBeenCalledTimes(1);
  });
});
