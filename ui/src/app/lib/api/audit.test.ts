/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { egressFromAudit, exitCodeFromAudit } from "./audit";
import type { AuditEvent } from "../types";

// egressFromAudit is the ONLY source of the run-detail egress table — the backend
// has no /egress endpoint, so a projection bug here silently blanks or mislabels
// the security-relevant egress decisions. It had zero coverage. Its sole
// consumer (run-detail.tsx) has no test, so this is the projection's only pin.
function ev(partial: Partial<AuditEvent>): AuditEvent {
  return {
    id: "e1",
    time: "2026-07-17T00:00:00Z",
    actor_type: "agent",
    actor: "run",
    action: "egress.deny",
    outcome: "denied",
    ...partial,
  };
}

describe("egressFromAudit", () => {
  it("maps the three egress actions to their decisions", () => {
    const out = egressFromAudit([
      ev({ id: "a", action: "egress.allow", target: "api.anthropic.com:443" }),
      ev({ id: "d", action: "egress.deny", target: "evil.example.com:443" }),
      ev({ id: "p", action: "egress.pending", target: "pkg.example.com:443" }),
    ]);
    expect(out.map((d) => d.decision)).toEqual(["allow", "deny", "pending"]);
  });

  it("ignores every non-egress action (does not leak unrelated audit rows)", () => {
    const out = egressFromAudit([
      ev({ action: "run.create" }),
      ev({ action: "run.kill" }),
      ev({ action: "egress.deny", target: "x.example.com:443" }),
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].decision).toBe("deny");
  });

  it("prefers an explicit data.domain over the target host", () => {
    const [d] = egressFromAudit([
      ev({ action: "egress.allow", target: "1.2.3.4:443", data: { domain: "cdn.example.com" } }),
    ]);
    expect(d.domain).toBe("cdn.example.com");
  });

  it("strips a :port off the target when no explicit domain is present", () => {
    const [d] = egressFromAudit([ev({ action: "egress.deny", target: "host.example.com:8443" })]);
    expect(d.domain).toBe("host.example.com");
  });

  it("falls back to an em-dash when neither domain nor target is present", () => {
    const [d] = egressFromAudit([ev({ action: "egress.pending", target: undefined })]);
    expect(d.domain).toBe("—");
  });

  it("carries the byte count from data when numeric, and omits it otherwise", () => {
    const [withBytes] = egressFromAudit([
      ev({ action: "egress.allow", target: "a:1", data: { bytes: 4096 } }),
    ]);
    const [noBytes] = egressFromAudit([
      ev({ action: "egress.allow", target: "a:1", data: { bytes: "lots" } }),
    ]);
    expect(withBytes.bytes).toBe(4096);
    expect(noBytes.bytes).toBeUndefined();
  });
});

describe("exitCodeFromAudit", () => {
  it("reads the code off run.complete, including a legitimate exit 0", () => {
    expect(exitCodeFromAudit([ev({ action: "run.complete", data: { exit_code: 137 } })])).toBe(137);
    expect(exitCodeFromAudit([ev({ action: "run.complete", data: { exit_code: 0 } })])).toBe(0);
  });

  it("is undefined when no run.complete recorded a numeric code", () => {
    expect(exitCodeFromAudit([ev({ action: "run.kill" })])).toBeUndefined();
    expect(exitCodeFromAudit([ev({ action: "run.reconcile", data: { reason: "daemon restart" } })]))
      .toBeUndefined();
  });

  // The forensics variants (Wait error / panic recover) emit run.complete with
  // NO exit_code, and the deferred one fires LAST — taking "the last
  // run.complete" would blank a code that was actually recorded.
  it("keeps the last DEFINED code, not the last run.complete event", () => {
    expect(
      exitCodeFromAudit([
        ev({ action: "run.complete", data: { exit_code: 1 } }),
        ev({ action: "run.complete", data: { panic: "boom" } }),
      ]),
    ).toBe(1);
  });
});
