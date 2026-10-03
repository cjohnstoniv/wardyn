/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The provider-dependent parts of the starter policy templates, derived from
// /setup/status `model_providers`. Absent, empty or all-off means the
// no-provider set: today's api.anthropic.com templates. A turned-off provider
// is left out (runs can't choose it), so a template never grants a host no run
// can use.
import type { RunPolicySpec, SetupModelProvider } from "../../lib/types";
import type { ModelProviderKind } from "../../lib/types/site";
import { MODEL_PROVIDERS } from "../../lib/model-providers-copy";
import { POLICY_TEMPLATE_COPY as C } from "./copy/policy-templates";

type Grant = NonNullable<RunPolicySpec["eligible_grants"]>[number];

// The two key kinds keep an api_key grant; every other kind is injected by the
// proxy or signed in the sandbox, and dispatch adds its host itself.
const KEY_GRANTS: Record<string, { header: string; format: string; secret_name: string }> = {
  anthropic_api_key: { header: "x-api-key", format: "%s", secret_name: "anthropic-api-key" },
  openai_api_key: { header: "Authorization", format: "Bearer %s", secret_name: "openai-api-key" },
};

export interface TemplateProviders {
  hosts: string[];
  grants: Grant[];
  /** Names for the caption; empty when no provider counts. */
  names: string[];
  /** The registries template's model hosts (both vendors when no provider counts). */
  registryHosts: string[];
  minimalHint: string;
  modelProviderHint: string;
}

const keyGrant = (host: string, g: { header: string; format: string; secret_name: string }): Grant => ({
  kind: "api_key",
  scope: { host, header: g.header, format: g.format, secret_name: g.secret_name },
  ttl_seconds: 3600,
  requires_approval: false,
});

const NONE: TemplateProviders = {
  hosts: ["api.anthropic.com"],
  grants: [keyGrant("api.anthropic.com", KEY_GRANTS.anthropic_api_key)],
  names: [],
  registryHosts: ["api.anthropic.com", "api.openai.com"],
  minimalHint: C.MINIMAL_HINT,
  modelProviderHint: C.MODEL_PROVIDER_HINT_KEY,
};

export function templateProviders(providers?: readonly SetupModelProvider[]): TemplateProviders {
  const active = (providers ?? []).filter((p) => !p.disabled && p.host);
  if (active.length === 0) return NONE;
  const hosts = [...new Set(active.map((p) => p.host))];
  const grants: Grant[] = [];
  for (const p of active) {
    const g = KEY_GRANTS[p.kind];
    if (g && !grants.some((x) => (x.scope as { host?: string }).host === p.host)) grants.push(keyGrant(p.host, g));
  }
  const names = [
    ...new Set(active.map((p) => p.name || MODEL_PROVIDERS.KIND[p.kind as ModelProviderKind] || p.kind)),
  ];
  const many = hosts.length > 1;
  const kind = active[0].kind;
  return {
    hosts,
    grants,
    names,
    registryHosts: hosts,
    minimalHint: many ? C.MINIMAL_HINT_MANY : C.MINIMAL_HINT,
    modelProviderHint: many
      ? C.MODEL_PROVIDER_HINT_MANY(hosts.length)
      : kind === "bedrock_sso"
        ? C.MODEL_PROVIDER_HINT_SSO
        : KEY_GRANTS[kind]
          ? C.MODEL_PROVIDER_HINT_KEY
          : C.MODEL_PROVIDER_HINT_PROXY,
  };
}
