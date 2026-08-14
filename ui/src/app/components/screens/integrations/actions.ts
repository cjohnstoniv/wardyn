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
import { genericIntegrationsApi, type IntegrationRow } from "../../../lib/api/integrations";
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

// deleteWireRow is deleteIntegration's twin for the base-component screens
// (list/detail, B3): same two rules, read straight off the wire row instead
// of the legacy IntegrationRow shape. A harness login has no wire secret at
// all (delivery is absent on its bespoke lane) and disconnects through its
// own endpoint; every other row's stored secret is left alone (UI-WS-4).
// git_host is the one kind whose site-config coupling survives the rebuild:
// gitHostRows (integrations.go) still derives from SiteConfig.ScmHosts, so a
// row's own `name` IS the bare host (SCM-SEAM-1 — without this the host stays
// in every future run's egress allowlist with no surface left to revoke it).
export async function deleteWireRow(wire: WireIntegration, harnessProvider?: string): Promise<void> {
  if (harnessProvider) {
    await harnessAuthApi.harnessDisconnect(harnessProvider);
    return;
  }
  if (wire.kind === "git_host") {
    const siteConfig = await health.getSiteConfig();
    const hosts = siteConfig.scm_hosts ?? [];
    const remaining = hosts.filter((h) => h.trim().toLowerCase() !== wire.name?.trim().toLowerCase());
    if (remaining.length !== hosts.length) {
      await health.putSiteConfig({ ...siteConfig, scm_hosts: remaining });
    }
  }
  // A never-adopted derived row 404s — that IS the done state (see
  // deleteIntegration's header comment; same server contract).
  await genericIntegrationsApi.remove(wire.id).catch((e) => {
    if (e instanceof HttpError && e.status === 404) return;
    throw e;
  });
}

/** The two DefaultFor marks the console can toggle (types.Integration.DefaultFor's closed set). */
export type DefaultForMark = "agent_runs" | "wardyn_features";

// Toggle one DefaultFor mark on an already-STORED row. PUT is a full
// replacement, so every other field round-trips from the effective wire row
// untouched — only `mark`'s membership changes. Radio semantics (naming a
// mark here clears it on every OTHER row) are enforced server-side
// (applyDefaultForRadio); callers should reload rather than guess the result.
//
// Never adopts (owner-approved mock): a derived row has no default-for
// control to toggle in the first place — the list/detail screens gate the
// checkbox on `wire.source === "stored"` and offer "Adopt to edit" instead,
// the only promotion path for a derived row. Silently adopting-on-toggle used
// to live here; it's gone because a checkbox must never have a write this
// large (a whole new stored row) as an invisible side effect.
export async function setDefaultFor(wire: WireIntegration, mark: DefaultForMark, on: boolean): Promise<void> {
  const current = wire.default_for ?? [];
  const default_for = on ? [...current, mark] : current.filter((m) => m !== mark);
  await genericIntegrationsApi.put(wire.id, {
    name: wire.name ?? "",
    kind: wire.kind,
    disabled: wire.disabled,
    secrets: wire.secrets,
    egress: wire.egress,
    config: wire.config,
    probe: wire.probe,
    docs: wire.docs,
    disabled_capabilities: wire.disabled_capabilities,
    default_for,
  });
}
