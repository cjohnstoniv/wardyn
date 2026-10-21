/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { AuditEvent } from "../../lib/types";
import { describeAction, describeEvent } from "./audit-copy";

const ev = (action: string, target = "", extra: Partial<AuditEvent> = {}): AuditEvent =>
  ({ id: "e", time: new Date().toISOString(), actor_type: "system", actor: "x", action, outcome: "success", target, ...extra }) as AuditEvent;

describe("audit sentences", () => {
  it("joins the target to the sentence for the whole line, and leaves it off the action", () => {
    expect(describeEvent(ev("run.create", "r1"))).toBe("Created the run — r1");
    expect(describeAction(ev("run.create", "r1"))).toBe("Created the run");
    expect(describeEvent(ev("egress.allow", "a.example"))).toBe("Allowed egress to a.example");
    expect(describeAction(ev("egress.allow", "a.example"))).toBe("Allowed egress");
    expect(describeEvent(ev("egress.deny"))).toBe("Denied egress to an unknown host");
    expect(describeAction(ev("llm.scan.flag", "h"))).toBe("LLM content scan (flag)");
    expect(describeEvent(ev("llm.scan.flag", "h"))).toBe("LLM content scan (flag) for h");
  });

  it("falls back to the raw key for an action it does not know", () => {
    expect(describeAction(ev("brand.new.thing"))).toBe("brand.new.thing");
    expect(describeEvent(ev("brand.new.thing", "t"))).toBe("brand.new.thing — t");
  });
});
