/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupGuide — per-tier runtime-install guidance: the single `wardyn setup
// <tier>` command EnvironmentStep renders inline for a todo confinement
// class. PROVIDER_GUIDES (CLI-login guides) and the dialog that rendered them
// (SetupGuideDialog) were deleted with the host-CLI login flow they
// described — the container-login range replaced "log in to the Claude CLI
// on the host" with the managed in-container login, and the guide that told
// operators to do the retired thing went with it. TIER_GUIDES is the only
// surviving reader-facing export; keep this file scoped to it so a future
// host-run-this-command dialog doesn't get built on top of guides for a flow
// that no longer exists.
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

// W4-S1-5/W27-S1-4: the k8s runner substrate isn't a `docker info` host at
// all — there's no daemon.json to edit and no `wardyn setup wall/vault`
// command for wardynd to run against ITSELF. The actual lever is a
// cluster-registered RuntimeClass, pinned to a Confinement Class via Helm
// (deploy/helm/wardyn/README.md's k8s.runtimeClasses.CC2/.CC3) — a value only
// the cluster operator can set, so the "command" here is the `helm upgrade`
// invocation, not something Re-check can ever satisfy on its own.
export const K8S_TIER_GUIDES: Partial<Record<ConfinementClass, SetupGuide>> = {
  CC2: {
    title: "Enable the Wall tier",
    description:
      "Wall runs the agent inside gVisor — a userspace kernel that intercepts every syscall so nothing touches the node's kernel.",
    command: "helm upgrade --set k8s.runtimeClasses.CC2=<runtimeclass-name> ...",
    docNote:
      "Register a gVisor RuntimeClass in the cluster (its object name is operator-chosen — Wardyn can't guess it), then pin it here. Then Re-check.",
  },
  CC3: {
    title: "Enable the Vault tier",
    description:
      "Vault runs the agent in its own hardware-virtualized microVM with its own kernel — the strongest isolation.",
    command: "helm upgrade --set k8s.runtimeClasses.CC3=<runtimeclass-name> ...",
    docNote:
      "Register a Kata RuntimeClass on a KVM-capable node pool, then pin it here. Then Re-check.",
  },
};
