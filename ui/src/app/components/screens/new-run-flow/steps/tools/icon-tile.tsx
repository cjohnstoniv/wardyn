/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/** The prototype's decorative icon tile at the left of a catalogue card and of an included or added row. */
export function IconTile({ mark }: { mark: string }) {
  return (
    <span aria-hidden className="grid size-[34px] shrink-0 place-items-center rounded-lg border border-border bg-muted font-mono text-[11px] font-semibold text-muted-foreground">{mark}</span>
  );
}
