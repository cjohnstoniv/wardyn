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
