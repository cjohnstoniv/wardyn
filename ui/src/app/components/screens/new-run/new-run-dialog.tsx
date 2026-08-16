/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// NewRunDialog — the entry point for launching a run. Stage-1 refactor: the
// AI Run Composer (describe → clarify → review, with "Configure manually" as
// its escape hatch) is retired; this now opens the manual PermissionWizard
// directly, unconditionally. The wizard owns its own chrome (it renders its
// own Dialog), so this is a thin pass-through, not a wrapper component.
import type { AgentRun } from "../../../lib/types";
import { PermissionWizard } from "./wizard";

export function NewRunDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (run: AgentRun) => void;
}) {
  return <PermissionWizard open={open} onOpenChange={onOpenChange} onCreated={onCreated} />;
}
