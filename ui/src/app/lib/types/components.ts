/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Component wire types (#1914). Types only: nothing here is imported at run time.
// Every interface mirrors one Go struct and wire-parity.test.ts pins its json
// tags to that struct's source, so a Go rename fails a TS test.
//
//   Component, ComponentDefinition, ComponentSecret, ComponentDelivery, ComponentRef
//                        internal/types/component.go (aliased by pkg/client/types.go)
//   ComponentRequest, ComponentRequirement, ComponentSaved, ComponentSecretView,
//   OrgComponentView, MyComponents
//                        pkg/client/components.go
//   ComponentFact, ComponentSecretFact
//                        internal/api/components_facts.go (the `components` of
//                        POST /runs/preflight and POST /runs/policy-preview)

import type { AutonomyLevel } from "../api/governance";
import type { AgentFact, LocalDeliveryMode, RepoAccessFact, TokenScopeFact } from "./new-run-contract";
import type { PushRulesSpec } from "./policy";
import type { SetupItem } from "./runs";

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
  hosts: string[];
  secrets?: ComponentSecret[];
  // Plain, non-secret environment.
  config?: Record<string, string>;
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
  hosts: string[];
  secrets: ComponentSecretView[];
  config_keys: string[];
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
