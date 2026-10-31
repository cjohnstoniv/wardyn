/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { INBOUND_APP, OPEN_APP } from "./ui-apps";

// The approved mock (r16 T2-b): the strings here are the canon, spelled out so a reword fails.
describe("inbound app canon strings", () => {
  it("the run page's inbound app row", () => {
    expect(INBOUND_APP).toEqual({
      TAG: "Inbound",
      CALLERS: "Callers:",
      ONLY_YOU: "Only you",
      COPY_LINK: "Copy link for people",
      COPY_ADDRESS: "Copy address for API clients",
      PATH_MODE_NOTE:
        "This deployment serves apps from one shared host, so people other than you can't open this in a browser. API clients can still call it.",
    });
  });

  it("the /open page's five states", () => {
    expect(OPEN_APP.OPENING("api")).toBe("Opening api…");
    expect(OPEN_APP.NOT_SHARED).toBe("This app isn't shared with you, or the run no longer exists.");
    expect(OPEN_APP.NOT_RUNNING).toBe("This run isn't running.");
    expect(OPEN_APP.SHARED_HOST).toBe(
      "This deployment serves apps from one shared host, so only the run's owner can open them in a browser. Ask an administrator to set per-run hosts.",
    );
    expect(OPEN_APP.GATEWAY_OFF).toBe("The app gateway is turned off on this deployment.");
  });

  it("the shared-host sentence is byte-equal to the server's", () => {
    const go = readFileSync(resolve(process.cwd(), "../internal/api/inbound_ticket.go"), "utf8");
    expect(/const inboundNeedsHostModeMsg = "([^"]*)"/.exec(go)?.[1]).toBe(OPEN_APP.SHARED_HOST);
  });
});
