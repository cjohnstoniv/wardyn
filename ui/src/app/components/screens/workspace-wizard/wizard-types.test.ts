/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  DEFAULT_TARGET,
  baseImageStateFromWorkspace,
  canDriveClaudeCode,
  credFlags,
  defaultBaseImageState,
  defaultTargetFor,
  deriveInitialRequirements,
  fmtElapsed,
  initialStepFor,
  isRemovable,
  isSourceShapeValid,
  isSshRemote,
  newSourceRow,
  parseRepoSource,
  removeSource,
  requirementKey,
  seedFloor,
  sourceRowsFromWorkspace,
  storableSecretName,
  splitRequirementKey,
  suggestedRegistryImage,
  summarizeRequirements,
  toBaseImageInput,
  toSourceInput,
  unmetRequiredSecrets,
  type SourceRow,
  type WorkspaceRequirementsMap,
} from "./wizard-types";
import type { Workspace, WorkspaceProfile } from "../../../lib/types";

// Minimal onboarded-workspace fixture for the edit-hydration tests below —
// only the fields sourceRowsFromWorkspace/baseImageStateFromWorkspace/
// initialStepFor actually read vary per test; the rest are the required
// scalars every Workspace carries.
function fixtureWorkspace(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  };
}

describe("seedFloor / removeSource — the >=1-source floor", () => {
  it("seedFloor() yields exactly one seeded ephemeral row", () => {
    const rows = seedFloor();
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ type: "ephemeral", seeded: true });
  });

  it("removeSource() re-seeds the floor when the last source is removed", () => {
    const rows = seedFloor();
    const next = removeSource(rows, rows[0].id);
    expect(next).toHaveLength(1);
    expect(next[0]).toMatchObject({ type: "ephemeral", seeded: true });
    // A fresh id, not the same row resurrected.
    expect(next[0].id).not.toBe(rows[0].id);
  });

  it("removeSource() just drops the row when another source remains", () => {
    const floor = seedFloor();
    const dir = newSourceRow("local_dir");
    const rows = [...floor, dir];
    const next = removeSource(rows, floor[0].id);
    expect(next).toEqual([dir]);
  });

  it("isRemovable() disables only the lone ephemeral floor row", () => {
    const floor = seedFloor();
    expect(isRemovable(floor[0], floor)).toBe(false);
    const dir = newSourceRow("local_dir");
    const rows = [...floor, dir];
    expect(isRemovable(floor[0], rows)).toBe(true); // no longer alone
    expect(isRemovable(dir, rows)).toBe(true);
  });
});

describe("defaultTargetFor — auto-derived distinct sub-paths", () => {
  it("uses the bare default target for a single source", () => {
    const row = newSourceRow("local_dir");
    expect(defaultTargetFor(row, [row])).toBe(DEFAULT_TARGET);
  });

  it("derives a distinct sub-path per source once several exist", () => {
    const a = { ...newSourceRow("local_dir"), path: "/home/me/payments" };
    const b = { ...newSourceRow("repo"), source: "acme/payments-service" };
    expect(defaultTargetFor(a, [a, b])).toBe(`${DEFAULT_TARGET}/payments`);
    expect(defaultTargetFor(b, [a, b])).toBe(`${DEFAULT_TARGET}/payments-service`);
  });

  it("an operator-typed target always wins over the auto-derived one", () => {
    const a = { ...newSourceRow("local_dir"), path: "/x", target: "/custom" };
    const b = newSourceRow("repo");
    expect(defaultTargetFor(a, [a, b])).toBe("/custom");
  });
});

describe("parseRepoSource / isSshRemote", () => {
  it("recognizes an SSH remote", () => {
    expect(parseRepoSource("git@github.com:acme/payments.git")).toEqual({ host: "github.com", ssh: true });
  });
  it("recognizes an https URL", () => {
    expect(parseRepoSource("https://gitlab.com/acme/ingest")).toEqual({ host: "gitlab.com", ssh: false });
  });
  it("assumes github.com for a bare org/repo slug", () => {
    expect(parseRepoSource("acme/payments-service")).toEqual({ host: "github.com", ssh: false });
  });
  it("returns null for empty or unrecognized input", () => {
    expect(parseRepoSource("")).toBeNull();
    expect(parseRepoSource("not a url")).toBeNull();
  });
  it("isSshRemote is true only for a repo row whose source is an SSH remote", () => {
    const ssh = { ...newSourceRow("repo"), source: "git@ghes.corp.internal:acme/payments.git" };
    const https = { ...newSourceRow("repo"), source: "https://github.com/acme/payments" };
    const dir = newSourceRow("local_dir");
    expect(isSshRemote(ssh)).toBe(true);
    expect(isSshRemote(https)).toBe(false);
    expect(isSshRemote(dir)).toBe(false);
  });
});

describe("isSourceShapeValid / toSourceInput", () => {
  it("requires an absolute path for local_dir", () => {
    expect(isSourceShapeValid({ ...newSourceRow("local_dir"), path: "relative" })).toBe(false);
    expect(isSourceShapeValid({ ...newSourceRow("local_dir"), path: "/abs" })).toBe(true);
  });
  it("requires a parseable source for repo", () => {
    expect(isSourceShapeValid({ ...newSourceRow("repo"), source: "" })).toBe(false);
    expect(isSourceShapeValid({ ...newSourceRow("repo"), source: "acme/payments" })).toBe(true);
  });
  it("ephemeral is always shape-valid", () => {
    expect(isSourceShapeValid(newSourceRow("ephemeral"))).toBe(true);
  });
  it("toSourceInput strips UI-only fields and fills the derived target", () => {
    const row: SourceRow = { ...newSourceRow("repo"), source: "acme/payments", ref: "main" };
    expect(toSourceInput(row, [row])).toEqual({
      type: "repo",
      source: "acme/payments",
      ref: "main",
      target: DEFAULT_TARGET,
    });
  });
  // H1: writable must round-trip through toSourceInput -> sourceRowsFromWorkspace
  // -> toSourceInput without silently downgrading to read-only.
  it("toSourceInput emits writable:true for a checked local_dir row (H1)", () => {
    const row: SourceRow = { ...newSourceRow("local_dir"), path: "/srv/payments", writable: true };
    expect(toSourceInput(row, [row])).toMatchObject({ writable: true });
  });
  it("toSourceInput omits writable (undefined, not false) when unchecked", () => {
    const row: SourceRow = { ...newSourceRow("local_dir"), path: "/srv/payments" };
    expect(toSourceInput(row, [row]).writable).toBeUndefined();
  });
});

describe("credFlags — warn-never-block credential-shaped-line detection", () => {
  it("flags an AWS-shaped key, a GitHub token, and a private key block, each with its line number", () => {
    const text = ["FROM ubuntu:24.04", "ENV AWS_KEY=AKIAABCDEFGHIJKLMNOP", "RUN echo hi"].join("\n");
    expect(credFlags(text)).toEqual([{ line: 2, kind: "AWS access key–shaped" }]);
  });
  it("flags a GitHub token-shaped value", () => {
    const flags = credFlags("ENV GITHUB_TOKEN=ghp_Zx9q4tW8kLmNo2rPvJd6yBhTcE1aSfUgIjKl");
    expect(flags).toEqual([{ line: 1, kind: "GitHub token–shaped" }]);
  });
  it("never flags an ordinary build step", () => {
    expect(credFlags("RUN apt-get install -y protobuf-compiler\nENV GOFLAGS=-mod=vendor")).toEqual([]);
  });
  it("returns [] for empty input", () => {
    expect(credFlags("")).toEqual([]);
  });
});

describe("canDriveClaudeCode — reads the same IMPOSSIBLE map Integrations renders reasons from", () => {
  it("is false for a type that flatly can't drive Claude Code", () => {
    expect(canDriveClaudeCode("openai_api_key")).toBe(false);
    expect(canDriveClaudeCode("azure_openai")).toBe(false);
  });
  it("is true for a type with no such impossibility", () => {
    expect(canDriveClaudeCode("anthropic_api_key")).toBe(true);
    expect(canDriveClaudeCode("bedrock")).toBe(true);
  });
  it("is false when no type is resolved at all", () => {
    expect(canDriveClaudeCode(undefined)).toBe(false);
  });
});

describe("requirementKey / splitRequirementKey — the server's fixed key grammar", () => {
  it("round-trips a simple key", () => {
    expect(splitRequirementKey(requirementKey("secret", "acme-key"))).toEqual({ type: "secret", rest: "acme-key" });
  });
  it("splits on the FIRST colon only — a write path may itself contain one", () => {
    expect(splitRequirementKey("write:/home/user:repo")).toEqual({ type: "write", rest: "/home/user:repo" });
  });
  it("rejects a key with no colon or an empty suffix", () => {
    expect(splitRequirementKey("nocolon")).toBeNull();
    expect(splitRequirementKey("secret:")).toBeNull();
  });
});

describe("storableSecretName — env-var name → store-grammar name", () => {
  it("maps detected env-var names onto the server's lowercase grammar", () => {
    expect(storableSecretName("AWS_DEFAULT_REGION")).toBe("aws-default-region");
    expect(storableSecretName("already-storable.key")).toBe("already-storable.key");
    expect(storableSecretName("My Token")).toBe("my-token");
  });
  it("returns '' when nothing storable remains", () => {
    expect(storableSecretName("___")).toBe("");
    expect(storableSecretName("")).toBe("");
  });
});

describe("deriveInitialRequirements — seeding the contract from a scan profile", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [
      { name: "DATABASE_URL", kind: "postgres" },
      { name: "REDIS_URL", kind: "redis", optional: true },
    ],
    egress_domains: ["registry.npmjs.org"],
  };

  it("defaults a secret to required unless the scan flagged it optional — keyed by the STORABLE name", () => {
    const reqs = deriveInitialRequirements(profile, []);
    // The scan reports env-var names; the contract row names a store entry the
    // server's lowercase grammar can actually hold (the live 400 this pins).
    expect(reqs["secret:database-url"]).toEqual({ level: "required", provenance: "scan_seeded" });
    expect(reqs["secret:redis-url"]).toEqual({ level: "optional", provenance: "scan_seeded" });
    expect(reqs["secret:DATABASE_URL"]).toBeUndefined();
  });

  it("defaults an auto-allowed egress host to required", () => {
    const reqs = deriveInitialRequirements(profile, []);
    expect(reqs["egress:registry.npmjs.org"]).toEqual({ level: "required", provenance: "scan_seeded" });
  });

  it("defaults a local_dir's write access to optional", () => {
    const reqs = deriveInitialRequirements(null, ["/home/me/payments"]);
    expect(reqs["write:/home/me/payments"]).toEqual({ level: "optional", provenance: "scan_seeded" });
  });

  it("never overwrites an existing (operator-set) entry", () => {
    const existing: WorkspaceRequirementsMap = {
      "secret:database-url": { level: "optional", provenance: "operator_set" },
    };
    const reqs = deriveInitialRequirements(profile, [], existing);
    expect(reqs["secret:database-url"]).toEqual({ level: "optional", provenance: "operator_set" });
  });

  it("handles an empty/missing profile without throwing", () => {
    expect(deriveInitialRequirements(undefined, [])).toEqual({});
    expect(deriveInitialRequirements(null, [])).toEqual({});
  });
});

describe("summarizeRequirements / unmetRequiredSecrets", () => {
  const reqs: WorkspaceRequirementsMap = {
    "secret:DATABASE_URL": { level: "required", provenance: "scan_seeded" },
    "secret:REDIS_URL": { level: "optional", provenance: "scan_seeded" },
    "egress:registry.npmjs.org": { level: "required", provenance: "scan_seeded" },
    "write:/home/me/payments": { level: "optional", provenance: "scan_seeded" },
  };

  it("splits required vs optional into 'always' / 'on request' phrases", () => {
    expect(summarizeRequirements(reqs)).toEqual({
      always: ["1 secret", "1 host"],
      onRequest: ["1 secret", "write access"],
    });
  });

  it("reports required secrets missing from the store, ignoring optional ones", () => {
    expect(unmetRequiredSecrets(reqs, [])).toEqual(["DATABASE_URL"]);
    expect(unmetRequiredSecrets(reqs, ["DATABASE_URL"])).toEqual([]);
  });
});

describe("toBaseImageInput / suggestedRegistryImage — the base-image wire shape", () => {
  it("recommended needs no image or steps", () => {
    expect(toBaseImageInput(defaultBaseImageState())).toEqual({ kind: "recommended" });
  });

  it("registry uses the detected-stack heuristic, falling back to a generic base", () => {
    expect(suggestedRegistryImage(["Go 1.22"])).toBe("mcr.microsoft.com/devcontainers/go:1");
    expect(suggestedRegistryImage([])).toBe("mcr.microsoft.com/devcontainers/base:ubuntu");
    expect(toBaseImageInput({ ...defaultBaseImageState(), choice: "registry" }, ["Go 1.22"])).toEqual({
      kind: "registry",
      image: "mcr.microsoft.com/devcontainers/go:1",
    });
  });

  it("byo carries the operator's image ref verbatim", () => {
    expect(toBaseImageInput({ ...defaultBaseImageState(), choice: "byo", byoRef: "ghcr.io/acme/dev:latest" })).toEqual({
      kind: "byo",
      image: "ghcr.io/acme/dev:latest",
    });
  });

  it("custom carries the base image plus non-empty, trimmed build steps", () => {
    const state = {
      ...defaultBaseImageState(),
      choice: "custom" as const,
      customBase: "ubuntu:24.04",
      buildSteps: "RUN echo hi\n\n  ENV FOO=bar  \n",
    };
    expect(toBaseImageInput(state)).toEqual({
      kind: "custom",
      image: "ubuntu:24.04",
      steps: ["RUN echo hi", "ENV FOO=bar"],
    });
  });
});

describe("sourceRowsFromWorkspace — edit hydration, the inverse of toSourceInput", () => {
  it("maps each wire source to a SourceRow with a fresh id", () => {
    const ws = fixtureWorkspace({
      sources: [
        { type: "local_dir", path: "/srv/a", target: "/home/agent/work" },
        { type: "repo", source: "acme/x", ref: "main", target: "/home/agent/work/x" },
      ],
    });
    const rows = sourceRowsFromWorkspace(ws);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({ type: "local_dir", path: "/srv/a", target: "/home/agent/work" });
    expect(rows[1]).toMatchObject({ type: "repo", source: "acme/x", ref: "main", target: "/home/agent/work/x" });
    expect(rows[0].id).not.toBe(rows[1].id);
  });

  it("falls back to the seeded floor when the workspace has no sources", () => {
    const rows = sourceRowsFromWorkspace(fixtureWorkspace({ sources: undefined }));
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ type: "ephemeral", seeded: true });
  });

  // H1: a stored local_dir source's writable flag used to have nowhere to
  // land on SourceRow — hydration silently dropped it, so a round-trip save
  // with zero operator edits downgraded a writable mount to read-only (and,
  // since the server compares the whole Sources row, tripped sourcesChanged
  // and wiped the reviewed contract as a side effect).
  it("reads writable off a stored local_dir source (H1)", () => {
    const rows = sourceRowsFromWorkspace(
      fixtureWorkspace({ sources: [{ type: "local_dir", path: "/srv/a", target: "/w", writable: true }] }),
    );
    expect(rows[0].writable).toBe(true);
  });

  it("defaults writable to false when the stored source omits it", () => {
    const rows = sourceRowsFromWorkspace(
      fixtureWorkspace({ sources: [{ type: "local_dir", path: "/srv/a", target: "/w" }] }),
    );
    expect(rows[0].writable).toBe(false);
  });
});

describe("baseImageStateFromWorkspace — edit hydration, the inverse of toBaseImageInput", () => {
  it("no base_image (or 'recommended') hydrates the default state", () => {
    expect(baseImageStateFromWorkspace(fixtureWorkspace())).toEqual(defaultBaseImageState());
    expect(baseImageStateFromWorkspace(fixtureWorkspace({ base_image: { kind: "recommended" } }))).toEqual(
      defaultBaseImageState(),
    );
  });

  it("registry hydrates the registry choice (the exact image is re-derived on Continue, not carried)", () => {
    const state = baseImageStateFromWorkspace(
      fixtureWorkspace({ base_image: { kind: "registry", image: "mcr.microsoft.com/devcontainers/go:1" } }),
    );
    expect(state.choice).toBe("registry");
  });

  it("byo carries the image ref forward", () => {
    const state = baseImageStateFromWorkspace(
      fixtureWorkspace({ base_image: { kind: "byo", image: "ghcr.io/acme/dev:latest" } }),
    );
    expect(state).toMatchObject({ choice: "byo", byoRef: "ghcr.io/acme/dev:latest" });
  });

  it("custom carries the base image and re-joins the steps as lines", () => {
    const state = baseImageStateFromWorkspace(
      fixtureWorkspace({
        base_image: { kind: "custom", image: "ubuntu:24.04", steps: ["RUN echo hi", "ENV FOO=bar"] },
      }),
    );
    expect(state).toMatchObject({
      choice: "custom",
      customBase: "ubuntu:24.04",
      buildSteps: "RUN echo hi\nENV FOO=bar",
    });
  });

  it("round-trips through toBaseImageInput for byo/custom (registry is re-derived by design)", () => {
    const byo = fixtureWorkspace({ base_image: { kind: "byo", image: "ghcr.io/acme/dev:latest" } });
    expect(toBaseImageInput(baseImageStateFromWorkspace(byo))).toEqual(byo.base_image);

    const custom = fixtureWorkspace({
      base_image: { kind: "custom", image: "ubuntu:24.04", steps: ["RUN echo hi"] },
    });
    expect(toBaseImageInput(baseImageStateFromWorkspace(custom))).toEqual(custom.base_image);
  });
});

describe("initialStepFor — the edit wizard's landing step", () => {
  it("not yet scanned -> Sources", () => {
    expect(initialStepFor(fixtureWorkspace({ status: "pending_scan" }))).toBe("sources");
  });

  it("scanned (or scanning/error) but no image built yet -> Base image", () => {
    expect(initialStepFor(fixtureWorkspace({ status: "scanned", image_ref: "" }))).toBe("image");
    expect(initialStepFor(fixtureWorkspace({ status: "scanning", image_ref: "" }))).toBe("image");
    expect(initialStepFor(fixtureWorkspace({ status: "error", image_ref: "" }))).toBe("image");
  });

  it("scanned AND an image already built -> Requirements", () => {
    expect(initialStepFor(fixtureWorkspace({ status: "scanned", image_ref: "wardyn-workspace/ws-1:abc" }))).toBe(
      "reqs",
    );
  });
});

describe("fmtElapsed", () => {
  it("formats mm:ss and never goes negative", () => {
    const now = 1_000_000;
    expect(fmtElapsed(now - 65_000, now)).toBe("01:05");
    expect(fmtElapsed(now + 5_000, now)).toBe("00:00");
  });
});
