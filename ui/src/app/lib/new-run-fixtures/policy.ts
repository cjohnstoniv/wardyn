/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M-NR-3: the Policy panel as the review (/m-nr/policy/…). Each frame is a
// resolved preview plus the provenance rows the chips are drawn from.
import { adoComponent, agentComponent, emptySpec, preview, RESOURCES_ORG, type FixtureProvenance, type FixtureBody } from "./base";

const row = (field: string, value: string, source: FixtureProvenance["source"], effect: FixtureProvenance["effect"] = "added"): FixtureProvenance => ({ field, value, source, effect });
const sourcePolicy = { kind: "policy" as const, name: "Team default" };
const person = { kind: "person" as const };
const ceiling = { kind: "ceiling" as const };

const resolved = preview({
  spec: emptySpec({ allowed_domains: ["api.anthropic.com", "registry.npmjs.org", "api.stripe.com"], denied_domains: ["corp.internal"] }),
  source: { kind: "stored", name: "Team default", policy_id: "55555555-5555-4555-8555-555555555555" },
  components: [agentComponent()],
  resources: [RESOURCES_ORG],
});

const standard: FixtureProvenance[] = [
  row("allowed_domains", "api.anthropic.com", { kind: "model_provider", name: "Anthropic API" }),
  row("allowed_domains", "registry.npmjs.org", sourcePolicy),
  row("allowed_domains", "api.stripe.com", { kind: "workspace", id: "ws-payments", name: "payments" }),
  row("denied_domains", "corp.internal", ceiling),
];

export const POLICY_FIXTURES: FixtureBody[] = [
  { route: "policy/review-default", note: "Default mode: the resolved policy with a chip per entry.", preview: { ...resolved, source: { kind: "default" } }, provenance: standard },
  { route: "policy/review-saved", note: "Saved mode.", preview: resolved, provenance: standard },
  { route: "policy/review-custom", note: "Custom mode: the raw editor is the advanced path.", preview: { ...resolved, source: { kind: "inline" } }, provenance: standard },
  {
    route: "policy/only-changed",
    note: "'Only what I changed' keeps added, changed, narrowed, removed and clamped entries.",
    preview: resolved,
    provenance: [...standard, row("allowed_domains", "api.example.com", person), row("min_confinement_class", "CC3", person, "narrowed")],
  },
  { route: "policy/only-changed-empty", note: "Nothing changed for this run.", preview: resolved, provenance: standard },
  { route: "policy/clamped", note: "An entry the person asked for and the ceiling dropped: struck, with both chips.", preview: resolved, provenance: [...standard, row("allowed_domains", "pastebin.com", person, "clamped")] },
  { route: "policy/removed", note: "A value the person removed: struck, 'Changed by you'.", preview: resolved, provenance: [...standard, row("azure_devops_capabilities", "policy_admin", person, "removed")] },
  {
    route: "policy/narrowed",
    note: "A capability set narrowed by the person.",
    preview: { ...resolved, components: [agentComponent(), adoComponent({ capabilities: ["code_read"] })], spec: { ...resolved.spec, azure_devops_capabilities: ["code_read"] } },
    provenance: [...standard, row("azure_devops_capabilities", "code_write", person, "narrowed")],
  },
  { route: "policy/changed-scalar", note: "A scalar the person changed: CPU 4 instead of the default 2.", preview: { ...resolved, spec: { ...resolved.spec, resources: { cpu_millis: 4000 } } }, provenance: [...standard, row("resources.cpu_millis", "4000", person, "narrowed")] },
  {
    route: "policy/multi-source",
    note: "One host with several sources shows each chip.",
    preview: resolved,
    provenance: [...standard, row("allowed_domains", "api.stripe.com", sourcePolicy), row("allowed_domains", "api.stripe.com", { kind: "component", name: "Internal API" })],
  },
  { route: "policy/redacted", note: "A member's redacted read: the source chip still shows.", preview: { ...resolved, redacted: true }, provenance: standard },
  { route: "policy/stale", note: "A stale preview keeps the last spec and its chips together.", preview: resolved, provenance: standard },
  { route: "policy/invalid", note: "An invalid source: no chips presented as current; the filter is disabled.", preview: { ...resolved, warnings: ["policy does not parse"] } },
  { route: "policy/editing", note: "Custom editing; the starter no longer seeds model-provider hosts.", preview: { ...resolved, spec: emptySpec({ allowed_domains: [] }), source: { kind: "inline" } } },
  { route: "policy/templates", note: "New Run templates build without providers.", preview: { ...resolved, spec: emptySpec({ allowed_domains: [] }), source: { kind: "inline" } } },
];
