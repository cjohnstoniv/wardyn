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

/**
 * Removes the component the access row stands for: a stored one by id, an inline one by the row's own
 * "inline:<n>", the position the server read it at in the request's components (a name is optional).
 */
export function detachComponent(intent: RunIntent, id: string): RunIntent {
  const inline = /^inline:(\d+)$/.exec(id);
  const left = (intent.components ?? []).filter((c, i) => (inline ? i !== Number(inline[1]) : c.id !== id));
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

/**
 * An https URL's pieces the way the server reads them: `authority` keeps a
 * non-default port (the component's host entry must name it), `hostname` is the
 * bare host a header secret is delivered to. A URL with credentials or a
 * fragment is none of these.
 */
export function httpsParts(url: string): { authority: string; hostname: string } | undefined {
  try {
    const u = new URL(url.trim());
    if (u.protocol !== "https:" || u.username || u.password || u.hash) return undefined;
    return { authority: u.host, hostname: u.hostname };
  } catch {
    return undefined;
  }
}

/** A remote MCP server by URL, with its token (one saved secret) added by the proxy as a header, or none. */
export function mcpRef(name: string, url: string, tokenSecret: string | undefined): ComponentRef | undefined {
  const parts = httpsParts(url);
  if (!parts) return undefined;
  const inline: McpDefinition = {
    hosts: [parts.authority],
    ...(tokenSecret && { secrets: [{ secret_name: tokenSecret, delivery: { mode: "header" as const, host: parts.hostname } }] }),
    mcp: { url: url.trim(), transport: "http" },
  };
  return { name, inline };
}
