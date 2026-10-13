/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { AccessCredential, AccessFacts, AccessHost, AccessRow } from "../../types/new-run-access";
import type { SetupItem } from "../../types/preflight";
import { everyScenario, type ByScenario } from "./index";

/** The server names a harness row by its id; the console maps it to these (GET /setup/status harnesses). */
export const HARNESS_NAMES: ReadonlyMap<string, string> = new Map([["claude-code", "Claude Code"], ["codex-cli", "Codex CLI"]]);

const BEDROCK_HOST = "bedrock-runtime.us-east-1.amazonaws.com";

const harnessRow = (id: string, over: Partial<AccessRow> = {}): AccessRow => ({
  id: `agent:${id}`, kind: "harness", from: "harness", name: id, status: "ready",
  hosts: [BEDROCK_HOST], credentials: [`agent:${id}:model`], source: { kind: "harness", name: id }, ...over,
});
const modelCredential = (id: string, over: Partial<AccessCredential> = {}): AccessCredential => ({
  id: `agent:${id}:model`, row: `agent:${id}`, kind: "model_provider", owner: "own", delivery: "proxy", after_end: "gone", ...over,
});
const host = (name: string, rows: string[], over: Partial<AccessHost> = {}): AccessHost => ({
  host: name, via: "egress", rows, verdict: "allowed", source: { kind: "component" }, ...over,
});

const REPO_ROW: AccessRow = {
  id: "git_provider:github:app", kind: "repository", from: "repository", name: "acme/web", status: "ready",
  hosts: [], credentials: ["git_provider:github:app:cred"], source: { kind: "workspace" },
};
const REPO_CREDENTIAL: AccessCredential = {
  id: "git_provider:github:app:cred", row: REPO_ROW.id, kind: "github_app", owner: "org_app", delivery: "git_broker", after_end: "gone",
};
const DRIVE_ROW: AccessRow = { id: "drive:main", kind: "drive", from: "drive", name: "Your drive", status: "ready", hosts: [], credentials: [], source: { kind: "workspace" } };
const COMMAND_ROW: AccessRow = { id: "command", kind: "command", from: "command", name: "npm test", status: "ready", hosts: ["registry.npmjs.org"], credentials: [], source: { kind: "policy" } };

const MEMBER: AccessFacts = {
  rows: [harnessRow("claude-code"), REPO_ROW, DRIVE_ROW],
  hosts: [host(BEDROCK_HOST, ["agent:claude-code"], { via: "model_lane", source: { kind: "model_provider" } })],
  credentials: [modelCredential("claude-code"), REPO_CREDENTIAL],
  unlisted: { mode: "deny_with_review", allow_all: false, decider: "you" },
  harnesses: [],
  setup: [],
};

const CHOOSE_PROVIDER: SetupItem = {
  id: "llm_access:codex-cli", kind: "llm_access", label: "Choose a model provider", required_by: "codex-cli", status: "missing",
  fix: { action: "choose_model_provider", tool: "codex-cli" },
};
const SIGN_IN: SetupItem = {
  id: "llm_access:claude-code", kind: "llm_access", label: "Connect Bedrock", required_by: "claude-code", status: "missing",
  fix: { action: "sign_in", target: "model_provider" },
};
/** A secret an added secret or a template names and the caller does not have. */
const ADD_SECRET: SetupItem = {
  id: "secret:deploy-key", kind: "secret", label: "deploy-key", required_by: "Application", status: "missing",
  fix: { action: "add_secret", secret_name: "deploy-key" },
};

/**
 * A Bedrock SSO provider with the proxy-inject switch on: one short-lived
 * credential Wardyn renews. Off: the session's access token is in the sandbox as
 * a file too, and the role credentials last only as long as it does.
 */
export const BEDROCK_SSO_SWITCH_ON: AccessFacts = {
  ...MEMBER,
  credentials: [modelCredential("claude-code", { delivery: "sandbox_short_lived", renewed: true, after_end: "expires" }), REPO_CREDENTIAL],
};
export const BEDROCK_SSO_SWITCH_OFF: AccessFacts = {
  ...MEMBER,
  rows: [harnessRow("claude-code", { credentials: ["agent:claude-code:model", "agent:claude-code:sso_token"] }), REPO_ROW, DRIVE_ROW],
  credentials: [
    modelCredential("claude-code", { delivery: "sandbox_short_lived", after_end: "expires" }),
    modelCredential("claude-code", { id: "agent:claude-code:sso_token", delivery: "sandbox_file", after_end: "expires" }),
    REPO_CREDENTIAL,
  ],
};

/** The `access` block of POST /runs/policy-preview, per scenario. */
export const SCENARIO_ACCESS: ByScenario<AccessFacts> = everyScenario(MEMBER, {
  governed: { ...MEMBER, unlisted: { mode: "always_deny", allow_all: false, decider: "admin" } },
  admin: { ...MEMBER, unlisted: { mode: "wait_for_review", allow_all: false, hold_sec: 30, decider: "you" } },
  interactive2: {
    ...MEMBER,
    rows: [
      harnessRow("claude-code"),
      harnessRow("codex-cli", { status: "needs_setup", hosts: [], credentials: [] }),
      REPO_ROW,
      DRIVE_ROW,
    ],
    setup: [CHOOSE_PROVIDER],
  },
  app: {
    ...MEMBER,
    rows: [COMMAND_ROW, REPO_ROW, DRIVE_ROW],
    hosts: [host("registry.npmjs.org", ["policy"], { source: { kind: "policy" } })],
    credentials: [REPO_CREDENTIAL],
    setup: [ADD_SECRET],
  },
});

export const SETUP_ITEMS = { chooseProvider: CHOOSE_PROVIDER, signIn: SIGN_IN, addSecret: ADD_SECRET } as const;
