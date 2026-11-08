/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { AuditEvent } from "../../lib/types";
import { AUDIT, describeAction, describeEvent } from "./audit-copy";
// The run page's Audit tab and the Audit screen read the lib table.
import { awsSigningLine, describeAction as describeTabAction, describeEvent as describeTabEvent, hasNoCaller, inboundDetail, serviceSource } from "../../lib/audit-copy";

const ev = (action: string, target = "", extra: Partial<AuditEvent> = {}): AuditEvent =>
  ({ id: "e", time: new Date().toISOString(), actor_type: "system", actor: "x", action, outcome: "success", target, ...extra }) as AuditEvent;

describe("audit sentences", () => {
  it("a skipped run.build row says so, with its reason as the detail; a failed build is unchanged", () => {
    const reason = "no image builder wired; base image dropped for the convention image";
    const skipped = ev("run.build", "", { outcome: "failure", data: { skipped: true, fail_closed: false, reason } });
    expect(describeTabEvent(skipped)).toBe("Skipped part of the sandbox image build");
    expect(describeTabAction(skipped)).toBe("Skipped part of the sandbox image build");
    expect(inboundDetail(skipped)).toBe(reason);
    expect(inboundDetail(skipped, true)).toBe(reason);
    const failed = ev("run.build", "", { outcome: "failure", data: { reason } });
    expect(describeTabEvent(failed)).toBe("Built the sandbox image");
    expect(inboundDetail(failed)).toBeNull();
  });

  it("says a failed session.recording.write row could not save the recording, with its reason as the detail; a success row is unchanged", () => {
    const failed = ev("session.recording.write", "run-1~agent", { outcome: "failure", data: { session: "agent", reason: "mask_gap" } });
    expect(describeTabEvent(failed)).toBe("Could not save the terminal recording — run-1~agent");
    expect(describeTabAction(failed)).toBe("Could not save the terminal recording");
    expect(inboundDetail(failed)).toBe("reason: mask_gap");
    expect(inboundDetail(ev("session.recording.write", "run-1~agent", { outcome: "failure" }))).toBeNull();
    const saved = ev("session.recording.write", "run-1~agent", { data: { session: "agent", reason: "ignored" } });
    expect(describeTabEvent(saved)).toBe("Recorded the terminal session — run-1~agent");
    expect(inboundDetail(saved)).toBeNull();
  });

  it("words a gateway token mint row on both tables, and leaves an outcome it does not know as the raw action", () => {
    const signed = ev("gateway_identity.mint", "corp-gateway");
    const refused = ev("gateway_identity.mint", "old-gateway", { outcome: "denied" });
    expect(describeEvent(signed)).toBe("Signed a gateway token for corp-gateway");
    expect(describeEvent(refused)).toBe("Refused a gateway token for old-gateway");
    expect(describeAction(signed)).toBe("Signed a gateway token");
    expect(describeTabEvent(signed)).toBe("Signed a gateway token for corp-gateway");
    expect(describeTabEvent(refused)).toBe("Refused a gateway token for old-gateway");
    expect(describeTabAction(refused)).toBe("Refused a gateway token for");
    expect(describeTabEvent(ev("gateway_identity.mint", ""))).toBe("Signed a gateway token for an unknown provider");
    expect(describeEvent(ev("gateway_identity.mint", "p", { outcome: "failure" }))).toBe("gateway_identity.mint — p");
    expect(describeTabEvent(ev("gateway_identity.mint", "p", { outcome: "failure" }))).toBe("gateway_identity.mint — p");
  });

  it("joins the target to the sentence for the whole line, and leaves it off the action", () => {
    expect(describeEvent(ev("run.create", "r1"))).toBe("Created the run — r1");
    expect(describeAction(ev("run.create", "r1"))).toBe("Created the run");
    expect(describeEvent(ev("egress.allow", "a.example"))).toBe("Allowed egress to a.example");
    expect(describeAction(ev("egress.allow", "a.example"))).toBe("Allowed egress");
    expect(describeEvent(ev("egress.deny"))).toBe("Denied egress to an unknown host");
    expect(describeAction(ev("llm.scan.flag", "h"))).toBe("LLM content scan (flag)");
    expect(describeEvent(ev("llm.scan.flag", "h"))).toBe("LLM content scan (flag) for h");
  });

  // #2972: one row per Pause-to-Resume session in a VM's desktop, labelled as the board lists it.
  it("labels a desktop sign-in session as pausing the agent to sign in inside the desktop", () => {
    expect(describeTabAction(ev("desktop.sign_in.pause", "r1"))).toBe("Paused the agent to sign in inside the desktop");
  });

  it("falls back to the raw key for an action it does not know", () => {
    expect(describeAction(ev("brand.new.thing"))).toBe("brand.new.thing");
    expect(describeEvent(ev("brand.new.thing", "t"))).toBe("brand.new.thing — t");
  });
});

// Mock packet rc.6 §2b: how dispatch set an AWS sign-in lane up, read from the recorded mode and reason.
describe("AWS signing line", () => {
  const br = (data: Record<string, unknown>) => awsSigningLine(ev("run.bedrock.configure", "r1", { data }));
  const cli = (data: Record<string, unknown>) => awsSigningLine(ev("run.aws_cli.configure", "c1", { data }));

  it("names the proxy, or the sandbox and the recorded reason", () => {
    expect(br({ mode: "sso-sigv4-proxy", aws_signing: "proxy" })).toEqual({ text: AUDIT.AWS_SIGNING_PROXY, sandbox: false });
    expect(br({ mode: "sso-inject-proxy", aws_signing: "sandbox", aws_signing_reason: "unsigned_host", aws_signing_host: "bedrock.corp.example.com" })?.text).toBe(
      "AWS signing set up in the sandbox, which gets role credentials: the proxy can't sign for bedrock.corp.example.com",
    );
    expect(cli({ aws_signing: "sandbox", aws_signing_reason: "port", aws_signing_host: "s3.eu-west-1.amazonaws.com:8443" })?.text).toBe(
      "AWS signing set up in the sandbox, which gets role credentials: s3.eu-west-1.amazonaws.com:8443 is not on port 443",
    );
    expect(cli({ aws_signing: "proxy" })?.text).toBe(AUDIT.AWS_SIGNING_PROXY);
  });

  it("takes the sign-in session's place from the mode, not the reason", () => {
    expect(br({ mode: "sso-inject", aws_signing: "sandbox", aws_signing_reason: "off" })?.text).toBe(
      "AWS signing set up in the sandbox, which gets the sign-in session and role credentials: signing at the proxy is off on this deployment",
    );
  });

  it("says an older row recorded no reason, and gives bearer, unknown modes and other rows no line", () => {
    expect(br({ mode: "sso-inject-proxy" })?.text).toBe("AWS signing set up in the sandbox, which gets role credentials: fallback reason not recorded");
    expect(br({ mode: "sso-sigv4-proxy" })?.text).toBe(AUDIT.AWS_SIGNING_PROXY);
    expect(br({ mode: "bearer" })).toBeNull();
    expect(br({ mode: "something-new" })).toBeNull();
    expect(br({ mode: "constructor" })).toBeNull();
    expect(cli({ aws_signing: "sandbox", aws_signing_reason: "toString" })?.text).toBe(AUDIT.AWS_SIGNING_SANDBOX("toString"));
    expect(br({})).toBeNull();
    expect(cli({})).toBeNull();
    expect(awsSigningLine(ev("run.create", "r1", { data: { aws_signing: "proxy" } }))).toBeNull();
    expect(describeTabAction(ev("run.aws_cli.configure", "c1"))).toBe("Configured the AWS CLI for the run");
  });
});

// Mock r15 T2-f: the service's rows, and the lease renewal's reason.
describe("service audit rows", () => {
  it("names the health and restart rows on the Audit screen and the run's Audit tab", () => {
    expect(describeAction(ev("run.service.health", "r1"))).toBe("Service health changed");
    expect(describeTabAction(ev("run.service.health", "r1"))).toBe("Service health changed");
    expect(describeAction(ev("run.service.restart", "r1"))).toBe("Restarted the service");
    expect(describeTabAction(ev("run.service.restart", "r1"))).toBe("Restarted the service");
  });

  it("words a run.end.set row by its reason, and leaves any other reason as the raw action", () => {
    const renewed = ev("run.end.set", "r1", { data: { reason: "service_renewed" } });
    expect(AUDIT.SERVICE_RENEWED).toBe("renewed the service's lease");
    expect(describeAction(renewed)).toBe("Renewed the service's lease");
    expect(describeTabAction(renewed)).toBe("Renewed the service's lease");
    expect(describeEvent(renewed)).toBe("Renewed the service's lease — r1");
    expect(describeAction(ev("run.end.set", "r1", { data: { reason: "limits_reclamped" } }))).toBe("run.end.set");
    expect(describeTabAction(ev("run.end.set", "r1"))).toBe("run.end.set");
  });

  // r15 T2-f: the two inbound rows, as the mock words them; the app is the object, the request is the second line.
  it("words an inbound call by its decision, with the app as the object and the request as the detail", () => {
    const allow = ev("inbound.allow", "api", { data: { method: "GET", path: "/v1/invoices", via: "token", caller: "service:billing" } });
    const deny = ev("inbound.deny", "api", { outcome: "denied", data: { reason: "credential_invalid", via: "token", method: "POST", path: "/v1/refunds" } });
    expect(AUDIT.INBOUND_ALLOW).toBe("Allowed inbound");
    expect(AUDIT.INBOUND_DENY).toBe("Denied inbound");
    expect(describeEvent(allow)).toBe("Allowed inbound to api");
    expect(describeAction(allow)).toBe("Allowed inbound");
    expect(describeEvent(deny)).toBe("Denied inbound to api");
    expect(describeEvent(ev("inbound.deny"))).toBe("Denied inbound to an unknown app");
    expect(describeTabAction(allow)).toBe("Allowed inbound to");
    expect(describeTabEvent(deny)).toBe("Denied inbound to api");
    expect(inboundDetail(allow)).toBe("GET /v1/invoices · via token · caller service:billing");
    expect(inboundDetail(deny)).toBe("reason credential_invalid · via token · POST /v1/refunds");
    expect(inboundDetail(ev("inbound.deny", "api", { data: { reason: "not_found", via: "session", method: "GET", path: "/" } }))).toBe("reason not_found · via session · GET /");
  });

  it("ends an inbound detail with the run's short id on request, names no caller for a refusal with no actor, and labels the service rows", () => {
    const allow = ev("inbound.allow", "api", { run_id: "run_7c41e0a9-aaaa", data: { method: "GET", path: "/v1/invoices", via: "token", caller: "service:billing" } });
    expect(inboundDetail(allow, true)).toBe("GET /v1/invoices · via token · caller service:billing · 7c41e0a9");
    expect(inboundDetail(allow)).toBe("GET /v1/invoices · via token · caller service:billing");
    expect(inboundDetail(ev("inbound.allow", "api", { run_id: "run_7c41e0a9-aaaa" }), true)).toBeNull();
    expect(hasNoCaller(ev("inbound.deny", "api", { actor: "" }))).toBe(true);
    expect(hasNoCaller(ev("inbound.deny", "api", { actor: "service:billing" }))).toBe(false);
    expect(hasNoCaller(ev("inbound.allow", "api", { actor: "" }))).toBe(false);
    expect([AUDIT.NO_CALLER, AUDIT.SOURCE_SANDBOX, AUDIT.SOURCE_SERVER, AUDIT.OUTCOME_ALLOW, AUDIT.OUTCOME_DENY]).toEqual(["no caller", "sandbox", "server", "allow", "deny"]);
    expect(serviceSource(ev("run.service.health"))).toBe("sandbox");
    expect(serviceSource(ev("run.service.restart"))).toBe("sandbox");
    expect(serviceSource(ev("run.end.set", "r1", { data: { reason: "service_renewed" } }))).toBe("server");
    expect(serviceSource(ev("run.end.set", "r1", { data: { reason: "limits_tightened" } }))).toBeNull();
    expect(serviceSource(ev("run.create"))).toBeNull();
  });

  it("leaves out an inbound field the row lacks, and gives no detail to any other row", () => {
    expect(inboundDetail(ev("inbound.allow", "api", { data: { method: "GET", path: "/" } }))).toBe("GET /");
    expect(inboundDetail(ev("inbound.allow", "api"))).toBeNull();
    expect(inboundDetail(ev("egress.allow", "a.example", { data: { method: "GET", path: "/" } }))).toBeNull();
  });

  // A destructive admin verb reads by its outcome, so a failed or refused row never says it was done above its badge.
  it.each([
    ["person.erasure", "Erased a person's data", "Person data erasure failed", "Person data erasure refused"],
    ["credential.erase", "Erased a person's stored credentials", "Credential erasure failed", "Credential erasure refused"],
    ["person.deprovision", "Deprovisioned a person", "Person deprovisioning failed", "Person deprovisioning refused"],
    ["service_identity.retire", "Retired a service identity", "Retiring a service identity failed", "Retiring a service identity refused"],
    ["token.revoke", "Revoked an API token", "Revoking an API token failed", "Revoking an API token refused"],
    ["session.revoke", "Signed a person out everywhere", "Signing a person out failed", "Signing a person out refused"],
    ["access.role_mapping.delete", "Removed a role mapping", "Removing a role mapping failed", "Removing a role mapping refused"],
    ["template.delete", "Deleted a template", "Deleting a template failed", "Deleting a template refused"],
    ["audit.retention.partition_dropped", "Dropped an audit partition", "Dropping an audit partition failed", "Dropping an audit partition refused"],
    ["scim.user.deactivate", "Deactivated by your identity provider", "Deactivation by your identity provider failed", "Deactivation by your identity provider refused"],
  ])("%s reads by its outcome", (action, done, failed, refused) => {
    expect(describeTabEvent(ev(action, "t1"))).toBe(`${done} — t1`);
    expect(describeTabEvent(ev(action, "t1", { outcome: "failure" }))).toBe(`${failed} — t1`);
    expect(describeTabEvent(ev(action, "t1", { outcome: "denied" }))).toBe(`${refused} — t1`);
    expect(describeTabAction(ev(action, "t1", { outcome: "failure" }))).toBe(failed);
  });

  it("a person.erasure that failed on an unknown scope is not worded as an erasure", () => {
    const row = ev("person.erasure", "priya@acme.io", { outcome: "failure", data: { reason: "erasure_scope_unknown" } });
    expect(describeTabEvent(row)).toBe("Person data erasure failed — priya@acme.io");
  });

  it("a success row that kept some data reads as partial, not as completed", () => {
    expect(describeTabEvent(ev("person.erasure", "p", { data: { not_erased: { drives: 1 } } }))).toBe("Erased part of a person's data — p");
    expect(describeTabEvent(ev("person.deprovision", "p", { data: { not_erased: { drives: 1 } } }))).toBe("Deprovisioned a person; some data was kept — p");
    expect(describeTabEvent(ev("person.erasure", "p", { data: { scopes: ["credentials"] } }))).toBe("Erased a person's data — p");
  });
});

describe("a prompt left to a model gateway", () => {
  const left = (action: string, deferred?: string) => ev(action, "gateway.example.com", deferred ? { data: { deferred_to: deferred } } : {});

  it("reads as left to the provider on both surfaces, only for the skip row that names one", () => {
    expect(AUDIT.SCAN_LEFT_TO).toBe("Left prompt checks to");
    expect(describeEvent(left("llm.scan.skip", "corp-gateway"))).toBe("Left prompt checks to corp-gateway");
    expect(describeTabEvent(left("llm.scan.skip", "corp-gateway"))).toBe("Left prompt checks to corp-gateway");
    expect(describeEvent(left("llm.scan.skip"))).toBe("LLM content scan (skip) for gateway.example.com");
    expect(describeEvent(left("llm.scan.block", "corp-gateway"))).toBe("LLM content scan (block) for gateway.example.com");
  });
});

describe("the unit admin's Audit copy", () => {
  const TAIL =
    ", and the changes made to those units. Sign-ins aren't listed here, and a run's row leaves out names, paths and error text.";

  it("names one unit, and says the units below it", () => {
    expect(AUDIT.unitNoteBody(["payments"])).toBe(`Showing runs in \`payments\` and the units below it${TAIL}`);
  });

  it("lists up to three units, comma separated, below them", () => {
    expect(AUDIT.unitNoteBody(["a", "b", "c"])).toBe(`Showing runs in \`a\`, \`b\`, \`c\` and the units below them${TAIL}`);
  });

  it("names three of five units and counts the other two", () => {
    expect(AUDIT.unitNoteBody(["a", "b", "c", "d", "e"])).toBe(
      `Showing runs in \`a\`, \`b\`, \`c\` and 2 more and the units below them${TAIL}`,
    );
  });

  it("names the first unit in the empty state", () => {
    expect(AUDIT.unitEmptyBody("payments")).toBe(
      "Every egress decision, brokered credential, approval and enforcement action of a run in `payments` or below it appears here once that run starts.",
    );
  });

  it("holds the fixed unit texts", () => {
    expect(AUDIT.UNIT_DESCRIPTION).toBe(
      "Append-only. Every egress decision, brokered credential, approval and enforcement action, across the runs in your units.",
    );
    expect(AUDIT.UNIT_NOTE_TITLE).toBe("Runs in your units");
    expect(AUDIT.UNIT_EMPTY_TITLE).toBe("No runs in your units yet.");
    expect(AUDIT.UNIT_DRILL_TITLE).toBe("This run is in your unit");
    expect(AUDIT.UNIT_DRILL_BODY).toBe("Its page isn't open to you. The trail below is what the audit view can show.");
    expect(AUDIT.UNIT_DRILL_BACK).toBe("Back to all runs in your units");
  });
});
