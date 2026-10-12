/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Start screen: three ways into one draft. It has no step shell and no
// Ctrl/Cmd+Enter; each path is its own control.

import * as React from "react";
import { FileCode2, ListChecks, Sparkles } from "lucide-react";
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
      <header className="flex flex-col gap-2">
        <p className="label-eyebrow">{NR_START.EYEBROW}</p>
        <h1 ref={heading} tabIndex={-1} className="text-display font-semibold tracking-[-0.02em] outline-none">{NR_START.TITLE}</h1>
        <p className="text-muted-foreground">{NR_START.LEDE}</p>
      </header>
      <div className="grid gap-4 md:grid-cols-3">
        <PathCard comingSoon icon={<Sparkles className="size-5" />} title={NR_START.AI_TITLE} description={NR_START.AI_BODY} foot={NR_START.AI_FOOT} />
        <PathCard
          icon={<ListChecks className="size-5" />}
          title={NR_START.GUIDED_TITLE}
          badge={NR_START.GUIDED_BADGE}
          description={NR_START.GUIDED_BODY}
          actions={<Button type="button" onClick={() => go({ screen: "step", step: "shape" })}>{NR_START.GUIDED_FOOT}</Button>}
        />
        <PathCard
          icon={<FileCode2 className="size-5" />}
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
      <p className="text-sm text-muted-foreground">{NR_START.TRUST_LINE}</p>
    </section>
  );
}
