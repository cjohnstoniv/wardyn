/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared rotate/delete semantics for an integration row — used by both the
// list screen's kebab and the detail page's header/danger-zone, so "delete
// this integration" means the same real thing wherever it's triggered.
//
// A row backed by a generic secret is torn down by deleting that secret (its
// entire footprint IS the secret); a harness login (subscription / AWS SSO)
// is disconnected through its own endpoint, matching llm-access.tsx's
// disconnectManaged. An SCM row's credential deletion mirrors
// scm-provider-step.tsx's "Delete credential"/"Remove App" exactly — the
// host's site-config registration is untouched (a separate concern owned by
// the SCM Provider step). Artifact-mirror/host-proxy rows exist ONLY because
// of a site-config field, so deleting them means unregistering that field —
// never touching a secret that may be reused elsewhere.
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { harnessAuth as harnessAuthApi } from "../../../lib/api/harness-auth";
import { health } from "../../../lib/api/health";
import type { IntegrationRow } from "../../../lib/api/integrations";
import type { SiteConfig } from "../../../lib/types";

// The one secret a "Rotate credential…" click targets, for a row backed by
// more than one (the GitHub App: rotate the PEM, never the App ID — matches
// ScmProviderStep's own "Rotate PEM" item).
export function primarySecretName(row: IntegrationRow): string | undefined {
  if (row.isGithubApp) return "github-app-key";
  return row.secretNames[0];
}

// Whether this row has anything a generic "Rotate credential…" can act on —
// false for a harness login (rotate = reconnect, handled on the detail page)
// and for a site-config-only row with no secret ref (e.g. an unauthenticated
// mirror override).
export function canRotateInline(row: IntegrationRow): boolean {
  return !row.harnessProvider && row.secretNames.length > 0;
}

function hostnameOf(url: string): string {
  try {
    return new URL(url).hostname || url;
  } catch {
    return url;
  }
}

export async function deleteIntegration(row: IntegrationRow, siteConfig: SiteConfig | null): Promise<void> {
  if (row.category === "ai_provider") {
    if (row.harnessProvider) {
      await harnessAuthApi.harnessDisconnect(row.harnessProvider);
      return;
    }
    const results = await Promise.allSettled(row.secretNames.map((n) => secretsApi.deleteSecret(n)));
    for (const r of results) if (r.status === "rejected") throw r.reason;
    return;
  }

  if (row.category === "scm_host") {
    const results = await Promise.allSettled(row.secretNames.map((n) => secretsApi.deleteSecret(n)));
    for (const r of results) if (r.status === "rejected") throw r.reason;
    return;
  }

  if (row.category === "artifact_mirror") {
    // Current shape: row.redirect carries the exact entry to drop, addressed
    // by its position in SiteConfig.egress_redirects (the id encodes that
    // index — see lib/api/integrations.ts's egressRedirectRows).
    if (row.redirect) {
      const redirects = siteConfig?.egress_redirects ?? [];
      const i = Number(row.id.slice("mirror:".length));
      await health.putSiteConfig({ ...(siteConfig ?? {}), egress_redirects: redirects.filter((_, j) => j !== i) });
      return;
    }
    // Legacy shape: a row groups every ecosystem override pointed at the same
    // destination host — drop them all.
    const overrides = { ...(siteConfig?.artifact_overrides ?? {}) };
    for (const [eco, ov] of Object.entries(overrides)) {
      if (hostnameOf(ov.base_url) === row.name) delete overrides[eco];
    }
    await health.putSiteConfig({ ...(siteConfig ?? {}), artifact_overrides: overrides });
    return;
  }

  // host_proxy — a singleton keyed off either upstream_proxy_url (plain) or
  // upstream_proxy_secret_ref (credentialed); clear whichever is set.
  await health.putSiteConfig({ ...(siteConfig ?? {}), upstream_proxy_url: undefined, upstream_proxy_secret_ref: undefined });
}
