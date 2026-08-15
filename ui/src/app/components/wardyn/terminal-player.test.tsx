/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import type { Recording } from "../../lib/types";

// asciinema-player drives real DOM/canvas terminal emulation — stub it so the
// component mounts without a real player instance.
vi.mock("asciinema-player", () => ({
  create: vi.fn(() => ({ dispose: vi.fn() })),
}));
vi.mock("asciinema-player/dist/bundle/asciinema-player.css", () => ({}));

import { TerminalPlayer } from "./terminal-player";

function rec(): Recording {
  return {
    run_id: "run_1",
    header: { version: 2, width: 80, height: 24, title: "bash" },
    events: [[0, "o", "hi"]],
    cast: "x",
  };
}

describe("TerminalPlayer", () => {
  // WCAG AA regression: the chrome bar mimics a terminal window titlebar and
  // sits on a hardcoded #0d1117 body, so it must stay dark in every app
  // theme. It used to route its background AND text through the theme-
  // REACTIVE bg-card/60 + text-muted-foreground tokens, which in light theme
  // pull --card (white) into the bar, diluting it to a pale gray with ~1.8:1
  // text contrast (needs 4.5:1 AA). Both must now be theme-invariant.
  it("keeps the chrome bar's background and text theme-invariant, not the reactive card/muted-foreground tokens", () => {
    const { container } = render(<TerminalPlayer recording={rec()} />);

    const bar = container.querySelector(".border-b");
    expect(bar).not.toBeNull();
    expect(bar).not.toHaveClass("bg-card/60");

    // No descendant of the bar may fall back to the theme-reactive text token.
    expect(bar!.querySelectorAll(".text-muted-foreground")).toHaveLength(0);

    const title = screen.getByText("bash");
    expect(title).not.toHaveClass("text-muted-foreground");
  });
});
