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
import type { IntegrationRow } from "../../../lib/api/integrations";

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
