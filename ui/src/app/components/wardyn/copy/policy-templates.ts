/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policy template copy, from mock packet M9 (approved 2026-10-03). The reused
// literals moved here unchanged from policy-panel.tsx.
export const POLICY_TEMPLATE_COPY = {
  START: "Start from a template",
  FROM_PROVIDERS: (names: string[]) =>
    `Model hosts follow this deployment's model providers: ${names.join(", ")}.`,
  MINIMAL: "Minimal",
  MINIMAL_HINT: "The model provider and nothing else; unlisted hosts raise an approval.",
  MINIMAL_HINT_MANY: "The model providers and nothing else; unlisted hosts raise an approval.",
  MODEL_PROVIDER: "Model provider only",
  MODEL_PROVIDER_HINT_KEY: "One host, one proxy-injected API key. Nothing else is reachable.",
  MODEL_PROVIDER_HINT_PROXY:
    "One host, each person's own credential injected by the proxy. Nothing else is reachable.",
  MODEL_PROVIDER_HINT_SSO:
    "One host, no key: each person's AWS sign-in signs requests inside the sandbox. Nothing else is reachable.",
  MODEL_PROVIDER_HINT_MANY: (n: number) =>
    `${n} hosts, each with its own credential. Nothing else is reachable.`,
  REGISTRIES: "Package registries",
  REGISTRIES_HINT: "Model providers plus the language package registries — the build-and-install set.",
  CI: "CI baseline",
  CI_HINT: "No egress, no grants, stopped after an idle hour — the unattended default.",
  ALLOW_ALL: "Allow-all — observe first",
  ALLOW_ALL_HINT: "Reaches almost any site (except a block-list); watch the audit log, then tighten.",
  // Mock packet 088-mock C2: New Run's "Use the default policy" mode.
  DEFAULT_TITLE: "Use the default policy",
  DEFAULT_HINT: "Launch under the policy set for this deployment.",
  DEFAULT_HINT_PROFILE: (name: string) => `Launch under the policy set by your profile, ${name}.`,
  DEFAULT_PREVIEW: "Default policy, read-only",
  // The attached workspace still mounts into the run, so the note says so.
  DEFAULT_NOTE:
    "This run launches under this policy as it stands. Your attached workspace mounts into it; nothing else on this page is merged.",
  // The default lane carries one workspace by reference, so a second attached
  // one holds Launch and Check again rather than being left out of the run.
  DEFAULT_ONE_WORKSPACE:
    "The default policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all.",
  DEFAULT_LOADING: "Loading the default policy…",
  DEFAULT_UNAVAILABLE: "Couldn't load the default policy to show here. The run still launches under it.",
} as const;
