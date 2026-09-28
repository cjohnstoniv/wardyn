/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1200 compact cards: every CollapsibleCard (wardyn/collapsible-card.tsx)
// starts collapsed, so a suite that used to find a card's body on render now
// has one extra step first — one helper here rather than a copy of the same
// two lines in every settings/setup suite this touches.
//
// review FINAL-PR-1329-r2.md R2-M1: CollapsibleCard's heading takes its name
// from its OWN content (no `aria-label` override — that override made screen
// readers announce the title alone, dropping the summary and expanded state
// from heading navigation). So both the card's `<h3>` and its toggle
// `<button>` now share one accessible name, "{title} {summary}", and a query
// naming only the title must match it as a PREFIX, not the whole string.
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

/** A `getByRole` name matcher: the accessible name starts with `prefix`. */
export function startsWith(prefix: string): (name: string) => boolean {
  return (name: string) => name.startsWith(prefix);
}

/** Expands the CollapsibleCard whose header (title + summary) matches `title`. */
export async function expandCard(title: string) {
  await userEvent.setup().click(screen.getByRole("button", { name: startsWith(title) }));
}
