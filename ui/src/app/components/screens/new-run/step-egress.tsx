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
import type { FirstUseMode, Workspace } from "../../../lib/types";
import { cn } from "../../ui/utils";
import { Chip } from "../../wardyn/primitives";
import { DomainPillList, Field } from "./step-shell";
import { PRESET_DOMAINS, impliedEgressHosts, isValidDomain, type WizardState } from "./wizard-types";

export function StepEgress({
  state,
  patch,
  workspaces = [],
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  // Resolves state.workspaces against onboarded workspaces, so a selected
  // repo-kind workspace can be recognized as an implied-egress reason below
  // (D6/claim3) — same list buildSpec resolves against. Optional/defaulted:
  // omitting it just means a repo selection won't explain itself yet.
  workspaces?: Workspace[];
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
      setCustomError("Use a bare host, not a URL — e.g. example.com, *.example.com, example.com:443.");
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
      setDenyError("Use a bare host, not a URL — e.g. example.com, *.example.com, example.com:443.");
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
  // D6/claim3: buildSpec unions these into allowed_domains regardless of what
  // this grid shows — the SAME predicate/host-list (wizard-types.ts), so this
  // row can never disagree with what actually ships.
  const implied = impliedEgressHosts(state, workspaces);

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
                  aria-pressed={on}
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
          {/* Non-removable: these hosts stay allowed regardless of what's
              toggled above — a grant (or a repo workspace, which is not a
              grant, hence "automatically") needs them, so untoggling a
              coincidentally-matching preset above wouldn't actually close
              them off. The why rides srLabel too — title alone is hover-only
              and invisible to keyboard/screen-reader users on the one screen
              that owns egress. */}
          {implied.length > 0 && (
            <div
              className="flex flex-wrap items-center gap-1.5 pt-1 text-[0.6875rem] text-muted-foreground"
              data-testid="egress-implied-hosts"
            >
              <span>Added automatically:</span>
              {implied.map((h, i) => (
                <Chip
                  key={`${h.why}-${h.host}-${i}`}
                  tone="neutral"
                  mono
                  title={h.why}
                  srLabel={`${h.host} — added by ${h.why}`}
                >
                  {h.host}
                </Chip>
              ))}
            </div>
          )}
        </Field>
      )}

      {!allowAll && (
        <Field
          label="Add a custom domain"
          hint="A bare host, optionally with a leading *. wildcard and/or a :port — example.com, *.example.com, example.com:443, registry:5000."
        >
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
