/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 3 — Egress: the allowed-domain allowlist (preset toggle chips + custom
// add), first-use approval, and an optional deny-list.
import * as React from "react";
import { Plus } from "lucide-react";
import { Input } from "../../ui/input";
import { Switch } from "../../ui/switch";
import { Button } from "../../ui/button";
import { Label } from "../../ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../ui/select";
import type { FirstUseMode } from "../../../lib/types";
import { cn } from "../../ui/utils";
import { DomainPillList, Field } from "./step-shell";
import { PRESET_DOMAINS, isValidDomain, type WizardState } from "./wizard-types";

export function StepEgress({
  state,
  patch,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
}) {
  const [customDraft, setCustomDraft] = React.useState("");
  const [customError, setCustomError] = React.useState<string | null>(null);
  const [denyDraft, setDenyDraft] = React.useState("");
  const [denyError, setDenyError] = React.useState<string | null>(null);

  const allowed = new Set(state.allowedDomains);
  // Custom domains are those not in the preset set.
  const customDomains = state.allowedDomains.filter((d) => !PRESET_DOMAINS.includes(d));

  const togglePreset = (domain: string) => {
    const next = new Set(state.allowedDomains);
    if (next.has(domain)) next.delete(domain);
    else next.add(domain);
    patch({ allowedDomains: Array.from(next) });
  };

  const addCustom = () => {
    const d = customDraft.trim();
    if (!isValidDomain(d)) {
      setCustomError("Use an exact host (a.b.c) or a wildcard (*.b.c).");
      return;
    }
    if (allowed.has(d)) {
      setCustomError("Already allowed.");
      return;
    }
    patch({ allowedDomains: [...state.allowedDomains, d] });
    setCustomDraft("");
    setCustomError(null);
  };

  const removeAllowed = (domain: string) => {
    patch({ allowedDomains: state.allowedDomains.filter((d) => d !== domain) });
  };

  const addDenied = () => {
    const d = denyDraft.trim();
    if (!isValidDomain(d)) {
      setDenyError("Use an exact host (a.b.c) or a wildcard (*.b.c).");
      return;
    }
    if (state.deniedDomains.includes(d)) {
      setDenyError("Already denied.");
      return;
    }
    patch({ deniedDomains: [...state.deniedDomains, d] });
    setDenyDraft("");
    setDenyError(null);
  };

  const removeDenied = (domain: string) => {
    patch({ deniedDomains: state.deniedDomains.filter((d) => d !== domain) });
  };

  const allowAll = state.allowAllEgress;

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between rounded-lg border border-border p-3">
        <div>
          <Label htmlFor="allow-all-egress">Allow all egress (deny-list only)</Label>
          <p className="mt-0.5 text-[0.6875rem] text-muted-foreground">
            Permit any public host except the deny-list below. Private/internal IPs
            stay blocked by the SSRF guard. Credential injection still needs an
            explicit allowlisted host.
          </p>
        </div>
        <Switch
          id="allow-all-egress"
          checked={allowAll}
          onCheckedChange={(c) => patch({ allowAllEgress: c })}
        />
      </div>

      {!allowAll && (
        <Field
          label="Allowed domains"
          hint="Toggle common targets, or add your own. Everything else is denied (or escalated, below)."
        >
          <div className="flex flex-wrap gap-1.5">
            {PRESET_DOMAINS.map((domain) => {
              const on = allowed.has(domain);
              return (
                <button
                  key={domain}
                  type="button"
                  onClick={() => togglePreset(domain)}
                  className={cn(
                    "rounded-md border px-2 py-1 font-mono text-[0.6875rem] transition-colors",
                    on
                      ? "border-primary bg-primary/10 text-foreground"
                      : "border-border text-muted-foreground hover:border-border-strong",
                  )}
                >
                  {domain}
                </button>
              );
            })}
          </div>
        </Field>
      )}

      {!allowAll && (
        <Field label="Add a custom domain" hint="Exact host or a single-label wildcard (*.example.com).">
          <div className="flex items-center gap-2">
            <Input
              placeholder="api.internal.acme.com"
              value={customDraft}
              onChange={(e) => {
                setCustomDraft(e.target.value);
                setCustomError(null);
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  addCustom();
                }
              }}
              className="font-mono"
            />
            <Button type="button" variant="outline" size="sm" onClick={addCustom}>
              <Plus className="size-4" /> Add
            </Button>
          </div>
          {customError && <p className="text-[0.6875rem] text-danger">{customError}</p>}
          {customDomains.length > 0 && (
            <div className="pt-1">
              <DomainPillList domains={customDomains} onRemove={removeAllowed} />
            </div>
          )}
        </Field>
      )}

      {!allowAll && (
        <div className="rounded-lg border border-border p-3">
          <Label htmlFor="first-use">Unknown domains</Label>
          <p className="mb-2 mt-0.5 text-[0.6875rem] text-muted-foreground">
            How the proxy handles a domain that isn't on the allow-list.
          </p>
          <Select
            value={state.firstUseApproval}
            onValueChange={(v) => patch({ firstUseApproval: v as FirstUseMode })}
          >
            <SelectTrigger id="first-use">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="always_deny">Always deny — block silently, no approval</SelectItem>
              <SelectItem value="deny_with_review">
                Deny + review — block now, ask you; a retry passes once approved
              </SelectItem>
              <SelectItem value="wait_for_review">
                Wait for review — hold the request live until you approve or deny
              </SelectItem>
            </SelectContent>
          </Select>
        </div>
      )}

      <Field
        label={
          <span>
            Denied domains <span className="font-normal text-muted-foreground">(optional)</span>
          </span>
        }
        hint="Explicitly block these even if they'd otherwise match an allow rule."
      >
        <div className="flex items-center gap-2">
          <Input
            placeholder="telemetry.example.com"
            value={denyDraft}
            onChange={(e) => {
              setDenyDraft(e.target.value);
              setDenyError(null);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                addDenied();
              }
            }}
            className="font-mono"
          />
          <Button type="button" variant="outline" size="sm" onClick={addDenied}>
            <Plus className="size-4" /> Add
          </Button>
        </div>
        {denyError && <p className="text-[0.6875rem] text-danger">{denyError}</p>}
        <div className="pt-1">
          <DomainPillList domains={state.deniedDomains} onRemove={removeDenied} tone="danger" />
        </div>
      </Field>
    </div>
  );
}
