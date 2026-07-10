import { Fingerprint, ShieldHalf, KeyRound, Hand, ScrollText, ChevronRight, type LucideIcon } from "lucide-react";
import { cn } from "../ui/utils";

// The 5-node how-it-works pipeline (brief §6.0 / §7.3), reused in Welcome and the intro panel.
// Node tones follow the semantic palette: the barrier is teal (primary), gating is amber.
type NodeTone = "neutral" | "primary" | "warning";

interface PipeNode {
  icon: LucideIcon;
  title: string;
  qualifier: string;
  tone: NodeTone;
}

const NODES: PipeNode[] = [
  {
    icon: Fingerprint,
    title: "Own identity",
    qualifier: "Every run, cryptographically scoped",
    tone: "neutral",
  },
  {
    icon: ShieldHalf,
    title: "Behind a barrier",
    qualifier: "Fence, Wall, or Vault — you choose",
    tone: "primary",
  },
  {
    icon: KeyRound,
    title: "Keys stay brokered",
    qualifier: "Short-lived tokens, never your real keys",
    tone: "neutral",
  },
  {
    icon: Hand,
    title: "You gate the risky bits",
    qualifier: "Egress and writes ask first",
    tone: "warning",
  },
  {
    icon: ScrollText,
    title: "Everything recorded",
    qualifier: "Append-only audit; session replay where the runner supports it",
    tone: "neutral",
  },
];

const TONE: Record<NodeTone, { ring: string; iconWrap: string }> = {
  neutral: { ring: "border-border", iconWrap: "bg-muted text-foreground" },
  primary: { ring: "border-primary/40", iconWrap: "bg-primary/15 text-primary" },
  warning: { ring: "border-warning/40", iconWrap: "bg-warning-subtle text-warning" },
};

export function PipelineDiagram({ className }: { className?: string }) {
  return (
    <ol
      className={cn(
        "grid grid-cols-1 gap-2 sm:grid-cols-2 lg:flex lg:items-stretch",
        className,
      )}
      aria-label="How Wardyn protects each run"
    >
      {NODES.map((node, i) => {
        const Icon = node.icon;
        const tone = TONE[node.tone];
        return (
          <li key={node.title} className="flex items-center gap-2 lg:flex-1">
            <div className={cn("flex-1 rounded-xl border bg-card p-3", tone.ring)}>
              <div className={cn("mb-2 inline-flex size-8 items-center justify-center rounded-lg", tone.iconWrap)}>
                <Icon className="size-4" aria-hidden />
              </div>
              <div className="text-sm text-foreground">{node.title}</div>
              <div className="mt-0.5 text-xs text-muted-foreground">{node.qualifier}</div>
            </div>
            {i < NODES.length - 1 && (
              <ChevronRight className="hidden size-4 shrink-0 text-muted-foreground lg:block" aria-hidden />
            )}
          </li>
        );
      })}
    </ol>
  );
}
