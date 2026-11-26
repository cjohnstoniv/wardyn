/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The assistant starting: a run under the person's name, with the policy Wardyn writes for it. For a repository draft
// the build files are fetched first. The screen moves on when the assistant says it is ready.
import { Loader2 } from "lucide-react";
import { Link } from "react-router-dom";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";

export interface ComposerStartingProps {
  provider: string;
  /** The assistant's run, once it has one. */
  runId?: string;
  /** The build files are still being read, so the run does not exist yet. */
  fetching: boolean;
}

export function ComposerStarting({ provider, runId, fetching }: ComposerStartingProps) {
  return (
    <section data-testid="composer-starting" className="flex max-w-[640px] flex-col gap-3 rounded-xl border border-border p-3">
      {fetching && (
        <div className="flex items-start gap-2">
          <Loader2 aria-hidden="true" className="mt-0.5 size-4 shrink-0 animate-spin text-primary" />
          <div>
            <p className="text-sm font-medium text-foreground">{T.FETCHING}</p>
            <p className="mt-0.5 text-meta text-muted-foreground">{T.FETCH_LINE}</p>
          </div>
        </div>
      )}
      <div className={fetching ? "flex items-start gap-2 opacity-60" : "flex items-start gap-2"}>
        <Loader2 aria-hidden="true" className="mt-0.5 size-4 shrink-0 animate-spin" />
        <div>
          <p className="text-sm font-medium text-foreground">{T.STARTING}</p>
          <p className="mt-0.5 text-meta text-muted-foreground">{T.RUN_POLICY(provider)}</p>
          {runId && (
            <Link className="text-meta text-info underline" to={`/runs/${runId}`}>
              {T.OPEN_RUN}
            </Link>
          )}
        </div>
      </div>
    </section>
  );
}
