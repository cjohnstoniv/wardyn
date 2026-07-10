import { useState } from "react";
import { KeyRound, Plus, BookOpen, RefreshCw, LifeBuoy, Settings } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { StatusChip, type StatusKind } from "../StatusChip";
import { AddSecretDialog, type AddSecretRequest } from "../dialogs/AddSecretDialog";
import {
  hasConnectedModel,
  type ModelFamily,
  type ModelMechanism,
  type SetupStatus,
} from "../../../data/setupFixtures";
import {
  CLI_SPECS,
  SECRET_OPTIONS,
  BEDROCK_KEY_OPTIONS,
  isCli,
  isBedrockOption,
  dialogIdForOption,
  mechanismFor,
  type SetupDialogId,
} from "../../../data/setupDialogs";

export function ModelStep({
  status,
  secretExists,
  onOpenCliDialog,
  connectApiKey,
  onRefreshDetection,
}: {
  status: SetupStatus;
  secretExists: (name: string) => boolean;
  onOpenCliDialog: (id: SetupDialogId) => void;
  connectApiKey: (familyId: string, mech: ModelMechanism) => Promise<void>;
  onRefreshDetection: () => void;
}) {
  const connected = hasConnectedModel(status);
  const [secretReq, setSecretReq] = useState<AddSecretRequest | null>(null);

  return (
    <div className="space-y-5">
      <p className="text-sm text-muted-foreground">
        {connected
          ? "One connected path is enough — you're already covered."
          : "Connect a stored API key the proxy injects, or a resident CLI subscription."}
      </p>

      {/* Rescue box — sealed compose can't see the host's ~/.claude login */}
      {status.needsHostRescue && (
        <div className="rounded-xl border border-info/40 bg-info-subtle p-4">
          <div className="flex items-start gap-3">
            <LifeBuoy className="mt-0.5 size-5 text-info" aria-hidden />
            <div>
              <div className="text-sm text-foreground">
                Logged in on the host, but the sandbox can't see it.
              </div>
              <p className="mt-1 text-sm text-muted-foreground">
                A sealed compose deployment can't read your host's{" "}
                <span className="font-mono">~/.claude</span> login. Bridge it once:
              </p>
              <code className="mt-2 inline-block rounded-md border bg-background px-3 py-1.5 font-mono text-xs text-foreground">
                $ make setup-host
              </code>
            </div>
          </div>
        </div>
      )}

      {status.models.map((family) => (
        <FamilyCard
          key={family.id}
          family={family}
          secretExists={secretExists}
          onOpenCliDialog={onOpenCliDialog}
          onOpenSecretDialog={setSecretReq}
          connectApiKey={connectApiKey}
        />
      ))}

      <Button
        variant="link"
        size="sm"
        className="h-auto p-0"
        onClick={onRefreshDetection}
      >
        <RefreshCw className="size-3.5" aria-hidden />
        Refresh detection
      </Button>

      {/* AddSecretDialog — the one write-only value surface for this step */}
      <AddSecretDialog
        request={secretReq}
        secretExists={secretExists}
        onClose={() => setSecretReq(null)}
      />
    </div>
  );
}

function mechChip(mech: ModelMechanism): StatusKind {
  if (mech.state === "connected") return "connected";
  if (mech.state === "expired") return "expired";
  if (mech.state === "unverified") return "unverified";
  return "needs-setup";
}

function FamilyCard({
  family,
  secretExists,
  onOpenCliDialog,
  onOpenSecretDialog,
  connectApiKey,
}: {
  family: ModelFamily;
  secretExists: (name: string) => boolean;
  onOpenCliDialog: (id: SetupDialogId) => void;
  onOpenSecretDialog: (req: AddSecretRequest) => void;
  connectApiKey: (familyId: string, mech: ModelMechanism) => Promise<void>;
}) {
  const detected = family.mechanisms.filter((m) => m.detected);
  const bedrockDetected = detected.find((m) => m.id === "bedrock");

  function openSecretForOption(optLabel: string) {
    const opt = SECRET_OPTIONS[optLabel.trim().toLowerCase()];
    if (!opt) return;
    onOpenSecretDialog({
      prefillName: opt.prefillName,
      title: opt.title,
      subtitle: opt.subtitle,
      onSaved: (name) => {
        toast.loading("Re-checking…", { id: "recheck" });
        void connectApiKey(opt.familyId, mechanismFor(opt, name)).then(() =>
          toast.dismiss("recheck"),
        );
      },
    });
  }

  return (
    <section className="rounded-xl border bg-card p-4">
      <div className="mb-3 flex items-center justify-between gap-2">
        <span className="text-foreground">{family.label}</span>
        <StatusChip kind={family.connected ? "connected" : "needs-setup"} />
      </div>

      {detected.length > 0 && (
        <ul className="mb-3 space-y-2">
          {detected.map((mech) => (
            <li
              key={mech.id}
              className={cn(
                "flex flex-wrap items-start justify-between gap-2 rounded-lg border px-3 py-2.5",
                mech.state === "expired" && "border-danger/40 bg-danger-subtle",
              )}
            >
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <KeyRound className="size-3.5 text-muted-foreground" aria-hidden />
                  <span className="text-sm text-foreground">{mech.label}</span>
                </div>
                {mech.detail && (
                  <p className="mt-0.5 text-xs text-muted-foreground">{mech.detail}</p>
                )}
                {mech.mono && (
                  <p className="mt-0.5 font-mono text-xs text-muted-foreground">{mech.mono}</p>
                )}
              </div>
              <div className="flex shrink-0 items-center gap-2">
                <StatusChip kind={mechChip(mech)} />
                {mech.state === "expired" && (() => {
                  const id = dialogIdForOption(mech.label);
                  return id ? (
                    <Button variant="outline" size="sm" onClick={() => onOpenCliDialog(id)}>
                      Re-check login
                    </Button>
                  ) : null;
                })()}
              </div>
            </li>
          ))}
        </ul>
      )}

      {/* AWS Bedrock inline config (read-only — region/model are not secrets) */}
      {family.id === "claude" && (bedrockDetected || family.setupOptions.some(isBedrockOption)) && (
        <BedrockBlock
          config={family.bedrockConfig}
          detected={!!bedrockDetected}
          secretExists={secretExists}
          onOpenSecretDialog={onOpenSecretDialog}
          connectApiKey={connectApiKey}
          familyId={family.id}
        />
      )}

      {/* Collapsed set-up chips */}
      <div className="flex flex-wrap items-center gap-2 mt-2">
        <span className="text-xs text-muted-foreground">
          {detected.length > 0 ? "Add another way:" : "Set up:"}
        </span>
        {family.setupOptions
          .filter((opt) => !isBedrockOption(opt))
          .map((opt) => {
            const id = dialogIdForOption(opt);
            if (!id) return null;
            const cli = isCli(opt);
            return (
              <button
                key={opt}
                onClick={() => cli ? onOpenCliDialog(id) : openSecretForOption(opt)}
                className="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs text-foreground hover:bg-muted"
              >
                {cli ? (
                  <BookOpen className="size-3" aria-hidden />
                ) : (
                  <Plus className="size-3" aria-hidden />
                )}
                Set up {opt}
              </button>
            );
          })}
      </div>
    </section>
  );
}

function BedrockBlock({
  config,
  detected,
  secretExists,
  onOpenSecretDialog,
  connectApiKey,
  familyId,
}: {
  config?: { region: string; model: string };
  detected: boolean;
  secretExists: (name: string) => boolean;
  onOpenSecretDialog: (req: AddSecretRequest) => void;
  connectApiKey: (familyId: string, mech: ModelMechanism) => Promise<void>;
  familyId: string;
}) {
  const defaultRegion = config?.region ?? "us-east-1";
  const defaultModel = config?.model ?? "anthropic.claude-3-5-sonnet";
  const hasAccessKey = secretExists("aws-access-key-id");
  const hasSecretKey = secretExists("aws-secret-access-key");

  function openBedrockKey(opt: (typeof BEDROCK_KEY_OPTIONS)[number]) {
    onOpenSecretDialog({
      prefillName: opt.prefillName,
      title: opt.title,
      subtitle: opt.subtitle,
      onSaved: (name) => {
        // Once both keys exist, flip the Bedrock mechanism to connected.
        const otherName =
          opt.prefillName === "aws-access-key-id"
            ? "aws-secret-access-key"
            : "aws-access-key-id";
        const bothExist = secretExists(otherName) || name === otherName;
        if (bothExist) {
          void connectApiKey(familyId, {
            id: "bedrock",
            label: "AWS Bedrock",
            mono: `${defaultRegion} · ${defaultModel}`,
            state: "connected",
            detected: true,
          });
        }
      },
    });
  }

  if (detected) return null; // already shown in the detected row above

  return (
    <div className="mb-3 rounded-lg border bg-muted/30 p-3">
      <div className="mb-2 flex items-center gap-2 text-sm text-foreground">
        <Settings className="size-3.5 text-muted-foreground" aria-hidden />
        AWS Bedrock
      </div>
      <div className="mb-2 grid grid-cols-2 gap-2 font-mono text-xs text-muted-foreground">
        <span>Region: <span className="text-foreground">{defaultRegion}</span></span>
        <span>Model: <span className="text-foreground">{defaultModel}</span></span>
      </div>
      <div className="flex flex-wrap gap-2">
        {BEDROCK_KEY_OPTIONS.map((opt) => {
          const exists = secretExists(opt.prefillName);
          return (
            <button
              key={opt.prefillName}
              onClick={() => openBedrockKey(opt)}
              className={cn(
                "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs hover:bg-muted",
                exists
                  ? "border-success/30 bg-success-subtle text-success"
                  : "text-foreground",
              )}
            >
              {exists ? (
                <span className="font-mono">{opt.prefillName} ✓</span>
              ) : (
                <>
                  <Plus className="size-3" aria-hidden />
                  {opt.title}
                </>
              )}
            </button>
          );
        })}
      </div>
    </div>
  );
}
