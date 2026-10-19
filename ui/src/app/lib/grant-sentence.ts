/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Reads a credential grant's scope as a sentence, for the Approvals card and the run's Credentials widget:
// people see "GitHub token · read contents · no repositories listed", never the API's JSON.
export type GrantScope = Record<string, unknown>;

// The GitHub CLI's grant (api: true) carries capabilities instead of permissions; these name a write.
const WRITE_CAPABILITY = /write|admin|execute|^pr$/;

function capabilities(scope: GrantScope): string[] {
  return Array.isArray(scope.capabilities) ? scope.capabilities.filter((c): c is string => typeof c === "string") : [];
}

/** Does the github_token scope grant any write, admin, push or maintain, by permission or by capability? */
export function grantsWrite(scope: GrantScope): boolean {
  if (capabilities(scope).some((c) => WRITE_CAPABILITY.test(c))) return true;
  const perms = scope.permissions;
  if (perms && typeof perms === "object") {
    return Object.values(perms as Record<string, unknown>).some(
      (v) => typeof v === "string" && /write|admin|push|maintain/i.test(v),
    );
  }
  return false;
}

/** The repositories a github_token scope names. */
export function scopeRepos(scope: GrantScope): string[] {
  return Array.isArray(scope.repos) ? scope.repos.filter((r): r is string => typeof r === "string") : [];
}

const KIND_LABEL: Record<string, string> = {
  github_token: "GitHub token",
  git_pat: "Git access token",
  ssh_key: "SSH key",
  api_key: "API key",
  env_secret: "Environment secret",
  file_secret: "File secret",
};

function permissionPhrases(scope: GrantScope): string[] {
  const perms = scope.permissions;
  if (!perms || typeof perms !== "object") return [];
  return Object.entries(perms as Record<string, unknown>)
    .filter(([, level]) => typeof level === "string")
    .map(([resource, level]) => `${level} ${resource.replace(/_/g, " ")}`);
}

/** "GitHub token · read contents · no repositories listed". An unknown kind keeps its wire word. */
export function grantSentence(kind: string, scope: GrantScope): string {
  const parts = [KIND_LABEL[kind] ?? kind];
  if (kind === "github_token") {
    const perms = [...permissionPhrases(scope), ...capabilities(scope).map((c) => (c === "pr" ? "pull requests" : c.replace(/_/g, " ")))];
    if (perms.length) parts.push(perms.join(", "));
    const repos = scopeRepos(scope);
    parts.push(repos.length ? repos.join(", ") : "no repositories listed");
  } else if (typeof scope.host === "string" && scope.host) {
    parts.push(scope.host);
  }
  return parts.join(" · ");
}
