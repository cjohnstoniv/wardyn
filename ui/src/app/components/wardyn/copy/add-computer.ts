/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Add this computer" (mock M-ATT-4, DESIGN-ATTACH 11.10 ATT-RCARD): the three steps on Admin -> Runners and in
// Your account -> My runner. Strings are the approved boards' (MATT4-AddComputer, -States, -Phone), character for
// character. LAZY like runners.ts: reached only through the block, never re-exported from the eager copy.ts barrel.

/**
 * Every command the block draws, in one table so a change to a command's form is one edit here (the CLI's own
 * forms are ATT-INSTALL's install lines, ATT-LOGIN-CLI's `wardyn login` and ATT-RUNNERD's `wardyn runner setup`).
 * The install lines come from lib/cli-install.ts, which reads the release version and the advertised mirror.
 */
export const ADD_COMPUTER_COMMANDS = {
  login: (url: string) => `wardyn login ${url}`,
  setup: "wardyn runner setup",
  /** The documented path for managed computers (DESIGN 11.1); the token file is delivered by device management. */
  mdm: (url: string) => `wardyn-runnerd setup --org ${url} --token-file <file>`,
} as const;

/** "Other ways to install" points at the ranked table of the install docs. */
export const INSTALL_DOCS_URL = "https://github.com/cjohnstoniv/wardyn/blob/main/docs/runner-registration.md#install-the-cli";

export const ADD_COMPUTER = {
  TITLE: "Add this computer",
  LEAD: "Run these three commands in a terminal on the computer you are adding. It becomes a runner for your own runs.",
  WSL2_NOTE: "Runners run in WSL2. Open your WSL2 distribution and run all three commands there.",
  STEP_INSTALL: "Install the CLI",
  UNIX: "macOS, Linux and WSL2",
  WINDOWS: "Windows (PowerShell)",
  OTHER_OS: "Another operating system",
  SHOW: "Show ▾",
  HIDE: "Hide ▴",
  OTHER_WAYS: "Other ways to install",
  STEP_SIGN_IN: "Sign in",
  SIGN_IN_NOTE: "A browser tab opens. Approve the sign-in there once.",
  NO_CLI_SIGN_IN:
    "This server does not offer CLI sign-in yet. Set WARDYN_TOKEN to a personal API token first; a personal token works for setup. A Wardyn admin can invite you to create one, and you claim it under Your account.",
  STEP_SETUP: "Set up the runner",
  SETUP_NOTE: "It checks this computer, registers and claims the runner, starts it, and waits up to 60 seconds for it to connect. Run it again to upgrade.",
  MDM_TITLE: "Managed install (MDM)",
  MDM_LEAD: "For computers set up by device management: create a registration token for a person and deliver it to their computer.",
  COPY: "Copy",

  // Not on the M-ATT-4 boards. The two install-panel lines are DESIGN-ATTACH 11.14 answer 3 (and ATT-CARD's board);
  // the Copy buttons' accessible names say what is copied, as CopyButton asks.
  NO_INSTALL_LINE: "This deployment didn't report its version, so there is no install line here. See Other ways to install.",
  TRY_AGAIN: "Try again",
  COPY_INSTALL_UNIX: "Copy install line for macOS, Linux and WSL2",
  COPY_INSTALL_WINDOWS: "Copy install line for Windows",
  COPY_SIGN_IN: "Copy sign-in command",
  COPY_SETUP: "Copy setup command",
} as const;
