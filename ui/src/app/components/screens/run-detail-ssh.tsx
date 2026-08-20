/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Attach from your terminal" — the two ways into a running sandbox that are
// not this browser tab.
//
// This card used to be SSH-only and returned NULL whenever the deployment had
// the gateway off (/healthz's `ssh` absent). That is the DEFAULT for the
// compose stack, so most operators saw a live terminal in the browser with
// nothing anywhere saying a real terminal could reach the same session — which
// is exactly what was reported.
//
// `wardyn attach <run-id>` needs no gateway and no operator configuration: it
// dials the same WebSocket this page's terminal uses, lands in the SAME tmux
// session, and works on every deployment. Verified against a live run — a file
// written from one attach was read back by a second, and by the browser
// terminal. So the CLI lane is always shown, and SSH is the second lane: rich
// when enabled, one honest line about what turns it on when not.
//
// Still owner-only, matching the server rather than merely hiding a control it
// would refuse (sshgateway.go's sshAuth; attach.go's own owner-or-admin gate),
// and still absent when the run isn't RUNNING — there is nothing to attach to.
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
        // fix: a failed fetch used to set keys=[] — identical to a
        // confirmed-empty response — which rendered "no key is registered"
        // even for an owner who does have keys, on a transient error. Leave
        // it null (not-yet-loaded) so hasKeys keeps assuming keys exist
        // instead of asserting a fact the fetch never confirmed.
      });
    return () => {
      alive = false;
    };
  }, [owned, running]);

  if (!owned || !running) return null;

  const [host, port] = splitHostPort(ssh?.advertise_addr ?? "");
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
  const sshOn = !!ssh?.enabled;

  // The console already knows the address it is served from, so the env line is
  // this deployment's real URL rather than a placeholder the operator has to
  // translate. Omitted when it matches the CLI's own default.
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const cliEnv = origin && origin !== "http://localhost:8080" ? `WARDYN_URL=${origin} ` : "";
  const cliCommand = `${cliEnv}wardyn attach ${run.id}`;

  return (
    <SectionCard title="Attach from your terminal" Icon={KeyRound}>
      <p className="mb-2 text-[0.7813rem] leading-relaxed text-muted-foreground">
        The terminal on this page is one attachment to a persistent session. These land in the{" "}
        <span className="text-foreground">same session</span> — what you type in one shows up in the other, and
        detaching never ends the run.
      </p>

      <p className="text-[0.75rem] font-medium text-foreground">Wardyn CLI</p>
      <p className="mt-0.5 mb-1.5 text-[0.7188rem] leading-relaxed text-muted-foreground">
        Works on every deployment — no gateway to enable, no key to register. Needs your admin token in{" "}
        <Mono className="text-foreground">WARDYN_ADMIN_TOKEN</Mono>. Ctrl-C or closing the session detaches.
      </p>
      <CodeBlock text={cliCommand} />
      <p className="mt-1.5 text-[0.7188rem] leading-relaxed text-muted-foreground">
        Started with <Mono className="text-foreground">make setup</Mono>? The binary is at{" "}
        <Mono className="text-foreground">./bin/wardyn</Mono> in the repo.
      </p>

      <div className="mt-4 border-t border-border pt-3">
        <p className="text-[0.75rem] font-medium text-foreground">SSH</p>
        {!sshOn && (
          <p className="mt-0.5 text-[0.7188rem] leading-relaxed text-muted-foreground">
            Off on this deployment. It gives you <Mono className="text-foreground">ssh</Mono>, scp and VS Code
            Remote-SSH straight into the sandbox, keyed to the SSH keys in Settings. An operator turns it on by
            setting <Mono className="text-foreground">WARDYN_SSH_LISTEN</Mono> and{" "}
            <Mono className="text-foreground">WARDYN_SSH_ADVERTISE</Mono> where wardynd starts.
          </p>
        )}
      </div>

      {sshOn && !hasKeys && (
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

      {sshOn && (
      <div className={cn("mt-2", !hasKeys && "opacity-50")}>
        <CodeBlock text={command} />
        <p className="mt-1.5 text-[0.7188rem] leading-relaxed text-muted-foreground">
          Or skip retyping it: <Mono className="text-foreground">wardyn ssh {run.id}</Mono>
        </p>

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
      )}

      {sshOn && (
        <Link to="/ssh-keys" className="mt-3 inline-block text-[0.7813rem] font-medium text-primary hover:underline">
          Manage SSH keys
        </Link>
      )}
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
