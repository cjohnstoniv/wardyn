/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1200 compact cards: every CollapsibleCard (wardyn/collapsible-card.tsx)
// starts collapsed, so a suite that used to find a card's body on render now
// has one extra step first — one helper here rather than a copy of the same
// two lines in every settings/setup suite this touches.
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

/** Expands the CollapsibleCard whose header (title + summary) matches
 *  `title` — the button's accessible name is the title AND the summary text
 *  concatenated, so this matches on the title being a PREFIX of it, not the
 *  whole name (which changes with the card's state). */
export async function expandCard(title: string) {
  await userEvent
    .setup()
    .click(screen.getByRole("button", { name: (accessibleName) => accessibleName.startsWith(title) }));
}
