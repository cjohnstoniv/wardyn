/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The M8 boards' sentences, byte for byte, where the copy module composes them.
import { describe, expect, it } from "vitest";
import { NEEDS_YOU, askedAgo, heldClock } from "./needs-you";

describe("needs-you copy", () => {
  it("composes the boards' sentences", () => {
    expect(NEEDS_YOU.banner.asked(askedAgo(2 * 60_000 + 5_000), "desktop")).toBe(
      "Asked 2 min ago. The agent waits until you take over or decline.",
    );
    expect(NEEDS_YOU.banner.asked(askedAgo(60_000), "terminal")).toBe(
      "Asked 1 min ago. Taking over opens the terminal. The agent waits until you take over or decline.",
    );
    expect(NEEDS_YOU.strip.control("desktop")).toBe("You have control of the desktop");
    expect(heldClock(302_000)).toBe("5:02");
    expect(NEEDS_YOU.inUse.title("priya", "10:13")).toBe("priya took over at 10:13.");
    expect(NEEDS_YOU.toast.browserTitle("Renew staging certificates")).toBe("Needs you: Renew staging certificates");
  });
});
