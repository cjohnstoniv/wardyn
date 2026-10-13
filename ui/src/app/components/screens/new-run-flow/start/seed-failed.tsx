/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A template or a run to run again could not be read: the server's own sentence,
// and a way back to an empty draft. No sentence of ours stands between them.

import * as React from "react";
import { getErrorMessage } from "../../../../lib/format";
import { Button } from "../../../ui/button";
import { NR_TEMPLATES } from "../../../wardyn/copy/new-run/templates";
import type { NewRunScreenProps } from "../new-run-route";

export function SeedFailed({ seedError, startFromScratch }: NewRunScreenProps) {
  const button = React.useRef<HTMLButtonElement>(null);
  React.useEffect(() => button.current?.focus(), []);
  return (
    <section className="flex flex-col items-start gap-4">
      <p role="alert" className="text-sm text-danger">{getErrorMessage(seedError)}</p>
      <Button ref={button} type="button" onClick={startFromScratch}>{NR_TEMPLATES.NEW_FROM_SCRATCH}</Button>
    </section>
  );
}
