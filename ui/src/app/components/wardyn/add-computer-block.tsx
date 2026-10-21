/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Add this computer" (mock M-ATT-4, DESIGN-ATTACH 11.10 ATT-RCARD): install the CLI, sign in, set up the
// runner, three commands in a terminal on the computer being added. Admin -> Runners and Your account -> My
// runner draw the same block; only the admin page passes `mdm`, the folded "Managed install (MDM)" section.
//
// Step 1's line is the one the attach card draws (lib/cli-install.ts, from /healthz.version and the operator's
// advertised mirror). Step 2 says so when /healthz.cli_sign_in is not true, because then `wardyn login` cannot
// sign in and a personal token in WARDYN_TOKEN is the way (11.4 step 1). Nothing here shows a start command, a
// proxy image or a token value.
import * as React from "react";
import { health } from "../../lib/api/health";
import { appURL } from "../../lib/base-path";
import { installLines, mirrorRejected, viewerIsWindows } from "../../lib/cli-install";
import { cn } from "../ui/utils";
import { ADD_COMPUTER, ADD_COMPUTER_COMMANDS, INSTALL_DOCS_URL } from "./copy/add-computer";
import { CopyButton } from "./copy-button";

interface Facts {
  /** The first /healthz read has settled, answered or not. */
  loaded: boolean;
  /** /healthz actually answered: a swallowed failure is {} and says nothing about the deployment. */
  answered: boolean;
  version?: string;
  artifactBase?: string;
  cliSignIn: boolean;
}

function useFacts(): [Facts, () => void] {
  const [facts, setFacts] = React.useState<Facts>({ loaded: false, answered: false, cliSignIn: false });
  const [attempt, setAttempt] = React.useState(0);
  React.useEffect(() => {
    let alive = true;
    void health.health().then((h) => {
      if (!alive) return;
      setFacts({ loaded: true, answered: h.status === "ok", version: h.version, artifactBase: h.cli_artifact_base, cliSignIn: h.cli_sign_in === true });
    });
    return () => {
      alive = false;
    };
  }, [attempt]);
  return [facts, () => setAttempt((n) => n + 1)];
}

/**
 * One line to copy: the code and a Copy button. On a phone (MATT4-Phone) the code sits in its own bordered box and
 * the button is a separate full-width 44 px row 8 px below it; from `sm` up the box's border wraps both, in one row.
 */
function CmdRow({ text, label }: { text: string; label: string }) {
  return (
    <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:gap-2 sm:rounded-lg sm:border sm:border-border sm:bg-background sm:py-2 sm:pl-3 sm:pr-2">
      <div data-cmd-box className="min-w-0 flex-1 rounded-lg border border-border bg-background p-2 sm:rounded-none sm:border-0 sm:bg-transparent sm:p-0">
        <code className="block break-all px-1 font-mono text-xs leading-relaxed sm:px-0">{text}</code>
      </div>
      <CopyButton
        text={text}
        label={label}
        className="h-11 w-full shrink-0 justify-center gap-1.5 rounded-md border border-border px-3 text-sm text-foreground hover:bg-surface-2 sm:h-[26px] sm:w-auto sm:text-xs"
      >
        {ADD_COMPUTER.COPY}
      </CopyButton>
    </div>
  );
}

function Disclosure({ summary, className, children }: { summary: React.ReactNode; className?: string; children: React.ReactNode }) {
  return (
    <details className={cn("group", className)}>
      <summary className="flex min-h-11 cursor-pointer items-center gap-2 sm:min-h-0">
        <span className="flex-1">{summary}</span>
        <span className="text-xs text-muted-foreground group-open:hidden">{ADD_COMPUTER.SHOW}</span>
        <span className="hidden text-xs text-muted-foreground group-open:inline">{ADD_COMPUTER.HIDE}</span>
      </summary>
      {children}
    </details>
  );
}

type Level = 2 | 3 | 4 | 5 | 6;
const H = ({ level, ...rest }: React.ComponentProps<"h2"> & { level: Level }) => React.createElement(`h${level}`, rest);

function Step({ n, title, level, headingRef, children }: { n: number; title: string; level: Level; headingRef?: React.Ref<HTMLHeadingElement>; children: React.ReactNode }) {
  return (
    <li className="flex items-start gap-2.5">
      <span aria-hidden className="mt-px inline-grid size-5 shrink-0 place-items-center rounded-full bg-primary text-[11px] font-bold text-primary-foreground">
        {n}
      </span>
      <div className="min-w-0 flex-1 space-y-1.5">
        <H level={level} ref={headingRef} tabIndex={-1} className="text-[13px] font-semibold text-foreground outline-none">
          {title}
        </H>
        {children}
      </div>
    </li>
  );
}

const NOTE = "text-meta leading-relaxed text-muted-foreground";

function InstallStep({ facts, level, headingRef, onRetry }: { facts: Facts; level: Level; headingRef: React.Ref<HTMLHeadingElement>; onRetry: (button: HTMLElement) => void }) {
  const lines = installLines(facts.version, facts.artifactBase);
  const windows = viewerIsWindows();
  // The shell line leads on every system: from Windows the runner lives in WSL2, so it is the only line there.
  return (
    <Step n={1} title={ADD_COMPUTER.STEP_INSTALL} level={level} headingRef={headingRef}>
      {!facts.loaded ? null : lines ? (
        <>
          <p className={NOTE}>{ADD_COMPUTER.UNIX}</p>
          <CmdRow text={lines.sh} label={ADD_COMPUTER.COPY_INSTALL_UNIX} />
          {!windows && (
            <Disclosure summary={<span className="text-meta text-foreground">{ADD_COMPUTER.OTHER_OS}</span>}>
              <p className={cn(NOTE, "mt-2 mb-1")}>{ADD_COMPUTER.WINDOWS}</p>
              <CmdRow text={lines.ps} label={ADD_COMPUTER.COPY_INSTALL_WINDOWS} />
            </Disclosure>
          )}
        </>
      ) : mirrorRejected(facts.artifactBase) ? null : (
        <div className="flex flex-wrap items-center gap-2">
          <p className={NOTE}>{ADD_COMPUTER.NO_INSTALL_LINE}</p>
          {!facts.answered && (
            <button type="button" onClick={(e) => onRetry(e.currentTarget)} className="h-11 rounded-md border border-border px-2.5 text-xs text-foreground hover:bg-surface-2 sm:h-7">
              {ADD_COMPUTER.TRY_AGAIN}
            </button>
          )}
        </div>
      )}
      <p className="text-meta">
        <a href={INSTALL_DOCS_URL} target="_blank" rel="noopener noreferrer" className="inline-flex min-h-11 items-center text-primary underline-offset-2 hover:underline sm:min-h-0">
          {ADD_COMPUTER.OTHER_WAYS}
        </a>
      </p>
    </Step>
  );
}

/**
 * `level` is the block title's heading level (steps are one deeper): 2 on the Admin page, 4 inside the My runner
 * card, whose own title is an h3.
 */
export function AddComputerBlock({
  mdm,
  level = 2,
}: {
  /** The folded "Managed install (MDM)" content; absent where a person adds only their own computer. */
  mdm?: React.ReactNode;
  level?: 2 | 4;
}) {
  const [facts, retry] = useFacts();
  const url = window.location.origin + appURL("");
  const windows = viewerIsWindows();
  const step = (level + 1) as Level;
  // Try again unmounts itself when /healthz finally answers. Focus moves to the first step's title only if it was
  // lost with the button (on the page, or still on the button); a person who moved on while the read was out keeps
  // where they are.
  const installTitle = React.useRef<HTMLHeadingElement | null>(null);
  const retryButton = React.useRef<HTMLElement | null>(null);
  React.useEffect(() => {
    const button = retryButton.current;
    if (!button || !facts.answered) return;
    retryButton.current = null;
    const active = document.activeElement;
    if (!active || active === document.body || active === button) installTitle.current?.focus();
  }, [facts]);
  return (
    <section aria-labelledby="add-computer-title" data-add-computer className="space-y-3 rounded-xl border border-border bg-card px-4 py-4 sm:px-[18px]">
      <div className="space-y-1">
        <H level={level} id="add-computer-title" className="text-sm font-semibold text-foreground">
          {ADD_COMPUTER.TITLE}
        </H>
        <p className={NOTE}>{ADD_COMPUTER.LEAD}</p>
        {windows && <p className="text-body text-foreground">{ADD_COMPUTER.WSL2_NOTE}</p>}
      </div>
      <ol className="space-y-3">
        <InstallStep
          facts={facts}
          level={step}
          headingRef={installTitle}
          onRetry={(button) => {
            retryButton.current = button;
            retry();
          }}
        />
        <Step n={2} title={ADD_COMPUTER.STEP_SIGN_IN} level={step}>
          <CmdRow text={ADD_COMPUTER_COMMANDS.login(url)} label={ADD_COMPUTER.COPY_SIGN_IN} />
          {/* Silent until /healthz answers: nothing is promised about sign-in capability that was not read. */}
          {facts.answered && <p className={NOTE}>{facts.cliSignIn ? ADD_COMPUTER.SIGN_IN_NOTE : ADD_COMPUTER.NO_CLI_SIGN_IN}</p>}
        </Step>
        <Step n={3} title={ADD_COMPUTER.STEP_SETUP} level={step}>
          <CmdRow text={ADD_COMPUTER_COMMANDS.setup} label={ADD_COMPUTER.COPY_SETUP} />
          <p className={NOTE}>{ADD_COMPUTER.SETUP_NOTE}</p>
        </Step>
      </ol>
      {mdm && (
        <Disclosure className="border-t border-border pt-3" summary={<span className="text-[13px] font-semibold text-foreground">{ADD_COMPUTER.MDM_TITLE}</span>}>
          <div data-mdm className="space-y-3 pt-2">
            <p className={NOTE}>{ADD_COMPUTER.MDM_LEAD}</p>
            {mdm}
          </div>
        </Disclosure>
      )}
    </section>
  );
}
