/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

const MAX_TITLE_LEN = 80;

/**
 * A run's default title: the task's own first line, cut at a word boundary
 * within 80 characters, never mid-word and never past a line break the task
 * chose. The server never requires a title; this is the console's default,
 * not a second validation rule.
 */
export function titleFromTask(task: string): string {
  const firstLine = task.split("\n", 1)[0];
  // A task tolerates tab and CR (the multiline exemption) but a title does not,
  // so every run of control characters and whitespace collapses to one space
  // before the cut: the default always passes the check a typed title does.
  const collapsed = firstLine.replace(/[\p{Cc}\s]+/gu, " ").trim();
  if (collapsed.length <= MAX_TITLE_LEN) return collapsed;
  const cut = collapsed.slice(0, MAX_TITLE_LEN);
  const wordBoundary = cut.lastIndexOf(" ");
  return (wordBoundary > 0 ? cut.slice(0, wordBoundary) : cut).trim();
}
