/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { GIT_PAT_SCOPE } from "./git-pat";
import * as copy from "../copy";

// Packet M7's table, character for character. A rewording fails here instead of
// drifting from the approved mock.
describe("GIT_PAT_SCOPE (packet M7)", () => {
  it("is re-exported from copy.ts", () => {
    expect(copy.GIT_PAT_SCOPE).toBe(GIT_PAT_SCOPE);
  });

  it("carries the approved strings", () => {
    expect(GIT_PAT_SCOPE.SECTION_TITLE).toBe("Git access tokens");
    expect(GIT_PAT_SCOPE.SECTION_LEAD).toBe("Narrow what a run may do with each stored git access token.");
    expect(GIT_PAT_SCOPE.FORGE).toBe("Forge");
    expect(GIT_PAT_SCOPE.FORGE_HINT).toBe("How this host names repositories. Other is the strictest.");
    expect(Object.values(GIT_PAT_SCOPE.FORGE_OPTIONS).join(" · ")).toBe("GitLab · Bitbucket Server · Gitea · Other");
    expect(GIT_PAT_SCOPE.FORGE_OPTIONS.generic).toBe("Other");
    expect(GIT_PAT_SCOPE.REPOS).toBe("Repositories");
    expect(GIT_PAT_SCOPE.REPOS_HINT).toBe(
      "One path per line, e.g. group/project. End with /* for everything under a group. Leave empty for every repository the token reaches.",
    );
    expect(GIT_PAT_SCOPE.REPOS_NONE).toBe("No repositories. Runs can't use this token.");
    expect(GIT_PAT_SCOPE.ACCESS).toBe("Access");
    expect(GIT_PAT_SCOPE.ACCESS_WRITE).toBe("Read and push");
    expect(GIT_PAT_SCOPE.API).toBe("Forge API");
    expect(GIT_PAT_SCOPE.API_HINT).toBe(
      "Merge requests, comments and reads for the repositories above. Merging, file writes, GraphQL and search stay refused.",
    );
    expect(GIT_PAT_SCOPE.API_NEEDS_FORGE).toBe(
      "Choose GitLab or Gitea to allow the API. Other forges' APIs can't be narrowed.",
    );
    expect(GIT_PAT_SCOPE.HONESTY_TOKEN).toBe(
      "This narrows the run, not the token. Outside Wardyn the token can still do everything its issuer allowed.",
    );
    expect(GIT_PAT_SCOPE.HONESTY_API).toBe(
      "On the API, numeric project ids, GraphQL, search and the Other forge can't be narrowed, so Wardyn refuses them.",
    );
    expect(GIT_PAT_SCOPE.HONESTY_BROKER).toBe(
      "Narrowing needs the token broker. With it off, runs with a narrowed token are refused.",
    );
    expect(GIT_PAT_SCOPE.RUN_REPOS_ALL).toBe("Every repository the token reaches");
    expect(GIT_PAT_SCOPE.RUN_API).toBe("Forge API");
    expect(GIT_PAT_SCOPE.BROKER_OFF_NARROWING).toBe(
      "The token broker is off, so runs whose policy narrows a git access token are refused.",
    );
  });
});
