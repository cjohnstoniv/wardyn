/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Getting-started "Connect a model" step body. Renders the LLM-access list
// (LlmAccess) and, when running on the containerized control plane (a coming-soon
// team feature) that can't see the host's Claude login, a "use host mode" hint.
// Has NO composer-backends section by design (owner decision: zero composer UI here).
import * as React from "react";
import { Info } from "lucide-react";
import type { SetupStatus } from "../../../lib/types";
import { LlmAccess } from "./llm-access";
import type { SetupGuide } from "./setup-guide";
import type { Readiness } from "../onboarding/intro";

export function ModelStep({
  status,
  readiness,
  onAddSecret,
  onSetup,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  readiness: Readiness;
  onAddSecret: (name: string) => void;
  onSetup: (g: SetupGuide) => void;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  // Guidance for the most common first-run snag: a personal machine running the
  // sealed (compose/team) control plane, which can't see the host's Claude login —
  // so this step reads "not connected" even when the operator IS logged in. Only
  // shown when the model is genuinely undetected AND we're blind-in-compose on a
  // local box (host_like === false + local auth); host mode never sees it.
  const suggestHostMode =
    !readiness.llmReady && status.deployment?.host_like === false && status.auth.mode === "local";

  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">
        {readiness.llmReady
          ? `One connected path is enough — you're already covered by ${readiness.llmLabel || "a connected model"}.`
          : "Wardyn needs a way for the agent to talk to an LLM — a stored API key the proxy injects, or a resident CLI subscription."}
      </p>

      {suggestHostMode && (
        <div className="flex items-start gap-2.5 rounded-lg border border-border bg-muted/40 p-3">
          <Info className="mt-0.5 size-4 shrink-0 text-primary" />
          <div className="min-w-0 flex-1 space-y-1.5 text-xs leading-relaxed">
            <p className="text-foreground">
              Sandboxing your <span className="font-medium">own machine</span>, and already logged into the
              Claude CLI? You&apos;re on the containerized control plane — a <span className="font-medium">coming-soon
              team feature</span>. wardynd runs sealed in a container that can&apos;t see your host&apos;s{" "}
              <code className="rounded bg-background/70 px-1 py-0.5 text-xs">~/.claude</code> login, which is why it
              reads &quot;not connected&quot; even though you are. Host mode is the supported setup — it uses your
              existing login automatically, no re-login, no stored key:
            </p>
            <p>
              <code className="rounded bg-background/70 px-1.5 py-0.5 font-mono text-xs text-foreground">
                make setup
              </code>
            </p>
          </div>
        </div>
      )}

      <LlmAccess
        status={status}
        onAddSecret={onAddSecret}
        onSetup={onSetup}
        onRecheck={onRecheck}
        rechecking={rechecking}
      />
    </div>
  );
}
