/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupGuide — a reusable dialog for the "set up thru the UI" actions that the
// daemon can't perform itself (installing a confinement runtime, logging in a
// CLI). It shows a single copy-paste command the operator runs on the host with
// their own privileges, a manual-steps fallback, and a Re-check that re-probes
// /setup/status. (API-key setup stays one-click via AddSecretDialog; only the
// host-side actions route through here — wardynd holds no host privileges.)
import * as React from "react";
import { Check, Copy, Loader2, RotateCw } from "lucide-react";
import { Button } from "../../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import { Mono } from "../../wardyn/code-block";
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import type { ConfinementClass } from "../../../lib/types";

export interface SetupGuide {
  title: string;
  description?: string;
  /** The single command the operator runs on the host. */
  command: string;
  /** A short honest note under the command. */
  docNote?: string;
  /** Fallback manual steps, collapsed by default. */
  manualSteps?: string[];
}

// Per-tier runtime installs — a single `wardyn setup <tier>` the operator runs
// with their own sudo (the daemon never installs anything).
export const TIER_GUIDES: Partial<Record<ConfinementClass, SetupGuide>> = {
  CC2: {
    title: "Enable the Wall tier",
    description:
      "Wall runs the agent inside gVisor — a userspace kernel that intercepts every syscall so nothing touches your host kernel.",
    command: "wardyn setup wall",
    docNote:
      "This detects your Docker setup (native Docker, Docker Desktop, macOS/Colima, or WSL) and prints the exact one-time steps for your host; add `--run` to execute them with your own privileges — Wardyn's daemon never installs anything. Then click Re-check.",
    manualSteps: [
      "Native Docker (Linux): install gVisor's runsc — https://gvisor.dev/docs/user_guide/install/ — then `sudo runsc install` and reload Docker (`sudo systemctl reload docker`).",
      "Docker Desktop (any OS): its engine runs in a managed VM where a runsc runtime can't persist — run a native Docker Engine and point wardynd at it via DOCKER_HOST. `wardyn setup wall` prints the exact steps.",
      "macOS: use Colima (a VM you control) — `wardyn setup wall` prints the Colima steps.",
      "Re-check — Wall shows available once `docker info` lists the runsc runtime.",
    ],
  },
  CC3: {
    title: "Enable the Vault tier",
    description:
      "Vault runs the agent in its own hardware-virtualized microVM with its own kernel — the strongest isolation.",
    command: "wardyn setup vault",
    docNote:
      "Vault needs KVM-capable hardware (bare-metal or a nested-virt VM) — unavailable on macOS and Docker Desktop. `wardyn setup vault` prints the Kata steps and the KVM check to run; add `--run` to execute them (it won't edit your daemon.json). Then Re-check.",
    manualSteps: [
      "Confirm /dev/kvm exists (Vault is impossible without it).",
      "Install Kata Containers — https://github.com/kata-containers/kata-containers — and register a runtime named `kata` in /etc/docker/daemon.json.",
      "Restart Docker, then Re-check — Vault shows available once `docker info` lists a kata* runtime.",
    ],
  },
};

// CLI logins are interactive by nature — guided only.
export const PROVIDER_GUIDES: Record<string, SetupGuide> = {
  claude: {
    title: "Connect your Claude subscription",
    description:
      "Log in to the Claude CLI on the host so agents can use your Claude.ai subscription — no API key needed.",
    command: "claude login",
    docNote: "Opens an interactive login in your terminal. Then Re-check.",
  },
  codex: {
    title: "Install & connect Codex",
    description:
      "Codex isn't on this host's PATH yet. Install the Codex CLI, then log in so agents can use it — about two minutes.",
    command: "npm i -g @openai/codex && codex login",
    docNote:
      "Installs the Codex CLI globally, then opens an interactive login in your terminal. It runs with your own privileges; Wardyn's daemon never installs anything. Then click Re-check.",
    manualSteps: [
      "Install the Codex CLI (see https://github.com/openai/codex) so `codex` is on the wardynd host PATH.",
      "Run `codex login` to authenticate interactively.",
      "Re-check — Codex shows Ready once the host reports it installed and logged in.",
    ],
  },
};

export function SetupGuideDialog({
  guide,
  onClose,
  onRecheck,
  rechecking,
}: {
  guide: SetupGuide | null;
  onClose: () => void;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  // No auto-reset timer here — copied resets when the dialog is re-opened for
  // a different guide (below), not on a clock.
  const { copied, setCopied, copyAsync } = useCopyToClipboard(null);
  const [showManual, setShowManual] = React.useState(false);
  React.useEffect(() => {
    if (guide) {
      setCopied(false);
      setShowManual(false);
    }
  }, [guide, setCopied]);
  if (!guide) return null;

  const copy = () => void copyAsync(guide.command);

  return (
    <Dialog open={!!guide} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{guide.title}</DialogTitle>
          {guide.description && <DialogDescription>{guide.description}</DialogDescription>}
        </DialogHeader>

        <div className="space-y-4 py-1">
          <div className="space-y-2">
            <p className="text-sm text-muted-foreground">Run this on the Wardyn host:</p>
            <div className="flex items-center gap-2 rounded-lg border border-border bg-surface-2 px-3 py-2">
              <Mono className="min-w-0 flex-1 truncate text-sm text-foreground">{guide.command}</Mono>
              <Button size="sm" variant="ghost" onClick={copy} aria-label="Copy command">
                {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
              </Button>
            </div>
          </div>

          {guide.docNote && (
            <p className="text-xs leading-snug text-muted-foreground">{guide.docNote}</p>
          )}

          {guide.manualSteps && guide.manualSteps.length > 0 && (
            <div>
              <button
                onClick={() => setShowManual((v) => !v)}
                className="text-xs font-medium text-info underline-offset-2 hover:underline"
              >
                {showManual ? "Hide manual steps" : "Or set it up manually"}
              </button>
              {showManual && (
                <ol className="mt-2 list-decimal space-y-1.5 pl-5 text-xs leading-snug text-muted-foreground">
                  {guide.manualSteps.map((s, i) => (
                    <li key={i}>{s}</li>
                  ))}
                </ol>
              )}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
          <Button onClick={onRecheck} disabled={rechecking}>
            {rechecking ? <Loader2 className="size-4 animate-spin" /> : <RotateCw className="size-4" />}
            Re-check
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
