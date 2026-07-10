import {
  CheckCircle2,
  AlertTriangle,
  XCircle,
  Loader2,
  Minus,
  HelpCircle,
  type LucideIcon,
} from "lucide-react";
import { cn } from "../ui/utils";

// Fixed status vocabulary (brief §4.2) — no synonyms. Never color-only: every chip pairs an
// icon with its label so state reads without relying on hue.
export type StatusKind =
  | "ready"
  | "needs-setup"
  | "unavailable"
  | "incompatible"
  | "checking"
  | "connected"
  | "unverified"
  | "optional"
  | "expired"
  | "danger";

const MAP: Record<
  StatusKind,
  { label: string; icon: LucideIcon; className: string; spin?: boolean }
> = {
  ready: {
    label: "Ready",
    icon: CheckCircle2,
    className: "text-success bg-success-subtle border-success/30",
  },
  connected: {
    label: "Connected",
    icon: CheckCircle2,
    className: "text-success bg-success-subtle border-success/30",
  },
  // Grants & warnings are amber, never green (brief §4.2).
  "needs-setup": {
    label: "Needs setup",
    icon: AlertTriangle,
    className: "text-warning bg-warning-subtle border-warning/30",
  },
  unavailable: {
    label: "Unavailable here",
    icon: XCircle,
    className: "text-muted-foreground bg-muted border-border",
  },
  incompatible: {
    label: "Incompatible here",
    icon: XCircle,
    className: "text-danger bg-danger-subtle border-danger/30",
  },
  expired: {
    label: "Expired",
    icon: XCircle,
    className: "text-danger bg-danger-subtle border-danger/30",
  },
  danger: {
    label: "Error",
    icon: XCircle,
    className: "text-danger bg-danger-subtle border-danger/30",
  },
  checking: {
    label: "Checking…",
    icon: Loader2,
    className: "text-info bg-info-subtle border-info/30",
    spin: true,
  },
  unverified: {
    label: "Unverified",
    icon: HelpCircle,
    className: "text-muted-foreground bg-muted border-border",
  },
  optional: {
    label: "Optional",
    icon: Minus,
    className: "text-muted-foreground bg-muted border-border",
  },
};

export function StatusChip({
  kind,
  label,
  className,
}: {
  kind: StatusKind;
  /** Override the default label (e.g. "Ready · 2 of 3 barriers"). */
  label?: string;
  className?: string;
}) {
  const meta = MAP[kind];
  const Icon = meta.icon;
  const isLive = kind === "checking";
  return (
    <span
      role="status"
      aria-live={isLive ? "polite" : undefined}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs whitespace-nowrap",
        meta.className,
        className,
      )}
    >
      <Icon className={cn("size-3.5", meta.spin && "animate-spin")} aria-hidden />
      {label ?? meta.label}
    </span>
  );
}
