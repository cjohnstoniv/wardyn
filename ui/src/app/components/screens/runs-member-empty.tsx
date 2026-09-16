/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// X3-F4 — the MEMBER's empty runs board. RunsFirstRun (runs-first-run.tsx) is
// the OPERATOR's first-run funnel: a host-barrier readout derived from
// confinement classes, a setup checklist, and the demo grid. A member's
// /setup/status is redacted, so that readout renders blank-as-fact, and the
// steps it offers are routes their role cannot reach. Its own file rather than a
// branch inside runs-first-run.tsx, which is lazy-loaded for the demo catalog a
// member never sees.
import { Link } from "react-router-dom";
import { Rocket } from "lucide-react";
import { Button } from "../ui/button";
import { EmptyState } from "../wardyn/states";
import { RUNS_MEMBER_EMPTY as T } from "../wardyn/copy";

export function RunsMemberEmpty() {
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      <EmptyState
        icon={Rocket}
        title={T.TITLE}
        description={T.BODY}
        action={
          <div className="flex items-center gap-3">
            <Button asChild size="sm">
              <Link to="/runs/new">{T.ACTION}</Link>
            </Button>
            <Link to="/setup" className="text-sm text-muted-foreground underline underline-offset-4 hover:text-foreground">
              {T.GUIDE}
            </Link>
          </div>
        }
      />
    </div>
  );
}
