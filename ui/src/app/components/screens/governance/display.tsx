/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Display helpers shared by the Governance surfaces. Three small ones, none of
// which invents copy: they decide how a FROZEN string is presented, never what
// it says.
import * as React from "react";
import { cn } from "../../ui/utils";
import { Mono } from "../../wardyn/code-block";

// Backtick-mono rendering (governance-prompt.md §7 header rule): a backticked
// substring inside a frozen string is an env var or a wire value and renders
// font-mono — uniformly, everywhere that substring recurs, not only on its
// first appearance.
//
// The strings carry the terms as PLAIN TEXT (the discipline people-access-copy
// .ts states in its own header) — mono is a display concern, and this is the
// ONE place that decides which substrings get it. A profile NAME is never here:
// §7 renders it inside double quotes because it is a human-chosen label, not a
// literal.
const MONO_TERMS = ["WARDYN_DEFAULT_POLICY", "task_mode=exec"];
const MONO_RE = new RegExp(`(${MONO_TERMS.join("|")})`, "g");

export function withMono(text: string): React.ReactNode {
  return text.split(MONO_RE).map((part, i) =>
    MONO_TERMS.includes(part) ? (
      <Mono key={i} className="text-inherit">
        {part}
      </Mono>
    ) : (
      <React.Fragment key={i}>{part}</React.Fragment>
    ),
  );
}

// A standing note under a heading. `tone` carries the meaning: plain for the
// precedence/preview facts, red for a refusal, amber for the omission warnings
// — CONSOLE-RULES §2's state-colour convention, where amber and red mean
// genuine risk and error and nothing else.
//
// ponytail: the same ten lines permissions.tsx keeps locally, plus the amber
// arm this screen needs. Two copies beat a shared primitive nobody else asks
// for; hoist to wardyn/states.tsx when a third screen wants one.
export type NoteTone = "plain" | "red" | "amber";

// Exported so a dialog can wear the same note WITHOUT a nested <div>: Radix's
// AlertDialogDescription renders a <p> and is what gives the dialog its
// aria-describedby, so the styling has to travel to it rather than the other
// way round.
export function noteClass(tone: NoteTone = "plain"): string {
  return cn(
    "mt-2 flex max-w-[82ch] flex-col gap-1 rounded-lg px-3 py-2 text-xs leading-relaxed",
    tone === "red"
      ? "bg-danger-subtle text-danger"
      : tone === "amber"
        ? "bg-warning-subtle text-warning"
        : "bg-muted text-muted-foreground",
  );
}

export function Note({
  tone = "plain",
  role,
  children,
}: {
  tone?: NoteTone;
  /** "alert" for a note that appears in response to an action. */
  role?: string;
  children: React.ReactNode;
}) {
  return (
    <div role={role} className={noteClass(tone)}>
      {children}
    </div>
  );
}

// Splits a frozen confirmation into the question the dialog titles itself with
// and the consequence below it — the shape the mock draws for DELETE_CONFIRM
// and UNASSIGN_CONFIRM ('Delete "X"?' over "It isn't assigned to anyone, …").
//
// A DISPLAY split of one frozen string, never a rewrite: the two halves
// concatenate back to the original byte for byte, and the assigned-delete
// dialog reuses the head alone because its consequence half would be false.
export function question(s: string): [string, string] {
  const at = s.indexOf("? ");
  return at < 0 ? [s, ""] : [s.slice(0, at + 1), s.slice(at + 2)];
}
