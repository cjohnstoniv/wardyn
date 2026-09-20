/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Extracted from harness-login-pane.tsx: the per-provider login
// table is pure data plus one presentational component — nothing here reads
// or writes the pane's own state. Moved out so the pane, which now carries
// three lanes' worth of logic (0.7.5's P5 wait, 0.7.6's starting-detail
// reason, Finding 7a's tab and Finding 7b's watch), stays under the size cap.
// Re-exported from the pane (`export { loginFlow, ExpectList, ... }`) so
// every existing importer — agents-tab.tsx, this pane's own tests — keeps its
// import path.
import * as React from "react";
import { AWS_BLURB_MANAGED_OPENING } from "./login-pane-copy";

// Per-provider login conventions. Adding a provider is a new row here (mirrors
// the server-side agentHarnessLogin table), not a forked component.
//
// The two flows differ in how the credential comes back:
//   · anthropic — `claude setup-token` prints the token, so we scrape it off the
//     PTY and PUT it (capture: "scrape").
//   · aws — `aws sso login` writes its token to ~/.aws/sso/cache/*.json and
//     prints only a short-lived device code + verification URL. The in-sandbox
//     `wardyn-aws-sso` helper uploads the file through the brokered internal
//     endpoint, so the pane never sees (and must never scrape) a credential —
//     it just watches for the helper's success marker (capture: "helper").
export type CaptureMode = "scrape" | "helper";
export type LoginFlow = {
  cmd: string;
  title: string;
  // U-8: taken as a function of `startURLManaged` because the aws flow's opening
  // clause is false under a managed row (there is no field, and the server
  // ignores a supplied URL). Every other flow ignores the argument.
  blurb: (startURLManaged: boolean) => React.ReactNode;
  capture: CaptureMode;
  // What the "done" phase's success line names as connected — provider-specific
  // so an AWS SSO capture never claims a Claude subscription (or vice versa).
  doneLabel: string;
  // Marker the in-sandbox helper prints on success (capture: "helper" only).
  doneMarker?: string;
  // Marker the in-sandbox helper prints on a refused capture (capture: "helper"
  // only). Unused for now — cmd/wardyn-aws-sso's TestFailMarker_UIParity reads
  // this literal by source parse, the same way TestSuccessMarker_UIParity reads
  // doneMarker above, so it stays byte-identical across the two languages.
  failMarker?: string;
  // The flow cannot start until the operator supplies their AWS access-portal
  // start URL: `aws sso login` reads sso_start_url + sso_region from the
  // sandbox's ~/.aws/config, and Wardyn stores no start URL anywhere (the region
  // is boot config; the start URL is per-organization and asked for here). The
  // server seeds both into the sandbox before the command is auto-typed.
  needsStartUrl?: boolean;
  // "What happens next" — shown before anything launches (the intro phase, or
  // above the AWS start-URL form), so the terminal and the browser auth prompt
  // arrive announced. Includes what is required of the operator.
  expects: React.ReactNode[];
};

export const LOGIN_FLOWS: Record<string, LoginFlow> = {
  anthropic: {
    cmd: "claude setup-token",
    title: "Connect a Claude subscription via container login",
    capture: "scrape",
    doneLabel: "your Claude subscription is connected",
    expects: [
      <>
        A sandboxed login run starts and a terminal appears here, running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">claude setup-token</code>. Nothing on this
        machine is touched.
      </>,
      <>
        A new tab opens on claude.ai asking you to sign in and approve — you&apos;ll need an active Claude
        subscription. If the pop-up is blocked, a click-through link appears here instead.
      </>,
      <>Some logins hand you a code: paste it into the field under the terminal, not the terminal itself.</>,
      <>
        The token it prints is captured, stored write-only, and the login sandbox is shut down. Runs get it injected
        proxy-side — a run&apos;s sandbox never holds it.
      </>,
    ],
    blurb: () => (
      <>
        Wardyn opened a sandbox and is running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">claude setup-token</code> for you. It opens the
        Claude login page in a new tab — approve it, then paste the code it gives you into the{" "}
        <span className="font-medium">field below</span> (not the terminal) and hit Send. Wardyn captures the printed
        token automatically and connects your subscription; the token is injected proxy-side into every run and the
        sandbox never holds a live credential.
      </>
    ),
  },
  aws: {
    // --sso-session wardyn selects the [sso-session wardyn] block the server
    // seeded into ~/.aws/config; chained so the helper uploads the moment the
    // login succeeds — the operator never has to run a second command.
    cmd: "aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso",
    title: "Connect an AWS SSO session via container login",
    doneLabel: "your AWS SSO session is connected",
    capture: "helper",
    doneMarker: "wardyn: aws sso credential captured",
    failMarker: "wardyn: aws sso credential rejected:",
    needsStartUrl: true,
    expects: [
      <>
        A sandboxed login run starts and a terminal appears here, running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">aws sso login</code> — with no credential to
        start from.
      </>,
      <>
        A browser tab opens the AWS verification page: enter the short code the terminal shows and approve with your
        IAM Identity Center login.
      </>,
      <>
        The SSO session is uploaded from inside the sandbox and stored write-only; Bedrock runs exchange it for
        short-lived role credentials.
      </>,
    ],
    blurb: (startURLManaged: boolean) => (
      <>
        {startURLManaged ? `${AWS_BLURB_MANAGED_OPENING} Wardyn` : "Give Wardyn your organization’s AWS access portal URL and it"}{" "}
        opens a sandbox, writes a minimal{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">~/.aws/config</code> holding just that URL and
        the configured SSO region (no credential — the sandbox has none to start with), and runs{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">aws sso login</code> for you. It prints a
        verification URL and a short user code — open the link in any browser, enter the code, and approve. Wardyn then
        captures the SSO session automatically so later Bedrock runs can exchange it for short-lived role credentials —
        with no host <code className="rounded bg-background/70 px-1 py-0.5 font-mono">~/.aws</code> mount and no static
        keys.
      </>
    ),
  },
};

export function loginFlow(provider: string): LoginFlow {
  return LOGIN_FLOWS[provider] ?? LOGIN_FLOWS.anthropic;
}

// The numbered "what happens next" — the consent gate's content. Each flow
// states its own steps and what is required of the operator.
export function ExpectList({ items }: { items: React.ReactNode[] }) {
  return (
    <ol className="space-y-1.5">
      {items.map((item, i) => (
        <li key={i} className="flex gap-2 text-xs leading-relaxed text-muted-foreground">
          <span className="mt-px inline-flex size-4 shrink-0 items-center justify-center rounded-full border border-border font-mono text-meta text-foreground">
            {i + 1}
          </span>
          <span className="min-w-0">{item}</span>
        </li>
      ))}
    </ol>
  );
}
