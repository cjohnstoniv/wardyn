/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Desktop tile: a VM run's desktop, in its own tile apart from "Attach from your terminal". The button is at the top; the kind line, the notes, the sign-ins and the Files panel follow. It is the
// gateway's ticket -> bind -> enter hand-off for the built-in desktop app, never a declared ui_app.
//
// A background run has no terminal, so its desktop comes in one of two ways: a macOS VM run whose
// background_watch says "open" gets Watch desktop, a view-only door with its own ticket; every other background run
// gets one sentence and no button. The sentence for a macOS run is the server's own (background_watch.message),
// never the console's guess; "not_run_owner" has none.
import * as React from "react";
import { Monitor } from "lucide-react";
import type { RunDetail } from "../../lib/types";
import { health as healthApi } from "../../lib/api/health";
import { runs as runsApi } from "../../lib/api/runs";
import { Button } from "../ui/button";
import { isMacOSVM, isVMKind } from "../../lib/types/machine-kind";
import { entryErrorMessage, isBackgroundRun, mayEnterRunOrUnknown, sponsorOnlyEntry } from "../../lib/run-entry";
import { RUN_MODE_REFUSAL } from "../../lib/run-mode-refusals";
import { Mono } from "../wardyn/code-block";
import { RUN_SSH, UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { MAC_WINDOW } from "../wardyn/copy/mac-window";
import { RECORDING_DESKTOP, desktopRecordedAt } from "../wardyn/copy/recording-level";
import { RUN_VM_PAGE } from "../wardyn/copy/run-cockpit";
import { useOperator, useOperatorResolved, usePrincipal } from "../wardyn/operator-context";
import { Chip, WidgetCard } from "../wardyn/primitives";
import { KEEP_LIST } from "../wardyn/copy/keep-machine";
import { MacOSDiskNote, macOSRunFactsOf, type MacOSRunFacts } from "./run-detail/macos-run-facts";
import { DesktopFilesPanel } from "./run-detail/desktop-files-panel";
import { DesktopPausedNote, DesktopSignInButton, DesktopSignInsError, DesktopSignInsNote, useDesktopSignIns } from "./run-detail/desktop-sign-ins";
import { MacWindowButton, macWindowHasButton, MacWindowRefusal } from "./run-detail/mac-window-section";
import { DESKTOP_APP, enterUIApp, openEnterTab } from "./run-detail-ssh";

// Exported for the tests: standalone-testable without mounting the whole screen's fetch graph.
export function DesktopCard({ run, macOS: given }: { run: RunDetail; macOS?: MacOSRunFacts }) {
  const macOS = given ?? macOSRunFactsOf(run);
  const principal = usePrincipal();
  const operator = useOperator();
  const resolved = useOperatorResolved();
  // The entry rule (lib/run-entry.ts, #1476), as the Attach tile's: the run's person, or an admin on a run no person
  // owns. Both hooks run unconditionally — `||` on a hook CALL would reorder them.
  const mayAttach = mayEnterRunOrUnknown(run, principal, operator, resolved);
  const running = run.state === "RUNNING";
  // A service identity's sponsor opens the desktop's apps, never its Files.
  const sponsorOnly = resolved && sponsorOnlyEntry(run, principal, operator);
  const background = isBackgroundRun(run);

  // The UI-sandbox enter facts, from the same /healthz read the Attach tile makes. `healthAnswered` is the BODY's
  // `status` (health() cannot reject: it resolves {} on any failure), so the Off copy never renders off a blip.
  const [uiSandbox, setUISandbox] = React.useState<{
    enabled?: boolean;
    enter_post_url?: string;
    enter_url_template?: string;
    bind_url?: string;
  } | null>(null);
  const [healthAnswered, setHealthAnswered] = React.useState(false);
  // health() resolves {} on any failure; "checked, and it did not say ok" is a failed check, not a gateway that is off.
  const [healthFailed, setHealthFailed] = React.useState(false);
  const [healthAttempt, setHealthAttempt] = React.useState(0);
  const [openingApp, setOpeningApp] = React.useState(false);
  const [appError, setAppError] = React.useState<string | null>(null);
  // #2972: a VM desktop's sign-ins, once the gateway can open it. A background run has none: nobody types in it.
  const signIns = useDesktopSignIns(
    run.id,
    mayAttach && running && isVMKind(run.machine_kind) && !!uiSandbox?.enabled && macOS?.desktop !== "observe" && !background,
  );

  React.useEffect(() => {
    if (!mayAttach || !running) return; // nothing to show either way — skip the fetch
    let alive = true;
    void healthApi.health().then((h) => {
      if (!alive) return;
      setUISandbox(h.ui_sandbox ?? null);
      setHealthAnswered(h.status === "ok");
      setHealthFailed(h.status !== "ok");
    });
    return () => {
      alive = false;
    };
  }, [mayAttach, running, healthAttempt]);

  // A finished macOS VM run keeps this tile while the Mac reports its disk.
  if (!isVMKind(run.machine_kind) || !mayAttach || (!running && !macOS?.disk)) return null;

  const mac = isMacOSVM(run);
  const watch = run.background_watch;
  const watchable = background && mac && watch?.state === "open";
  // A background macOS run's refusal, in the server's words. Nothing for a person who isn't the run's owner.
  const watchRefusal = background && mac && watch && watch.state !== "open" && watch.state !== "not_run_owner" ? watch.message : undefined;
  // A background run on any other VM has no desktop door at all (the existing refusal sentence).
  const noDoor = background && !mac;
  const macOff = !!macOS?.disk;
  const viewOnly = (macOS?.desktop === "observe" && running) || watchable;
  const desktopOn = !!uiSandbox?.enabled && !macOff;
  const uiApps = run.ui_apps ?? [];
  // The window on the Mac (mac_window, live macOS VM runs only): its button comes first and the desktop's turns outline.
  const windowState = running ? run.mac_window?.state : undefined;
  const windowButton = running && macWindowHasButton(run.mac_window);
  const windowWay = windowButton || windowState === "window_open";

  // Mints a single-use ticket and opens the built-in desktop in a new tab. A background watch has its own ticket
  // route; every other desktop uses the UI-app ticket the Attach lanes use. The ticket mint is the entry check.
  async function openDesktop() {
    setAppError(null);
    // The tab opens here, before the ticket request, so the browser still counts it as the click's own.
    const tab = openEnterTab();
    if (!tab) {
      setAppError(RUN_SSH.POPUP_BLOCKED);
      return;
    }
    setOpeningApp(true);
    try {
      const ticket = watchable ? await runsApi.watchTicket(run.id) : undefined;
      await enterUIApp(run.id, DESKTOP_APP.name, uiSandbox, { ticket, tab });
    } catch (err) {
      tab.close();
      setAppError(entryErrorMessage(err, (e) => (e instanceof Error ? e.message : String(e)), run));
    } finally {
      setOpeningApp(false);
    }
  }

  // The chip: a background macOS run says View only while it has a watch to say it of, and nothing when the Mac allows
  // control or the caller isn't its owner (its sentence says why); a finished run's tile is Off with the disk line.
  const bgMac = background && mac && running;
  const noWatchChip = bgMac && (!watch || watch.state === "control" || watch.state === "not_run_owner");
  const chip =
    windowState === "window_open" ? (
      <Chip tone="info">{MAC_WINDOW.openChip}</Chip>
    ) : (!healthAnswered && !macOff) || noDoor || noWatchChip ? null : desktopOn && (viewOnly || bgMac) ? (
      <Chip tone="neutral">{DESKTOP.viewOnlyChip}</Chip>
    ) : desktopOn && signIns.view?.paused ? (
      <Chip tone="warning">{DESKTOP.agentPaused}</Chip>
    ) : (
      <Chip tone={desktopOn ? "success" : "neutral"}>{desktopOn ? DESKTOP.ready : DESKTOP.statusOff}</Chip>
    );

  return (
    // The body scrolls, not the tile: the canvas tile clips (overflow-hidden), so a tile that sizes to its content
    // loses its bottom on a short one.
    <WidgetCard title={DESKTOP.title} Icon={Monitor} right={chip} grow bodyClassName="scroll-thin overflow-y-auto p-4">
      <section aria-label={DESKTOP.title}>
        <p className="mb-2 text-meta text-muted-foreground">{mac ? DESKTOP.kindMacOS : DESKTOP.kind}</p>
        {running && <MacWindowButton run={run} />}
        {desktopOn && !noDoor && (!background || watchable) && (
          <div className={`flex flex-wrap gap-2 ${windowWay ? "mt-4" : ""}`}>
            <Button size="sm" variant={windowButton ? "outline" : undefined} disabled={openingApp} onClick={() => void openDesktop()}>
              {openingApp ? UI_APPS_LANE.ctaBusy : viewOnly ? DESKTOP.watch : UI_APPS_LANE.cta("desktop")}
            </Button>
            {!background && <DesktopSignInButton s={signIns} />}
          </div>
        )}
        {/* What the screen recording keeps, once, where the desktop opens. No frames are kept while the agent is paused for a sign-in. */}
        {desktopOn && !noDoor && (!background || watchable) && desktopRecordedAt(run) && (
          <p className="mt-2 text-meta text-muted-foreground" data-testid="desktop-recorded-line">
            {!background && signIns.view?.paused ? RECORDING_DESKTOP.PAUSED : RECORDING_DESKTOP.WATCHED}
          </p>
        )}
        {desktopOn && !background && <DesktopSignInsError s={signIns} />}
        {healthFailed && !macOff && !noDoor && (
          <div className="mt-2 flex flex-wrap items-center gap-2">
            <p role="alert" className="text-meta leading-relaxed text-muted-foreground">{RUN_VM_PAGE.gatewayUnknown}</p>
            <Button size="sm" variant="outline" onClick={() => { setHealthFailed(false); setHealthAttempt((n) => n + 1); }}>
              {RUN_VM_PAGE.tryAgain}
            </Button>
          </div>
        )}
        {appError && <p role="alert" className="mt-2 font-mono text-meta leading-relaxed text-muted-foreground">{appError}</p>}
        {noDoor && <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{RUN_MODE_REFUSAL.BACKGROUND_ONLY("desktop")}</p>}
        {watchRefusal && <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{watchRefusal}</p>}
        {/* What a person can do on this desktop is only said while the gateway can open it; with it off, the Off sentence is the one claim. */}
        {watchable && desktopOn && (
          <>
            <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{DESKTOP.backgroundWatchTile}</p>
            <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{DESKTOP.backgroundWatchExposure}</p>
          </>
        )}
        {!background && viewOnly && (
          <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{windowWay ? MAC_WINDOW.watchLine : DESKTOP.observeOnlyMacOS}</p>
        )}
        {/* A window refusal sits under the line above: the browser can still watch, and this says why no window opens. */}
        {running && <MacWindowRefusal run={run} />}
        {healthAnswered && !uiSandbox?.enabled && !macOff && !noDoor && (
          <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{offCopy(DESKTOP.off, "WARDYN_UI_SANDBOX_LISTEN")}</p>
        )}
        <MacOSDiskNote disk={macOS?.disk} />
        {desktopOn && !background && (
          <>
            <DesktopPausedNote s={signIns} />
            {!viewOnly && <p className="mt-2 text-meta leading-relaxed text-muted-foreground sm:hidden">{DESKTOP.phoneBefore}</p>}
          </>
        )}
        {desktopOn && (!background || watchable) && <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{UI_APPS_LANE.newTab}</p>}
        {/* Files moves with the desktop, under the buttons: the same gate it had in the Attach tile
            (the run's person, not a sponsor, while it runs, with a declared UI app or a Linux VM's built-in desktop),
            plus never on a background run, which has no files door. The panel shows the run's own desktop rules. */}
        {desktopOn && !background && (uiApps.length > 0 || !mac) && mayAttach && !sponsorOnly && running && <DesktopFilesPanel runId={run.id} vm />}
        {desktopOn && !background && <DesktopSignInsNote s={signIns} keptOn={run.machine && !run.machine.deleted && run.machine.keep_at_end ? run.machine.runner_name || KEEP_LIST.THIS_MAC : undefined} />}
      </section>
    </WidgetCard>
  );
}

// The env var in the Off sentence in <Mono>, as the Attach tile draws the same token.
function offCopy(text: string, token: string): (string | React.ReactElement)[] {
  const [head, ...rest] = text.split(token);
  return rest.length === 0
    ? [text]
    : [head, <Mono key="t" className="text-foreground">{token}</Mono>, rest.join(token)];
}
