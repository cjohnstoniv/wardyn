/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Ban } from "lucide-react";
import type { RunnerView } from "../../../lib/types/runners";
import { Chip } from "../../wardyn/primitives";
import { CHIP_WORD, runnerChipKind } from "./runner-display";
import { TRUST_LABEL } from "./runner-trust";

export function RunnerChip({ runner }: { runner: Pick<RunnerView, "state" | "online"> }) {
  const kind = runnerChipKind(runner);
  // Terminal state swaps the dot for an icon (CONSOLE-RULES section 5); offline is neutral, not a failure.
  return (
    <Chip tone={kind === "online" ? "success" : "neutral"} dot={kind !== "revoked"}>
      {kind === "revoked" && <Ban className="size-3" aria-hidden />}
      {CHIP_WORD[kind]}
    </Chip>
  );
}

/** The trust class on a list row: an organization's runner always, a person's only when it is not the default Personal (keeps laptops quiet). */
export function TrustChip({ runner }: { runner: Pick<RunnerView, "trust_class"> }) {
  return runner.trust_class === "personal" ? null : <Chip>{TRUST_LABEL[runner.trust_class]}</Chip>;
}
