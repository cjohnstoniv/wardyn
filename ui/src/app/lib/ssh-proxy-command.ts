/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The daemon's own rule for an advertised ssh ProxyCommand
// (internal/cliutil/ssh_proxy.go's CheckSSHProxyCommand), applied again to
// what /healthz delivers: the value is pasted inside single quotes into a
// shell one-liner and onto one ssh_config line, so a quote, a newline or any
// other control character would break out of either. The daemon refuses these
// at boot, so a failure here is a skewed daemon.
export const MAX_PROXY_COMMAND_BYTES = 512;

// Go's unicode.IsControl: the C0 and C1 controls (this covers \n and \r).
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;

export function proxyCommandIsSafe(v: string): boolean {
  return new TextEncoder().encode(v).length <= MAX_PROXY_COMMAND_BYTES && !CONTROL.test(v) && !v.includes("'");
}
