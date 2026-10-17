/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { reviewWarningText } from "./drop-warnings";

describe("reviewWarningText", () => {
  it("says one dropped host in the ceiling's words", () => {
    expect(reviewWarningText("dropped 1 egress domain(s) not in operator allowlist: pastebin.com")).toBe(NR_REVIEW.DROPPED_HOST_ONE("pastebin.com"));
  });

  it("names every dropped host once, joined", () => {
    expect(reviewWarningText("dropped 3 egress domain(s) not in operator allowlist: a.test,b.test:8443,c.test")).toBe(
      NR_REVIEW.DROPPED_HOSTS_MANY("a.test, b.test:8443 and c.test"),
    );
    expect(reviewWarningText("dropped 2 egress domain(s) not in operator allowlist: a.test,b.test")).toBe(NR_REVIEW.DROPPED_HOSTS_MANY("a.test and b.test"));
  });

  it("says a dropped private address the same way", () => {
    const server = 'dropped private address entry "10.0.0.5": not in operator allowlist, and allow_all_egress does not cover a private address';
    expect(reviewWarningText(server)).toBe(NR_REVIEW.DROPPED_HOST_ONE("10.0.0.5"));
  });

  it("says a dropped clone host the same way", () => {
    const server = 'dropped clone host "10.0.0.5" of workspace "app": a private address not in operator allowlist; allow it in the workspace\'s approved egress';
    expect(reviewWarningText(server)).toBe(NR_REVIEW.DROPPED_HOST_ONE("10.0.0.5"));
  });

  it("leaves every other server sentence as it came", () => {
    for (const w of ['confinement raised from "CC1" to operator minimum "CC2"', "resources capped to operator maximum", "dropped 1 ui_app(s) not in operator allowlist: web:3000"]) {
      expect(reviewWarningText(w)).toBe(w);
    }
  });
});
