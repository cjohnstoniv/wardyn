/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The empty Runs board's "See it work" card grid, split out of
// runs-first-run.tsx for ONE reason: the demo catalog.
//
// RunsScreen is EAGER (App.tsx keeps /runs unlazied — see its comment), so
// everything runs-first-run.tsx statically imports lands in the entry chunk.
// demo-catalog.ts is mostly PROSE — per-demo overviews, numbered command
// walkthroughs, setup instructions — none of which this grid reads: it renders
// `title`, `teaches` and the two gate fields, and nothing else. Left as a
// static import it shipped ~50 kB of demo copy to every first paint of the runs
// board, and bundle-split.test.ts's entry budget is what caught it.
//
// So the grid is React.lazy'd by its only caller. The catalog goes with it,
// into the same chunk the Getting Started funnel already pulls, and the entry
// carries none of it. Keep the DEMOS import in THIS file, never back in
// runs-first-run.tsx.
import { Link } from "react-router-dom";
import { Button } from "../ui/button";
import { DEMOS } from "./demos/demo-catalog";

export default function FirstRunDemoGrid({
  llmReady,
  secretNames,
}: {
  llmReady: boolean;
  secretNames: string[];
}) {
  return (
    <div className="w-full max-w-[900px] space-y-4">
      <div className="space-y-1 text-center">
        <h3 className="text-[0.6875rem] font-semibold uppercase tracking-wider text-muted-foreground">
          See it work
        </h3>
        <p className="text-sm text-muted-foreground">
          No model, no key, no repo. Each one runs a real governed sandbox in about a minute.
        </p>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {DEMOS.map((demo) => {
          // The flagship agent demo needs a connected model — the same gate
          // the funnel's own walk applies (steps.ts's stepOrder drops the
          // step until llmReady), so this card can never link to a step that
          // isn't there.
          const needsModel = demo.needsModel && !llmReady;
          // Its mirror for the granted secrets demos: without the secret
          // stored, run-create 422s on the unresolvable grant ref and the
          // step is filtered out of the walk — so name the missing secret
          // instead of linking to nothing.
          const needsSecret = demo.needsSecret && !secretNames.includes(demo.needsSecret);
          // `needsGitHubApp` deliberately gets NO branch here. Unlike the two
          // above it does not drop the step from the walk — the card stays and
          // closes its own Start — so "Run it" still leads somewhere real, and
          // a third muted variant here would just duplicate the gate copy the
          // step itself renders.
          return (
            <div
              key={demo.id}
              data-testid={`runs-empty-demo-${demo.id}`}
              className="flex flex-col gap-2.5 rounded-xl border border-border bg-card p-4"
            >
              <h4 className="text-sm font-semibold text-foreground">{demo.title}</h4>
              <p className="flex-1 text-xs leading-snug text-muted-foreground">{demo.teaches}</p>
              {needsModel ? (
                <p className="text-xs text-muted-foreground">
                  Needs a model provider ·{" "}
                  <Link to="/integrations" className="font-medium text-primary hover:underline">
                    Connect →
                  </Link>
                </p>
              ) : needsSecret ? (
                <p className="text-xs text-muted-foreground">
                  Needs the <code className="font-mono">{demo.needsSecret}</code> secret ·{" "}
                  <Link to="/secrets" className="font-medium text-primary hover:underline">
                    Add it →
                  </Link>
                </p>
              ) : (
                // Secondary/outline — a teal fill is reserved for the one
                // primary action on the page ("New run" above). Per-card: the
                // funnel step for THIS demo, not a catalog page to hunt in.
                <Button variant="outline" size="sm" className="self-start" asChild>
                  <Link to={`/setup?step=${demo.id}`}>Run it</Link>
                </Button>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
