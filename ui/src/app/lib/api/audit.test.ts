/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { audit, demoAuditRows, egressFromAudit, exitCodeFromAudit } from "./audit";
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

  // A tool call the run's own tool_rules answered rides an egress.allow/deny
  // event whose TARGET is the control plane (emitLocalDecision logs against
  // controlPlaneURL). It rendered in the Egress tile as a deny against
  // "wardynd" — a host the sandbox never dialled — while the Audit tab called
  // the same event "Decided by rule".
  it("drops rule-decided tool calls: they are decisions, not connections", () => {
    const out = egressFromAudit([
      ev({ id: "tool", action: "egress.deny", target: "wardynd:8443", data: { rule_source: "policy:tool-deny" } }),
      ev({ id: "tool2", action: "egress.allow", target: "wardynd:8443", data: { rule_source: "policy:tool-allow" } }),
      // Ordinary policy-decided egress is a real connection and stays.
      ev({ id: "wire", action: "egress.deny", target: "evil.example.com:443", data: { rule_source: "policy" } }),
    ]);
    expect(out.map((d) => d.id)).toEqual(["wire"]);
  });

  // Negative control for 6a (ui/src/app/lib/types/audit.ts's ruleSourceLabel,
  // the console's rule_source chip): a mixed feed of tool-rule AND real
  // rule_source-carrying egress rows must project identically to before that
  // change — egressFromAudit keys on toolRuleDecision alone, never on the new
  // ruleSourceLabel function, so a non-tool rule_source (site-config's
  // internal-host lift, a builtin guard refusal) is a REAL connection and stays.
  it("6a negative control: non-tool rule_source values are real connections, not decisions", () => {
    const out = egressFromAudit([
      ev({ id: "tool", action: "egress.deny", target: "wardynd:8443", data: { rule_source: "policy:tool-deny" } }),
      ev({
        id: "internal-host",
        action: "egress.allow",
        target: "registry.corp.example:443",
        data: { rule_source: "site-config:internal-host" },
      }),
      ev({
        id: "guard-refused",
        action: "egress.deny",
        target: "10.0.0.5:443",
        data: { rule_source: "builtin:private-ip" },
      }),
    ]);
    expect(out.map((d) => d.id)).toEqual(["internal-host", "guard-refused"]);
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

// demoAuditRows is the secrets-section demos' widened panel projection —
// egress decisions UNION secret.read/credential.mint (which carry an outcome
// + secret/grant target, no domain, and don't fit EgressDecision at all).
describe("demoAuditRows", () => {
  it("unions egress decisions with credential/secret rows", () => {
    const out = demoAuditRows([
      ev({ id: "e1", action: "egress.deny", target: "wardyn-api:8443" }),
      ev({ id: "s1", action: "secret.read", target: "wardyn-demo-key", outcome: "success" }),
      ev({ id: "c1", action: "credential.mint", target: "grant-1", outcome: "success" }),
    ]);
    expect(out).toHaveLength(3);
    expect(out.filter((r) => r.kind === "egress")).toHaveLength(1);
    expect(out.filter((r) => r.kind === "credential")).toHaveLength(2);
  });

  it("ignores every action that is neither egress.* nor secret.read/credential.mint", () => {
    const out = demoAuditRows([ev({ action: "run.create" }), ev({ action: "run.complete" })]);
    expect(out).toHaveLength(0);
  });

  it("carries the credential row's outcome and target verbatim, no domain", () => {
    const [row] = demoAuditRows([ev({ id: "c1", action: "credential.mint", target: "grant-1", outcome: "denied" })]);
    expect(row).toMatchObject({ kind: "credential", action: "credential.mint", target: "grant-1", outcome: "denied" });
    expect((row as { domain?: string }).domain).toBeUndefined();
  });
});

// W21-S1-5: listAudit's `action` param — run-detail issues a SECOND, filtered
// fetch (?run_id=&action=session.recording) so the recording picker's index
// doesn't compete with every other action for the shared 1000-row cap on a
// chatty run's (oldest-first) audit trail.
describe("listAudit — action filter reaches the wire (W21-S1-5)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("sends ?run_id=&action= together, and omits action entirely when unset", async () => {
    // A fresh Response per call — a body stream can only be read once, and
    // this test drives two separate listAudit() calls against the same mock.
    const fetchMock = vi.fn().mockImplementation(
      async () => new Response(JSON.stringify([]), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await audit.listAudit("run_1", "session.recording");
    const url1 = String(fetchMock.mock.calls[0][0]);
    expect(url1).toContain("run_id=run_1");
    expect(url1).toContain("action=session.recording");

    await audit.listAudit("run_1");
    const url2 = String(fetchMock.mock.calls[1][0]);
    expect(url2).toContain("run_id=run_1");
    expect(url2).not.toContain("action=");
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
