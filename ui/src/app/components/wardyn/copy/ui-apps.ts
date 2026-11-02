/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// UI apps lane (docs/design/ui-sandboxes-prompt.md §7, FROZEN) — the run-detail
// "Attach from your terminal" card's third lane and the policies screen's
// read-only ui_apps row. Every byte here is canon; run-detail-ssh.tsx and
// run-detail-ssh.test.tsx must render/assert these verbatim, never a paraphrase.
export const UI_APPS_LANE = {
  title: "UI apps",
  intro:
    "Wardyn relays a port the sandbox is already listening on to your browser. The sandbox gets no network of its own — the relay rides the same exec lane the terminal does.",
  appSub: (port: number, path: string) => `localhost:${port}${path}`,
  cta: (app: string) => `Open ${app}`,
  ctaBusy: "Opening…",
  newTab:
    "Opens in a new tab, on a different address than this console. That separation is deliberate: the app is the sandbox's own code, and it must never be able to read your console session.",
  noRecording:
    "Session recording does not capture this: no keystrokes, no screen, no page content. Wardyn records that you opened and closed the app, never what you did in it.",
  off: "Off on this deployment. It relays a declared loopback port inside the sandbox — a code editor, a dev server — to your browser through Wardyn. An operator turns it on by setting WARDYN_UI_SANDBOX_LISTEN where wardynd starts.",
  // The off-state's one affordance (mock M6): a pointer to the page that says
  // how to turn it on, next to the need rather than in a footer (§9). NOT an
  // <a href> — the console does not serve docs/, so a real link would 404;
  // the repo's pattern is to name the file, as policy-panel.tsx does for
  // docs/POLICIES.md. No button, either: a viewer cannot flip a server env var,
  // and offering one would be a lie about who can act.
  offDoc: "Read how to enable UI sandboxes",
  offDocPath: "docs/UI-SANDBOXES.md",
  noApps:
    "On for this deployment, but this run's policy declares no UI apps. The relay serves only ports named in the policy's ui_apps list — an app is a name, a loopback port and a path.",
  errorTitle: (app: string) => `Couldn't start ${app}`,
  errorLauncher: (app: string) =>
    `This image has no /usr/local/bin/wardyn-ui-${app}. Use an image that ships the launcher (deploy/images/vscode/), or add one to your own image.`,
} as const;

// Prefix of the server's verbatim missing-launcher body (docs/design/ui-
// sandboxes-prompt.md §7's "server-side counterpart"), used to decide whether
// to prepend the friendly lane.error.launcher guidance above the raw text.
export const UI_APPS_LAUNCHER_MISSING_PREFIX = "no UI launcher in this image:";

// Policies screen's read-only ui_apps detail row (same frozen table, §7).
export const POLICY_UI_APPS = {
  label: "UI apps",
  none: "None declared",
  value: (app: string, port: number, path: string) => `${app} → localhost:${port}${path}`,
} as const;


// An app the run declares inbound (ui_apps[].inbound), on the run page and at the /open link people are given.
// Byte for byte from the approved mock (r16 T2-b); ui-apps.test.ts freezes them.
export const INBOUND_APP = {
  TAG: "Inbound",
  CALLERS: "Callers:",
  ONLY_YOU: "Only you",
  COPY_LINK: "Copy link for people",
  COPY_ADDRESS: "Copy address for API clients",
  PATH_MODE_NOTE:
    "This deployment serves apps from one shared host, so people other than you can't open this in a browser. API clients can still call it.",
} as const;

// The /open/{run}/{app} page: Opening, then one sentence for each way it is refused. The shared-host sentence is
// the server's (inboundNeedsHostModeMsg); the page never says whether a run exists.
export const OPEN_APP = {
  OPENING: (app: string) => `Opening ${app}…`,
  NOT_SHARED: "This app isn't shared with you, or the run no longer exists.",
  NOT_RUNNING: "This run isn't running.",
  SHARED_HOST:
    "This deployment serves apps from one shared host, so only the run's owner can open them in a browser. Ask an administrator to set per-run hosts.",
  GATEWAY_OFF: "The app gateway is turned off on this deployment.",
} as const;
