/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ② Base image — mock frames V2S2ScanA / V2S2Failed / V2S2Rec / V2S2Custom
// / V2S2CustomCred / V2S2Byo / V2S2NoModel / V2S2Partial (mockup/wardyn-workspaces.js's
// AddWorkspaceWizardV2, "---------- ② Base image ----------" section).
// Phase A: per-source scan progress. Phase B: what the image needs + four
// base-image cards + the resolved model/harness power source and its peek.
import * as React from "react";
import { AlertTriangle, Check, Loader2 } from "lucide-react";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Checkbox } from "../../ui/checkbox";
import { cn } from "../../ui/utils";
import { Field } from "../new-run/step-shell";
import { baseImagesApi } from "../../../lib/api/sources";
import type { BaseImageEntry } from "../../../lib/types";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { C, V2C } from "../../../lib/workspace-copy";
import { SOURCE_META } from "./step-sources";
import {
  credFlags,
  fmtElapsed,
  type BaseImageChoice,
  type BaseImageState,
  type SourceRow,
  type SourceScanState,
} from "./wizard-types";

// ============================ Phase A — per-source scan progress ============================
function scanRowLabel(row: SourceRow): string {
  if (row.type === "ephemeral") return "Ephemeral scratch directory";
  if (row.type === "local_dir") return row.path || "path…";
  return `${row.source || "source…"}${row.ref ? ` @${row.ref}` : ""}`;
}

function ScanRow({
  row,
  scan,
  onEditSource,
  onRescan,
}: {
  row: SourceRow;
  scan: SourceScanState | undefined;
  onEditSource: () => void;
  onRescan: () => void;
}) {
  const meta = SOURCE_META[row.type];
  const status = row.type === "ephemeral" ? "ephemeral" : (scan?.status ?? "pending");

  return (
    <div className="space-y-2 border-t border-border p-3 first:border-t-0" data-testid="scan-row">
      <div className="flex flex-wrap items-center gap-2">
        <meta.Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className={cn("text-xs text-foreground", row.type !== "ephemeral" && "font-mono")}>
          {scanRowLabel(row)}
        </span>
        <span className="ml-auto flex items-center gap-1.5 text-[0.6875rem] text-muted-foreground">
          {status === "ephemeral" && "nothing to scan"}
          {status === "pending" && "queued"}
          {status === "scanning" && (
            <>
              <Loader2 className="size-3.5 animate-spin" />
              <Mono>elapsed {fmtElapsed(scan?.startedAt ?? Date.now())}</Mono>
            </>
          )}
          {status === "done" && (
            <>
              <Check className="size-3.5 text-success" /> scanned{scan?.secs != null ? ` · ${scan.secs}s` : ""}
            </>
          )}
          {status === "failed" && <Chip tone="danger">failed</Chip>}
        </span>
      </div>
      {status === "scanning" && row.type === "repo" && (
        <p className="pl-6 text-[0.6875rem] leading-snug text-primary">
          Running as a real confined run — watch it under Runs ↗
        </p>
      )}
      {status === "failed" && (
        <div className="ml-6 space-y-2 rounded-lg border border-danger/40 bg-danger-subtle p-2.5">
          <div className="flex items-center gap-2 text-danger">
            <AlertTriangle className="size-3.5 shrink-0" />
            <span className="text-xs font-semibold">Scan failed</span>
          </div>
          <Mono className="text-xs leading-relaxed text-foreground">{scan?.error || "Scan failed."}</Mono>
          <div className="flex flex-wrap gap-2">
            <Button type="button" size="sm" variant="outline" onClick={onEditSource}>
              Edit source
            </Button>
            <Button type="button" size="sm" variant="outline" onClick={onRescan}>
              Rescan
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

export function ScanProgress({
  sources,
  scans,
  onEditSource,
  onRescan,
}: {
  sources: SourceRow[];
  scans: Record<string, SourceScanState>;
  onEditSource: (id: string) => void;
  onRescan: (id: string) => void;
}) {
  const anyScanning = sources.some((r) => r.type !== "ephemeral" && scans[r.id]?.status === "scanning");
  return (
    <div className="space-y-3">
      <div className="rounded-xl border border-border">
        {sources.map((row) => (
          <ScanRow
            key={row.id}
            row={row}
            scan={scans[row.id]}
            onEditSource={() => onEditSource(row.id)}
            onRescan={() => onRescan(row.id)}
          />
        ))}
      </div>
      {anyScanning && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SCAN_REPO}</p>}
    </div>
  );
}

// ============================ Phase B — credential-shaped-line detection ============================
// A mono multi-line editor for Dockerfile RUN/ENV/ARG steps. Flags a
// credential-shaped line per-line (amber gutter marker) and with one
// consolidated warning naming the line + detector kind — WARNS, never blocks;
// the value itself is never echoed anywhere (V2C.CRED_WARN).
export function BuildStepsEditor({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const flags = credFlags(value);
  const flaggedLines = new Set(flags.map((f) => f.line));
  const lines = value.split("\n");
  const lineHeight = 19;

  return (
    <div className="space-y-1.5">
      <p className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">
        Custom build steps (optional)
      </p>
      <div className="flex overflow-hidden rounded-lg border border-border bg-surface-2/40 focus-within:ring-2 focus-within:ring-ring">
        <div aria-hidden className="flex w-[22px] shrink-0 flex-col items-center py-2">
          {lines.map((_, i) => (
            <span
              key={i}
              style={{ height: lineHeight, lineHeight: `${lineHeight}px` }}
              className={cn(
                "font-mono text-[10px]",
                flaggedLines.has(i + 1) ? "text-warning" : "text-muted-foreground",
              )}
            >
              {flaggedLines.has(i + 1) ? "⚠" : "·"}
            </span>
          ))}
        </div>
        <Textarea
          aria-label="Custom build steps"
          value={value}
          spellCheck={false}
          placeholder={"RUN apt-get install -y protobuf-compiler\nENV GOFLAGS=-mod=vendor"}
          rows={Math.max(4, lines.length)}
          className="min-h-0 flex-1 resize-y rounded-none border-0 bg-transparent py-2 font-mono text-xs leading-[19px] shadow-none focus-visible:ring-0"
          onChange={(e) => onChange(e.target.value)}
        />
      </div>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">{V2C.STEPS_HELP}</p>
      {flags.length > 0 && (
        <div className="space-y-1 rounded-lg border border-warning/30 bg-warning-subtle p-2.5" data-testid="cred-warning">
          {flags.map((f, i) => (
            <p key={i} className="text-[0.6875rem] leading-snug text-warning">
              <strong>⚠ Line {f.line} looks like a credential ({f.kind}).</strong> {V2C.CRED_WARN}
            </p>
          ))}
        </div>
      )}
    </div>
  );
}

// The per-card inventory: what this image carries, as the same mono chips the
// needs card speaks in. Inventory may NAME a tool (claude-code is one tool
// among tools); only the requirements and run surfaces say what a tool is FOR
// — the tools-not-AI law this step now follows. Empty = say nothing (the BYO
// card: Wardyn doesn't inspect, so it makes no inventory claim).
function Carries({ tools }: { tools: string[] }) {
  if (tools.length === 0) return null;
  return (
    <div className="space-y-1.5">
      <p className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">Carries</p>
      <div className="flex flex-wrap items-center gap-1.5">
        {tools.map((t) => (
          <Chip key={t} tone="neutral" mono>
            {t}
          </Chip>
        ))}
      </div>
    </div>
  );
}

function ImageCard({
  id,
  selected,
  onSelect,
  children,
}: {
  // A fixed choice id, or "catalog-<uuid>" for a saved tier-2 entry.
  id: BaseImageChoice | `catalog-${string}`;
  selected: boolean;
  onSelect: () => void;
  children: React.ReactNode;
}) {
  return (
    <div
      role="radio"
      aria-checked={selected}
      tabIndex={0}
      data-testid={`image-card-${id}`}
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
      className={cn(
        "cursor-pointer space-y-2 rounded-lg border p-3 transition-colors",
        selected ? "border-primary bg-primary/10" : "border-border hover:border-border-strong",
      )}
    >
      {children}
    </div>
  );
}

// A card's disclosed body must not toggle the card's own selection when the
// operator clicks inside an input/checkbox/button it contains.
function stopPropagation(e: React.MouseEvent) {
  e.stopPropagation();
}

export function ImageCards({
  detectedChips,
  partial,
  harnessAvailable,
  state,
  onChange,
}: {
  detectedChips: string[];
  partial: boolean;
  harnessAvailable: boolean;
  state: BaseImageState;
  onChange: (patch: Partial<BaseImageState>) => void;
}) {
  const credCount = credFlags(state.buildSteps).length;
  // The tier-2 catalog: images already saved and shared across workspaces.
  // Best-effort — an empty/failed catalog just means no saved cards.
  const [catalog, setCatalog] = React.useState<BaseImageEntry[]>([]);
  React.useEffect(() => {
    let live = true;
    baseImagesApi
      .listBaseImages()
      .then((rows) => live && setCatalog(rows))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  return (
    <div role="radiogroup" aria-label="Base image" className="space-y-2">
      <ImageCard id="recommended" selected={state.choice === "recommended"} onSelect={() => onChange({ choice: "recommended" })}>
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-[0.8125rem] font-medium text-foreground">Recommended — built for this workspace</span>
          <Chip tone="primary">recommended</Chip>
          {partial && <Chip tone="warning">based on a partial scan</Chip>}
        </div>
        <Carries tools={[...detectedChips, ...(harnessAvailable ? ["claude-code"] : [])]} />
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">{V2C.REC_SUB}</p>
      </ImageCard>

      {catalog.map((entry) => (
        <ImageCard
          key={entry.id}
          id={`catalog-${entry.id}`}
          selected={state.choice === "catalog" && state.catalog?.id === entry.id}
          onSelect={() =>
            onChange({
              choice: "catalog",
              catalog: { id: entry.id, kind: entry.kind, name: entry.name, image: entry.image, steps: entry.steps },
            })
          }
        >
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-[0.8125rem] font-medium text-foreground">{entry.name}</span>
            <Chip tone="neutral">from your catalog</Chip>
          </div>
          <p className="font-mono text-[0.6875rem] text-muted-foreground">
            {entry.image}
            {entry.steps?.length ? ` · ${entry.steps.length} step${entry.steps.length === 1 ? "" : "s"}` : ""}
          </p>
        </ImageCard>
      ))}

      <ImageCard id="registry" selected={state.choice === "registry"} onSelect={() => onChange({ choice: "registry" })}>
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-[0.8125rem] font-medium text-foreground">A registry image that fits</span>
          {partial && <Chip tone="warning">based on a partial scan</Chip>}
        </div>
        <Carries tools={detectedChips} />
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          Official language base matching the detected stack.
        </p>
      </ImageCard>

      <ImageCard id="custom" selected={state.choice === "custom"} onSelect={() => onChange({ choice: "custom" })}>
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-[0.8125rem] font-medium text-foreground">Customize the build</span>
          {credCount > 0 && (
            <Chip tone="warning">
              {credCount} credential-shaped build step{credCount > 1 ? "s" : ""}
            </Chip>
          )}
        </div>
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">Starts from the recommended recipe.</p>
        {state.choice === "custom" && (
          <div onClick={stopPropagation} className="space-y-3 border-t border-border pt-3">
            <Field label="Base image" htmlFor="bi-custom-base">
              <Input
                id="bi-custom-base"
                className="font-mono"
                value={state.customBase}
                onChange={(e) => onChange({ customBase: e.target.value })}
              />
            </Field>
            <div className="space-y-1.5">
              <p className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">
                Tools &amp; features
              </p>
              <div className="grid grid-cols-1 gap-1.5 sm:grid-cols-2">
                {detectedChips.map((chip) => (
                  <label key={chip} className="flex items-center gap-2 text-xs text-foreground">
                    <Checkbox
                      checked={state.customTools[chip] ?? true}
                      onCheckedChange={(v) => onChange({ customTools: { ...state.customTools, [chip]: !!v } })}
                    />
                    {chip}
                  </label>
                ))}
                {harnessAvailable && (
                  <label className="flex items-center gap-2 text-xs text-foreground">
                    <Checkbox checked={state.harnessTool} onCheckedChange={(v) => onChange({ harnessTool: !!v })} />
                    Claude Code CLI
                  </label>
                )}
              </div>
            </div>
            {state.extraTools.length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {state.extraTools.map((t) => (
                  <Chip key={t} tone="neutral" mono>
                    {t}
                  </Chip>
                ))}
              </div>
            )}
            <div className="flex flex-wrap items-center gap-2">
              <Input
                className="h-8 w-56 font-mono text-xs"
                placeholder="Add a tool (e.g. protoc)"
                value={state.toolDraft}
                onChange={(e) => onChange({ toolDraft: e.target.value })}
              />
              <Button
                type="button"
                size="sm"
                variant="ghost"
                disabled={!state.toolDraft.trim()}
                onClick={() => onChange({ extraTools: [...state.extraTools, state.toolDraft.trim()], toolDraft: "" })}
              >
                Add
              </Button>
            </div>
            <BuildStepsEditor value={state.buildSteps} onChange={(v) => onChange({ buildSteps: v })} />
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{V2C.CUSTOM_SUB}</p>
          </div>
        )}
      </ImageCard>

      <ImageCard id="byo" selected={state.choice === "byo"} onSelect={() => onChange({ choice: "byo" })}>
        <span className="text-[0.8125rem] font-medium text-foreground">Bring your own image</span>
        {state.choice === "byo" ? (
          <div onClick={stopPropagation} className="space-y-2 border-t border-border pt-3">
            <Field label="Image ref" htmlFor="bi-byo-ref">
              <Input
                id="bi-byo-ref"
                className="font-mono"
                placeholder="ghcr.io/acme/dev:latest"
                value={state.byoRef}
                onChange={(e) => onChange({ byoRef: e.target.value })}
              />
            </Field>
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.IMG_NO_SCAN}</p>
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{V2C.IMG_NO_INJECT}</p>
          </div>
        ) : (
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">An image ref Wardyn pulls as-is.</p>
        )}
      </ImageCard>
    </div>
  );
}

// ============================ The step ============================
export function StepBaseImage({
  sources,
  scans,
  phaseA,
  partial,
  onEditSource,
  onRescan,
  detectedChips,
  harnessAvailable,
  state,
  onChange,
}: {
  sources: SourceRow[];
  scans: Record<string, SourceScanState>;
  phaseA: boolean;
  partial: boolean;
  onEditSource: (id: string) => void;
  onRescan: (id: string) => void;
  detectedChips: string[];
  harnessAvailable: boolean;
  state: BaseImageState;
  onChange: (patch: Partial<BaseImageState>) => void;
}) {
  if (phaseA) {
    return <ScanProgress sources={sources} scans={scans} onEditSource={onEditSource} onRescan={onRescan} />;
  }

  return (
    <div className="space-y-4">
      <div className="space-y-2 rounded-lg border border-border bg-surface-2/40 p-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">
            What the image needs
          </span>
          {partial && <Chip tone="warning">based on a partial scan</Chip>}
        </div>
        <div className="flex flex-wrap gap-1.5">
          {detectedChips.map((chip) => (
            <Chip key={chip} tone="neutral" mono>
              {chip}
            </Chip>
          ))}
        </div>
      </div>

      <ImageCards
        detectedChips={detectedChips}
        partial={partial}
        harnessAvailable={harnessAvailable}
        state={state}
        onChange={onChange}
      />
    </div>
  );
}
