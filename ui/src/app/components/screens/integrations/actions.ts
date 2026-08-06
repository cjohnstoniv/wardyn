/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared rotate/delete semantics for an integration row — used by both the
// list screen's kebab and the detail page's header/danger-zone, so "delete
// this integration" means the same real thing wherever it's triggered.
//
// Every remaining row is backed by stored credential material, so deleting one
// means deleting that material: a generic secret goes through the secret store
// (its entire footprint IS the secret), a harness login (subscription / AWS
// SSO) is disconnected through its own endpoint, matching llm-access.tsx's
// disconnectManaged. An SCM row's credential deletion mirrors
// scm-provider-step.tsx's "Delete credential"/"Remove App" exactly — the
// host's site-config registration is untouched (a separate concern owned by
// the SCM Provider step). No site-config branch survives here: the two
// categories that existed only as a site-config field (host proxy, egress
// redirection) moved to the Corporate network step, which owns their removal.
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { harnessAuth as harnessAuthApi } from "../../../lib/api/harness-auth";
import { genericIntegrationsApi, integrationsApi, type IntegrationRow } from "../../../lib/api/integrations";
import type { WireIntegration } from "../../../lib/types/setup";

// The one secret a "Rotate credential…" click targets, for a row backed by
// more than one (the GitHub App: rotate the PEM, never the App ID — matches
// ScmProviderStep's own "Rotate PEM" item).
export function primarySecretName(row: IntegrationRow): string | undefined {
  if (row.isGithubApp) return "github-app-key";
  return row.secretNames[0];
}

// Whether this row has anything a generic "Rotate credential…" can act on —
// false for a harness login (rotate = reconnect, handled on the detail page)
// and for a row whose credential Wardyn only detects rather than stores (a
// host-CLI Claude login, a boot-config Bedrock lane).
export function canRotateInline(row: IntegrationRow): boolean {
  return !row.harnessProvider && row.secretNames.length > 0;
}

export async function deleteIntegration(row: IntegrationRow): Promise<void> {
  if (row.harnessProvider) {
    await harnessAuthApi.harnessDisconnect(row.harnessProvider);
    return;
  }
  const results = await Promise.allSettled(row.secretNames.map((n) => secretsApi.deleteSecret(n)));
  for (const r of results) if (r.status === "rejected") throw r.reason;
}

/** The two DefaultFor marks the console can toggle (types.Integration.DefaultFor's closed set). */
export type DefaultForMark = "agent_runs" | "wardyn_features";

// WireIntegration (lib/types/setup.ts) doesn't model config/disabled_capabilities
// yet, but the server echoes them on every read (types.Integration's own json
// tags) — read them off the wire object directly so a default-for PUT
// round-trips the row's full state. Skipping this would be a silent, real
// regression for any row Config carries (a Bedrock's region/model, a
// subscription's lane), not a cosmetic gap.
type FullWireIntegration = WireIntegration & { config?: unknown; disabled_capabilities?: string[] };

// Toggle one DefaultFor mark on a row, adopting it first when it has no
// stored identity yet (an unadopted legacy row can't be PUT directly — POST
// .../adopt persists it VERBATIM first, so this never has to reconstruct
// hosts/credentials it can't see client-side). PUT is a full replacement, so
// every other field round-trips from the effective wire row untouched — only
// `mark`'s membership changes. Radio semantics (naming a mark here clears it
// on every OTHER row) are enforced server-side (applyDefaultForRadio);
// callers should reload rather than guess the result.
export async function setDefaultFor(wire: WireIntegration, mark: DefaultForMark, on: boolean): Promise<void> {
  if (wire.source !== "stored") await integrationsApi.adoptIntegration(wire.id);
  const full = wire as FullWireIntegration;
  const current = wire.default_for ?? [];
  const default_for = on ? [...current, mark] : current.filter((m) => m !== mark);
  await genericIntegrationsApi.put(wire.id, {
    name: wire.name ?? "",
    category: wire.category,
    type: wire.type,
    disabled: wire.disabled,
    hosts: wire.hosts,
    header: wire.header,
    format: wire.format,
    docs: wire.docs,
    credentials: wire.credentials,
    config: full.config as Record<string, unknown> | undefined,
    disabled_capabilities: full.disabled_capabilities,
    default_for,
  });
}
