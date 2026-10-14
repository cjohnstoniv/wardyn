/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Button } from "../../../../ui/button";
import type { NewRunLocation } from "../../draft/new-run-url";

/** A Change disclosure's way to the step that owns the field. */
export function JumpButton({ label, to, go }: { label: string; to: NewRunLocation; go: (loc: NewRunLocation) => void }) {
  return (
    <Button type="button" variant="outline" className="w-fit" onClick={() => go(to)}>
      {label}
    </Button>
  );
}
