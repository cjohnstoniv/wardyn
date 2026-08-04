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

/** A category section on the surface. Sections, not filters — ordered by how often they matter. */
export type IntegrationGroupId =
  | "model"
  | "scm"
  | "pkg"
  | "registry"
  | "cloud"
  | "data"
  | "mcp"
  | "work"
  | "obs"
  | "other";

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
  /** Shown when this section has nothing in it and the honest answer is "that's fine". */
  empty?: string;
}

export const INTEGRATION_GROUPS: readonly IntegrationGroup[] = [
  {
    id: "model",
    label: "Model providers",
    category: "ai_provider",
    desc: "Powers a coding agent's model calls, or Wardyn's own AI features.",
    empty:
      "None. Runs work without a model — add one to have a coding agent drive a run, or to use Wardyn's own AI features.",
  },
  {
    id: "scm",
    label: "Source control",
    category: "scm_host",
    desc: "Lets a run clone, and push when its grant allows.",
    empty: "None. Public repos clone without any credential.",
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
 * How a credential reaches a request. A STATED FACT per type — it follows from
 * how the system authenticates, so it is never an operator's choice.
 */
export type DeliveryMode =
  "proxy" | "brokered" | "resident" | "none" | "notbuilt" | "cp" | "varies";

export interface DeliveryMeta {
  label: string;
  tone: "success" | "warning" | "neutral";
  line: string;
}

export const DELIVERY_META: Record<DeliveryMode, DeliveryMeta> = {
  proxy: {
    label: "proxy-injected",
    tone: "success",
    line: "Wardyn holds the secret; the egress proxy adds the header to requests bound for that host. The sandbox never holds it and cannot read it. This is the generic path — the one mechanism that works for a host Wardyn has never heard of.",
  },
  brokered: {
    label: "brokered",
    tone: "success",
    line: "A short-lived, scoped credential minted per run. The GitHub App is the only one today. Nothing long-lived exists in the sandbox.",
  },
  resident: {
    label: "resident",
    tone: "warning",
    line: "The secret is inside the sandbox and the process there can read it — an SSH key file, AWS environment keys, a captured SSO cache file, the git credential helper handing a PAT to git. Every resident lane is hand-written per provider: there is no generic “deliver this secret as a file or a variable” mechanism, so resident delivery can't be offered for a new integration without backend work.",
  },
  none: {
    label: "no credential",
    tone: "neutral",
    line: "A public endpoint. Egress only, no credential.",
  },
  notbuilt: {
    label: "egress only",
    tone: "warning",
    line: "Wardyn can open the path to the host — which is the difference between a run reaching it and not reaching it at all. Delivering this system's credential into the sandbox isn't built yet, and the row says so rather than showing an empty field.",
  },
  cp: {
    label: "control-plane side",
    tone: "neutral",
    line: "Called from Wardyn's control plane. Nothing about it enters a sandbox.",
  },
  varies: {
    label: "varies by lane",
    tone: "neutral",
    line: "Several credential lanes exist and the active one decides residency — the lane table states which.",
  },
};

/** The credential SHAPE a type takes, which decides what the Add flow asks for. */
export type CredShape =
  "header" | "optional" | "lanes" | "resident" | "notbuilt";

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
  cred: CredShape;
  /** HTTP field the credential is presented in (proxy-injected types only). */
  header?: string;
  /** Wraps the secret into the header value; exactly one %s. */
  format?: string;
  /** Conventional secret name, prefilled — never a value. */
  secret?: string;
  delivery: DeliveryMode;
  addLane: AddLane;
  /** types.Integration.Type to write. Absent when no backend type exists yet. */
  apiType?: string;
  /** What it powers, as short chips. */
  chips: readonly string[];
  powers: string;
  /** A qualifying fact worth stating on the type (lane residency, token expiry, ...). */
  note?: string;
  /** Why this type has no credential lane — verbatim, for the honest "egress only" rows. */
  why?: string;
  /** Lead copy for the generic escape hatch. */
  lead?: string;
}

export const INTEGRATION_TYPES: readonly IntegrationTypeMeta[] = [
  {
    id: "anthropic",
    group: "model",
    label: "Anthropic",
    hosts: ["api.anthropic.com"],
    cred: "header",
    header: "x-api-key",
    format: "%s",
    secret: "anthropic-api-key",
    delivery: "proxy",
    addLane: "ai",
    apiType: "anthropic_api_key",
    chips: ["Claude Code", "Direct API", "Wardyn features"],
    powers:
      "Claude Code runs, direct API calls a sandbox makes itself, and Wardyn's own AI features.",
  },
  {
    id: "openai",
    group: "model",
    label: "OpenAI",
    hosts: ["api.openai.com"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "openai-api-key",
    delivery: "proxy",
    addLane: "ai",
    apiType: "openai_api_key",
    chips: ["Codex CLI", "Direct API", "Wardyn features"],
    powers: "Codex CLI runs, direct API calls, and Wardyn's own AI features.",
  },
  {
    id: "bedrock",
    group: "model",
    label: "AWS Bedrock",
    hosts: ["bedrock-runtime.us-east-1.amazonaws.com"],
    cred: "lanes",
    secret: "bedrock-bearer-token",
    delivery: "varies",
    addLane: "ai",
    apiType: "bedrock",
    chips: ["Claude Code", "Wardyn features"],
    powers:
      "Claude models through your AWS account — four credential lanes, switchable later.",
    note: "Bearer token is proxy-injected; the SSO, host-profile and access-key lanes are resident.",
  },
  {
    id: "azure",
    group: "model",
    label: "Azure OpenAI",
    hosts: ["<resource>.openai.azure.com"],
    cred: "header",
    header: "api-key",
    format: "%s",
    secret: "azure-openai-key",
    delivery: "cp",
    addLane: "ai",
    apiType: "azure_openai",
    chips: ["Wardyn features"],
    powers:
      "Wardyn's own AI features only — neither agent tool can be pointed at an Azure deployment.",
  },
  {
    id: "compat",
    group: "model",
    label: "Self-hosted or OpenAI-compatible endpoint",
    hosts: [],
    hostPlaceholder: "ollama.corp.internal:11434",
    cred: "optional",
    header: "Authorization",
    format: "Bearer %s",
    secret: "llm-endpoint-token",
    delivery: "proxy",
    addLane: "unsupported",
    chips: ["Codex CLI", "Direct API"],
    powers:
      "Anything that speaks the OpenAI API — Ollama, vLLM, a gateway you run.",
    note: "New: a self-hosted endpoint has no home in Wardyn today.",
  },
  {
    id: "github",
    group: "scm",
    label: "GitHub",
    hosts: ["github.com", "api.github.com", "codeload.github.com"],
    cred: "lanes",
    delivery: "brokered",
    addLane: "scm",
    apiType: "github_app",
    chips: ["clone", "push & PRs"],
    powers:
      "Clone for every run; push and pull requests when a run's grant asks for write.",
    note: "App is brokered — a ≤1h scoped token minted per run. PAT and SSH lanes are resident.",
  },
  {
    id: "gitlab",
    group: "scm",
    label: "GitLab",
    hosts: ["gitlab.com"],
    cred: "resident",
    secret: "gitlab-pat",
    delivery: "resident",
    addLane: "scm",
    apiType: "git_host",
    chips: ["clone", "push"],
    powers: "Clone and push over HTTPS.",
    note: "The git credential helper hands the PAT to git inside the sandbox.",
  },
  {
    id: "bitbucket",
    group: "scm",
    label: "Bitbucket",
    hosts: ["bitbucket.org"],
    cred: "resident",
    secret: "bitbucket-pat",
    delivery: "resident",
    addLane: "scm",
    apiType: "git_host",
    chips: ["clone", "push"],
    powers: "Clone and push over HTTPS.",
  },
  {
    id: "ado",
    group: "scm",
    label: "Azure DevOps",
    hosts: ["dev.azure.com"],
    cred: "resident",
    secret: "ado-pat",
    delivery: "resident",
    addLane: "scm",
    apiType: "git_host",
    chips: ["clone", "push"],
    powers: "Clone and push over HTTPS.",
  },
  {
    id: "gitssh",
    group: "scm",
    label: "Git over SSH",
    hosts: [],
    hostPlaceholder: "git.corp.internal",
    cred: "resident",
    secret: "ssh-key-git-corp-internal",
    delivery: "resident",
    addLane: "scm",
    apiType: "git_host",
    chips: ["clone", "push"],
    powers: "Clone and push to a git host you name.",
    note: "A key file is written into the sandbox; the process there can read it.",
  },
  {
    id: "artifactory",
    group: "pkg",
    label: "JFrog Artifactory",
    hosts: ["artifactory.corp.internal"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "artifactory-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "artifactory",
    chips: ["package installs"],
    powers: "npm, pip, maven, go, cargo and nuget installs from your feed.",
  },
  {
    id: "nexus",
    group: "pkg",
    label: "Sonatype Nexus",
    hosts: ["nexus.corp.internal"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "nexus-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "nexus",
    chips: ["package installs"],
    powers: "Package installs from your Nexus repositories.",
  },
  {
    id: "ghpkg",
    group: "pkg",
    label: "GitHub Packages",
    hosts: ["npm.pkg.github.com", "maven.pkg.github.com"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "github-packages-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "ghpkg",
    chips: ["package installs"],
    powers: "Installs from private GitHub Packages feeds.",
  },
  {
    id: "codeartifact",
    group: "pkg",
    label: "AWS CodeArtifact",
    hosts: ["<domain>-<acct>.d.codeartifact.us-east-1.amazonaws.com"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "codeartifact-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "codeartifact",
    chips: ["package installs"],
    powers: "Installs from a CodeArtifact repository.",
    note: "CodeArtifact tokens expire after 12 hours. A pasted token is injected until it does — minting one per run isn't built.",
  },
  {
    id: "npmscope",
    group: "pkg",
    label: "Private npm scope",
    hosts: ["registry.npmjs.org"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "npm-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "npmscope",
    chips: ["npm installs"],
    powers: "Installing a private scope from the public registry.",
  },
  {
    id: "pypi",
    group: "pkg",
    label: "Private PyPI",
    hosts: ["pypi.corp.internal"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "pypi-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "pypi",
    chips: ["pip installs"],
    powers: "pip installs from your index.",
  },
  {
    id: "ghcr",
    group: "registry",
    label: "GitHub Container Registry",
    hosts: ["ghcr.io"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "ghcr-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "ghcr",
    chips: ["image pull", "image push"],
    powers: "Pulling a base image and pushing a built one.",
  },
  {
    id: "dockerhub",
    group: "registry",
    label: "Docker Hub",
    hosts: ["registry-1.docker.io", "auth.docker.io"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "dockerhub-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "dockerhub",
    chips: ["image pull"],
    powers:
      "Pulling images, and pulling them without the anonymous rate limit.",
  },
  {
    id: "harbor",
    group: "registry",
    label: "Harbor",
    hosts: ["harbor.corp.internal"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "harbor-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "harbor",
    chips: ["image pull", "image push"],
    powers: "Pulls and pushes against your Harbor project.",
  },
  {
    id: "ecr",
    group: "registry",
    label: "Amazon ECR",
    hosts: ["<acct>.dkr.ecr.us-east-1.amazonaws.com"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "ecr",
    chips: ["image pull"],
    powers:
      "Reaching ECR at all. Its login token is minted through the AWS chain, so delivering it is the same gap the cloud providers have.",
    why: "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  },
  {
    id: "gar",
    group: "registry",
    label: "Google Artifact Registry",
    hosts: ["us-docker.pkg.dev"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "gar",
    chips: ["image pull"],
    powers:
      "Reaching Artifact Registry at all; its token comes from a Google OAuth chain.",
    why: "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  },
  {
    id: "aws",
    group: "cloud",
    label: "Amazon Web Services",
    hosts: ["s3.us-east-1.amazonaws.com", "sts.amazonaws.com"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "aws",
    chips: ["bucket reads", "SDK calls"],
    powers:
      "Reaching AWS endpoints from a run. SigV4 signing means the proxy can't add a header on the run's behalf.",
    why: "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  },
  {
    id: "gcp",
    group: "cloud",
    label: "Google Cloud",
    hosts: ["storage.googleapis.com", "oauth2.googleapis.com"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "gcp",
    chips: ["bucket reads", "SDK calls"],
    powers: "Reaching Google Cloud endpoints from a run.",
    why: "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  },
  {
    id: "azurecloud",
    group: "cloud",
    label: "Microsoft Azure",
    hosts: ["management.azure.com", "<account>.blob.core.windows.net"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "azurecloud",
    chips: ["blob reads", "SDK calls"],
    powers: "Reaching Azure endpoints from a run.",
    why: "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  },
  {
    id: "postgres",
    group: "data",
    label: "PostgreSQL",
    hosts: ["db.corp.internal:5432"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "postgres",
    chips: ["queries"],
    powers: "Reaching the database host and port from a run.",
    why: "Postgres, MySQL, Redis and Mongo speak their own wire protocols, not HTTP, so the proxy has nothing to inject.",
  },
  {
    id: "mysql",
    group: "data",
    label: "MySQL",
    hosts: ["mysql.corp.internal:3306"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "mysql",
    chips: ["queries"],
    powers: "Reaching the database host and port from a run.",
    why: "Postgres, MySQL, Redis and Mongo speak their own wire protocols, not HTTP, so the proxy has nothing to inject.",
  },
  {
    id: "redis",
    group: "data",
    label: "Redis",
    hosts: ["cache.corp.internal:6379"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "redis",
    chips: ["cache access"],
    powers: "Reaching the cache host and port from a run.",
    why: "Postgres, MySQL, Redis and Mongo speak their own wire protocols, not HTTP, so the proxy has nothing to inject.",
  },
  {
    id: "mongo",
    group: "data",
    label: "MongoDB",
    hosts: ["mongo.corp.internal:27017"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "mongo",
    chips: ["queries"],
    powers: "Reaching the database host and port from a run.",
    why: "Postgres, MySQL, Redis and Mongo speak their own wire protocols, not HTTP, so the proxy has nothing to inject.",
  },
  {
    id: "s3compat",
    group: "data",
    label: "S3-compatible object storage",
    hosts: ["minio.corp.internal:9000"],
    cred: "notbuilt",
    delivery: "notbuilt",
    addLane: "generic",
    apiType: "s3compat",
    chips: ["object reads"],
    powers:
      "Reaching the endpoint from a run; SigV4 signing is the same gap AWS has.",
    why: "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  },
  {
    id: "mcp",
    group: "mcp",
    label: "MCP server over HTTP",
    hosts: [],
    hostPlaceholder: "mcp.corp.internal",
    cred: "optional",
    header: "Authorization",
    format: "Bearer %s",
    secret: "mcp-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "mcp",
    chips: ["agent tools"],
    powers:
      "An agent in a run connecting to this MCP server and calling its tools.",
  },
  {
    id: "jira",
    group: "work",
    label: "Jira",
    hosts: ["<site>.atlassian.net"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "jira-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "jira",
    chips: ["read tickets", "comment"],
    powers: "Reading the ticket a run is about, and reporting back on it.",
  },
  {
    id: "linear",
    group: "work",
    label: "Linear",
    hosts: ["api.linear.app"],
    cred: "header",
    header: "Authorization",
    format: "%s",
    secret: "linear-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "linear",
    chips: ["read issues", "comment"],
    powers: "Reading the issue a run is about, and reporting back on it.",
  },
  {
    id: "ghissues",
    group: "work",
    label: "GitHub Issues",
    hosts: ["api.github.com"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "github-issues-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "ghissues",
    chips: ["read issues", "comment"],
    powers:
      "Reading and commenting on issues, separately from the clone credential.",
  },
  {
    id: "slack",
    group: "work",
    label: "Slack",
    hosts: ["slack.com", "api.slack.com"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "slack-bot-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "slack",
    chips: ["post message"],
    powers: "Posting a result into a channel.",
  },
  {
    id: "notion",
    group: "work",
    label: "Notion",
    hosts: ["api.notion.com"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "notion-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "notion",
    chips: ["read pages"],
    powers: "Reading the page a run is working from.",
  },
  {
    id: "datadog",
    group: "obs",
    label: "Datadog",
    hosts: ["api.datadoghq.com"],
    cred: "header",
    header: "DD-API-KEY",
    format: "%s",
    secret: "datadog-api-key",
    delivery: "proxy",
    addLane: "generic",
    apiType: "datadog",
    chips: ["read metrics", "read logs"],
    powers: "A run reading metrics or logs while it investigates.",
  },
  {
    id: "sentry",
    group: "obs",
    label: "Sentry",
    hosts: ["sentry.io"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "sentry-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "sentry",
    chips: ["read issues"],
    powers: "A run reading the error it was asked to fix.",
  },
  {
    id: "grafana",
    group: "obs",
    label: "Grafana",
    hosts: ["grafana.corp.internal"],
    cred: "header",
    header: "Authorization",
    format: "Bearer %s",
    secret: "grafana-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "grafana",
    chips: ["read dashboards"],
    powers: "A run reading dashboards and queries.",
  },
  {
    id: "pagerduty",
    group: "obs",
    label: "PagerDuty",
    hosts: ["api.pagerduty.com"],
    cred: "header",
    header: "Authorization",
    format: "Token token=%s",
    secret: "pagerduty-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "pagerduty",
    chips: ["read incidents"],
    powers: "A run reading the incident it was opened for.",
  },
  {
    id: "other",
    group: "other",
    label: "Other service",
    hosts: [],
    hostPlaceholder: "api.vendor.example.com",
    cred: "optional",
    header: "Authorization",
    format: "Bearer %s",
    secret: "service-token",
    delivery: "proxy",
    addLane: "generic",
    apiType: "other",
    chips: [],
    powers:
      "Whatever your run does with it — Wardyn opens the path and presents the credential.",
    lead: "A name, its hosts, an optional credential and how to present it. Anything Wardyn hasn't listed is this — and it works today, because the proxy can inject a header for a host it has never heard of.",
  },
];

/** Extra search terms per type — what someone types when they don't know the product name. */
const SEARCH_ALIAS: Readonly<Record<string, string>> = {
  compat:
    "ollama vllm llama.cpp gateway litellm openai-compatible self-hosted local model",
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
  openai: "gpt codex api key model llm",
  bedrock: "aws claude model llm",
  azure: "openai model llm entra",
  other:
    "generic custom service webhook api anything else ci deploy kubernetes terraform",
};

/** Copy canon, verbatim from the mock's T object. Do not paraphrase. */
export const CATALOG_COPY = {
  LEDE: "Named connections to the systems outside Wardyn that a run — or Wardyn itself — has to reach. Each one bundles where the system lives, what credential it takes, how that credential reaches the request, and what it powers. A governed sandbox reaches nothing by default; an integration is how you open one door, by name.",
  REACH:
    "Adding an integration is what puts its hosts within a run's reach — that's why a host is on the allowlist, instead of being hand-listed in every workspace that needs it.",
  WS_SEAM:
    "A workspace's requirements can name an integration instead of restating its hosts and secret names. One name, and the hosts and the credential ride along.",
  CORP_POINTER:
    "Your corporate proxy and any egress redirects aren't integrations — they're network topology, and they live in Corporate network under Getting started, on the same screen as the probe that proves them. The line between them: an integration owns the system and its credential; a redirect owns rerouting a public endpoint to it.",
  FOOTNOTE:
    "Wardyn doesn't test-connect a stored credential. Everything here is what's stored and what Wardyn can see locally — the one exception is the GitHub App's ref-confinement row, which really asks GitHub.",
  STORE_NOTE: "Wardyn stores this — it doesn't dial the system to check it.",
  EMPTY_TITLE: "No integrations",
  EMPTY_BODY:
    "A run reaches nothing outside Wardyn until you name what it may reach. Governed commands, interactive runs and terminal recordings need none of this — add an integration when a run, an agent, or a Wardyn feature needs one.",
  NOTHING_ELSE: "Nothing connected under ",
  VIEWER_HINT: "Operator role required",
  VIEWER_LINE:
    "You're a viewer — everything here is readable; adding, rotating, defaults and deletion need an operator.",
  DELIVERY_HEAD: "How a credential reaches a request",
  DELIVERY_INTRO:
    "Four mechanisms exist, and the difference between them is the most safety-relevant fact on this page. Each integration states which one it uses — it follows from how the system authenticates, so it is never an operator's choice.",
  MOUNT_CAVEAT:
    "A file or a mount is not automatically secret-free: the AWS SSO cache and the staged subscription credentials are both live credentials sitting in a sandbox.",
  NOTBUILT_CLOUD:
    "AWS, GCP and Azure authenticate by signing the request or through an SDK chain — not with a header the proxy can add. Only the bespoke Bedrock lanes exist today.",
  NOTBUILT_DATA:
    "Postgres, MySQL, Redis and Mongo speak their own wire protocols, not HTTP, so the proxy has nothing to inject.",
  ADD_DESC:
    "Type what you're connecting, or browse the categories. Every category is optional — Wardyn runs with none of them.",
  SEARCH_PH: "Artifactory, ghcr.io, Postgres, an MCP server…",
  SEARCH_NONE:
    "No type matches that. Anything Wardyn hasn't listed is an Other service — a name, its hosts, and an optional credential.",
  TYPE_DESC:
    "Pick the system. Each type arrives with its known hosts and its credential shape already filled in; you can change both.",
  CONNECT_DESC:
    "Where it lives, what it takes to get in, how that reaches the request, and what it powers.",
  HOSTS_HEAD: "Hosts",
  HOSTS_HINT:
    "What a run is allowed to reach because this integration exists. One per line — wildcards are fine.",
  CRED_HEAD: "Credential",
  CRED_WRITEONLY:
    "The store is write-only — the value can be replaced or removed, never read back.",
  DELIV_HEAD: "Delivery",
  DELIV_STATED:
    "Stated, not chosen — it follows from how this system authenticates.",
  POWERS_HEAD: "What this powers",
  EGRESS_HEAD: "Egress",
  EGRESS_LINE:
    "These hosts become reachable from a run that's granted this integration. Nothing is ambient: a run gets it when its workspace requires it or its grant names it.",
  DOCS_HINT:
    "Optional. Where whoever comes after you finds out what this system is.",
  OTHER_LEAD:
    "A name, its hosts, an optional credential and how to present it. Anything Wardyn hasn't listed is this — and it works today, because the proxy can inject a header for a host it has never heard of.",
  NO_CRED_LABEL: "No credential — the endpoint is read-public",
  HAS_CRED_LABEL: "Takes a credential",
  CHECK_STORED: "is present in the write-only store.",
  CHECK_WIRED: "The egress proxy is set to add ",
  CHECK_ALLOW:
    " on this integration's hosts, so a run granted it can reach them.",
  USED_NONE: "No workspace names this integration yet.",
  RUN_LINE:
    "Nothing is ambient. A run gets this integration when the workspace it runs in requires it, or when its grant names it — never automatically.",
  REDIRECT_SEAM:
    "This feed can also stand in for a public endpoint. That rerouting is topology and stays in Corporate network — but the redirect row there can take its token from this integration instead of naming a bare secret.",
  DEL_HEAD: "Delete this integration",
} as const;

export function integrationGroup(id: IntegrationGroupId): IntegrationGroup {
  return (
    INTEGRATION_GROUPS.find((g) => g.id === id) ??
    INTEGRATION_GROUPS[INTEGRATION_GROUPS.length - 1]
  );
}

export function integrationTypeById(
  id: string,
): IntegrationTypeMeta | undefined {
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
    const hay = [
      t.label,
      t.group,
      integrationGroup(t.group).label,
      ...t.hosts,
      SEARCH_ALIAS[t.id] ?? "",
    ]
      .join(" ")
      .toLowerCase();
    return hay.includes(q);
  }).slice(0, 7);
}

/** The delivery facts for a type, for a chip plus its explanation. */
export function deliveryFor(type: IntegrationTypeMeta): DeliveryMeta {
  return DELIVERY_META[type.delivery];
}
