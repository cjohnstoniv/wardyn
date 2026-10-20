/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { ComponentFact } from "../../../../../lib/types/components";
import { clusterName, githubRepos, kubectlLimits } from "./tool-access";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";

const fact = (over: Partial<ComponentFact>): ComponentFact => ({ kind: "git_provider", id: "x", reason: "workspace", status: "ready", requirements: [], ...over });

describe("githubRepos", () => {
  it("is the repositories of the GitHub App lane, as owner/name, once each", () => {
    expect(githubRepos([
      fact({ provider: "github", lane: "app", repos: ["github.com/acme/web-app", "https://github.com/acme/api.git"] }),
      fact({ provider: "github", lane: "app", repos: ["github.com/acme/web-app"] }),
    ])).toEqual(["acme/web-app", "acme/api"]);
  });

  it("leaves out another provider, another lane and a component that is not a Git provider", () => {
    expect(githubRepos([
      fact({ provider: "azure_devops", lane: "entra", repos: ["dev.azure.com/org/p/r"] }),
      fact({ provider: "github", lane: "pat", repos: ["github.com/acme/pat"] }),
      fact({ kind: "custom", provider: "github", lane: "app", repos: ["github.com/acme/x"] }),
    ])).toEqual([]);
    expect(githubRepos(undefined)).toEqual([]);
  });
});

describe("kubectl words", () => {
  it("names the cluster from the row, dropping the kubectl prefix", () => {
    expect(clusterName("kubectl · staging")).toBe("staging");
    expect(clusterName("prod-eu")).toBe("prod-eu");
    expect(clusterName("kubectl · ")).toBe("kubectl · ");
  });

  it("always says no exec, and read only or read and write only when the row says", () => {
    expect(kubectlLimits({ kind: "kubectl", access: "read" })).toEqual([NR_TOOLS.KUBE_READ_ONLY, NR_TOOLS.KUBE_NO_EXEC]);
    expect(kubectlLimits({ kind: "kubectl", access: "write" })).toEqual([NR_TOOLS.KUBE_READ_WRITE, NR_TOOLS.KUBE_NO_EXEC]);
    expect(kubectlLimits({ kind: "kubectl" })).toEqual([NR_TOOLS.KUBE_NO_EXEC]);
  });
});
