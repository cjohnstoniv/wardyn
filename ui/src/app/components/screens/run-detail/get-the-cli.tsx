/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Wardyn CLI lane and "Get the CLI" of "Attach from your terminal" (M-ATT-1,
// DESIGN-ATTACH §11.10 ATT-CARD). Split out of run-detail-ssh.tsx so the card keeps
// one job per file; the strings are copy/run-ssh.ts's RUN_CLI.
//
// The primary action is `wardyn attach <run-id>` and nothing else: no environment
// prefix, no token, no ticket. The console's own address reaches the CLI through
// the one first-time line, `wardyn login <url>`, which is true before and after
// CLI sign-in exists (an older control plane makes the CLI explain the token path
// itself), so the card never has two forms of it.
import { cn } from "../../ui/utils";
import { Mono, makeMono } from "../../wardyn/code-block";
import { CopyButton } from "../../wardyn/copy-button";
import { RUN_CLI, RUN_SSH } from "../../wardyn/copy";
import { installLines, viewerIsWindows, type OrgCAFingerprint } from "../../../lib/cli-install";

// Where "Other ways to install" points: the ranked table of the install docs.
export const INSTALL_DOCS_URL = "https://github.com/cjohnstoniv/wardyn/blob/main/docs/runner-registration.md#install-the-cli";

const caMono = makeMono(["wardyn ca add <file>"]);

/** One line to copy: the code and a small labelled Copy button. */
function CmdRow({ text, label, className }: { text: string; label: string; className?: string }) {
  return (
    <div
      className={cn(
        "flex items-center gap-2 rounded-lg border border-border bg-surface-2/60 py-2 pl-3 pr-2",
        className,
      )}
    >
      <code className="min-w-0 flex-1 break-all font-mono text-xs leading-relaxed">{text}</code>
      <CopyButton
        text={text}
        label={label}
        className="h-11 shrink-0 gap-1.5 rounded-md border border-border px-3 text-xs text-foreground hover:bg-surface-2 sm:h-[26px]"
      >
        {RUN_CLI.COPY}
      </CopyButton>
    </div>
  );
}

/** Heading, note, the attach command, its copy button, and the one first-time line. */
export function CliLane({ runId, consoleURL }: { runId: string; consoleURL: string }) {
  const command = RUN_CLI.COMMAND(runId);
  return (
    <>
      <p className="text-xs font-medium text-foreground">{RUN_CLI.TITLE}</p>
      <p className="mt-0.5 mb-1.5 text-meta leading-relaxed text-muted-foreground">{RUN_CLI.NOTE}</p>
      <div className="rounded-lg border border-border bg-surface-2/60 px-3 py-2.5">
        <code className="break-all font-mono text-xs leading-relaxed">{command}</code>
      </div>
      <CopyButton
        text={command}
        label={RUN_SSH.COPY_CLI}
        className="mt-2 h-11 w-full justify-center gap-2 rounded-lg bg-primary px-3 text-sm font-semibold text-primary-foreground hover:bg-primary/95 sm:h-[34px]"
      >
        {RUN_SSH.COPY_CLI}
      </CopyButton>
      <p className="mt-2 text-meta leading-relaxed text-muted-foreground">
        {RUN_CLI.FIRST_TIME} <Mono className="text-foreground">{RUN_CLI.LOGIN(consoleURL)}</Mono>
      </p>
    </>
  );
}

/** The organisation's published CA fingerprints, one line each, labelled with the subject CN when given. */
function CaFingerprints({ items }: { items: OrgCAFingerprint[] }) {
  return (
    <div className="mt-2.5 border-t border-dashed border-border pt-2.5">
      <p className="text-xs font-medium text-foreground">{RUN_CLI.CA_TITLE}</p>
      <p className="mt-0.5 mb-1.5 text-meta leading-relaxed text-muted-foreground">{caMono(RUN_CLI.CA_BODY)}</p>
      <div className="space-y-1.5">
        {items.map((it, i) => (
          <div key={`${i}-${it.fingerprint}`}>
            {it.subject && <p className="mb-0.5 text-meta text-muted-foreground">{it.subject}</p>}
            <CmdRow text={it.fingerprint} label={RUN_CLI.COPY_FINGERPRINT} />
          </div>
        ))}
      </div>
    </div>
  );
}

/**
 * Get the CLI: the install line for the viewer's OS, the other OS folded, then
 * "Other ways to install" and, when the operator published them, the org CA
 * fingerprints. Without a release version there is no line to draw; the panel says
 * so (and, when /healthz never answered, offers Try again) rather than hiding.
 */
export function GetTheCli({
  version,
  artifactBase,
  fingerprints,
  healthAnswered,
  onRetry,
}: {
  version: string | undefined;
  artifactBase: string | undefined;
  fingerprints: OrgCAFingerprint[];
  healthAnswered: boolean;
  onRetry: () => void;
}) {
  const lines = installLines(version, artifactBase);
  const windows = viewerIsWindows();
  const unix = lines && { label: RUN_CLI.UNIX, text: lines.sh, copy: RUN_CLI.COPY_INSTALL_UNIX };
  const win = lines && { label: RUN_CLI.WINDOWS, text: lines.ps, copy: RUN_CLI.COPY_INSTALL_WINDOWS };
  const [lead, other] = windows ? [win, unix] : [unix, win];
  return (
    <div className="mt-4 border-t border-border pt-3">
      <p className="text-xs font-medium text-foreground">{RUN_CLI.GET_TITLE}</p>
      {lines && lead && other ? (
        <>
          <p className="mt-0.5 text-meta leading-relaxed text-muted-foreground">{RUN_CLI.VERSION(lines.version)}</p>
          <p className="mt-0.5 text-meta leading-relaxed text-muted-foreground sm:hidden">{RUN_CLI.PHONE_LINE}</p>
          <p className="mt-2 mb-1 text-meta text-muted-foreground">{lead.label}</p>
          <CmdRow text={lead.text} label={lead.copy} />
          <details className="group mt-2">
            <summary className="flex cursor-pointer items-center gap-2 text-meta text-foreground">
              <span className="flex-1">{RUN_CLI.OTHER_OS}</span>
              <span className="text-xs text-muted-foreground group-open:hidden">{RUN_CLI.SHOW}</span>
              <span className="hidden text-xs text-muted-foreground group-open:inline">{RUN_CLI.HIDE}</span>
            </summary>
            <p className="mt-2 mb-1 text-meta text-muted-foreground">{other.label}</p>
            <CmdRow text={other.text} label={other.copy} />
          </details>
          {lines.mirror && <p className="mt-1.5 text-meta leading-relaxed text-muted-foreground">{RUN_CLI.MIRROR_NOTE}</p>}
        </>
      ) : (
        <div className="mt-0.5 flex flex-wrap items-center gap-2">
          <p className="text-meta leading-relaxed text-muted-foreground">{RUN_CLI.NO_INSTALL_LINE}</p>
          {!healthAnswered && (
            <button
              type="button"
              onClick={onRetry}
              className="rounded-md border border-border px-2.5 py-1 text-xs text-foreground hover:bg-surface-2"
            >
              {RUN_CLI.TRY_AGAIN}
            </button>
          )}
        </div>
      )}
      <p className="mt-2 text-xs">
        <a
          href={INSTALL_DOCS_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="font-medium text-primary hover:underline"
        >
          {RUN_CLI.OTHER_WAYS}
        </a>
      </p>
      <p className="mt-0.5 text-meta leading-relaxed text-muted-foreground">{RUN_CLI.OTHER_WAYS_BODY}</p>
      {fingerprints.length > 0 && <CaFingerprints items={fingerprints} />}
    </div>
  );
}
