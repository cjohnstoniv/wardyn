/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sign-in door's progress, ready and opened states (#628, the approved
// sign-in progress packet). Presentational only: the pane owns the login run
// and decides which step it is on; this draws it. Split from
// harness-login-pane.tsx to keep that file under the size cap.
import { ExternalLink } from "lucide-react";
import { Button } from "../../ui/button";
import { CopyButton } from "../../wardyn/copy-button";
import { ProgressSteps, type StepRow } from "../../wardyn/progress-steps";
import { isImagePullFailure } from "../run-status-detail";
import { SIGNIN_PROGRESS } from "./login-pane-copy";

// Where the door is. "download-failed" is state 7: the list stops at the step
// that failed, so "Waiting for …" is not drawn at all.
export type SignInStep = "start" | "download" | "wait" | "ready" | "download-failed";

type Mark = StepRow["mark"];

// Moved to run-status-detail.ts (the run page's startup view reads it too);
// re-exported so the pane's import keeps working.
export { isImagePullFailure };

// The device code AWS pre-fills on its own page (verificationUriComplete's
// user_code). "" for a link that carries none — Claude's never does. A regex,
// not URLSearchParams: the portal form puts the query inside the fragment
// (`/start/#/device?user_code=…`), where `URL.searchParams` never looks.
export function deviceCodeOf(url: string): string {
  return /[?&]user_code=([^&#\s]+)/.exec(url)?.[1] ?? "";
}

function marksFor(step: SignInStep): [Mark, Mark, Mark | null] {
  switch (step) {
    case "start":
      return ["active", "pending", "pending"];
    case "download":
      return ["done", "active", "pending"];
    case "wait":
      return ["done", "done", "active"];
    case "ready":
      return ["done", "done", "done"];
    case "download-failed":
      return ["done", "failed", null];
  }
}

export function SignInSteps({ step, provider }: { step: SignInStep; provider: string }) {
  const [start, download, wait] = marksFor(step);
  const downloadLabel =
    download === "active"
      ? SIGNIN_PROGRESS.STEP_DOWNLOAD_ACTIVE
      : download === "failed"
        ? SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED
        : SIGNIN_PROGRESS.STEP_DOWNLOAD;
  const rows: StepRow[] = [
    { key: "start", label: SIGNIN_PROGRESS.STEP_START, mark: start },
    { key: "download", label: downloadLabel, mark: download },
  ];
  if (wait) rows.push({ key: "wait", label: SIGNIN_PROGRESS.STEP_WAIT(provider), mark: wait });
  return <ProgressSteps rows={rows} testId="signin-progress" />;
}

export function DeviceCode({ code, label }: { code: string; label?: string }) {
  if (!code) return null;
  return (
    <code
      aria-label={label}
      className="rounded-md border border-border bg-background/70 px-2 py-1 font-mono text-sm text-foreground"
      data-testid="signin-device-code"
    >
      {code}
    </code>
  );
}

/**
 * States 3–5 of the packet, once the sandbox is up: waiting on the provider
 * (no link yet), ready (the Open button — the one gesture a browser honors
 * with a popup, Finding 7a — the device code, and the copy-link fallback), and
 * opened (Reopen, the code kept on screen). `url` is "" while waiting.
 */
export function SignInDoorState({
  provider,
  url,
  opened,
  showUrl,
  onOpen,
}: {
  provider: string;
  url: string;
  opened: boolean;
  // AWS prints its device link beside the fallback; Claude's OAuth link is
  // ~250 characters, so it is copied, never printed.
  showUrl: boolean;
  onOpen: () => void;
}) {
  const code = deviceCodeOf(url);
  if (opened) {
    return (
      <div className="space-y-2" data-testid="signin-tab-open">
        <p role="status" className="text-xs text-muted-foreground">
          {SIGNIN_PROGRESS.TAB_OPEN(provider)}
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <DeviceCode code={code} />
          <Button size="sm" variant="outline" onClick={onOpen}>
            {SIGNIN_PROGRESS.REOPEN}
          </Button>
        </div>
      </div>
    );
  }
  return (
    <div className="space-y-2">
      <SignInSteps step={url ? "ready" : "wait"} provider={provider} />
      {!url ? (
        <p role="status" className="text-xs text-muted-foreground">
          {SIGNIN_PROGRESS.WAIT_HINT(provider)}
        </p>
      ) : (
        <div className="space-y-2" data-testid="signin-ready">
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" onClick={onOpen}>
              <ExternalLink className="size-3.5" /> {SIGNIN_PROGRESS.OPEN(provider)}
            </Button>
            <DeviceCode code={code} />
          </div>
          <p className="text-xs text-muted-foreground">
            {SIGNIN_PROGRESS.COPY_LEAD}{" "}
            <CopyButton
              text={url}
              label={SIGNIN_PROGRESS.COPY_LINK}
              className="gap-1 font-medium text-info hover:underline"
              iconClassName="size-3"
            >
              {SIGNIN_PROGRESS.COPY_LINK}
            </CopyButton>
            {showUrl && (
              <>
                {" — "}
                <span className="break-all font-mono">{url}</span>
              </>
            )}
          </p>
        </div>
      )}
    </div>
  );
}
