/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { EPISODES, MEMBER_SECTION_IDS, episodeUrl, releasePageUrl, episodesFor } from "./demo-videos";
import { STEP_LABEL } from "../components/screens/setup/steps";

describe("demo-videos", () => {
  it("builds the exact download URL for a shipped episode", () => {
    const e = EPISODES.find((x) => x.id === "01");
    expect(e).toBeDefined();
    expect(episodeUrl(e!)).toBe(
      "https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-01-why-govern-agents.mp4",
    );
  });

  it("returns null for a reserved episode with no tag (negative control)", () => {
    const e = EPISODES.find((x) => x.id === "02b");
    expect(e).toBeDefined();
    expect(e!.tag).toBeNull();
    expect(episodeUrl(e!)).toBeNull();
  });

  it("builds the release page URL", () => {
    expect(releasePageUrl("v0.6.0")).toBe("https://github.com/cjohnstoniv/wardyn/releases/tag/v0.6.0");
  });

  // Shape C partition, frozen at the approved mock round (2026-08-31). The
  // three audience flips ride inside it: 05 admin->everyone, 12
  // everyone->admin, 13 everyone->member. A new episode must take a stance
  // here on purpose, not inherit one silently.
  it("pins every episode's (path, audience) to the approved Shape C partition", () => {
    const partition = Object.fromEntries(EPISODES.map((e) => [e.id, `${e.path}/${e.audience}`]));
    expect(partition).toEqual({
      // 00 is the front door: core, and for everyone — it assumes nothing but
      // "I want to sandbox an AI coding agent".
      "00": "core/everyone",
      "01": "core/everyone",
      "02": "single/admin",
      "02b": "single/admin",
      "02c": "multi/admin",
      "03a": "core/everyone",
      "03b": "core/everyone",
      "03c": "core/everyone",
      "03d": "core/everyone",
      "04": "single/admin",
      "04b": "multi/member",
      "04c": "multi/admin",
      // 04d is the MEMBER's episode on the multi path: the admin registers and
      // allocates in its opening, but the drive is the member's and the
      // persistence proof is filmed from their seat.
      "04d": "multi/member",
      "05": "core/everyone",
      "06": "any/member",
      "07": "any/member",
      "08": "any/member",
      "09": "core/everyone",
      "10": "any/member",
      "11": "any/everyone",
      "12": "multi/admin",
      "12b": "multi/admin",
      "13": "multi/member",
    });
  });

  it("has unique episode ids", () => {
    const ids = EPISODES.map((e) => e.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("ships exactly 13 episodes with a non-null tag", () => {
    expect(EPISODES.filter((e) => e.tag !== null).length).toBe(13);
  });

  // Coupling check: every step id an episode declares has to resolve
  // somewhere. Admin ids are checked against the REAL Getting Started step
  // contract (steps.ts's STEP_LABEL). Member ids are checked against this
  // file's own MEMBER_SECTION_IDS: the member sections those ids name are a
  // separate namespace, not steps in the admin funnel (see the comment on that
  // const in demo-videos.ts) — which is why episode 04 declares "workspaces"
  // and 04b declares "workspace".
  const knownAdminStepIds = new Set<string>(Object.keys(STEP_LABEL));
  const knownMemberStepIds = new Set<string>(MEMBER_SECTION_IDS);

  it("every episode's steps value is a real admin or member step id", () => {
    for (const e of EPISODES) {
      for (const stepId of e.steps) {
        const known = knownAdminStepIds.has(stepId) || knownMemberStepIds.has(stepId);
        expect(known, `${e.id}: unknown step id "${stepId}"`).toBe(true);
      }
    }
  });

  it("episodesFor returns the episodes declaring a given step", () => {
    expect(
      episodesFor("environment")
        .map((e) => e.id)
        .sort(),
    ).toEqual(["02", "02b", "02c"]);
  });

  it("a step id outside both contracts is NOT known (negative control)", () => {
    const bogus = "not-a-real-step";
    expect(knownAdminStepIds.has(bogus) || knownMemberStepIds.has(bogus)).toBe(false);
  });

  it("episodesFor returns nothing for a step no episode declares", () => {
    expect(episodesFor("not-a-real-step")).toEqual([]);
  });
});
