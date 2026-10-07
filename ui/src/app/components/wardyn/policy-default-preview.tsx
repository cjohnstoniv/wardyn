/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The read-only preview New Run's "Use the default policy" mode shows: the
// spec GET /policies/default answers for this caller, which is exactly what a
// run with no policy of its own launches under. A failed read never blocks
// Launch (the server resolves the default itself); it only says so here.
import { Loader2 } from "lucide-react";
import type { RunPolicySpec } from "../../lib/types";
import { useDeferredBusy } from "../../lib/use-deferred-busy";
import { POLICY_TEMPLATE_COPY as C } from "./copy/policy-templates";
import { Button } from "../ui/button";
import { YamlBlock } from "./code-block";
import { SectionLabel } from "./primitives";
import { STATES } from "./states";

export interface DefaultPolicyView {
  status: "loading" | "ready" | "error";
  spec?: RunPolicySpec;
  /** GET /policies/default's governance_profile_name; names the mode's card. */
  profileName?: string;
  /** Why Launch and Check again are held in this mode, when they are. */
  problem?: string | null;
  onRetry: () => void;
}

export function DefaultPolicyPreview({ view }: { view: DefaultPolicyView }) {
  const { showSpinner } = useDeferredBusy(view.status === "loading", 1000);
  return (
    <div className="space-y-2">
      <SectionLabel>{C.DEFAULT_PREVIEW}</SectionLabel>
      {view.status === "loading" && showSpinner && (
        <p role="status" className="flex items-center gap-2 text-meta text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" aria-hidden />
          {C.DEFAULT_LOADING}
        </p>
      )}
      {view.status === "error" && (
        <div role="status" className="flex flex-wrap items-center gap-2">
          <p className="text-meta text-muted-foreground">{C.DEFAULT_UNAVAILABLE}</p>
          <Button type="button" variant="outline" size="sm" onClick={view.onRetry}>
            {STATES.RETRY}
          </Button>
        </div>
      )}
      {view.status === "ready" && view.spec && (
        <>
          <YamlBlock value={view.spec} />
          <p className="text-meta text-muted-foreground">{C.DEFAULT_NOTE}</p>
        </>
      )}
      {view.problem && (
        <p role="status" className="text-meta text-warning">
          {view.problem}
        </p>
      )}
    </div>
  );
}
