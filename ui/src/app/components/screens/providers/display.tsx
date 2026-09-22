/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Display + client-mirror helpers shared by the /providers tabs. None of them
// invent copy: they decide how a FROZEN string is presented, or mirror a
// server rule so a bad row is caught before an attempt — never a second
// opinion about what the server ultimately decides.
import type { GitLane, GitProvider, GitProviderKind } from "../../../lib/api/providers";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";

export const KIND_LABEL: Record<GitProviderKind, string> = {
  github: PROVIDERS.KIND_GITHUB,
  azure_devops: PROVIDERS.KIND_AZURE_DEVOPS,
};

// The lowercase host of a URL string, or "" if it doesn't parse — never
// throws, since a row mid-edit is routinely unparseable.
export function hostOf(raw: string): string {
  try {
    return new URL(raw.trim()).hostname.toLowerCase();
  } catch {
    return "";
  }
}

// adoEgressDomains' own classifier (internal/api/runs_scm.go:484), ported: the
// two ADO hosts, which belong to the azure_devops kind and to no other.
function isADOHost(host: string): boolean {
  return host === "dev.azure.com" || host.endsWith(".visualstudio.com");
}

// Client mirror of validateWorkspaceProviders' base-URL shape rule
// (internal/api/workspace_providers.go:230-310), checked BEFORE a credential is
// written (§2.5) — the server's own 400s (§7.1, unparsed) are still what a
// post-attempt refusal renders, under SAVE_REFUSED_TITLE. The exact-segment and
// kind x host branches below are validateProviderHostForKind's own four, ported
// case for case; the shared literal table both sides are checked against lives
// in display.test.tsx and internal/api/workspace_providers_test.go
// (TestBaseURLClientMirrorParity) — a rule changed on one side reds the other.
//
// Deliberately does NOT enforce "at least one path segment" on a host that is
// neither well-known (Q12, open at the prompt's Adjudication gate): the model
// admits a bare `https://git.corp.example` GHES-style host with an OPTIONAL org
// path (prompt §5.1), and a stricter client mirror would reject a row the server
// accepts.
export function baseURLError(raw: string, kind: GitProviderKind): string | null {
  const s = raw.trim();
  if (!s) return null;
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return PROVIDERS.BASE_URL_INVALID;
  }
  if (u.protocol !== "https:") return PROVIDERS.BASE_URL_INVALID;
  if (u.username || u.password) return PROVIDERS.BASE_URL_INVALID;
  if (u.port) return PROVIDERS.BASE_URL_INVALID;
  if (u.search || u.hash) return PROVIDERS.BASE_URL_INVALID;
  if (u.pathname.includes("*")) return PROVIDERS.BASE_URL_INVALID;
  // No percent-encoding in the path — the server refuses `u.Path !=
  // u.EscapedPath()`, which ANY escape in the path trips: "acme%2Fevil" hides a
  // second segment from the count below, and "%60id%60" decodes to a backtick
  // shellSafeSiteString refuses on sight.
  if (u.pathname.includes("%")) return PROVIDERS.BASE_URL_INVALID;
  const host = u.hostname.toLowerCase();
  // hostrules.ValidApprovedHost's dotted-host rule, reached through validSiteURL:
  // a single-label host ("localhost") is refused at write.
  if (!host.includes(".")) return PROVIDERS.BASE_URL_INVALID;
  const segments = u.pathname.split("/").filter(Boolean);
  if (segments.length > 2) return PROVIDERS.BASE_URL_INVALID;
  // validateProviderHostForKind: the two well-known hosts belong to exactly one
  // kind each, and each takes an exact number of path segments there. Any OTHER
  // host is accepted for either kind — nothing on the wire distinguishes a GHES
  // host from an ADO Server one, so the admin's own label is the only fact there.
  if (kind === "github") {
    if (isADOHost(host)) return PROVIDERS.BASE_URL_INVALID;
    // github.com takes an optional single /<org>; a deeper path names a repo.
    if (host === "github.com" && segments.length > 1) return PROVIDERS.BASE_URL_INVALID;
  } else {
    if (host === "github.com") return PROVIDERS.BASE_URL_INVALID;
    // dev.azure.com is shared by every org on the planet, so the organization
    // segment is REQUIRED there — and only the organization.
    if (host === "dev.azure.com" && segments.length !== 1) return PROVIDERS.BASE_URL_INVALID;
  }
  return null;
}

// Whether this row is one the server is GUARANTEED to refuse, so the screen can
// withhold Save rather than spend a round trip on a 400 it already knows about.
// Both arms mirror validateWorkspaceProviders exactly, and NEITHER is gated on
// `disabled`: the server validates every row it is handed, on or off.
//
// The zero-address arm exists for this (V1 r2): baseURLError("") returns
// null by design — a blank LINE in a textarea is not an error — so without
// it an emptied field flags nothing and commits `base_urls: []`, which the
// server refuses and which leaves the row's credential lanes with no host
// to key by.
export function gitRowInvalid(row: GitProvider): boolean {
  if (row.base_urls.length === 0) return true;
  return row.base_urls.some((u) => baseURLError(u, row.kind) !== null);
}

/** Every non-blank line of a base-URLs textarea that fails baseURLError. */
export function invalidBaseURLLines(text: string, kind: GitProviderKind): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l && baseURLError(l, kind) !== null);
}

// app: the App broker mints repository-scoped GitHub tokens on github.com
// only (prompt §2.4) — unavailable for azure_devops outright, and for a
// github row whose base URLs are all a GHES host (no github.com among them).
export function appLaneAvailable(kind: GitProviderKind, baseUrls: string[]): boolean {
  if (kind !== "github") return false;
  if (baseUrls.length === 0) return true;
  return baseUrls.some((u) => hostOf(u) === "github.com");
}

// ssh: available on github.com / dev.azure.com only (SSH-over-443); a
// self-hosted GHES/ADO-Server row clones over HTTPS (a documented ceiling).
export function sshLaneAvailable(baseUrls: string[]): boolean {
  if (baseUrls.length === 0) return true;
  return baseUrls.some((u) => hostOf(u) === "github.com" || hostOf(u) === "dev.azure.com");
}

// sshLaneExceedsPathScope mirrors the server's rule (workspace_providers.go's
// function of the same name) bit for bit (#380 F5): true when EVERY base URL
// matching an SSH-over-443 host (github.com or dev.azure.com, literal) carries
// a path. SSH scoping is host-level only — a bare-host entry means the row
// already bounds the whole host and the lane widens nothing; a path on every
// matching entry means ticking ssh would exceed what the row's own addresses
// declare, which the console door refuses outright.
//
// For azure_devops this is unconditionally true: dev.azure.com's OWN host-kind
// rule (validateProviderHostForKind) makes the org segment MANDATORY, so no
// legal Azure DevOps row can ever present a bare dev.azure.com entry — the
// explicit SSH lane is therefore never selectable there, not a bug this
// mirror should paper over.
export function sshLaneExceedsPathScope(baseUrls: string[]): boolean {
  let sawSSHHost = false;
  for (const raw of baseUrls) {
    let u: URL;
    try {
      u = new URL(raw.trim());
    } catch {
      continue;
    }
    const host = u.hostname.toLowerCase();
    if (host !== "github.com" && host !== "dev.azure.com") continue;
    sawSSHHost = true;
    if (u.pathname.replace(/^\/+|\/+$/g, "") === "") return false; // bounds the whole host already
  }
  return sawSSHHost;
}

export function laneUnavailableReason(lane: GitLane, kind: GitProviderKind, baseUrls: string[]): string | null {
  if (lane === "app" && !appLaneAvailable(kind, baseUrls)) return PROVIDERS.LANE_APP_UNAVAILABLE;
  if (lane === "ssh") {
    if (!sshLaneAvailable(baseUrls)) return PROVIDERS.LANE_SSH_UNAVAILABLE;
    if (sshLaneExceedsPathScope(baseUrls)) return PROVIDERS.LANE_SSH_PATH_SCOPED;
  }
  return null;
}

// The three chips render LANE_META's label/tooltip (lib/scm-provider.ts) — the
// residency honesty, imported and never re-frozen (§7.1). GitLane and
// scm-provider's Lane are the same three-value string union.
export { LANE_META } from "../../../lib/scm-provider";
