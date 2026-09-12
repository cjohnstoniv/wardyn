/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Display + client-mirror helpers shared by the /providers tabs. None of them
// invent copy: they decide how a FROZEN string is presented, or mirror a
// server rule so a bad row is caught before an attempt — never a second
// opinion about what the server ultimately decides.
import type { GitLane, GitProviderKind } from "../../../lib/api/providers";
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

// Client mirror of validateWorkspaceProviders' base-URL shape rule
// (internal/api/workspace_providers.go), checked BEFORE a credential is
// written (§2.5) — the server's own 400s (§7.1, unparsed) are still what a
// post-attempt refusal renders, under SAVE_REFUSED_TITLE. Deliberately does
// NOT enforce "at least one path segment" (Q12, open at the prompt's
// Adjudication gate): the model admits a bare `https://github.com` GHES-style
// host with an optional org path (prompt §5.1), and a stricter client mirror
// would reject a row the server accepts. dev.azure.com's REQUIRED org path
// (§5.1) is unambiguous and is enforced here.
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
  const segments = u.pathname.split("/").filter(Boolean);
  if (segments.length > 2) return PROVIDERS.BASE_URL_INVALID;
  if (kind === "azure_devops" && u.hostname.toLowerCase() === "dev.azure.com" && segments.length < 1) {
    return PROVIDERS.BASE_URL_INVALID;
  }
  return null;
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

// The SSH scoping CEILING is visible exactly when both halves are true: this row
// carries an ORG PATH (so it reads as bounded) and it PERMITS the ssh lane (so a
// clone can actually take the host-level route). Either alone is honest already —
// a bare host bounds nothing to widen, and a row with no ssh lane never widens.
export function sshScopedHostLevel(baseUrls: string[], permitsSSH: boolean): boolean {
  if (!permitsSSH) return false;
  return baseUrls.some((raw) => {
    try {
      return new URL(raw.trim()).pathname.replace(/^\/+|\/+$/g, "") !== "";
    } catch {
      return false;
    }
  });
}

export function laneUnavailableReason(lane: GitLane, kind: GitProviderKind, baseUrls: string[]): string | null {
  if (lane === "app" && !appLaneAvailable(kind, baseUrls)) return PROVIDERS.LANE_APP_UNAVAILABLE;
  if (lane === "ssh" && !sshLaneAvailable(baseUrls)) return PROVIDERS.LANE_SSH_UNAVAILABLE;
  return null;
}

// The three chips render LANE_META's label/tooltip (lib/scm-provider.ts) — the
// residency honesty, imported and never re-frozen (§7.1). GitLane and
// scm-provider's Lane are the same three-value string union.
export { LANE_META } from "../../../lib/scm-provider";
