/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import {
  attachWorkspace, chooseNothing, commitSize, excludeRepo, includeRepo, mainDriveMiB, mibToGibText, parseGiB, repoIncluded,
  setMainDrive, setRepoRef, setStartsHere, startsAt,
} from "./files-lens";

const ALL = ["acme/api", "acme/web"];
const BASE: RunIntent = { shape: "environment", experience: "interactive", image: "registry.example/app:1" };

describe("Starts here never attaches (U6)", () => {
  it("choosing a start for an item that is not attached leaves the attachments exactly as they were", () => {
    const before: RunIntent = { ...BASE, workspaces: [{ workspace_id: "w1", repos: [{ repo: "acme/api" }] }] };
    const after = setStartsHere(before, "w2", "acme/web");
    expect(after.workspaces).toEqual(before.workspaces);
    expect(after.drive).toBeUndefined();
    expect(after.start_folder).toEqual({ kind: "attachment", attachment: "w2", repo: "acme/web" });
  });

  it("names the repository only when one is given", () => {
    expect(setStartsHere(BASE, "w1").start_folder).toEqual({ kind: "attachment", attachment: "w1" });
  });

  it("reads a start folder with no repo as the workspace's only repository", () => {
    const intent = setStartsHere(BASE, "w1");
    expect(startsAt(intent, "w1", "acme/api", 1)).toBe(true);
    expect(startsAt(intent, "w1", "acme/api", 2)).toBe(false);
    expect(startsAt(setStartsHere(BASE, "w1", "acme/web"), "w1", "acme/api", 2)).toBe(false);
  });
});

describe("repositories", () => {
  it("includes a repository by attaching its workspace with exactly that repository, and drops the 'nothing' choice", () => {
    const next = includeRepo(chooseNothing(BASE), "w1", "acme/api");
    expect(next.workspaces).toEqual([{ workspace_id: "w1", repos: [{ repo: "acme/api" }] }]);
    expect(next.no_repositories_or_drives).toBeUndefined();
  });

  it("adds a second repository to the same entry, keeps its other keys, and is idempotent", () => {
    const one: RunIntent = { ...BASE, workspaces: [{ workspace_id: "w1", read_only: true, repos: [{ repo: "acme/api", ref: "dev" }] }] };
    const two = includeRepo(one, "w1", "acme/web");
    expect(two.workspaces).toEqual([{ workspace_id: "w1", read_only: true, repos: [{ repo: "acme/api", ref: "dev" }, { repo: "acme/web" }] }]);
    expect(includeRepo(two, "w1", "acme/web")).toBe(two);
  });

  it("an entry with no repos list already clones every repository", () => {
    const all: RunIntent = { ...BASE, workspaces: [{ workspace_id: "w1" }] };
    expect(includeRepo(all, "w1", "acme/web")).toBe(all);
    expect(repoIncluded(all, "w1", "acme/web")).toBe(true);
    expect(repoIncluded(all, "w2", "acme/web")).toBe(false);
  });

  it("leaving one out of an all-repositories entry writes the rest as an explicit list", () => {
    const all: RunIntent = { ...BASE, workspaces: [{ workspace_id: "w1" }] };
    expect(excludeRepo(all, "w1", "acme/web", ALL).workspaces).toEqual([{ workspace_id: "w1", repos: [{ repo: "acme/api" }] }]);
  });

  it("leaving out the last repository detaches the workspace (an empty list would mean all) and clears its start", () => {
    const one = setStartsHere(includeRepo(BASE, "w1", "acme/api"), "w1", "acme/api");
    const next = excludeRepo(one, "w1", "acme/api", ALL);
    expect(next.workspaces).toBeUndefined();
    expect(next.start_folder).toBeUndefined();
  });

  it("clears the start only when it was the repository left out", () => {
    const two = includeRepo(includeRepo(BASE, "w1", "acme/api"), "w1", "acme/web");
    expect(excludeRepo(setStartsHere(two, "w1", "acme/api"), "w1", "acme/web", ALL).start_folder).toEqual({ kind: "attachment", attachment: "w1", repo: "acme/api" });
    expect(excludeRepo(setStartsHere(two, "w1", "acme/web"), "w1", "acme/web", ALL).start_folder).toBeUndefined();
  });

  it("sets and clears one repository's ref without touching the others", () => {
    const two = includeRepo(includeRepo(BASE, "w1", "acme/api"), "w1", "acme/web");
    const pinned = setRepoRef(two, "w1", "acme/api", "release/1.2", ALL);
    expect(pinned.workspaces?.[0].repos).toEqual([{ repo: "acme/api", ref: "release/1.2" }, { repo: "acme/web" }]);
    expect(setRepoRef(pinned, "w1", "acme/api", undefined, ALL).workspaces?.[0].repos).toEqual([{ repo: "acme/api" }, { repo: "acme/web" }]);
    expect(setRepoRef(BASE, "w1", "acme/api", "dev", ALL)).toBe(BASE);
  });

  it("attaching a whole workspace still works beside a picked repository", () => {
    expect(attachWorkspace(includeRepo(BASE, "w1", "acme/api"), "w2").workspaces).toHaveLength(2);
  });
});

describe("main drive size (A8)", () => {
  it("writes and clears resources.disk_mib and keeps the other resources", () => {
    const sized = setMainDrive({ ...BASE, resources: { cpu_millis: 2000 } }, 4096);
    expect(sized.resources).toEqual({ cpu_millis: 2000, disk_mib: 4096 });
    expect(mainDriveMiB(sized)).toBe(4096);
    expect(setMainDrive(sized, undefined).resources).toEqual({ cpu_millis: 2000 });
    expect(setMainDrive(setMainDrive(BASE, 512), undefined).resources).toBeUndefined();
  });

  it("takes one decimal of a GiB and rounds to whole MiB", () => {
    expect(parseGiB("0.5")).toBe(512);
    expect(parseGiB("2")).toBe(2048);
    expect(parseGiB("0.3")).toBe(307);
    for (const bad of ["", "  ", "0", "-1", "abc"]) expect(parseGiB(bad)).toBeUndefined();
  });

  it("shows a sub-GiB or fractional cap with one decimal, rounding a cap down", () => {
    expect(mibToGibText(512)).toBe("0.5");
    expect(mibToGibText(1536)).toBe("1.5");
    expect(mibToGibText(8192)).toBe("8");
    expect(mibToGibText(1000, "floor")).toBe("0.9");
    expect(mibToGibText(1000)).toBe("1");
  });

  it("a size over the cap becomes the cap and says it was clamped; one at or under is kept", () => {
    expect(commitSize("16", 8192)).toEqual({ mib: 8192, clamped: true });
    expect(commitSize("8", 8192)).toEqual({ mib: 8192, clamped: false });
    expect(commitSize("4.5", 8192)).toEqual({ mib: 4608, clamped: false });
    expect(commitSize("100", undefined)).toEqual({ mib: 102400, clamped: false });
    expect(commitSize("", 8192)).toEqual({ clamped: false });
  });
});
