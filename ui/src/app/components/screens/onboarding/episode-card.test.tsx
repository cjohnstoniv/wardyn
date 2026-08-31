/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EpisodeRow, StepEpisodes, EpisodeList, catalogSummary } from "./episode-card";
import { EPISODES, type Episode } from "../../../lib/demo-videos";

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

  it("a reserved (unrecorded) episode shows a neutral chip and no button or link", () => {
    render(<EpisodeRow episode={reserved} />);
    expect(screen.getByText("Not recorded yet")).toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("a load error swaps the player for the error copy and a release-page link", async () => {
    const user = userEvent.setup();
    const { container } = render(<EpisodeRow episode={shipped} />);
    await user.click(screen.getByRole("button", { name: "Watch" }));
    fireEvent.error(container.querySelector("video")!);

    expect(container.querySelector("video")).toBeNull();
    expect(screen.getByText(/Couldn't load this episode from GitHub/)).toBeInTheDocument();
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
      { id: "c", title: "C", audience: "everyone", path: "core", tag: null, file: "c.mp4", steps: [] },
    ];
    // Negative control: the reserved (tag: null) episode is excluded from
    // both the recorded count and the minutes sum, even though it has no
    // `minutes` to contribute anyway.
    expect(catalogSummary(fixture)).toEqual({ recorded: 2, minutes: 4 });
  });
});
