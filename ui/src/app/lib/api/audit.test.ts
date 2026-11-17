/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { audit, demoAuditRows, egressFromAudit, exitCodeFromAudit, runEndingFromAudit } from "./audit";
import { erasedFieldNames, type AuditEvent } from "../types";
import { aheadByHours } from "../test-clock";

// egressFromAudit is the ONLY source of the run-detail egress table — the backend
// has no /egress endpoint, so a projection bug here silently blanks or mislabels
// the security-relevant egress decisions. It had zero coverage. Its sole
// consumer (run-detail.tsx) has no test, so this is the projection's only pin.
function ev(partial: Partial<AuditEvent>): AuditEvent {
  return {
    id: "e1",
    time: aheadByHours(-1),
    actor_type: "agent",
    actor: "run",
    action: "egress.deny",
    outcome: "denied",
    ...partial,
  };
}

describe("egressFromAudit", () => {
  // 0.9 (M-RN-3 RS7): a decision replayed from a runner's offline spool keeps the time the runner made it, and
  // the refusal a full log forces is marked; an ordinary row carries neither key.
  it("marks a spooled row with the time the runner decided it, and a full-log refusal", () => {
    const observed = aheadByHours(-5);
    const out = egressFromAudit([
      ev({ id: "s", action: "egress.allow", target: "api.example.com:443", data: { spooled: true, observed_at: observed, rule_source: "policy:domain" } }),
      ev({ id: "f", action: "egress.deny", target: "x.example.com:443", data: { spooled: true, observed_at: observed, rule_source: "builtin:spool-full" } }),
      ev({ id: "n", action: "egress.allow", target: "plain.example.com:443", data: { rule_source: "policy:domain" } }),
    ]);
    expect(out[0]).toMatchObject({ id: "s", spooled_at: observed });
    expect(out[0].spool_full).toBeUndefined();
    expect(out[1]).toMatchObject({ id: "f", decision: "deny", spooled_at: observed, spool_full: true });
    expect("spooled_at" in out[2] || "spool_full" in out[2]).toBe(false);
  });

  it("does not call a row spooled without the spooled flag, or without an observed time", () => {
    const out = egressFromAudit([
      ev({ id: "a", action: "egress.allow", target: "a.example.com:443", data: { observed_at: aheadByHours(-5) } }),
      ev({ id: "b", action: "egress.allow", target: "b.example.com:443", data: { spooled: true } }),
    ]);
    expect(out.every((d) => d.spooled_at === undefined)).toBe(true);
  });

  it("maps the three egress actions to their decisions", () => {
    const out = egressFromAudit([
      ev({ id: "a", action: "egress.allow", target: "api.anthropic.com:443" }),
      ev({ id: "d", action: "egress.deny", target: "evil.example.com:443" }),
      ev({ id: "p", action: "egress.hold", target: "pkg.example.com:443" }),
    ]);
    expect(out.map((d) => d.decision)).toEqual(["allow", "deny", "pending"]);
  });

  it("maps a dial-failed and a tunnel-failed deny to failed; an ordinary deny stays deny", () => {
    const out = egressFromAudit([
      ev({ id: "f1", target: "a.example.com:443", data: { rule_source: "builtin:dial-failed" } }),
      ev({ id: "f2", target: "b.example.com:443", data: { rule_source: "builtin:tunnel-failed" } }),
      ev({ id: "d1", target: "c.example.com:443", data: { rule_source: "policy:denied" } }),
      ev({ id: "d2", target: "d.example.com:443", data: { rule_source: "builtin:private-ip" } }),
      ev({ id: "d3", target: "e.example.com:443" }),
    ]);
    expect(out.map((d) => d.decision)).toEqual(["failed", "failed", "deny", "deny", "deny"]);
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
    const [d] = egressFromAudit([ev({ action: "egress.hold", target: undefined })]);
    expect(d.domain).toBe("—");
  });

  // C-02 (#1062): egress.pending is egress.hold's pre-0.8 name
  // (docs/AUDIT-ACTIONS.md's "Renamed in 0.8"). A row written under it before
  // the upgrade is never rewritten, so it must still project as held.
  it("projects the pre-0.8 egress.pending action as held, same as egress.hold", () => {
    const [d] = egressFromAudit([ev({ action: "egress.pending", target: "pkg.example.com:443" })]);
    expect(d.decision).toBe("pending");
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

  // Negative control (wardyn/audit-decision.tsx's ruleSourceLabel, the
  // console's rule_source chip): a mixed feed of tool-rule AND real
  // rule_source-carrying egress rows must project identically — egressFromAudit
  // keys on toolRuleDecision alone, never on ruleSourceLabel, so a non-tool
  // rule_source (site-config's internal-host lift, a builtin guard refusal)
  // is a REAL connection and stays.
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

// listAudit's `filter` param — run-detail issues SECOND, filtered fetches
// (?run_id=&action=... or ?run_id=&action_prefix=...) so a narrow index
// doesn't compete with every other action for the shared 1000-row cap on a
// chatty run's (oldest-first) audit trail.
describe("listAudit — action/action_prefix filter reaches the wire", () => {
  // ticket: W21-S1-5
  afterEach(() => vi.unstubAllGlobals());

  it("sends ?run_id=&action= together, and omits both entirely when unset", async () => {
    // A fresh Response per call — a body stream can only be read once, and
    // this test drives two separate listAudit() calls against the same mock.
    const fetchMock = vi.fn().mockImplementation(
      async () => new Response(JSON.stringify([]), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await audit.listAudit("run_1", { action: "session.recording.write" });
    const url1 = String(fetchMock.mock.calls[0][0]);
    expect(url1).toContain("run_id=run_1");
    expect(url1).toContain("action=session.recording.write");
    expect(url1).not.toContain("action_prefix=");

    await audit.listAudit("run_1");
    const url2 = String(fetchMock.mock.calls[1][0]);
    expect(url2).toContain("run_id=run_1");
    expect(url2).not.toContain("action=");
  });

  // C-02 (#1062): the recording picker's index fetch must carry action_prefix
  // (not action), so both session.recording and session.recording.write rows
  // come back in one request.
  it("sends ?action_prefix= for a prefix filter, not ?action=", async () => {
    const fetchMock = vi.fn().mockImplementation(
      async () => new Response(JSON.stringify([]), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await audit.listAudit("run_1", { actionPrefix: "session.recording" });
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain("run_id=run_1");
    expect(url).toContain("action_prefix=session.recording");
    expect(url).not.toContain("&action=");
  });
});

describe("listAudit — newest page of a run", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("sends order=desc for a run and hands the rows back oldest-first", async () => {
    const fetchMock = vi.fn().mockImplementation(
      async () =>
        new Response(JSON.stringify([{ id: "c" }, { id: "b" }, { id: "a" }]), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const out = await audit.listAudit("run_1", { newest: true });
    expect(String(fetchMock.mock.calls[0][0])).toContain("order=desc");
    expect(out.map((e) => e.id)).toEqual(["a", "b", "c"]);
  });

  it("sends no order for the default read", async () => {
    const fetchMock = vi.fn().mockImplementation(async () => new Response("[]", { status: 200, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);

    await audit.listAudit("run_1");
    expect(String(fetchMock.mock.calls[0][0])).not.toContain("order=");
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

// 0.7.6 Finding 3 — "the failure names a destination instead of being one".
// Without this, the dispatch-time model-credential refusal would grade
// `unknown`: a complete sentence with no machine-readable class of its own,
// so the console could only print it. The class is one key on the audit row
// the refusal already writes.
describe("runEndingFromAudit — the model-credential refusal is its own ending", () => {
  const failed = (data: Record<string, unknown>): AuditEvent =>
    ev({ id: "c", actor_type: "system", actor: "wardynd", action: "run.create", outcome: "failure", data });
  const REFUSAL =
    "This run's model access is configured as Amazon Bedrock (captured AWS SSO session), and that session can no longer be renewed — sign in to AWS from Getting started in the console, or from the sign-in banner the console shows on every page. Wardyn does not substitute a different model provider.";

  it("grades `credential`, and carries the provider the refusal names", () => {
    const ending = runEndingFromAudit("FAILED", [
      failed({ error: REFUSAL, reason: "model_credential", provider: "bedrock-prod", kind: "bedrock_sso" }),
    ]);
    expect(ending?.kind).toBe("credential");
    expect(ending?.action).toBe("run.create");
    expect(ending?.provider).toBe("bedrock-prod");
  });

  it("carries NO detail — data.error is the run's own failure_hint, which the block already renders", () => {
    const ending = runEndingFromAudit("FAILED", [
      failed({ error: REFUSAL, reason: "model_credential", provider: "bedrock-prod", kind: "bedrock_sso" }),
    ]);
    expect(ending?.detail).toBeUndefined();
  });

  // Every OTHER run.create failure — a sandbox-create error, a lost
  // sandbox_ref, an api_key grant that would not compile — is still a cause
  // this build does not diagnose.
  it("a run.create failure with no reason still grades unknown", () => {
    expect(runEndingFromAudit("FAILED", [failed({ error: "create sandbox: no such image" })])?.kind).toBe("unknown");
  });

  it("a different reason on the same action grades unknown too", () => {
    expect(runEndingFromAudit("FAILED", [failed({ error: "x", reason: "roster_unreadable" })])?.kind).toBe("unknown");
  });

  // The FAILED_CAUSE scan runs FIRST: an image that could not be built is the
  // earlier cause, and the credential refusal that followed is fallout.
  it("run.build/failure still wins over a later credential refusal", () => {
    const ending = runEndingFromAudit("FAILED", [
      ev({ id: "b", action: "run.build", outcome: "failure", data: { error: "step 4/9: npm ci exited 1" } }),
      failed({ error: REFUSAL, reason: "model_credential", provider: "bedrock-prod", kind: "bedrock_sso" }),
    ]);
    expect(ending?.kind).toBe("image");
  });

  // A refusal naming no provider still grades credential, with no provider to
  // key a door by.
  it("grades credential without a provider key, leaving it undefined", () => {
    const ending = runEndingFromAudit("FAILED", [failed({ error: REFUSAL, reason: "model_credential" })]);
    expect(ending?.kind).toBe("credential");
    expect(ending?.provider).toBeUndefined();
  });

  it("says nothing about a run that did not fail", () => {
    expect(
      runEndingFromAudit("COMPLETED", [failed({ error: REFUSAL, reason: "model_credential" })]),
    ).toBeUndefined();
  });
});

// Audit retention (0.8.6): GET /audit/retention, POST /audit/retention/drop and
// GET /audit/export?partition=, all security tier.
describe("audit retention API", () => {
  afterEach(() => vi.restoreAllMocks());
  const respond = (status: number, body: unknown, type = "application/json") =>
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(typeof body === "string" ? body : JSON.stringify(body), { status, headers: { "Content-Type": type } }),
    );

  it("getRetention GETs /audit/retention and returns the server's status as sent", async () => {
    const status = { policy: { days: 0, effective_days: 0 }, cutover: aheadByHours(-1), partitions: [], months_ahead: 12 };
    const spy = respond(200, status);
    await expect(audit.getRetention()).resolves.toEqual(status);
    const [url, init] = spy.mock.calls[0];
    expect(String(url)).toMatch(/\/api\/v1\/audit\/retention$/);
    expect(init?.method).toBe("GET");
  });

  it("dropPartition POSTs the partition and the digest", async () => {
    const dropped = { partition: "audit_events_legacy", rows: 3, seq_lo: 1, seq_hi: 3, digest: "ab", event_seq: 4 };
    const spy = respond(200, dropped);
    await expect(audit.dropPartition("audit_events_legacy", "ab")).resolves.toEqual(dropped);
    const [url, init] = spy.mock.calls[0];
    expect(String(url)).toMatch(/\/api\/v1\/audit\/retention\/drop$/);
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ partition: "audit_events_legacy", digest: "ab" });
  });

  it("a refused drop throws an HttpError carrying the refusal's reason", async () => {
    respond(409, { error: "the digest does not match", reason: "audit_retention_digest_mismatch" });
    await expect(audit.dropPartition("p", "x")).rejects.toMatchObject({
      status: 409,
      reason: "audit_retention_digest_mismatch",
    });
  });

  it("exportPartition asks for the named partition and form, encoded, and returns the archive", async () => {
    const spy = respond(200, '{"type":"manifest"}\n', "application/x-ndjson");
    const blob = await audit.exportPartition("audit_events_2026_10", "raw");
    expect(await blob.text()).toBe('{"type":"manifest"}\n');
    expect(String(spy.mock.calls[0][0])).toMatch(/\/api\/v1\/audit\/export\?partition=audit_events_2026_10&form=raw$/);
  });

  it("a failed export is an error, never an empty archive", async () => {
    respond(409, { error: "That partition can still receive rows.", reason: "audit_partition_open" });
    await expect(audit.exportPartition("p", "readable")).rejects.toMatchObject({ status: 409, reason: "audit_partition_open" });
  });
});

describe("erasedFieldNames", () => {
  it("names the target and every data leaf that carries the erased value, at any depth", () => {
    expect(
      erasedFieldNames(
        ev({ target: "[erased]", data: { task: "[erased]", nested: { email: "[erased]", kept: "x" }, n: 1 } }),
      ),
    ).toEqual(["target", "task", "email"]);
  });

  it("is empty for an event with nothing erased", () => {
    expect(erasedFieldNames(ev({ target: "api.example.com", data: { reason: "fine" } }))).toEqual([]);
  });
});

// A kill whose sandbox was still stopping when it finished writes run.kill success with sandbox_stop
// "confirming", and its result lands later on a run.kill.confirm row (internal/api/run_kill_confirm.go).
// One case per row of the console's state table (E0–E8): only a row that proves the stop says confirmed.
describe("runEndingFromAudit — what a KILLED run's trail proves", () => {
  const KILL_AT = "2026-11-17T10:00:00Z";
  const kill = (outcome: AuditEvent["outcome"], data?: Record<string, unknown>, time = KILL_AT): AuditEvent =>
    ev({ id: `kill-${time}`, time, actor_type: "human", actor: "alice", action: "run.kill", outcome, data });
  const confirm = (outcome: AuditEvent["outcome"], time: string): AuditEvent =>
    ev({
      id: `confirm-${time}`,
      time,
      actor_type: "system",
      actor: "wardynd",
      action: "run.kill.confirm",
      outcome,
      data: outcome === "failure" ? { step: "kill_sandbox", reason: "pod still terminating" } : undefined,
    });
  const evidence = (events: AuditEvent[], confirmWaitOver = false) =>
    runEndingFromAudit("KILLED", events, { confirmWaitOver })?.evidence;
  const confirming = kill("success", { sandbox_stop: "confirming" });

  it("E0: no kill row proves nothing", () => {
    expect(evidence([])).toBe("unknown");
    expect(evidence([], true)).toBe("unknown");
  });
  it("E1: a stop queued for an offline runner is queued", () => {
    expect(evidence([kill("success", { sandbox_stop: "queued" })])).toBe("queued");
  });
  it("E2: a stop still being confirmed, no result yet, wait running, is confirming", () => {
    expect(evidence([confirming])).toBe("confirming");
  });
  it("E3: the same once the page's wait is over is unconfirmed", () => {
    expect(evidence([confirming], true)).toBe("unconfirmed");
  });
  it("E4: a confirm success row proves the stop, whether or not the wait is over", () => {
    expect(evidence([confirming, confirm("success", "2026-11-17T10:00:40Z")])).toBe("confirmed");
    expect(evidence([confirming, confirm("success", "2026-11-17T10:00:40Z")], true)).toBe("confirmed");
  });
  it("E5: a confirm failure row is partial, and names the confirm row as the one that failed", () => {
    const ending = runEndingFromAudit("KILLED", [confirming, confirm("failure", "2026-11-17T10:01:00Z")]);
    expect(ending).toMatchObject({ evidence: "partial", confirmFailed: true, action: "run.kill" });
  });
  it("E6: a success with no sandbox_stop is confirmed", () => {
    expect(evidence([kill("success")])).toBe("confirmed");
    expect(runEndingFromAudit("KILLED", [kill("success")])?.confirmFailed).toBeUndefined();
  });
  it("E7: any other sandbox_stop value fails closed to unconfirmed, never confirmed", () => {
    expect(evidence([kill("success", { sandbox_stop: "runner_revoked" })])).toBe("unconfirmed");
    expect(evidence([kill("success", { sandbox_stop: "stopped" })])).toBe("unconfirmed");
    expect(evidence([kill("success", { sandbox_stop: 1 })])).toBe("unconfirmed");
  });
  it("E8: a failed kill is partial, and a confirm row cannot lift it", () => {
    expect(evidence([kill("failure")])).toBe("partial");
    const ending = runEndingFromAudit("KILLED", [kill("failure"), confirm("success", "2026-11-17T10:00:40Z")]);
    expect(ending).toMatchObject({ evidence: "partial" });
    expect(ending?.confirmFailed).toBeUndefined();
  });

  it("a confirm row older than the latest kill is ignored", () => {
    const second = kill("success", { sandbox_stop: "confirming" }, "2026-11-17T10:05:00Z");
    expect(evidence([confirming, confirm("success", "2026-11-17T10:00:40Z"), second])).toBe("confirming");
    expect(evidence([confirming, confirm("success", "2026-11-17T10:00:40Z"), second], true)).toBe("unconfirmed");
  });
  it("a second kill during a confirm: the row that lands after it counts", () => {
    const second = kill("success", { sandbox_stop: "confirming" }, "2026-11-17T10:00:30Z");
    expect(evidence([confirming, second, confirm("success", "2026-11-17T10:00:50Z")])).toBe("confirmed");
    expect(evidence([confirming, second, confirm("failure", "2026-11-17T10:00:50Z")])).toBe("partial");
  });
  it("a confirm row at the kill's own timestamp counts", () => {
    expect(evidence([confirming, confirm("success", KILL_AT)])).toBe("confirmed");
  });
  it("the latest confirm row decides", () => {
    const rows = [confirming, confirm("failure", "2026-11-17T10:00:40Z"), confirm("success", "2026-11-17T10:01:00Z")];
    expect(evidence(rows)).toBe("confirmed");
  });
  it("a confirm read that failed (no confirm rows) never gives confirmed", () => {
    expect(evidence([confirming])).not.toBe("confirmed");
    expect(evidence([confirming], true)).not.toBe("confirmed");
  });
  it("a kill row with no readable time counts no confirm row", () => {
    const noTime = kill("success", { sandbox_stop: "confirming" }, "not a time");
    expect(evidence([noTime, confirm("success", "2026-11-17T10:00:40Z")])).toBe("confirming");
  });
});
