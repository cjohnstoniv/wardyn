/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The integration CATALOG: the taxonomy, the type table, and the delivery
// modes, as data. No React, no fetch — this is the anti-drift device the whole
// Integrations surface reads from, so a fact about a system lives in exactly
// one place.
//
// Every table below is MECHANICALLY DERIVED from the approved mock's own
// tables (the "Wardyn UI Mockup" project, wardyn-int2.js: GROUPS, TYPES,
// DELIVERY, SEARCH_ALIAS and the T copy object), not hand-transcribed. The mock
// is the source of truth for this surface; when it changes, re-derive rather
// than edit these by hand.
//
// One deliberate transformation: the mock writes a credential header as ONE
// string ("Authorization: Bearer"), because it only ever renders it. The API
// takes a header NAME plus a Format the secret substitutes into, so each type
// carries both — "Authorization" + "Bearer %s", "x-api-key" + "%s",
// "Authorization" + "Token token=%s".
import type { ResidencyKind } from "./integrations";

/** A category section on the surface. Sections, not filters — ordered by how often they matter. */
export type IntegrationGroupId =
  "model" | "scm" | "pkg" | "registry" | "cloud" | "data" | "mcp" | "work" | "obs" | "other";

/** The wire category (types.IntegrationCategory) a group's types are written as. */
export type ApiIntegrationCategory =
  | "ai_provider"
  | "scm_host"
  | "package_feed"
  | "container_registry"
  | "cloud_provider"
  | "data_store"
  | "mcp_server"
  | "work_tracking"
  | "observability"
  | "other_service";

export interface IntegrationGroup {
  id: IntegrationGroupId;
  label: string;
  category: ApiIntegrationCategory;
  desc: string;
}

export const INTEGRATION_GROUPS: readonly IntegrationGroup[] = [
  {
    id: "model",
    label: "Model providers",
    category: "ai_provider",
    desc: "Powers a coding agent's model calls, or Wardyn's own AI features.",
  },
  {
    id: "scm",
    label: "Source control",
    category: "scm_host",
    desc: "Lets a run clone, and push when its grant allows.",
  },
  {
    id: "pkg",
    label: "Package & artifact feeds",
    category: "package_feed",
    desc: "A private registry or feed a build installs from.",
  },
  {
    id: "registry",
    label: "Container registries",
    category: "container_registry",
    desc: "Pulling a base image, pushing a built one.",
  },
  {
    id: "cloud",
    label: "Cloud providers",
    category: "cloud_provider",
    desc: "A run that reads a bucket or deploys something.",
  },
  {
    id: "data",
    label: "Data stores",
    category: "data_store",
    desc: "A host, a port and a connection credential.",
  },
  {
    id: "mcp",
    label: "Agent tool servers (MCP)",
    category: "mcp_server",
    desc: "An agent connecting to an MCP server over HTTP.",
  },
  {
    id: "work",
    label: "Work tracking & collaboration",
    category: "work_tracking",
    desc: "Reading the ticket the work is about, or reporting a result.",
  },
  {
    id: "obs",
    label: "Observability & incident",
    category: "observability",
    desc: "A run reading logs, metrics or an incident.",
  },
  {
    id: "other",
    label: "Other service",
    category: "other_service",
    desc: "Anything Wardyn hasn't listed, named by you.",
  },
];

/**
 * Which Add path owns a type:
 *  - "generic"     — written straight through PUT /integrations; the row's own
 *                    hosts and header are the whole contract.
 *  - "ai" / "scm"  — the two TYPED backend categories, which have their own
 *                    established flows and legacy-derived rows.
 *  - "unsupported" — listed because it is real and people look for it, but it
 *                    has no home yet. Say so; never offer a broken Add.
 */
export type AddLane = "generic" | "ai" | "scm" | "unsupported";

export interface IntegrationTypeMeta {
  id: string;
  group: IntegrationGroupId;
  label: string;
  /** Known hosts, prefilled into the Add flow. Empty when the operator names their own. */
  hosts: readonly string[];
  hostPlaceholder?: string;
  /** HTTP field the credential is presented in (proxy-injected types only). */
  header?: string;
  /** Wraps the secret into the header value; exactly one %s. */
  format?: string;
  /** Conventional secret name, prefilled — never a value. */
  secret?: string;
  delivery: ResidencyKind;
  addLane: AddLane;
  /** types.Integration.Type to write. Absent when no backend type exists yet. */
  apiType?: string;
  powers: string;
  /** A qualifying fact worth stating on the type (lane residency, token expiry, ...). */
  note?: string;
  /** Lead copy for the generic escape hatch. */
  lead?: string;
}

export const INTEGRATION_TYPES: readonly IntegrationTypeMeta[] = [
  {
    id: "anthropic",
    group: "model",
    label: "Anthropic",
    hosts: ["api.anthropic.com"],
    header: "x-api-key",
    format: "%s",
    secret: "anthropic-api-key",
    delivery: "proxy_injected",
    addLane: "ai",
    apiType: "anthropic_api_key",
    powers: "Claude Code runs, direct API calls a sandbox makes itself, and Wardyn's own AI features.",
  },
  // OWNER-DIRECTED ADDITION (2026-08-04), not in wardyn-int2.js: the round-H
  // mock's model section lists only the API-key row, but the subscription Add
  // flow is real and Page-9-approved (the AI type panel, where its managed vs
  // host-login choice lives). Without this row a subscription is unfindable
  // from search — the exact gap the owner hit. Delivery is "varies" because
  // the lane decides residency: managed capture is proxy-injected, the
  // host-login lane mounts the resident credential.
  {
    id: "claude_subscription",
    group: "model",
    label: "Claude subscription",
    hosts: ["api.anthropic.com"],
    delivery: "varies",
    addLane: "ai",
    apiType: "anthropic_subscription",
    powers: "Claude Code runs on your Claude plan — no API key. Captured by a container login, or read from this host's own claude login.",
    note: "Managed capture is proxy-injected; the host-login lane mounts the resident credential.",
  },
  {
    id: "openai",
    group: "model",
    label: "OpenAI",
    hosts: ["api.openai.com"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "openai-api-key",
    delivery: "proxy_injected",
    addLane: "ai",
    apiType: "openai_api_key",
    powers: "Codex CLI runs, direct API calls, and Wardyn's own AI features.",
  },
  {
    id: "bedrock",
    group: "model",
    label: "AWS Bedrock",
    hosts: ["bedrock-runtime.us-east-1.amazonaws.com"],
    secret: "bedrock-bearer-token",
    delivery: "varies",
    addLane: "ai",
    apiType: "bedrock",
    powers: "Claude models through your AWS account — four credential lanes, switchable later.",
    note: "Bearer token is proxy-injected; the SSO, host-profile and access-key lanes are resident.",
  },
  {
    id: "azure",
    group: "model",
    label: "Azure OpenAI",
    hosts: ["<resource>.openai.azure.com"],
    header: "api-key",
    format: "%s",
    secret: "azure-openai-key",
    delivery: "control_plane",
    addLane: "ai",
    apiType: "azure_openai",
    powers: "Wardyn's own AI features only — neither agent tool can be pointed at an Azure deployment.",
  },
  {
    id: "compat",
    group: "model",
    label: "Self-hosted or OpenAI-compatible endpoint",
    hosts: [],
    hostPlaceholder: "ollama.corp.internal:11434",
    header: "Authorization",
    format: "Bearer %s",
    secret: "llm-endpoint-token",
    delivery: "proxy_injected",
    addLane: "unsupported",
    powers: "Anything that speaks the OpenAI API — Ollama, vLLM, a gateway you run.",
    note: "New: a self-hosted endpoint has no home in Wardyn today.",
  },
  {
    id: "github",
    group: "scm",
    label: "GitHub",
    hosts: ["github.com", "api.github.com", "codeload.github.com"],
    delivery: "brokered_mint",
    addLane: "scm",
    apiType: "github_app",
    powers: "Clone for every run; push and pull requests when a run's grant asks for write.",
    note: "App is brokered — a ≤1h scoped token minted per run. PAT and SSH lanes are resident.",
  },
  {
    id: "gitlab",
    group: "scm",
    label: "GitLab",
    hosts: ["gitlab.com"],
    secret: "gitlab-pat",
    delivery: "resident_mount",
    addLane: "scm",
    apiType: "git_host",
    powers: "Clone and push over HTTPS.",
    note: "The git credential helper hands the PAT to git inside the sandbox.",
  },
  {
    id: "bitbucket",
    group: "scm",
    label: "Bitbucket",
    hosts: ["bitbucket.org"],
    secret: "bitbucket-pat",
    delivery: "resident_mount",
    addLane: "scm",
    apiType: "git_host",
    powers: "Clone and push over HTTPS.",
  },
  {
    id: "ado",
    group: "scm",
    label: "Azure DevOps",
    hosts: ["dev.azure.com"],
    secret: "ado-pat",
    delivery: "resident_mount",
    addLane: "scm",
    apiType: "git_host",
    powers: "Clone and push over HTTPS.",
  },
  {
    id: "gitssh",
    group: "scm",
    label: "Git over SSH",
    hosts: [],
    hostPlaceholder: "git.corp.internal",
    secret: "ssh-key-git-corp-internal",
    delivery: "resident_mount",
    addLane: "scm",
    apiType: "git_host",
    powers: "Clone and push to a git host you name.",
    note: "A key file is written into the sandbox; the process there can read it.",
  },
  {
    id: "artifactory",
    group: "pkg",
    label: "JFrog Artifactory",
    hosts: ["artifactory.corp.internal"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "artifactory-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "artifactory",
    powers: "npm, pip, maven, go, cargo and nuget installs from your feed.",
  },
  {
    id: "nexus",
    group: "pkg",
    label: "Sonatype Nexus",
    hosts: ["nexus.corp.internal"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "nexus-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "nexus",
    powers: "Package installs from your Nexus repositories.",
  },
  {
    id: "ghpkg",
    group: "pkg",
    label: "GitHub Packages",
    hosts: ["npm.pkg.github.com", "maven.pkg.github.com"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "github-packages-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "ghpkg",
    powers: "Installs from private GitHub Packages feeds.",
  },
  {
    id: "codeartifact",
    group: "pkg",
    label: "AWS CodeArtifact",
    hosts: ["<domain>-<acct>.d.codeartifact.us-east-1.amazonaws.com"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "codeartifact-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "codeartifact",
    powers: "Installs from a CodeArtifact repository.",
    note: "CodeArtifact tokens expire after 12 hours. A pasted token is injected until it does — minting one per run isn't built.",
  },
  {
    id: "npmscope",
    group: "pkg",
    label: "Private npm scope",
    hosts: ["registry.npmjs.org"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "npm-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "npmscope",
    powers: "Installing a private scope from the public registry.",
  },
  {
    id: "pypi",
    group: "pkg",
    label: "Private PyPI",
    hosts: ["pypi.corp.internal"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "pypi-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "pypi",
    powers: "pip installs from your index.",
  },
  {
    id: "ghcr",
    group: "registry",
    label: "GitHub Container Registry",
    hosts: ["ghcr.io"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "ghcr-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "ghcr",
    powers: "Pulling a base image and pushing a built one.",
  },
  {
    id: "dockerhub",
    group: "registry",
    label: "Docker Hub",
    hosts: ["registry-1.docker.io", "auth.docker.io"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "dockerhub-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "dockerhub",
    powers: "Pulling images, and pulling them without the anonymous rate limit.",
  },
  {
    id: "harbor",
    group: "registry",
    label: "Harbor",
    hosts: ["harbor.corp.internal"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "harbor-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "harbor",
    powers: "Pulls and pushes against your Harbor project.",
  },
  {
    id: "ecr",
    group: "registry",
    label: "Amazon ECR",
    hosts: ["<acct>.dkr.ecr.us-east-1.amazonaws.com"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "ecr",
    powers:
      "Reaching ECR at all. Its login token is minted through the AWS chain, so delivering it is the same gap the cloud providers have.",
  },
  {
    id: "gar",
    group: "registry",
    label: "Google Artifact Registry",
    hosts: ["us-docker.pkg.dev"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "gar",
    powers: "Reaching Artifact Registry at all; its token comes from a Google OAuth chain.",
  },
  {
    id: "aws",
    group: "cloud",
    label: "Amazon Web Services",
    hosts: ["s3.us-east-1.amazonaws.com", "sts.amazonaws.com"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "aws",
    powers: "Reaching AWS endpoints from a run. SigV4 signing means the proxy can't add a header on the run's behalf.",
  },
  {
    id: "gcp",
    group: "cloud",
    label: "Google Cloud",
    hosts: ["storage.googleapis.com", "oauth2.googleapis.com"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "gcp",
    powers: "Reaching Google Cloud endpoints from a run.",
  },
  {
    id: "azurecloud",
    group: "cloud",
    label: "Microsoft Azure",
    hosts: ["management.azure.com", "<account>.blob.core.windows.net"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "azurecloud",
    powers: "Reaching Azure endpoints from a run.",
  },
  {
    id: "postgres",
    group: "data",
    label: "PostgreSQL",
    hosts: ["db.corp.internal:5432"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "postgres",
    powers: "Reaching the database host and port from a run.",
  },
  {
    id: "mysql",
    group: "data",
    label: "MySQL",
    hosts: ["mysql.corp.internal:3306"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "mysql",
    powers: "Reaching the database host and port from a run.",
  },
  {
    id: "redis",
    group: "data",
    label: "Redis",
    hosts: ["cache.corp.internal:6379"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "redis",
    powers: "Reaching the cache host and port from a run.",
  },
  {
    id: "mongo",
    group: "data",
    label: "MongoDB",
    hosts: ["mongo.corp.internal:27017"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "mongo",
    powers: "Reaching the database host and port from a run.",
  },
  {
    id: "s3compat",
    group: "data",
    label: "S3-compatible object storage",
    hosts: ["minio.corp.internal:9000"],
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "s3compat",
    powers: "Reaching the endpoint from a run; SigV4 signing is the same gap AWS has.",
  },
  {
    id: "mcp",
    group: "mcp",
    label: "MCP server over HTTP",
    hosts: [],
    hostPlaceholder: "mcp.corp.internal",
    header: "Authorization",
    format: "Bearer %s",
    secret: "mcp-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "mcp",
    powers: "An agent in a run connecting to this MCP server and calling its tools.",
  },
  {
    id: "jira",
    group: "work",
    label: "Jira",
    hosts: ["<site>.atlassian.net"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "jira-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "jira",
    powers: "Reading the ticket a run is about, and reporting back on it.",
  },
  {
    id: "linear",
    group: "work",
    label: "Linear",
    hosts: ["api.linear.app"],
    header: "Authorization",
    format: "%s",
    secret: "linear-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "linear",
    powers: "Reading the issue a run is about, and reporting back on it.",
  },
  {
    id: "ghissues",
    group: "work",
    label: "GitHub Issues",
    hosts: ["api.github.com"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "github-issues-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "ghissues",
    powers: "Reading and commenting on issues, separately from the clone credential.",
  },
  {
    id: "slack",
    group: "work",
    label: "Slack",
    hosts: ["slack.com", "api.slack.com"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "slack-bot-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "slack",
    powers: "Posting a result into a channel.",
  },
  {
    id: "notion",
    group: "work",
    label: "Notion",
    hosts: ["api.notion.com"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "notion-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "notion",
    powers: "Reading the page a run is working from.",
  },
  {
    id: "datadog",
    group: "obs",
    label: "Datadog",
    hosts: ["api.datadoghq.com"],
    header: "DD-API-KEY",
    format: "%s",
    secret: "datadog-api-key",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "datadog",
    powers: "A run reading metrics or logs while it investigates.",
  },
  {
    id: "sentry",
    group: "obs",
    label: "Sentry",
    hosts: ["sentry.io"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "sentry-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "sentry",
    powers: "A run reading the error it was asked to fix.",
  },
  {
    id: "grafana",
    group: "obs",
    label: "Grafana",
    hosts: ["grafana.corp.internal"],
    header: "Authorization",
    format: "Bearer %s",
    secret: "grafana-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "grafana",
    powers: "A run reading dashboards and queries.",
  },
  {
    id: "pagerduty",
    group: "obs",
    label: "PagerDuty",
    hosts: ["api.pagerduty.com"],
    header: "Authorization",
    format: "Token token=%s",
    secret: "pagerduty-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "pagerduty",
    powers: "A run reading the incident it was opened for.",
  },
  {
    id: "other",
    group: "other",
    label: "Other service",
    hosts: [],
    hostPlaceholder: "api.vendor.example.com",
    header: "Authorization",
    format: "Bearer %s",
    secret: "service-token",
    delivery: "proxy_injected",
    addLane: "generic",
    apiType: "other",
    powers: "Whatever your run does with it — Wardyn opens the path and presents the credential.",
    lead: "A name, its hosts, an optional credential and how to present it. Anything Wardyn hasn't listed is this — and it works today, because the proxy can inject a header for a host it has never heard of.",
  },
];

/** Extra search terms per type — what someone types when they don't know the product name. */
const SEARCH_ALIAS: Readonly<Record<string, string>> = {
  compat: "ollama vllm llama.cpp gateway litellm openai-compatible self-hosted local model",
  artifactory: "jfrog maven npm registry feed proxy repo",
  nexus: "sonatype maven npm feed repo",
  npmscope: "npm yarn pnpm registry",
  pypi: "pip python index simple",
  codeartifact: "aws feed npm maven",
  ghcr: "docker image container github",
  dockerhub: "docker image container",
  harbor: "docker image container",
  ecr: "aws docker image container",
  gar: "gcp google docker image container",
  aws: "amazon s3 bucket sts lambda",
  gcp: "google gcs bucket",
  azurecloud: "blob storage microsoft",
  postgres: "postgresql psql database sql",
  mysql: "mariadb database sql",
  redis: "cache valkey",
  mongo: "mongodb database",
  s3compat: "minio ceph object storage bucket",
  mcp: "model context protocol agent tools server",
  jira: "atlassian ticket issue",
  linear: "issue ticket",
  ghissues: "github issue ticket",
  slack: "chat message channel",
  notion: "docs page wiki",
  datadog: "metrics logs apm",
  sentry: "errors exceptions",
  grafana: "dashboards metrics",
  pagerduty: "incident oncall",
  github: "git clone pat ssh app",
  gitlab: "git clone pat",
  bitbucket: "git clone",
  ado: "azure devops git tfs",
  gitssh: "git ssh key self-hosted gitea forgejo",
  anthropic: "claude api key model llm",
  claude_subscription: "claude code subscription pro max plan login oauth setup-token managed anthropic",
  openai: "gpt codex api key model llm",
  bedrock: "aws claude model llm",
  azure: "openai model llm entra",
  other: "generic custom service webhook api anything else ci deploy kubernetes terraform",
};

/** Copy canon, verbatim from the mock's T object. Do not paraphrase. */
export const CATALOG_COPY = {
  WS_SEAM:
    "A workspace's requirements can name an integration instead of restating its hosts and secret names. One name, and the hosts and the credential ride along.",
  STORE_NOTE: "Wardyn stores this — it doesn't dial the system to check it.",
  EMPTY_BODY:
    "A run reaches nothing outside Wardyn until you name what it may reach. Governed commands, interactive runs and terminal recordings need none of this — add an integration when a run, an agent, or a Wardyn feature needs one.",
  NOTBUILT_CLOUD:
    "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  NOTBUILT_DATA:
    "Postgres, MySQL, Redis and Mongo speak their own wire protocols, not HTTP, so the proxy has nothing to inject.",
  ADD_DESC:
    "Type what you're connecting, or browse the categories. Every category is optional — Wardyn runs with none of them.",
  SEARCH_PH: "Artifactory, ghcr.io, Postgres, an MCP server…",
  SEARCH_NONE:
    "No type matches that. Anything Wardyn hasn't listed is an Other service — a name, its hosts, and an optional credential.",
  CONNECT_DESC: "Where it lives, what it takes to get in, how that reaches the request, and what it powers.",
  HOSTS_HEAD: "Hosts",
  HOSTS_HINT: "What a run is allowed to reach because this integration exists. One per line — wildcards are fine.",
  CRED_HEAD: "Credential",
  CRED_WRITEONLY: "The store is write-only — the value can be replaced or removed, never read back.",
  DELIV_STATED: "Stated, not chosen — it follows from how this system authenticates.",
  EGRESS_LINE:
    "These hosts become reachable from a run that's granted this integration. Nothing is ambient: a run gets it when its workspace requires it or its grant names it.",
  DOCS_HINT: "Optional. Where whoever comes after you finds out what this system is.",
} as const;

export function integrationGroup(id: IntegrationGroupId): IntegrationGroup {
  return INTEGRATION_GROUPS.find((g) => g.id === id) ?? INTEGRATION_GROUPS[INTEGRATION_GROUPS.length - 1];
}

// Registry, cloud and data-store types all land on "notbuilt" for one of two
// reasons — a signed-request/SDK auth chain (registry + cloud), or a non-HTTP
// wire protocol (data) — so the reason is a fact about the GROUP, not
// something each of the 10 affected types needs to restate identically.
const NOTBUILT_WHY: Partial<Record<IntegrationGroupId, string>> = {
  registry: CATALOG_COPY.NOTBUILT_CLOUD,
  cloud: CATALOG_COPY.NOTBUILT_CLOUD,
  data: CATALOG_COPY.NOTBUILT_DATA,
};

/** Why a "notbuilt" type has no credential lane yet — stated per group. */
export function notbuiltWhy(group: IntegrationGroupId): string | undefined {
  return NOTBUILT_WHY[group];
}

export function integrationTypeById(id: string): IntegrationTypeMeta | undefined {
  return INTEGRATION_TYPES.find((t) => t.id === id);
}

export function typesInGroup(group: IntegrationGroupId): IntegrationTypeMeta[] {
  return INTEGRATION_TYPES.filter((t) => t.group === group);
}

/**
 * Search-first Add: match a typed query against the label, its group, its known
 * hosts and the alias terms — so "ghcr.io", "postgres" and "jfrog" all land
 * somewhere sensible. Capped at 7, the mock's own cut-off.
 */
export function searchIntegrationTypes(query: string): IntegrationTypeMeta[] {
  const q = query.trim().toLowerCase();
  if (!q) return [];
  return INTEGRATION_TYPES.filter((t) => {
    const hay = [t.label, t.group, integrationGroup(t.group).label, ...t.hosts, SEARCH_ALIAS[t.id] ?? ""]
      .join(" ")
      .toLowerCase();
    return hay.includes(q);
  }).slice(0, 7);
}
