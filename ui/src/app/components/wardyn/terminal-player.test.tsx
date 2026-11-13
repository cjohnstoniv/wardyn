/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import * as AsciinemaPlayer from "asciinema-player";
import type { Recording } from "../../lib/types";

// asciinema-player drives real DOM/canvas terminal emulation — stub it so the
// component mounts without a real player instance. Each stub records its
// listeners and answers getCurrentTime with `at`.
type Stub = { at: number; on: Record<string, () => void>; seek: ReturnType<typeof vi.fn>; play: ReturnType<typeof vi.fn>; dispose: ReturnType<typeof vi.fn> };
const players: Stub[] = [];
vi.mock("asciinema-player", () => ({
  create: vi.fn(() => {
    const p: Stub = { at: 0, on: {}, seek: vi.fn(async () => {}), play: vi.fn(async () => {}), dispose: vi.fn() };
    players.push(p);
    return {
      dispose: p.dispose,
      seek: p.seek,
      play: p.play,
      getCurrentTime: () => p.at,
      addEventListener: (name: string, handler: () => void) => {
        p.on[name] = handler;
      },
    };
  }),
}));
vi.mock("asciinema-player/dist/bundle/asciinema-player.css", () => ({}));

import { TerminalPlayer } from "./terminal-player";

beforeEach(() => {
  players.length = 0;
  vi.mocked(AsciinemaPlayer.create).mockClear();
});

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

  // The speed radios were three Tab stops with dead arrow keys.
  it("makes the playback speed one Tab stop that the arrow keys move", async () => {
    render(<TerminalPlayer recording={rec()} />);
    const speed = (x: number) => screen.getByRole("radio", { name: `${x}x speed` });
    expect(speed(1)).toHaveAttribute("tabindex", "0");
    expect(speed(2)).toHaveAttribute("tabindex", "-1");
    speed(1).focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(speed(2)).toHaveFocus();
    expect(speed(2)).toHaveAttribute("aria-checked", "true");
    await userEvent.keyboard("{End}");
    expect(speed(4)).toHaveFocus();
  });

  // A speed change rebuilds the player, which used to start again from the beginning, paused.
  it("a speed change keeps the replay's place and keeps playing", async () => {
    render(<TerminalPlayer recording={rec()} />);
    const first = players[0];
    first.on.playing();
    first.at = 42.5;
    await userEvent.click(screen.getByRole("radio", { name: "2x speed" }));
    expect(AsciinemaPlayer.create).toHaveBeenCalledTimes(2);
    expect(vi.mocked(AsciinemaPlayer.create).mock.calls[1][2]).toMatchObject({ speed: 2 });
    expect(first.dispose).toHaveBeenCalled();
    const second = players[1];
    expect(second.seek).toHaveBeenCalledWith(42.5);
    expect(second.play).toHaveBeenCalled();
  });

  it("a paused replay stays paused at its place; a new recording starts from the beginning", async () => {
    const { rerender } = render(<TerminalPlayer recording={rec()} />);
    players[0].on.playing();
    players[0].on.pause();
    players[0].at = 7;
    await userEvent.click(screen.getByRole("radio", { name: "4x speed" }));
    expect(players[1].seek).toHaveBeenCalledWith(7);
    expect(players[1].play).not.toHaveBeenCalled();

    players[1].at = 9;
    rerender(<TerminalPlayer recording={{ ...rec(), run_id: "run_2", cast: "y" }} />);
    expect(players[2].seek).not.toHaveBeenCalled();
    expect(players[2].play).not.toHaveBeenCalled();
  });
});
