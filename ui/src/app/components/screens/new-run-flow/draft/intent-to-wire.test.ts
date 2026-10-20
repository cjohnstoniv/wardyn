/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { runs } from "../../../../lib/api/runs";
import { launchGateOpen } from "./launch-gate";
import { TEMPLATE_POLICY_FIELDS, TEMPLATE_REQUEST_FIELDS } from "../../../../lib/template-fields";
import { intentToWire, wireSerializer } from "./intent-to-wire";
import type { RunIntent } from "./run-intent";

// A document-loaded intent: carriers, a document-only end, a policy overlay
// and explicit false/0/[] choices.
const INTENT: RunIntent = {
  shape: "agent_task",
  experience: "background",
  workload: { kind: "agent_task", agent: "claude-code", task: "fix the tests" },
  tools: [{ id: "claude-code", kind: "harness" }],
  runner_pool_id: "c1111111-1111-4111-8111-111111111111",
  ends_after_sec: 5400,
  devcontainer_repo: "org/app",
  no_repositories_or_drives: true,
  components: [],
  inline_policy: { allowed_domains: [], auto_stop_after_sec: 0 },
};

let sent: { path: string; body: Record<string, unknown> }[] = [];
beforeEach(() => {
  sent = [];
  vi.stubGlobal("fetch", vi.fn((url: RequestInfo | URL, init?: RequestInit) => {
    sent.push({ path: new URL(String(url), "http://localhost").pathname, body: JSON.parse(String(init?.body)) });
    return Promise.resolve(new Response(JSON.stringify({ id: "r", setup_items: [], enforced_confinement_class: "CC2" }), {
      status: 200, headers: { "Content-Type": "application/json" },
    }));
  }));
});
afterEach(() => vi.unstubAllGlobals());

const without = (body: Record<string, unknown>, ...keys: string[]) =>
  Object.fromEntries(Object.entries(body).filter(([k]) => !keys.includes(k)));

describe("intentToWire (U1)", () => {
  it("preflight, policy preview and create send the same body, create adding only title and description", async () => {
    await runs.preflightRunWire(intentToWire(INTENT).body);
    await runs.previewRunPolicyWire(intentToWire(INTENT).body);
    await runs.createRunWire(intentToWire(INTENT, { title: "Nightly", description: "Why it runs" }).body);
    const [preflight, preview, create] = sent;
    expect(sent.map((s) => s.path)).toEqual(["/api/v1/runs/preflight", "/api/v1/runs/policy-preview", "/api/v1/runs"]);
    expect(preview.body).toEqual(preflight.body);
    expect(without(create.body, "title", "description")).toEqual(preflight.body);
    expect(create.body).toMatchObject({ title: "Nightly", description: "Why it runs" });
    expect(preflight.body).not.toHaveProperty("title");
    expect(preflight.body).not.toHaveProperty("description");
  });

  it("always marks the 0.9 contract and forwards every present key unchanged, document-only ones included", async () => {
    await runs.preflightRunWire(intentToWire(INTENT).body);
    const { inline_policy: overlay, ...rest } = INTENT;
    expect(sent[0].body).toEqual({ ...rest, policy_overlay: overlay, mode_contract: "0.9" });
    expect(sent[0].body.ends_after_sec).toBe(5400);
    expect(sent[0].body.devcontainer_repo).toBe("org/app");
  });

  it("sends the document's inline_policy as the policy overlay, never as a whole policy", () => {
    const { body } = intentToWire(INTENT);
    expect(body.policy_overlay).toEqual({ allowed_domains: [], auto_stop_after_sec: 0 });
    expect(body).not.toHaveProperty("inline_policy");
    expect(intentToWire({}).body).toEqual({ mode_contract: "0.9" });
  });

  it("carries the Tools and access carriers as the person set them", () => {
    const tools = { tools: [{ id: "claude-code", kind: "harness", tool_rules: [{ tool: "Bash", effect: "hold" }], default_effect: "deny" }] };
    const access = {
      components: [{ id: "mcp-docs", mcp_harnesses: ["claude-code"] }],
      overrides: { hosts: { add_allowed: ["api.example"] }, agent: { add_secrets: [{ secret_name: "k", host: "api.example" }] } },
    };
    const { body, unsupported } = intentToWire({ ...tools, ...access } as unknown as RunIntent);
    expect(body).toEqual({ ...tools, ...access, mode_contract: "0.9" });
    expect(unsupported).toEqual([]);
  });

  it("keeps absent keys absent and sends explicit false, 0 and [] as choices", () => {
    const { body } = intentToWire({ confinement_class: undefined, no_end: false, components: [], resources: { cpu_millis: 0 } });
    expect(body).toEqual({ mode_contract: "0.9", no_end: false, components: [], resources: { cpu_millis: 0 } });
    expect(body).not.toHaveProperty("confinement_class");
  });

  it("sends a component's explicit empty mcp_harnesses as the choice of no harness", () => {
    const components = [{ id: "mcp-docs", mcp_harnesses: [] }, { id: "mcp-other" }];
    expect(intentToWire({ components } as unknown as RunIntent).body.components).toEqual(components);
  });

  it("never clamps or defaults: the chosen class goes as chosen beside a stronger policy floor", () => {
    const { body } = intentToWire({ confinement_class: "CC1", inline_policy: { min_confinement_class: "CC3" } });
    expect(body.confinement_class).toBe("CC1");
  });

  it("adds a launch's description only when one was typed", () => {
    expect(intentToWire({}, { title: "T", description: "" }).body).toEqual({ mode_contract: "0.9", title: "T" });
  });
});

describe("intentToWire unsupported fields (U2)", () => {
  const odd = { ...INTENT, preset: "nightly", not_a_field: 1, inline_policy: { allowed_domains: ["a.example"], nope: true } } as unknown as RunIntent;

  it("reports a present key the registry does not carry, by path, instead of dropping it silently", () => {
    const { body, unsupported } = intentToWire(odd);
    expect(unsupported.sort()).toEqual(["inline_policy.nope", "not_a_field", "preset"]);
    expect(body).not.toHaveProperty("not_a_field");
    expect(body.policy_overlay).toEqual({ allowed_domains: ["a.example"] });
  });

  it("keeps Launch shut while an unsupported path is reported, even beside a fresh, current preflight", () => {
    const { unsupported } = intentToWire(odd);
    const now = Date.now();
    const gate = (blockers: number) => launchGateOpen({ blockers, preflight: { generation: 3, at: now }, generation: 3, now });
    expect(gate(unsupported.length)).toBe(false);
    expect(gate(intentToWire(INTENT).unsupported.length)).toBe(true);
  });
});

describe("the overlay's registry row, under either name", () => {
  const overlay = { inline_policy: { allowed_domains: ["a.example"] } } satisfies RunIntent;
  const rows = (rename: (name: string) => string | undefined) =>
    TEMPLATE_REQUEST_FIELDS.flatMap((r) => (rename(r.name) ? [{ ...r, name: rename(r.name) as string }] : []));

  it("sends the overlay when the registry names its row inline_policy", () => {
    expect(wireSerializer(rows((n) => n), TEMPLATE_POLICY_FIELDS)(overlay)).toEqual({
      body: { policy_overlay: { allowed_domains: ["a.example"] }, mode_contract: "0.9" }, unsupported: [],
    });
  });

  it("sends the overlay when the registry names its row policy_overlay", () => {
    const renamed = rows((n) => (n === "inline_policy" ? "policy_overlay" : n));
    expect(wireSerializer(renamed, TEMPLATE_POLICY_FIELDS)(overlay)).toEqual({
      body: { policy_overlay: { allowed_domains: ["a.example"] }, mode_contract: "0.9" }, unsupported: [],
    });
  });

  it("reports the overlay when the registry carries neither", () => {
    const without = rows((n) => (n === "inline_policy" ? undefined : n));
    expect(wireSerializer(without, TEMPLATE_POLICY_FIELDS)(overlay)).toEqual({ body: { mode_contract: "0.9" }, unsupported: ["inline_policy"] });
  });
});

describe("intentToWire: a start later", () => {
  it("adds start_at and start_time_zone to the create only, beside the Name dialog's fields", () => {
    const start = { start_at: "2026-10-21T02:00", start_time_zone: "Europe/London" };
    const create = intentToWire(INTENT, { title: "Nightly", description: "", start }).body;
    expect(create).toMatchObject({ title: "Nightly", ...start });
    const plain = intentToWire(INTENT, { title: "Nightly", description: "" }).body;
    expect(plain).not.toHaveProperty("start_at");
    expect(plain).not.toHaveProperty("start_time_zone");
    expect(intentToWire(INTENT).body).not.toHaveProperty("start_at");
  });

  it("reports a start in the document as unsupported: the registry leaves it to each launch", () => {
    expect(intentToWire({ ...INTENT, start_at: "2026-10-21T02:00" } as RunIntent).unsupported).toContain("start_at");
  });
});
