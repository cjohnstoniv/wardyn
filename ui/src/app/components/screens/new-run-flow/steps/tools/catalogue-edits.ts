/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add tools and access' edits to the draft. Each addition has exactly one
// carrier, and none writes inline_policy or policy_overlay (DS-7 7.1): a host is
// overrides.hosts.add_allowed, a header secret overrides.agent.add_secrets, and
// anything the proxy cannot carry alone (a file, an environment variable, an MCP
// server) is an inline component beside the policy.

import type { ComponentDefinition, ComponentRef } from "../../../../../lib/types/components";
import type { SecretOverride } from "../../../../../lib/types/new-run-contract";
import { withKeys } from "../../lenses/lens-keys";
import type { RunIntent } from "../../draft/run-intent";

/** An inline MCP server: the definition's `mcp` block (DS-7 4.1) until the wire type carries it. */
export type McpDefinition = ComponentDefinition & { mcp: { url: string; transport: "http" } };

export function attachComponent(intent: RunIntent, ref: ComponentRef): RunIntent {
  if (ref.id && intent.components?.some((c) => c.id === ref.id)) return intent;
  return withKeys(intent, { components: [...(intent.components ?? []), ref] });
}

/** Removes the component the access row stands for: a stored one by id, an inline one by the name it was added under. */
export function detachComponent(intent: RunIntent, id: string, name?: string): RunIntent {
  const left = (intent.components ?? []).filter((c) => c.id !== id && !(name && !c.id && c.name === name));
  return withKeys(intent, { components: left.length ? left : undefined });
}

/** The person's own secret as a request header the proxy adds for one host. */
export function addHeaderSecret(intent: RunIntent, secret: SecretOverride): RunIntent {
  const agent = intent.overrides?.agent;
  return withKeys(intent, {
    overrides: { ...intent.overrides, agent: { ...agent, add_secrets: [...(agent?.add_secrets ?? []), secret] } },
  });
}

export function removeHeaderSecret(intent: RunIntent, name: string): RunIntent {
  const agent = intent.overrides?.agent;
  const left = (agent?.add_secrets ?? []).filter((s) => s.secret_name !== name);
  const { add_secrets: _dropped, ...rest } = agent ?? {};
  return withKeys(intent, { overrides: { ...intent.overrides, agent: left.length ? { ...rest, add_secrets: left } : rest } });
}

export function removeAllowedHost(intent: RunIntent, host: string): RunIntent {
  const hosts = intent.overrides?.hosts;
  const left = (hosts?.add_allowed ?? []).filter((h) => h !== host);
  return withKeys(intent, { overrides: { ...intent.overrides, hosts: { ...hosts, add_allowed: left } } });
}

/** One saved secret placed in the sandbox, as an environment variable or a file, by an inline component with no hosts. */
export function residentSecretRef(secretName: string, delivery: { mode: "env"; var: string } | { mode: "file"; file: string }): ComponentRef {
  return { name: secretName, inline: { hosts: [], secrets: [{ secret_name: secretName, delivery }] } };
}

/** The bare host of an https URL, or undefined for anything else. */
export function httpsHost(url: string): string | undefined {
  try {
    const u = new URL(url.trim());
    return u.protocol === "https:" && !u.username && !u.password ? u.hostname : undefined;
  } catch {
    return undefined;
  }
}

/** A remote MCP server by URL, with its token (one saved secret) added by the proxy as a header, or none. */
export function mcpRef(name: string, url: string, tokenSecret: string | undefined): ComponentRef | undefined {
  const host = httpsHost(url);
  if (!host) return undefined;
  const inline: McpDefinition = {
    hosts: [host],
    ...(tokenSecret && { secrets: [{ secret_name: tokenSecret, delivery: { mode: "header" as const, host } }] }),
    mcp: { url: url.trim(), transport: "http" },
  };
  return { name, inline };
}
