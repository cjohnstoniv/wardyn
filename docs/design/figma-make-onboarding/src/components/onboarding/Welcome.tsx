import { Shield, ArrowRight } from "lucide-react";
import { Button } from "../ui/button";
import { PipelineDiagram } from "../setup/PipelineDiagram";
import { StatusChip, type StatusKind } from "../setup/StatusChip";
import {
  hasConnectedModel,
  TIER_META,
  type SetupStatus,
} from "../../data/setupFixtures";

// One-time Welcome hero (brief §6.0). Honest, console-toned — no marketing imagery.
export function Welcome({
  status,
  onGetSetup,
  onSkip,
}: {
  status: SetupStatus;
  onGetSetup: () => void;
  onSkip: () => void;
}) {
  const ready = status.ready;
  // Prefer the saved/selected tier if it's ready; otherwise the strongest ready tier (Vault > Wall > Fence).
  const TIER_STRENGTH: Record<string, number> = { vault: 3, wall: 2, fence: 1 };
  const savedReady = status.barriers.find((b) => b.id === status.savedTier && b.state === "ready");
  const strongestReady = [...status.barriers]
    .filter((b) => b.state === "ready")
    .sort((a, b) => (TIER_STRENGTH[b.id] ?? 0) - (TIER_STRENGTH[a.id] ?? 0))[0];
  const displayBarrier = savedReady ?? strongestReady;
  const modelConnected = hasConnectedModel(status);
  const connectedModel = status.models.find((m) => m.connected);

  // Live readiness chips probed from the host — never fabricated.
  const barrierChip: { kind: StatusKind; label?: string } = displayBarrier
    ? { kind: "ready", label: `Barrier: ${TIER_META[displayBarrier.id].name} ready` }
    : { kind: "needs-setup", label: "Barrier: Needs setup" };
  const modelChip: { kind: StatusKind; label?: string } = modelConnected && connectedModel
    ? { kind: "connected", label: `Model: ${connectedModel.label.split(" /")[0]} connected` }
    : { kind: "needs-setup", label: "Model: Needs setup" };
  return (
    <div className="mx-auto w-full max-w-[780px] px-6 py-12">
      <div className="flex flex-col items-start gap-4">
        <div className="inline-flex size-11 items-center justify-center rounded-xl bg-primary/15 text-primary">
          <Shield className="size-6" aria-hidden />
        </div>
        <h1 style={{ fontSize: "2rem", lineHeight: 1.2 }}>Let agents work. Keep your keys.</h1>
        <p className="max-w-[640px] text-muted-foreground">
          Wardyn runs your coding agents behind a barrier, with{" "}
          <span className="text-foreground">
            no resident credentials by default and no privileged host access
          </span>
          . Every run gets its own identity; you gate the risky moments; everything is audited.
        </p>
      </div>

      <PipelineDiagram className="mt-8" />

      <div className="mt-8 rounded-xl border bg-muted/40 p-4">
        <div className="mb-3 text-sm text-muted-foreground">This host right now:</div>
        <div className="flex flex-wrap gap-2">
          <StatusChip kind={barrierChip.kind} label={barrierChip.label} />
          <StatusChip kind={modelChip.kind} label={modelChip.label} />
        </div>
      </div>

      <div className="mt-8 flex flex-wrap items-center gap-3">
        <Button size="lg" onClick={onGetSetup}>
          {ready ? "Finish setup" : "Get set up — about 2 minutes"}
          <ArrowRight className="size-4" aria-hidden />
        </Button>
        <Button size="lg" variant="outline" onClick={onSkip}>
          Skip to the console
        </Button>
      </div>

      <p className="mt-6 text-xs text-muted-foreground">
        Shown once — everything lives on under “Getting started” in the sidebar.
      </p>
    </div>
  );
}
