/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hero's Watch desktop: the Desktop block's own action, on a macOS VM
// whose desktop is view only, where it is seen without scrolling. It renders nothing until /healthz says the
// gateway is on. The ticket mint is the entry check, as for Open desktop.
import * as React from "react";
import { health } from "../../../lib/api/health";
import { entryErrorMessage, isBackgroundRun } from "../../../lib/run-entry";
import type { RunDetail } from "../../../lib/types";
import { Button } from "../../ui/button";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { RUN_SSH, UI_APPS_LANE } from "../../wardyn/copy";
import { RUN_VM_PAGE } from "../../wardyn/copy/run-cockpit";
import { DESKTOP_APP, enterUIApp, openEnterTab, type UISandboxEnter } from "../run-detail-ssh";

export function WatchDesktopButton({ run }: { run: RunDetail }) {
  const [gateway, setGateway] = React.useState<UISandboxEnter>(null);
  const [opening, setOpening] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  // health() resolves {} on any failure, so a body without status "ok" is a check that failed, not a gateway that is off.
  const [unchecked, setUnchecked] = React.useState(false);
  const [attempt, setAttempt] = React.useState(0);

  React.useEffect(() => {
    let alive = true;
    void health.health().then((h) => {
      if (!alive) return;
      setUnchecked(h.status !== "ok");
      if (h.ui_sandbox?.enabled) setGateway(h.ui_sandbox);
    });
    return () => {
      alive = false;
    };
  }, [attempt]);

  // A background run is watched from the Desktop tile, by its own ticket; this button mints the attach ticket, which refuses it.
  if (isBackgroundRun(run)) return null;
  if (!gateway && unchecked) {
    return (
      <>
        <span role="alert" className="max-w-[24rem] min-w-0 break-words text-meta text-muted-foreground">{RUN_VM_PAGE.gatewayUnknown}</span>
        <Button size="sm" variant="outline" className="shrink-0" onClick={() => { setUnchecked(false); setAttempt((n) => n + 1); }}>
          {RUN_VM_PAGE.tryAgain}
        </Button>
      </>
    );
  }
  if (!gateway) return null;

  async function watch() {
    setError(null);
    // The tab opens inside the click; enterUIApp sends the desktop into it and closes it on failure.
    const tab = openEnterTab();
    if (!tab) {
      setError(RUN_SSH.POPUP_BLOCKED);
      return;
    }
    setOpening(true);
    try {
      await enterUIApp(run.id, DESKTOP_APP.name, gateway, { tab });
    } catch (err) {
      setError(entryErrorMessage(err, (e) => (e instanceof Error ? e.message : String(e)), run));
    } finally {
      setOpening(false);
    }
  }

  return (
    <>
      {error && <span role="alert" className="max-w-[24rem] min-w-0 break-words font-mono text-meta text-muted-foreground">{error}</span>}
      <Button size="sm" variant="outline" className="shrink-0" disabled={opening} onClick={() => void watch()}>
        {opening ? UI_APPS_LANE.ctaBusy : DESKTOP.watch}
      </Button>
    </>
  );
}
