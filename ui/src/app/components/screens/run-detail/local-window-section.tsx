/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop tile's window on your computer, for a live Linux VM run: GET /runs/{id}'s local_window, drawn only
// while the setup flag local_window is on. The window is opened by the CLI, so the section has no button: with no
// window open it gives the command to copy, or the server's refusal sentence in its place (local_window.message,
// shown raw); while one is open, since when and how to hand back, in the Mac's words. A background run has none.
import type { RunDetail } from "../../../lib/types";
import { shortTime } from "../../../lib/format";
import { isBackgroundRun } from "../../../lib/run-entry";
import { CopyButton } from "../../wardyn/copy-button";
import { RUN_CLI, RUN_SSH } from "../../wardyn/copy";
import { MAC_WINDOW } from "../../wardyn/copy/mac-window";
import { WINDOW_LOCAL } from "../../wardyn/copy/window-local";
import { useShellSetupStatus } from "../../wardyn/model-access-context";

const line = "mt-2 text-meta leading-relaxed text-muted-foreground";

export function LocalWindowSection({ run }: { run: RunDetail }) {
  const { status } = useShellSetupStatus();
  const w = run.local_window;
  if (status?.features?.local_window !== true || !w || run.machine_kind !== "linux_vm" || isBackgroundRun(run)) return null;
  const runner = run.runner_name || WINDOW_LOCAL.RUNNER_FALLBACK;
  const command = WINDOW_LOCAL.COMMAND(run.id);
  return (
    <div data-testid="local-window" className="mt-4 border-t border-dashed border-border pt-3">
      <p className="text-xs font-medium text-foreground">{WINDOW_LOCAL.TITLE}</p>
      {w.open ? (
        <>
          {w.opened_at && <p className="mt-2 text-meta leading-relaxed text-foreground">{WINDOW_LOCAL.OPEN_SINCE(runner, shortTime(w.opened_at))}</p>}
          <p className={line}>{MAC_WINDOW.handBack(runner)}</p>
          <p className={line}>{MAC_WINDOW.viewOnlyWhileOpen}</p>
        </>
      ) : w.message ? (
        <p className={line}>{w.message}</p>
      ) : (
        <>
          <p className={line}>{WINDOW_LOCAL.NOTE}</p>
          <div className="mt-2 flex items-center gap-2 rounded-lg border border-border bg-surface-2/60 py-2 pl-3 pr-2">
            <code className="min-w-0 flex-1 break-all font-mono text-xs leading-relaxed">{command}</code>
            <CopyButton
              text={command}
              label={RUN_SSH.COPY_CLI}
              className="h-11 shrink-0 gap-1.5 rounded-md border border-border px-3 text-xs text-foreground hover:bg-surface-2 sm:h-[26px]"
            >
              {RUN_CLI.COPY}
            </CopyButton>
          </div>
        </>
      )}
    </div>
  );
}
