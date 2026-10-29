/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One declared UI app on the run page (r16 T2-b): its name and address, the Open button, and for an inbound app
// the tag, who may call it and the two Copy buttons. Open stays the one teal button on the card (`primary`).
import * as React from "react";
import type { UIApp } from "../../../lib/types";
import { basePath } from "../../../lib/base-path";
import { Button, buttonVariants } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
import { INBOUND_APP, UI_APPS_LANE } from "../../wardyn/copy/ui-apps";
import { CopyButton } from "../../wardyn/copy-button";
import { Chip } from "../../wardyn/primitives";

/** What /healthz publishes about the gateway that this row reads. */
export interface InboundFacts {
  host_mode?: boolean;
  inbound_url_template?: string;
}

const fill = (template: string, run: string, app: string) =>
  template.split("{run}").join(encodeURIComponent(run)).split("{app}").join(encodeURIComponent(app));

export function UIAppRow({
  runId,
  app,
  primary,
  busy,
  onOpen,
  facts,
  children,
}: {
  runId: string;
  app: UIApp;
  /** The card's one teal Open button. */
  primary: boolean;
  busy: boolean;
  onOpen: () => void;
  facts: InboundFacts | null;
  /** Under the row: the failure of this app's last Open. */
  children?: React.ReactNode;
}) {
  const callers = app.callers ?? [];
  const link = `${window.location.origin}${basePath()}/open/${encodeURIComponent(runId)}/${encodeURIComponent(app.name)}`;
  const address = facts?.inbound_url_template ? fill(facts.inbound_url_template, runId, app.name) : "";
  const copyButton = "gap-1.5 " + buttonVariants({ variant: "outline", size: "sm" });
  return (
    <div className="mt-3 border-t border-border pt-3 first-of-type:border-t-0 first-of-type:pt-0" data-testid={`ui-app-${app.name}`}>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="flex flex-wrap items-center gap-2 text-xs font-medium text-foreground">
            {app.name}
            {app.inbound && <Chip tone="info">{INBOUND_APP.TAG}</Chip>}
          </p>
          <Mono className="text-meta text-muted-foreground">{UI_APPS_LANE.appSub(app.port, app.path || "/")}</Mono>
        </div>
        <Button size="sm" variant={primary ? "default" : "outline"} disabled={busy} onClick={onOpen}>
          {busy ? UI_APPS_LANE.ctaBusy : UI_APPS_LANE.cta(app.name)}
        </Button>
      </div>
      {app.inbound && callers.length > 0 ? (
        <div className="mt-2 flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          <span>{INBOUND_APP.CALLERS}</span>
          {callers.map((c) => (
            <Chip key={c} mono>
              {c}
            </Chip>
          ))}
        </div>
      ) : (
        <Chip className="mt-2">{INBOUND_APP.ONLY_YOU}</Chip>
      )}
      {app.inbound && (
        <>
          <div className="mt-2 flex flex-wrap gap-2">
            <CopyButton text={link} label={INBOUND_APP.COPY_LINK} className={copyButton}>
              {INBOUND_APP.COPY_LINK}
            </CopyButton>
            {address && (
              <CopyButton text={address} label={INBOUND_APP.COPY_ADDRESS} className={copyButton}>
                {INBOUND_APP.COPY_ADDRESS}
              </CopyButton>
            )}
          </div>
          {facts?.host_mode === false && (
            <p className="mt-2 rounded-md border border-border bg-surface-2/60 px-3 py-2 text-meta leading-relaxed text-muted-foreground">
              {INBOUND_APP.PATH_MODE_NOTE}
            </p>
          )}
        </>
      )}
      {children}
    </div>
  );
}
