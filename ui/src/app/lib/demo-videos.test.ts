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

  it("has unique episode ids", () => {
    const ids = EPISODES.map((e) => e.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("ships exactly 13 episodes with a non-null tag", () => {
    expect(EPISODES.filter((e) => e.tag !== null).length).toBe(13);
  });

  // Coupling check: every step id an episode declares has to resolve
  // somewhere. Admin ids are checked against the REAL Getting Started step
  // contract (steps.ts's STEP_LABEL) — "people" is unioned in because it is a
  // Phase 5 addition not in STEP_LABEL yet. Member ids are checked against
  // this file's own MEMBER_SECTION_IDS, because the member sections those ids
  // name don't exist as code until Phase 5 either (see the comment on that
  // const in demo-videos.ts).
  const knownAdminStepIds = new Set<string>([...Object.keys(STEP_LABEL), "people"]);
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

  it("episodesFor returns nothing for a step no episode declares", () => {
    expect(episodesFor("not-a-real-step")).toEqual([]);
  });
});
