/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Component wire types (#1914). Types only: nothing here is imported at run time.
// Every interface mirrors one Go struct and wire-parity.test.ts pins its json
// tags to that struct's source, so a Go rename fails a TS test.
//
//   Component, ComponentDefinition, ComponentSecret, ComponentDelivery, ComponentRef,
//   ComponentCLI, CLIGitHub, CLIAWS, CLIAzure
//                        internal/types/component.go (aliased by pkg/client/types.go)
//   ComponentRequest, ComponentRequirement, ComponentSaved, ComponentSecretView,
//   OrgComponentView, CLIView, MCPView, CatalogueCLI, MyComponents
//                        pkg/client/components.go
//   ComponentFact, ComponentSecretFact
//                        internal/api/components_facts.go (the `components` of
//                        POST /runs/preflight and POST /runs/policy-preview)

import type { AutonomyLevel } from "../api/governance";
import type { AgentFact, LocalDeliveryMode, RepoAccessFact, TokenScopeFact } from "./new-run-contract";
import type { PushRulesSpec } from "./policy";
import type { SetupItem } from "./preflight";

/** How one component secret reaches the run. */
export type ComponentDeliveryMode = "header" | "env" | "file";

/** Which of host/header/format/plain_http (header), var (env) or file (file) apply follows `mode`. */
export interface ComponentDelivery {
  mode: ComponentDeliveryMode;
  // header: one bare host (no port, no wildcard) of the definition's hosts.
  host?: string;
  header?: string;
  format?: string;
  // Org rows only: lets a header credential travel without TLS.
  plain_http?: boolean;
  // env: the environment variable name.
  var?: string;
  // file: a single file name, never a path.
  file?: string;
}

/** One secret a component carries: its NAME, never its value. */
export interface ComponentSecret {
  secret_name: string;
  // Org rows only, header delivery only: the operator's value, not the launcher's.
  shared?: boolean;
  delivery: ComponentDelivery;
}

export interface ComponentDefinition {
  // One plain line on the component's card.
  description?: string;
  hosts: string[];
  secrets?: ComponentSecret[];
  // Plain, non-secret environment.
  config?: Record<string, string>;
  // Org rows only: marks the row as a CLI credential card.
  cli?: ComponentCLI;
  // Makes the component an MCP server: a remote one, reached only through the proxy's broker route, or one its harness starts in the sandbox.
  mcp?: ComponentMCP;
}

/**
 * An MCP server. http: a remote one, at its streamable HTTP `url` (its host one of the component's hosts).
 * stdio: one its harness starts inside the run's sandbox from `launch`, with no url.
 */
export interface ComponentMCP {
  url?: string;
  transport: "http" | "stdio";
  launch?: ComponentMCPLaunch;
}

/** A local MCP server's command and arguments, started with no shell. */
export interface ComponentMCPLaunch {
  command: string;
  args?: string[];
}

export type CLIKind = "azure" | "aws" | "github";

/** The credential an org row gives a run's CLI; `kind` names the one arm that is set. */
export interface ComponentCLI {
  kind: CLIKind;
  azure?: CLIAzure;
  aws?: CLIAWS;
  github?: CLIGitHub;
}

export interface CLIGitHub {
  capabilities: string[];
}

export interface CLIAWS {
  start_url: string;
  sso_region: string;
  // 12 digits.
  account_id: string;
  role_name: string;
  region: string;
}

export interface CLIAzure {
  // 1 to 16 subscription GUIDs.
  subscriptions: string[];
  access: "read" | "write";
}

/** One stored component: an org row (no `owner`) or a person's saved row. */
export interface Component {
  id: string;
  owner?: string;
  name: string;
  definition: ComponentDefinition;
  version: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

/** Attaches one component to a run (`components[]` of create, preflight and preview): a stored one by `id`, a run-only `inline` definition, or a built-in Git provider (`builtin`, OD-4). */
export interface ComponentRef {
  id?: string;
  inline?: ComponentDefinition;
  // Labels an inline one.
  name?: string;
  // A built-in Git provider: API and git-over-HTTPS to the listed repositories without a workspace.
  builtin?: ComponentBuiltin;
  // The GitHub owner or the Azure DevOps organisation. Required with `builtin`.
  org?: string;
  // owner/name (GitHub, at least one) or project/repository (Azure DevOps; none reaches every repository the person can reach in `org`).
  repos?: string[];
  // Empty reads as read.
  access?: "read" | "write";
  // The included harnesses (tools[].id) an MCP component's server is turned on for; absent means every harness that can take it, [] means none.
  mcp_harnesses?: string[];
}

export type ComponentBuiltin = "github" | "azure_devops";

/** Body of POST /me/components, PUT /me/components/{id} and PUT /components/{id}. */
export interface ComponentRequest {
  name: string;
  definition: ComponentDefinition;
}

/** One thing a saved component still needs before a run can use it. */
export interface ComponentRequirement {
  kind: string;
  name?: string;
  status: string;
  fix?: string;
}

/** A component save's answer: the stored row plus what it still needs (empty when nothing). */
export interface ComponentSaved extends Component {
  requirements: ComponentRequirement[];
}

/** One secret of an org component as a granted person sees it: how it is delivered and whose it is, never its name. */
export interface ComponentSecretView {
  delivery: ComponentDelivery;
  shared?: boolean;
}

/** An org component as a granted person sees it: where it reaches and how, no secret names and no config values. */
export interface OrgComponentView {
  id: string;
  name: string;
  // The admin's one line for the card, when they wrote one.
  description?: string;
  hosts: string[];
  secrets: ComponentSecretView[];
  config_keys: string[];
  category: "cli" | "mcp" | "api";
  cli?: CLIView;
  mcp?: MCPView;
}

/** The non-secret face of a CLI row: which kind and what it pins. */
export interface CLIView {
  kind: CLIKind;
  // Azure.
  subscriptions?: number;
  access?: string;
  // AWS.
  account_id?: string;
  role_name?: string;
  region?: string;
  // GitHub.
  capabilities?: string[];
}

/** The non-secret face of an MCP server row. */
export interface MCPView {
  url: string;
  transport: string;
}

export type CatalogueCLIKind = CLIKind | "gcloud" | "kubectl";

/** One of the five CLI cards. `rows` are the granted org rows of that kind. */
export interface CatalogueCLI {
  kind: CatalogueCLIKind;
  state: "available" | "not_granted" | "coming_soon";
  rows: string[];
}

/** GET /me/components. */
export interface MyComponents {
  may_define: boolean;
  // False when the deployment refuses env and file delivery for every component.
  resident_delivery_allowed: boolean;
  // The level an unattended run carrying a component of one's own is held to; "" is no cap.
  autonomy_cap: AutonomyLevel | "";
  mine: Component[];
  org: OrgComponentView[];
  // Always five cards, in the order azure, github, aws, gcloud, kubectl.
  clis: CatalogueCLI[];
}

/** One secret of a component fact: how it is delivered and whose it is. Never its name. */
export interface ComponentSecretFact {
  delivery: ComponentDeliveryMode;
  shared: boolean;
  // How an organisation-held secret would reach the person's runner (OD-12). Absent for the person's own and until the placement lane decides.
  local_delivery?: LocalDeliveryMode;
}

export type ComponentFactKind = "custom" | "git_provider" | "agent" | "git_pat";
export type ComponentFactReason = "org" | "self" | "inline" | "workspace" | "agent";
export type ComponentFactStatus = "ready" | "needs_input" | "unavailable" | "unknown";
// types.GitLane's four, then the two a clone can take with no run credential.
export type ComponentFactLane = "app" | "pat" | "ssh" | "entra" | "direct" | "none";

/**
 * One row of `components` (internal/api's componentFact): something the run is
 * given access to, as the door decided it. A refused request has no facts.
 */
export interface ComponentFact {
  kind: ComponentFactKind;
  provider?: "github" | "azure_devops";
  // A stored component's id, "inline:<position>" or "git_provider:<provider>:<lane>[:<org>]".
  id: string;
  name?: string;
  version?: number;
  reason: ComponentFactReason;
  status: ComponentFactStatus;
  requirements: SetupItem[];
  lane?: ComponentFactLane;
  org?: string;
  repos?: string[];
  hosts?: string[];
  secrets?: ComponentSecretFact[];
  config_keys?: string[];
  self_defined?: boolean;
  autonomy_cap?: AutonomyLevel;
  vault_floor?: boolean;
  tls_intercept?: boolean;
  high_risk?: boolean;
  // The agent component's own facts (kind "agent").
  agent?: AgentFact;
  // A git_provider's additions: the run's resolved capability set on the lane
  // and the most the person's row allows, the push rules in force for this
  // provider and organisation, the Azure DevOps token mode and its scopes with
  // what each covers (#1880), per-repository access, and the GitHub App
  // install link (sent to operators only).
  capabilities?: string[];
  capability_ceiling?: string[];
  push_rules?: PushRulesSpec;
  token_mode?: "bearer" | "minted_pat" | "own_pat";
  token_scopes?: TokenScopeFact[];
  repo_access?: RepoAccessFact[];
  install_url?: string;
}
