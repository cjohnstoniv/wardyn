/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared rotate/delete semantics for an integration row — used by both the
// list screen's kebab and the detail page's header/danger-zone, so "delete
// this integration" means the same real thing wherever it's triggered.
//
// A harness login (subscription / AWS SSO) is disconnected through its own
// endpoint — a captured session isn't secret-store material, so the generic
// delete path below can't reach it. Every OTHER row's stored secret is left
// alone (UI-WS-4): it may be referenced by another workspace or site-config
// field, and blastRadius's own confirm copy already tells the operator so —
// deleting it here would silently break every other consumer. Remove it
// explicitly under Secrets when it's genuinely no longer needed. What delete
// DOES undo server-side: an SCM row's site-config scm_hosts registration
// (SCM-SEAM-1 — the removal control that died with the retired SCM Provider
// step), and, for ANY legacy row that was adopted, the stored Integration
// itself (its default_for radio mark + renamed identity) via the server's
// DELETE endpoint — a 404 there just means the row was never adopted, so
// there was nothing stored to remove. The credential-derived row still
// reappears on the next read until its secret is removed under Secrets (the
// server's own contract, handleDeleteIntegration). Host proxy / egress
// redirection still don't belong here: those two categories moved to the
// Corporate network step, which owns their removal.
import { harnessAuth as harnessAuthApi } from "../../../lib/api/harness-auth";
import { health } from "../../../lib/api/health";
import { HttpError } from "../../../lib/api/core";
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
  // The stored secret is deliberately NOT deleted (UI-WS-4) — see this file's
  // header comment.
  if (row.category === "scm_host") {
    // Re-GET (never the caller's possibly-stale copy) and drop the host only
    // when it's actually a member — a derivedFrom guess row was never
    // registered, so it must trigger no write. Case-insensitive: the site
    // config stores whatever case the operator typed, while row.typeLabel
    // (deriveProviders' r.host) is already lowercased.
    const siteConfig = await health.getSiteConfig();
    const hosts = siteConfig.scm_hosts ?? [];
    const remaining = hosts.filter((h) => h.trim().toLowerCase() !== row.typeLabel);
    if (remaining.length !== hosts.length) {
      await health.putSiteConfig({ ...siteConfig, scm_hosts: remaining });
    }
  }
  // Remove the ADOPTED stored integration — the one piece of server-side state
  // a legacy row can own (its default_for radio mark + a renamed identity),
  // written by setDefaultFor's adopt-then-PUT. Without this the UI's Delete
  // never called DELETE /integrations at all for a legacy row: deleting an
  // adopted default-holder left its default_for standing, so agent runs kept
  // resolving it as the default the blast-radius copy just warned they'd lose.
  // A never-adopted row has nothing stored — the server 404s and that IS the
  // done state (its credential-derived row keeps reappearing on the next read,
  // per handleDeleteIntegration's own contract, until the secret is removed
  // under Secrets, which is exactly what the confirm copy directs). Any other
  // status is a real failure and propagates, like the site-config write above.
  if (row.serverId) {
    await genericIntegrationsApi.remove(row.serverId).catch((e) => {
      if (e instanceof HttpError && e.status === 404) return;
      throw e;
    });
  }
}

/** The two DefaultFor marks the console can toggle (types.Integration.DefaultFor's closed set). */
export type DefaultForMark = "agent_runs" | "wardyn_features";

// Toggle one DefaultFor mark on a row, adopting it first when it has no
// stored identity yet (an unadopted legacy row can't be PUT directly — POST
// .../adopt persists it VERBATIM first, so this never has to reconstruct
// hosts/credentials it can't see client-side). PUT is a full replacement, so
// every other field round-trips from the effective wire row untouched — only
// `mark`'s membership changes. Radio semantics (naming a mark here clears it
// on every OTHER row) are enforced server-side (applyDefaultForRadio);
// callers should reload rather than guess the result.
//
// Atomic from the operator's point of view: a fresh adopt is only a means to
// the PUT, never the goal itself. If the PUT rejects (e.g. a half-set Bedrock
// row — region set, model empty — passes the checkbox gate but hard-400s
// server-side), the just-adopted row is deleted again so a failed "set
// default" attempt doesn't leave behind a row the operator never asked to
// store. The rollback is best-effort (its own failure is swallowed) — either
// way the PUT's real error propagates to the caller, which is what actually
// explains the failure.
export async function setDefaultFor(wire: WireIntegration, mark: DefaultForMark, on: boolean): Promise<void> {
  const adopted = wire.source !== "stored";
  if (adopted) await integrationsApi.adoptIntegration(wire.id);
  const current = wire.default_for ?? [];
  const default_for = on ? [...current, mark] : current.filter((m) => m !== mark);
  try {
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
      config: wire.config,
      disabled_capabilities: wire.disabled_capabilities,
      default_for,
    });
  } catch (e) {
    if (adopted) await genericIntegrationsApi.remove(wire.id).catch(() => {});
    throw e;
  }
}
