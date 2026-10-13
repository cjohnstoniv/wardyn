/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Start screen: three ways into one draft. It has no step shell and no
// Ctrl/Cmd+Enter; each path is its own control. Layout follows Make (App.tsx:1091-1160):
// the whole Guided card is the button, chevron and code marks, a centred trust line with its check.

import * as React from "react";
import { Check, ChevronRight, Code, Sparkles } from "lucide-react";
import { Button } from "../../../ui/button";
import { PathCard } from "../../../wardyn/path-card";
import { NR_START } from "../../../wardyn/copy/new-run/start";
import type { NewRunScreenProps } from "../new-run-route";
import { useAnnounce } from "../steps/step-shell";

export function StartScreen({ go }: NewRunScreenProps) {
  const heading = React.useRef<HTMLHeadingElement>(null);
  const announce = useAnnounce();
  React.useEffect(() => {
    heading.current?.focus();
    announce(NR_START.TITLE);
  }, [announce]);

  return (
    <section className="flex flex-col gap-6">
      {/* Make's StepHeader, as StepShell draws it for the steps (App.tsx:1093-1097). */}
      <header className="flex max-w-[680px] flex-col gap-2.5">
        <p className="mb-1 text-meta font-semibold uppercase tracking-[0.12em] text-primary">{NR_START.EYEBROW}</p>
        <h1 ref={heading} tabIndex={-1} className="text-[clamp(28px,4vw,42px)] leading-[1.08] font-normal tracking-[-0.04em] outline-none">{NR_START.TITLE}</h1>
        <p className="max-w-[620px] text-muted-foreground">{NR_START.LEDE}</p>
      </header>
      <div className="grid gap-4 md:grid-cols-3">
        <PathCard comingSoon icon={<Sparkles className="size-5" />} title={NR_START.AI_TITLE} description={NR_START.AI_BODY} foot={NR_START.AI_FOOT} />
        <PathCard
          icon={<ChevronRight className="size-5" />}
          title={NR_START.GUIDED_TITLE}
          badge={NR_START.GUIDED_BADGE}
          description={NR_START.GUIDED_BODY}
          foot={<span className="inline-flex items-center gap-1">{NR_START.GUIDED_FOOT}<ChevronRight aria-hidden="true" className="size-3.5" /></span>}
          onSelect={() => go({ screen: "step", step: "shape" })}
        />
        <PathCard
          icon={<Code className="size-5" />}
          title={NR_START.DOC_TITLE}
          badge={NR_START.DOC_BADGE}
          description={NR_START.DOC_BODY}
          actions={
            <>
              <Button type="button" variant="outline" onClick={() => go({ screen: "document", format: "yaml" })}>{NR_START.DOC_YAML}</Button>
              <Button type="button" variant="outline" onClick={() => go({ screen: "document", format: "json" })}>{NR_START.DOC_JSON}</Button>
            </>
          }
        />
      </div>
      <p className="mt-1 flex items-center justify-center gap-2 text-xs text-muted-foreground">
        <Check aria-hidden="true" className="size-[15px] text-success" />
        {NR_START.TRUST_LINE}
      </p>
    </section>
  );
}
