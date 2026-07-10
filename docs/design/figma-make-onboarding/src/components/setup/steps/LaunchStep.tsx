import { CheckCircle2, ExternalLink, Rocket } from "lucide-react";
import { Button } from "../../ui/button";
import { StatusChip } from "../StatusChip";
import { hasConnectedModel, TIER_META, type TierId } from "../../../data/setupFixtures";
import type { SetupStatus } from "../../../data/setupFixtures";

const TIER_STRENGTH: Record<string, number> = { vault: 3, wall: 2, fence: 1 };
const STRONGER: Partial<Record<TierId, TierId>> = { fence: "wall", wall: "vault" };

// Launch step. No composer: runs are manually configured — you review every grant.
// "Open Runs" navigates to the console Runs screen (not the New-run stub).
export function LaunchStep({
  status,
  onLaunch,
  onOpenRuns,
}: {
  status: SetupStatus;
  onLaunch: () => void;
  onOpenRuns: () => void;
}) {
  const modelConnected = hasConnectedModel(status);
  const barrierReady = status.barriers.some((b) => b.state === "ready");
  const canLaunch = barrierReady && modelConnected;

  // Prefer saved tier if ready; else strongest ready
  const savedReady = status.barriers.find(
    (b) => b.id === status.savedTier && b.state === "ready",
  );
  const strongestReady = [...status.barriers]
    .filter((b) => b.state === "ready")
    .sort((a, b) => (TIER_STRENGTH[b.id] ?? 0) - (TIER_STRENGTH[a.id] ?? 0))[0];
  const selectedBarrier = savedReady ?? strongestReady;

  // Suggest hardening only if a stronger compatible tier exists and isn't already selected.
  const hardenTo = selectedBarrier ? STRONGER[selectedBarrier.id as TierId] : undefined;
  const hardenBarrier = hardenTo
    ? status.barriers.find((b) => b.id === hardenTo)
    : undefined;

  return (
    <div className="space-y-6">
      {status.hasRuns && (
        <div className="flex items-center gap-2 rounded-xl border border-success/30 bg-success-subtle px-4 py-3 text-sm text-success">
          <CheckCircle2 className="size-5 shrink-0" aria-hidden />
          You've already launched a run on this control plane.
        </div>
      )}

      <p className="text-sm text-muted-foreground">
        Configure your first run — you review every grant before anything starts. Choose the
        workspace, barrier, and model; Wardyn shows you what the agent will be allowed to do
        before it does anything.
      </p>

      {/* Example run card — clearly stamped as not live config */}
      <section className="relative rounded-xl border bg-card p-4">
        <div className="absolute right-3 top-3 rounded border bg-muted px-2 py-0.5 text-xs font-mono text-muted-foreground">
          EXAMPLE · Not live config
        </div>
        <div className="mb-3 text-sm text-muted-foreground">
          Just to show the shape of a run — nothing is started here.
        </div>
        <dl className="space-y-2.5">
          <Row label="Task">Add a health check endpoint and a unit test for it.</Row>
          <Row label="Agent">Claude Code</Row>
          <Row label="Barrier">
            <span className="flex items-center gap-2">
              {selectedBarrier ? (
                <>
                  <StatusChip kind="ready" label={TIER_META[selectedBarrier.id].name} />
                  {hardenBarrier && (
                    <span className="text-xs text-muted-foreground">
                      ready now — harden to {TIER_META[hardenBarrier.id].name} when convenient
                    </span>
                  )}
                </>
              ) : (
                <StatusChip kind="needs-setup" label="No barrier ready" />
              )}
            </span>
          </Row>
          <Row label="Mode">
            Interactive{" "}
            <span className="text-muted-foreground">— you drive; it asks before it acts.</span>
          </Row>
        </dl>
      </section>

      <div className="flex flex-wrap gap-3">
        <Button disabled={!canLaunch} onClick={onLaunch}>
          <Rocket className="size-4" aria-hidden />
          Launch your first run
        </Button>
        <Button variant="outline" onClick={onOpenRuns}>
          <ExternalLink className="size-4" aria-hidden />
          Open Runs
        </Button>
      </div>

      {!canLaunch && (
        <p className="text-xs text-muted-foreground">
          Set up the essentials first — a barrier and a connected model are both required.
        </p>
      )}
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start gap-3">
      <dt className="w-16 shrink-0 text-xs text-muted-foreground pt-0.5">{label}</dt>
      <dd className="min-w-0 text-sm text-foreground">{children}</dd>
    </div>
  );
}
