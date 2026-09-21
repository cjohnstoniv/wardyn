/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EpisodeRow, StepEpisodes, EpisodeList, catalogSummary } from "./episode-card";
import { EPISODES, type Episode } from "../../../lib/demo-videos";
import * as useDemoVideoBaseUrlModule from "../../../lib/hooks/use-demo-video-base-url";

const shipped: Episode = {
  id: "01",
  title: "Why govern agents",
  audience: "everyone",
  path: "core",
  tag: "v0.6.0",
  file: "wardyn-01-why-govern-agents.mp4",
  minutes: "5:51",
  steps: [],
};

const reserved: Episode = {
  id: "02b",
  title: "Managed desktop",
  audience: "admin",
  path: "single",
  tag: null,
  file: "wardyn-02b-managed-desktop.mp4",
  steps: [],
};

// U-04: an unrecorded episode that already carries a projected `minutes`
// estimate (00, 03c, 04, 06, 07, 08, 09, 10 in the real catalog all do) —
// unlike `reserved` above, which has no `minutes` at all and so never
// exercised the bug.
const reservedWithMinutes: Episode = {
  id: "00",
  title: "Meet Wardyn",
  audience: "everyone",
  path: "core",
  tag: null,
  file: "wardyn-00-meet-wardyn.mp4",
  minutes: "5:53",
  steps: [],
};

describe("EpisodeRow", () => {
  it("mounts no <video> until Watch is pressed, then exactly one with the pinned src", async () => {
    const user = userEvent.setup();
    const { container } = render(<EpisodeRow episode={shipped} />);
    expect(container.querySelector("video")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Watch" }));

    const video = container.querySelector("video");
    expect(video).not.toBeNull();
    expect(container.querySelectorAll("video")).toHaveLength(1);
    expect(video).toHaveAttribute(
      "src",
      "https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-01-why-govern-agents.mp4",
    );
    expect(video).not.toHaveAttribute("autoplay");
    expect(video).not.toHaveAttribute("poster");
  });

  it("an operator-configured mirror (/healthz's demo_video_base_url) re-points the video src", async () => {
    const spy = vi
      .spyOn(useDemoVideoBaseUrlModule, "useDemoVideoBaseUrl")
      .mockReturnValue("https://videos.airgapped.example/wardyn-demos");
    try {
      const user = userEvent.setup();
      const { container } = render(<EpisodeRow episode={shipped} />);
      await user.click(screen.getByRole("button", { name: "Watch" }));
      const video = container.querySelector("video");
      expect(video).toHaveAttribute(
        "src",
        "https://videos.airgapped.example/wardyn-demos/v0.6.0/wardyn-01-why-govern-agents.mp4",
      );
    } finally {
      spy.mockRestore();
    }
  });

  it("a reserved (unrecorded) episode shows a neutral chip and no button or link", () => {
    render(<EpisodeRow episode={reserved} />);
    expect(screen.getByText("Not recorded yet")).toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("U-04: an unrecorded episode never shows a projected minutes estimate, even when the catalog carries one", () => {
    render(<EpisodeRow episode={reservedWithMinutes} />);
    expect(screen.getByText("Not recorded yet")).toBeInTheDocument();
    expect(screen.queryByText("5:53")).not.toBeInTheDocument();
  });

  it("a load error swaps the player for the error copy and a release-page link", async () => {
    const user = userEvent.setup();
    const { container } = render(<EpisodeRow episode={shipped} />);
    await user.click(screen.getByRole("button", { name: "Watch" }));
    fireEvent.error(container.querySelector("video")!);

    expect(container.querySelector("video")).toBeNull();
    expect(screen.getByText(/This deployment's media policy blocked this episode/)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Open the release page" });
    expect(link).toHaveAttribute("href", "https://github.com/cjohnstoniv/wardyn/releases/tag/v0.6.0");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link.getAttribute("rel")).toContain("noopener");
  });
});

describe("StepEpisodes", () => {
  it("renders nothing for a step with no episodes", () => {
    const { container } = render(<StepEpisodes stepId="no-such-step" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the Watch eyebrow and rows for a step that has episodes", () => {
    const { container } = render(<StepEpisodes stepId="first-run" />);
    // "Watch" also names each row's own button — scope to the eyebrow label.
    expect(container.querySelector(".label-eyebrow")).toHaveTextContent("Watch");
    expect(screen.getByText("Your first run")).toBeInTheDocument();
  });
});

describe("EpisodeList — Shape C path grouping (approved mock round 2026-08-31)", () => {
  it("single mode: core leads, the single-user path is the deployment group, multi collapses", () => {
    render(<EpisodeList mode="single" />);
    const { recorded, minutes } = catalogSummary(EPISODES);
    expect(screen.getByText("All episodes")).toBeInTheDocument();
    expect(
      screen.getByText(`${recorded} recorded · about ${minutes} minutes · streamed from GitHub on click`),
    ).toBeInTheDocument();
    expect(screen.getByText("Start here")).toBeInTheDocument();
    expect(screen.getByText("Your deployment — single-user")).toBeInTheDocument();
    expect(screen.getByText("Running work — any deployment")).toBeInTheDocument();
    expect(screen.queryByText("Your deployment — multi-user")).not.toBeInTheDocument();
    // The other path stays reachable, collapsed, with an honest count.
    const multiCount = EPISODES.filter((e) => e.path === "multi").length;
    expect(screen.getByText(`The multi-user path — ${multiCount} episodes`)).toBeInTheDocument();
    // Its rows are rendered inside the disclosure (native <details> keeps them
    // in the DOM), and no member chips show in single mode.
    expect(screen.getByText("One command to a cluster")).toBeInTheDocument();
    expect(screen.queryByText("For your members")).not.toBeInTheDocument();
  });

  it("multi mode: the deployment group swaps, member-audience rows are chipped, single collapses", () => {
    render(<EpisodeList mode="multi" />);
    expect(screen.getByText("Your deployment — multi-user")).toBeInTheDocument();
    expect(screen.queryByText("Your deployment — single-user")).not.toBeInTheDocument();
    const singleCount = EPISODES.filter((e) => e.path === "single").length;
    expect(screen.getByText(`The single-user path — ${singleCount} episodes`)).toBeInTheDocument();
    // 04b and 13 are the multi path's member-audience episodes — an admin
    // reading their own deployment group sees whose episodes those are.
    const memberChips = screen.getAllByText("For your members");
    expect(memberChips).toHaveLength(
      EPISODES.filter((e) => e.path === "multi" && e.audience === "member").length,
    );
  });
});

describe("catalogSummary", () => {
  it("counts only recorded (non-null tag) episodes and sums their minutes, rounded", () => {
    const fixture: Episode[] = [
      { id: "a", title: "A", audience: "everyone", path: "core", tag: "v1", file: "a.mp4", minutes: "1:30", steps: [] },
      { id: "b", title: "B", audience: "everyone", path: "core", tag: "v1", file: "b.mp4", minutes: "2:00", steps: [] },
      { id: "c", title: "C", audience: "everyone", path: "core", tag: null, file: "c.mp4", minutes: "5:00", steps: [] },
    ];
    // Negative control: the reserved (tag: null) episode is excluded from
    // both the recorded count AND the minutes sum — X4-F1's real shape, a
    // take that already exists (a `minutes` value) for an episode not yet
    // released: c's own 5:00 would push the total to 9 if it leaked in.
    expect(catalogSummary(fixture)).toEqual({ recorded: 2, minutes: 4 });
  });

  // X4-F8: re-derived from README.md's own "41 minutes across the six
  // recorded episodes" — a hardcoded pin, not catalogSummary(EPISODES)
  // checking itself, so a regression in the function (or an accidental
  // release-tag flip in EPISODES) actually fails this.
  it("matches README.md's 6 recorded / 41 minutes for the real catalog", () => {
    expect(catalogSummary(EPISODES)).toEqual({ recorded: 6, minutes: 41 });
  });
});
