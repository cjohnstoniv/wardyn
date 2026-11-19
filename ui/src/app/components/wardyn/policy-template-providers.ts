/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The provider-dependent parts of the starter policy templates, derived from
// /setup/status `model_providers`. Absent, empty or all-off means the
// no-provider set: today's api.anthropic.com templates. A turned-off provider
// is left out (runs can't choose it), so a template never grants a host no run
// can use.
import type { SetupModelProvider } from "../../lib/types";
import type { ModelProviderKind } from "../../lib/types/site";
import { MODEL_PROVIDERS } from "../../lib/model-providers-copy";
import { POLICY_TEMPLATE_COPY as C } from "./copy/policy-templates";

// No template carries an api_key grant for a model host. Each kind's credential
// is the person's own, injected by the proxy or signed in the sandbox, and
// dispatch drops any policy grant on a model host or under a retired model key
// name (the API refuses to store those names), so such a grant could never
// deliver a key. The two key kinds differ only in their hint.
const KEY_KINDS = new Set(["anthropic_api_key", "openai_api_key"]);

export interface TemplateProviders {
  hosts: string[];
  /** Names for the caption; empty when no provider counts. */
  names: string[];
  /** The registries template's model hosts (both vendors when no provider counts). */
  registryHosts: string[];
  minimalHint: string;
  modelProviderHint: string;
}

const NONE: TemplateProviders = {
  hosts: ["api.anthropic.com"],
  names: [],
  registryHosts: ["api.anthropic.com", "api.openai.com"],
  minimalHint: C.MINIMAL_HINT,
  modelProviderHint: C.MODEL_PROVIDER_HINT_KEY,
};

export function templateProviders(providers?: readonly SetupModelProvider[]): TemplateProviders {
  const active = (providers ?? []).filter((p) => !p.disabled && p.host);
  if (active.length === 0) return NONE;
  const hosts = [...new Set(active.map((p) => p.host))];
  const names = [
    ...new Set(active.map((p) => p.name || MODEL_PROVIDERS.KIND[p.kind as ModelProviderKind] || p.kind)),
  ];
  const many = hosts.length > 1;
  const kind = active[0].kind;
  return {
    hosts,
    names,
    registryHosts: hosts,
    minimalHint: many ? C.MINIMAL_HINT_MANY : C.MINIMAL_HINT,
    modelProviderHint: many
      ? C.MODEL_PROVIDER_HINT_MANY(hosts.length)
      : kind === "bedrock_sso"
        ? C.MODEL_PROVIDER_HINT_SSO
        : KEY_KINDS.has(kind)
          ? C.MODEL_PROVIDER_HINT_KEY
          : C.MODEL_PROVIDER_HINT_PROXY,
  };
}
