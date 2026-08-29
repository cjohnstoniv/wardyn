/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pinned "Needs you" lane — the runs that are ASKING for something, held
// at the top of the board above the title groups.
//
// It holds requests only: a held approval or a run awaiting confirmation.
// A failed or killed run needs review too, but it is a REPORT, not a request —
// it stays with the work it belongs to and carries a danger rail there
// (CONSOLE-RULES §5's precedence note). The board used to flatten both into one
// amber treatment, which made "someone is waiting on you" and "something is
// over" look identical.
//
// A run is in this lane XOR in a group, never both — the screen filters the
// lane's runs out before grouping the rest.
import { BellRing } from "lucide-react";
import type { AgentRun } from "../../../lib/types";
import { CardGrid, RunCard, SectionHeading } from "./run-card";
import type { RunSignals } from "./board-groups";

export function AttentionLane({
  runs,
  signals,
  onOpen,
  onKill,
}: {
  runs: AgentRun[];
  signals: RunSignals;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  if (runs.length === 0) return null;
  return (
    <section aria-label="Needs you">
      <SectionHeading Icon={BellRing} title="Needs you" count={runs.length} tone="warning" />
      <CardGrid>
        {runs.map((run) => (
          // Deliberately NOT `grouped`: a pinned card is out of its title
          // group, so it has to name itself again.
          <RunCard key={run.id} run={run} signals={signals} onOpen={onOpen} onKill={onKill} />
        ))}
      </CardGrid>
    </section>
  );
}
