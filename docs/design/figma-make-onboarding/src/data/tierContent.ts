import type { TierId } from "./setupFixtures";

// Barrier tier copy (brief §6.1). Honesty rule: every tier carries its "Doesn't stop:" residual
// risk — never dropped, never more than one interaction away. Copy may move, never soften.
export interface TierCopy {
  pickWhen: string;
  doesntStop: string;
  metaphor: string;
  mechanism: string;
}

export const TIER_COPY: Record<TierId, TierCopy> = {
  fence: {
    pickWhen:
      "Trying Wardyn out, or the code is your own — quickest start.",
    doesntStop:
      "A kernel exploit or container escape — it shares your machine's kernel (the 'holes' in the fence).",
    metaphor: "A fence has holes.",
    mechanism:
      "Shared-kernel container (runc) hardened with user namespaces, seccomp, AppArmor, and capability drop.",
  },
  wall: {
    pickWhen:
      "Real work on real repos — closes the Fence's holes so the agent never touches your kernel.",
    doesntStop:
      "A flaw in the sandbox software itself (rare); it's still not a fully separate machine.",
    metaphor: "A wall closes the holes, but over/under still exists.",
    mechanism: "gVisor userspace kernel intercepts every syscall.",
  },
  vault: {
    pickWhen:
      "Untrusted code or secrets nearby — the strongest box Wardyn can build.",
    doesntStop:
      "A flaw in the virtualization layer itself — a very rare hypervisor- or CPU-level escape.",
    metaphor: "A vault covers every side.",
    mechanism:
      "Kata microVM — hardware-virtualized, own guest kernel (requires /dev/kvm).",
  },
};

// The one constant note — keep verbatim, exactly one placement near the tier picker (brief §4.2).
export const CONSTANT_NOTE =
  "Whatever the barrier, every run still gets Wardyn's egress filtering, short-lived brokered credentials, human approvals, and full audit — those are set by policy, not the barrier. The barrier only decides how strongly the sandbox is walled off from your machine.";

// Protection comparison matrix rows (brief §6.1). "caveat" = qualified yes.
export interface MatrixRow {
  label: string;
  fence: "yes" | "no" | "caveat";
  wall: "yes" | "no" | "caveat";
  vault: "yes" | "no" | "caveat";
}

export const MATRIX_ROWS: MatrixRow[] = [
  {
    label: "Isolated from your files, processes, and network",
    fence: "yes",
    wall: "yes",
    vault: "yes",
  },
  {
    label: "A kernel exploit can't reach your host kernel",
    fence: "no",
    wall: "caveat",
    vault: "yes",
  },
  {
    label: "A full break-in stays sealed inside",
    fence: "no",
    wall: "caveat",
    vault: "caveat",
  },
];

export const MATRIX_WHERE: Record<TierId, string> = {
  fence: "Any host",
  wall: "Any Docker host",
  vault: "Needs KVM hardware",
};
