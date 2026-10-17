/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The git access token narrowing strings (packet M7, approved 2026-10-03) —
// canon, character for character; copy/git-pat.test.ts pins every one.
// Reused, not re-keyed: SUMMARY.grantKinds.git_pat "Git access token" and
// SUMMARY.readOnly "Read-only" (run-detail/policy-tab-copy.ts).
export const GIT_PAT_SCOPE = {
  SECTION_TITLE: "Git access tokens",
  SECTION_LEAD: "Narrow what a run may do with each stored git access token.",
  FORGE: "Forge",
  FORGE_HINT: "How this host names repositories. Other is the strictest.",
  // GitLab · Bitbucket Server · Gitea · Other. `generic` is "Other".
  FORGE_OPTIONS: {
    gitlab: "GitLab",
    bitbucket_server: "Bitbucket Server",
    gitea: "Gitea",
    generic: "Other",
  } as Record<string, string>,
  REPOS: "Repositories",
  REPOS_HINT:
    "One path per line, e.g. group/project. End with /* for everything under a group. Leave empty for every repository the token reaches.",
  REPOS_NONE: "No repositories. Runs can't use this token.",
  ACCESS: "Access",
  ACCESS_WRITE: "Read and push",
  API: "Forge API",
  API_HINT:
    "Merge requests, comments and reads for the repositories above. Merging, file writes, GraphQL and search stay refused.",
  API_NEEDS_FORGE: "Choose GitLab or Gitea to allow the API. Other forges' APIs can't be narrowed.",
  HONESTY_TOKEN:
    "This narrows the run, not the token. Outside Wardyn the token can still do everything its issuer allowed.",
  HONESTY_API:
    "On the API, numeric project ids, GraphQL, search and the Other forge can't be narrowed, so Wardyn refuses them.",
  HONESTY_BROKER: "Narrowing needs the token broker. With it off, runs with a narrowed token are refused.",
  RUN_REPOS_ALL: "Every repository the token reaches",
  RUN_API: "Forge API",
  BROKER_OFF_NARROWING: "The token broker is off, so runs whose policy narrows a git access token are refused.",
} as const;
