/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M-NR-2: the Access panel's frames (/m-nr/access/…).
import { NEW_RUN_REASON, WORKSPACE_REFUSAL } from "../new-run-refusals";
import type { ActiveSections, RunContractDraft } from "../run-contract-draft";
import type { ComponentFact } from "../types/components";
import {
  adoComponent,
  agentComponent,
  customComponent,
  githubComponent,
  noContract,
  preflight,
  preview,
  RESOURCES_ORG,
  resourcesFor,
  RUNNER_ONLINE,
  sources,
  type FixtureProvenance,
  type FixtureBody,
  type NewRunFixture,
} from "./base";

const preferred = { preview: preview({ components: [agentComponent()], resources: [RESOURCES_ORG] }) };
const withComponents = (components: ComponentFact[], over: Partial<NewRunFixture["preview"]> = {}) =>
  preview({ components: [agentComponent(), ...components], resources: [RESOURCES_ORG], ...over });
const active = (over: Partial<ActiveSections> = {}): ActiveSections => ({ agent: true, azureDevOps: false, gitPATHosts: [], pushKeys: [], ...over });
const contract = (overrides: RunContractDraft["access"]["overrides"]): RunContractDraft => ({ ...noContract(), access: { overrides } });
const none = { gitPAT: [], pushRules: [] };
const rProvider = (route: string, note: string, fact: Parameters<typeof agentComponent>[1], pending: NewRunFixture["preview"]["pending"] = []): FixtureBody => ({
  route,
  note,
  preview: withComponents([], { components: [agentComponent({}, fact)], pending }),
});

const patFact = (over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "git_pat",
  id: "git_pat:git.example.com",
  name: "git.example.com",
  reason: "workspace",
  status: "unknown",
  requirements: [],
  org: "git.example.com",
  repos: ["https://git.example.com/platform/tools"],
  repo_access: [{ repo: "platform/tools", access: "read", can_write: true }],
  ...over,
});

const hostSources: FixtureProvenance[] = sources(["api.anthropic.com", "model_provider"], ["pastebin.com", "person"], ["api.example.com", "component", "Internal API"]);
const clampedHost: FixtureProvenance = { field: "allowed_domains", value: "pastebin.com", source: { kind: "person" }, effect: "clamped" };

export const ACCESS_FIXTURES: FixtureBody[] = [
  { route: "access/agent", note: "The agent section: provider, hosts, secret, managed settings, telemetry.", ...preferred },
  rProvider("access/agent-r1", "R1: a single eligible provider; a static line, no picker.", {}),
  rProvider("access/agent-r2", "R2: a default and others.", {}),
  rProvider("access/agent-r3", "R3: the person is not connected; Needs input, never blocking.", { secrets: [{ kind: "aws", owner: "own", residency: "sandbox" }] }, ["credential_liveness"]),
  rProvider("access/agent-r4", "R4: residency sentence after the secret line.", {}),
  rProvider("access/agent-r5", "R5b: no provider granted for the agent.", { model_provider: undefined, hosts: [], secrets: [] }, ["model_provider_selection"]),
  rProvider("access/agent-r5c", "R5c: the default provider is turned off (an extra frame beside r5).", { model_provider: undefined, hosts: [], secrets: [] }, ["model_provider_selection"]),
  rProvider("access/agent-r6", "R6: several, no default; the person must choose.", { model_provider: undefined, hosts: [], secrets: [] }, ["model_provider_selection"]),
  rProvider("access/agent-r7", "R7: the harness switch moved the pick; announced once on Run.", {}),
  rProvider("access/agent-r8", "R8: the provider serves the next harness; kept silently.", {}),
  rProvider("access/agent-r9", "R9: no provider block, or none serves the harness.", { model_provider: undefined, hosts: [], secrets: [] }),
  {
    route: "access/agent-locked-l2",
    note: "Managed settings, locked L2 (B-P2) by the person's profile.",
    preview: withComponents([], {
      components: [agentComponent({}, { managed_settings: { path: "/etc/claude-code/managed-settings.json", document: '{\n  "allowManagedHooksOnly": true,\n  "allowManagedPermissionRulesOnly": true\n}\n', locked: true, locked_by: "profile", profile: "Contractors" } })],
    }),
  },
  { route: "access/agent-no-doc", note: "Claude Code with no managed document at this autonomy level.", preview: withComponents([], { components: [agentComponent({}, { managed_settings: undefined })] }) },
  { route: "access/codex", note: "Codex CLI: no managed-settings entry; Hold disabled.", preview: withComponents([], { components: [agentComponent({ id: "agent:codex-cli" }, { agent: "codex-cli", managed_settings: undefined, telemetry: undefined })] }) },
  { route: "access/git-github", note: "A GitHub App section: repositories with per-repository access, the App lane.", preview: withComponents([githubComponent()]), provenance: sources(["github.com", "workspace", "payments"]) },
  {
    route: "access/git-github-app-missing",
    note: "The GitHub App is not installed on the organisation: operators get the install link, members a sentence.",
    preview: withComponents([githubComponent({ status: "needs_input", lane: "direct", install_url: "https://github.com/apps/wardyn-acme/installations/new" })]),
  },
  {
    route: "access/git-ado-minted",
    note: "Azure DevOps, per-person sign-in with a minted token; capabilities in the section.",
    preview: withComponents([adoComponent({ push_rules: { deny_paths: [".github/workflows/**"] } })]),
    active: active({ azureDevOps: true, pushKeys: ["azure_devops/globex"] }),
  },
  {
    route: "access/git-ado-own-pat",
    note: "Azure DevOps with the person's own token: each scope names the capabilities it covers (#1880).",
    preview: withComponents([
      adoComponent({
        token_mode: "own_pat",
        token_scopes: [
          { scope: "Code (Read & write)", covers: ["code_read", "code_write", "pr_write", "policy_admin"] },
          { scope: "Project and Team (Read)", covers: ["project_read"] },
        ],
      }),
    ]),
  },
  { route: "access/git-other-forge", note: "A git_pat repository on another forge: a 'Git · {host}' section.", preview: withComponents([patFact()]), active: active({ gitPATHosts: ["git.example.com"] }) },
  {
    route: "access/git-push-rules",
    note: "Push rules per provider and organisation: the source's read-only, the person's added.",
    preview: withComponents([githubComponent({ push_rules: { deny_paths: [".github/workflows/**"], require_review_paths: ["infra/"] } })]),
    contract: contract({ ...none, pushRules: [{ provider: "github", org: "acme", deny_paths: ["secrets/"] }] }),
    active: active({ pushKeys: ["github/acme"] }),
  },
  { route: "access/custom-needs-input", note: "A custom component that needs a secret: blocks Launch.", preview: withComponents([customComponent("Internal API", { status: "needs_input", requirements: [{ id: "secret:internal-key", kind: "secret", label: "internal-key", required_by: "Internal API", status: "missing" }] })]) },
  { route: "access/custom-refused", note: "A refused component keeps its section with the server's sentence and Remove.", preview: withComponents([customComponent("Internal API", { status: "unavailable" })]) },
  { route: "access/source-policy", note: "A source policy that grants something no component explains.", preview: withComponents([], { spec: { allowed_domains: ["api.anthropic.com", "registry.example.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" } }), provenance: sources(["registry.example.com", "policy", "Team default"]) },
  { route: "access/empty", note: "A shell run with nothing to show: 'Nothing needs access yet'.", preview: preview({ resources: [RESOURCES_ORG] }) },
  { route: "access/add", note: "The Add access catalog.", ...preferred },
  { route: "access/add-local", note: "Catalog items with the 'Not available on your runner' chip (P4/P5/P7 only).", runners: [RUNNER_ONLINE], preview: withComponents([], { resources: [RESOURCES_ORG, resourcesFor(RUNNER_ONLINE)], local_placement: [{ kind: "component", key: "org-component", local_placeable: false, reason: "placement_credential" }] }) },
  { route: "access/add-empty", note: "An empty catalog.", ...preferred },
  { route: "access/add-builtin-swap", note: "A built-in item swaps the catalog for the manual Git form in the same dialog.", ...preferred },
  {
    route: "access/manual-github",
    note: "Manual GitHub: repositories required, always brokered. The request carries a built-in ref; a server that cannot honour it yet refuses (request_field_unavailable).",
    preview: withComponents([githubComponent({ id: "git_provider:github:app", reason: "inline" })]),
    refusal: { status: 422, reason: NEW_RUN_REASON.REQUEST_FIELD_UNAVAILABLE, text: "components[0]: built-in github access is not available on this server yet, so the run was not created." },
  },
  { route: "access/manual-ado", note: "Manual Azure DevOps with repositories.", preview: withComponents([adoComponent({ reason: "inline" })]) },
  { route: "access/manual-ado-every-repo", note: "Manual Azure DevOps without repositories: 'Every repository you can reach in {org}'.", preview: withComponents([adoComponent({ reason: "inline", repos: [] })]) },
  {
    route: "access/local-chips",
    note: "On My runner: host placeability and each org-held secret's OD-12 delivery word.",
    runners: [RUNNER_ONLINE],
    preview: withComponents([customComponent("Org API", { reason: "org", secrets: [{ delivery: "header", shared: true, local_delivery: "via_org" }, { delivery: "env", shared: true, local_delivery: "refuse" }] })], {
      resources: [RESOURCES_ORG, resourcesFor(RUNNER_ONLINE)],
      local_placement: [{ kind: "host", key: "api.example.com", local_placeable: false, reason: "placement_local_path" }],
    }),
  },
  { route: "access/git-two-sections", note: "Two Git sections (GitHub and a git_pat forge), each with its own push rules.", preview: withComponents([githubComponent(), patFact()]), active: active({ gitPATHosts: ["git.example.com"], pushKeys: ["github/acme"] }) },
  {
    route: "access/git-inactive",
    note: "A section whose workspace was removed stays inactive: its edits are kept in the draft and not sent.",
    preview: withComponents([]),
    contract: contract({ ...none, pushRules: [{ provider: "github", org: "acme", deny_paths: ["infra/"] }] }),
    active: active(),
  },
  {
    route: "access/git-inactive-push-moves",
    note: "Push rules are keyed by provider and organisation, so removing one organisation's section leaves only its own overrides inactive.",
    preview: withComponents([githubComponent({ org: "other", id: "git_provider:github:app:other" })]),
    contract: contract({ ...none, pushRules: [{ provider: "github", org: "acme", deny_paths: ["infra/"] }, { provider: "github", org: "other", deny_paths: ["ci/"] }] }),
    active: active({ pushKeys: ["github/other"] }),
  },
  {
    route: "access/git-ado-two-orgs",
    note: "Two Azure DevOps organisations in one run are refused.",
    preview: withComponents([adoComponent(), adoComponent({ id: "git_provider:azure_devops:entra:initech", org: "initech" })]),
    refusal: { status: 422, reason: NEW_RUN_REASON.WORKSPACE_ADO_ORG_CONFLICT, text: WORKSPACE_REFUSAL.ADO_ORG_CONFLICT("payments", "globex", "docs", "initech") },
  },
  {
    route: "access/agent-add-host",
    note: "Add host: an override on the agent component, ceiling-clamped widening.",
    preview: withComponents([]),
    contract: contract({ ...none, agent: { add_hosts: ["api.example.com"] } }),
    active: active(),
    provenance: hostSources,
  },
  {
    route: "access/agent-add-secret",
    note: "Add secret: a stored secret injected as a request header on one host.",
    preview: withComponents([]),
    contract: contract({ ...none, agent: { add_secrets: [{ secret_name: "my-api-key", host: "api.example.com" }] } }),
    active: active(),
  },
  {
    route: "access/agent-host-clamped",
    note: "A host the ceiling dropped: struck, with 'Not allowed by your ceiling'.",
    preview: withComponents([]),
    contract: contract({ ...none, agent: { add_hosts: ["pastebin.com"] } }),
    active: active(),
    provenance: [clampedHost],
  },
  { route: "access/narrow", note: "Narrow widths: same DOM order, wrapping buttons.", preview: withComponents([githubComponent(), adoComponent()]), preflight: preflight({ components: [agentComponent(), githubComponent(), adoComponent()] }) },
];
