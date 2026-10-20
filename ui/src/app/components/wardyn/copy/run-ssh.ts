/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The SSH lane of "Attach from your terminal" when /healthz publishes an
// ssh.proxy_command (088-mock S1, approved). The card's older inline strings
// stay in run-detail-ssh.tsx; only the proxy-command states live here. The
// ProxyCommand text, host, port and run id are data, never copy.
export const RUN_SSH = {
  PROXY_NOTE: "Your operator set a ProxyCommand. ssh runs it on your computer when you connect.",
  CONFIG_USE: (alias: string) => `Add this to ~/.ssh/config, then run ssh ${alias}`,
  ONE_LINE: "One-line form",
  CLI_PROXY: (id: string) => `From the CLI: wardyn run ssh --advertised-proxy ${id}`,
  CLI_PROXY_WHY: "Without the flag, the CLI prints the proxy command and stops.",
  PROXY_REFUSED:
    "This deployment published a proxy command that can't be shown safely, so the commands below may not reach the sandbox. Ask your operator.",
  COPY_CONFIG: "Copy ssh config",
  COPY_COMMAND: "Copy ssh command",
  COPY_CLI: "Copy CLI command",
} as const;

// The Wardyn CLI lane and "Get the CLI" of "Attach from your terminal" (M-ATT-1,
// approved 2026-10-20, ATT1-Card/States/Phone). The command, the run id, the
// URL, the version, the mirror and the fingerprints are data, never copy.
export const RUN_CLI = {
  TITLE: "Wardyn CLI",
  NOTE: "Works on every deployment — no gateway to enable, no key to register. Ctrl-C goes to the sandbox; close the window to detach.",
  COMMAND: (id: string) => `wardyn attach ${id}`,
  FIRST_TIME: "First time on this computer?",
  LOGIN: (url: string) => `wardyn login ${url}`,
  GET_TITLE: "Get the CLI",
  VERSION: (v: string) => `Version ${v}, the same as this deployment.`,
  PHONE_LINE: "Run this on the computer where you will type the command.",
  UNIX: "macOS, Linux and WSL2",
  WINDOWS: "Windows (PowerShell)",
  OTHER_OS: "Another operating system",
  SHOW: "Show ▾",
  HIDE: "Hide ▴",
  MIRROR_NOTE: "Your administrator set an internal mirror for the CLI; both lines use it.",
  OTHER_WAYS: "Other ways to install",
  OTHER_WAYS_BODY: "Managed Macs: the .pkg. Scripts and air-gapped hosts: the release files and VERIFY.md.",
  NO_INSTALL_LINE: "This deployment didn't report its version, so there is no install line here. See Other ways to install.",
  TRY_AGAIN: "Try again",
  CA_TITLE: "Your organisation's CA",
  CA_BODY: "If your network inspects TLS, add its CA with wardyn ca add <file> and check that the fingerprint it prints matches:",
  COPY: "Copy",
  COPY_INSTALL_UNIX: "Copy install line for macOS, Linux and WSL2",
  COPY_INSTALL_WINDOWS: "Copy install line for Windows",
  COPY_FINGERPRINT: "Copy CA fingerprint",
  SSH_TITLE: "SSH",
} as const;
