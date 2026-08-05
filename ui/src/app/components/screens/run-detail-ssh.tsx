/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Connect via SSH (prompt-v3-ssh-pane.md) — split out of run-detail.tsx
// (B4 file-size gate: the SSH-pane growth pushed it over the 1000-line cap)
// with zero behavior change. Owner-only, same as the gateway itself
// (sshgateway.go's sshAuth): a run's creator is the only human this card ever
// shows itself to, matching the server's own owner-only authorization rather
// than merely hiding a control the server would refuse anyway. Absent
// entirely — not disabled, not a placeholder — when the run isn't RUNNING,
// when the deployment has SSH off (/healthz's `ssh` field absent/disabled),
// or the run isn't the caller's own.
import * as React from "react";
import { Link } from "react-router-dom";
import { KeyRound } from "lucide-react";
import type { AgentRun, SSHPublicKey } from "../../lib/types";
import { health as healthApi } from "../../lib/api/health";
import { sshKeys as sshKeysApi } from "../../lib/api/ssh-keys";
import { Button } from "../ui/button";
import { CodeBlock, Mono } from "../wardyn/code-block";
import { usePrincipal } from "../wardyn/operator-context";
import { SectionCard } from "../wardyn/primitives";
import { cn } from "../ui/utils";

// Exported for run-detail-ssh.test.tsx: standalone-testable without mounting
// the whole screen's run/grants/egress/approvals/audit/recording fetch graph.
export function ConnectSSHCard({ run }: { run: AgentRun }) {
  const principal = usePrincipal();
  const owned = !!principal && run.created_by === principal;
  const running = run.state === "RUNNING";

  const [ssh, setSSH] = React.useState<{
    enabled?: boolean;
    advertise_addr?: string;
    host_key_fingerprint?: string;
  } | null>(null);
  // null = not loaded yet (treated as "assume keys exist" below, so a human
  // who HAS keys never sees the no-keys lead-in flash before the real answer
  // arrives).
  const [keys, setKeys] = React.useState<SSHPublicKey[] | null>(null);

  React.useEffect(() => {
    if (!owned || !running) return; // nothing to show either way — skip the fetch
    let alive = true;
    healthApi.health().then((h) => {
      if (alive) setSSH(h.ssh ?? null);
    });
    sshKeysApi
      .listKeys()
      .then((k) => {
        if (alive) setKeys(k);
      })
      .catch(() => {
        if (alive) setKeys([]);
      });
    return () => {
      alive = false;
    };
  }, [owned, running]);

  if (!owned || !running || !ssh?.enabled) return null;

  const [host, port] = splitHostPort(ssh.advertise_addr ?? "");
  const shortId = run.id.replace(/^run_/, "").slice(0, 8);
  const portFlag = port ? ` -p ${port}` : "";
  const command = `ssh ${run.id}@${host || "…"}${portFlag}`;
  const sshConfig = [
    `Host wardyn-${shortId}`,
    `  HostName ${host || "…"}`,
    `  Port ${port || "22"}`,
    `  User ${run.id}`,
  ].join("\n");
  const hasKeys = keys === null || keys.length > 0;

  return (
    <SectionCard title="Connect via SSH" Icon={KeyRound}>
      {!hasKeys && (
        <div className="mb-3 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
          <p className="text-[0.75rem] font-medium text-foreground">Add your SSH key first</p>
          <p className="mt-0.5 text-[0.7188rem] leading-relaxed text-muted-foreground">
            The command below is real, but no key is registered to connect with yet.
          </p>
          <Button asChild size="sm" className="mt-2">
            <Link to="/ssh-keys">
              <KeyRound className="size-3.5" /> Manage SSH keys
            </Link>
          </Button>
        </div>
      )}

      <div className={cn(!hasKeys && "opacity-50")}>
        <CodeBlock text={command} />

        <details className="mt-2.5">
          <summary className="cursor-pointer text-[0.75rem] text-muted-foreground hover:text-foreground">
            ssh config
          </summary>
          <CodeBlock text={sshConfig} className="mt-1.5" />
        </details>

        {ssh.host_key_fingerprint && (
          <div className="mt-2.5 text-[0.7188rem]">
            <Mono className="text-foreground">ED25519 {ssh.host_key_fingerprint}</Mono>
            <p className="mt-0.5 text-muted-foreground">Verify this against your client's prompt on first connect.</p>
          </div>
        )}

        <details className="mt-2.5">
          <summary className="cursor-pointer text-[0.75rem] text-muted-foreground hover:text-foreground">
            VS Code Remote-SSH
          </summary>
          <div className="mt-1.5 space-y-1.5 text-[0.7188rem] text-muted-foreground">
            <p>
              Remote-SSH → Connect to Host → <Mono className="text-foreground">wardyn-{shortId}</Mono>. One setting is
              required first, in VS Code's <Mono className="text-foreground">settings.json</Mono>:
            </p>
            <CodeBlock text={'"remote.SSH.localServerDownload": "always"'} />
            <p>
              Why: the sandbox has no internet; VS Code must upload its server through SSH instead of the remote
              host fetching it.
            </p>
          </div>
        </details>
      </div>

      <Link to="/ssh-keys" className="mt-3 inline-block text-[0.7813rem] font-medium text-primary hover:underline">
        Manage SSH keys
      </Link>
    </SectionCard>
  );
}

// splitHostPort divides an advertise_addr "host:port" (WARDYN_SSH_ADVERTISE)
// into its parts for the command/config rendering above. A bare host with no
// port renders with no `-p` flag / a conventional default Port 22 — honest
// either way, never a guessed port. Handles a bracketed IPv6 host+port
// ("[::1]:2222") and a BARE (unbracketed) IPv6 literal with no port
// ("::1", "2001:db8::1") — lastIndexOf(":") alone mangles both: the former
// needs the brackets stripped, not just the last colon split off, and the
// latter has no port to split at all (naive lastIndexOf would carve a
// fragment off the address itself).
export function splitHostPort(addr: string): [host: string, port: string] {
  const bracketed = addr.match(/^\[([^\]]+)\](?::(\d+))?$/);
  if (bracketed) return [bracketed[1], bracketed[2] ?? ""];
  // More than one colon, unbracketed: a bare IPv6 literal (always has 2+
  // colons), never a "host:port" pair (a hostname/IPv4 host has none).
  if ((addr.match(/:/g)?.length ?? 0) > 1) return [addr, ""];
  const i = addr.lastIndexOf(":");
  if (i < 0) return [addr, ""];
  return [addr.slice(0, i), addr.slice(i + 1)];
}
