import { RotateCw, Loader2, Server } from "lucide-react";
import { Button } from "../ui/button";
import { cn } from "../ui/utils";

// One status bar to rule the re-checks (brief §7.4): a single persistent host-status strip
// with an aria-live in-flight state, replacing per-step re-check buttons. Controlled by the
// shared setup-status store so saves elsewhere surface their "Checking…" state here too.
export function HostStatusBar({
  checking,
  lastCheckedLabel,
  onRecheck,
  className,
}: {
  checking: boolean;
  lastCheckedLabel: string;
  onRecheck: () => void;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex items-center justify-between gap-3 rounded-lg border bg-muted/50 px-3 py-2",
        className,
      )}
    >
      <div className="flex items-center gap-2 text-sm text-muted-foreground" aria-live="polite">
        <Server className="size-4 shrink-0" aria-hidden />
        {checking ? (
          <span className="inline-flex items-center gap-1.5 text-info">
            <Loader2 className="size-3.5 animate-spin" aria-hidden />
            Checking Wardyn's setup…
          </span>
        ) : (
          <span>
            Host status · last checked{" "}
            <span className="text-foreground">{lastCheckedLabel}</span>
          </span>
        )}
      </div>
      <Button variant="outline" size="sm" onClick={onRecheck} disabled={checking}>
        <RotateCw className={cn("size-3.5", checking && "animate-spin")} aria-hidden />
        Re-check
      </Button>
    </div>
  );
}
