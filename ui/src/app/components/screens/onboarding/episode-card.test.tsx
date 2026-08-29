/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EpisodeRow, StepEpisodes, EpisodeList } from "./episode-card";
import type { Episode } from "../../../lib/demo-videos";

const shipped: Episode = {
  id: "01",
  title: "Why govern agents",
  audience: "everyone",
  tag: "v0.6.0",
  file: "wardyn-01-why-govern-agents.mp4",
  minutes: "5:51",
  steps: [],
};

const reserved: Episode = {
  id: "02b",
  title: "Managed desktop",
  audience: "admin",
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

describe("EpisodeList", () => {
  it("groups episodes by audience under the three headings", () => {
    render(<EpisodeList />);
    expect(screen.getByText("All episodes")).toBeInTheDocument();
    expect(screen.getByText("13 recorded · about 72 minutes · streamed from GitHub on click")).toBeInTheDocument();
    expect(screen.getByText("For admins")).toBeInTheDocument();
    expect(screen.getByText("For members")).toBeInTheDocument();
    expect(screen.getByText("For everyone")).toBeInTheDocument();
  });
});
