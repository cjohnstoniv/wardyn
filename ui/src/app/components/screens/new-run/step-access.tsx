/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 2 — Access: a GitHub token grant (repos + permission + approval + TTL)
// and the LLM API key (an api_key grant resolving a stored secret by NAME).
//
// Redesign (C9): progressive disclosure. The Anthropic-auth choice is the ONE
// primary decision up front — three cards, each a full RadioGroup option — and
// its details (the secret picker, the host path) disclose ONLY inside the
// selected card, instead of a same-weight "Anthropic API key" card sitting
// below the radio group repeating the same decision. GitHub token and Git PAT
// stay switch-gated blocks (already progressive: off = collapsed) with the
// same card treatment for visual consistency.
import * as React from "react";
import {
  Check,
  ChevronsUpDown,
  Cloud,
  Fingerprint,
  GitBranch,
  Github,
  KeyRound,
  Plus,
  TriangleAlert,
} from "lucide-react";
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
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "../../ui/command";
import { cn } from "../../ui/utils";
import { Field } from "./step-shell";
import type { AnthropicAuth, GitHubPermission, WizardState } from "./wizard-types";

export function StepAccess({
  state,
  patch,
  secrets,
  secretsLoading,
  onAddSecret,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  secrets: string[];
  secretsLoading: boolean;
  onAddSecret: () => void;
}) {
  const isClaude = state.agent === "claude-code";

  return (
    <div className="space-y-5">
      {/* --- Primary choice: how the agent authenticates to the LLM. Claude Code
          gets three cards (apikey / subscription / bedrock); Codex CLI only ever
          uses the OpenAI key, so it gets a single always-open card. --- */}
      {isClaude ? (
        <div className="rounded-lg border border-border p-3">
          <Label className="text-sm font-semibold text-foreground">Anthropic auth</Label>
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            How Claude Code authenticates to Anthropic. Pick one — its details open below.
          </p>
          <RadioGroup
            className="mt-3 gap-2"
            value={state.anthropicAuth}
            onValueChange={(v) => patch({ anthropicAuth: v as AnthropicAuth })}
          >
            <AuthOption
              value="apikey"
              id="auth-apikey"
              icon={KeyRound}
              title="API key"
              badge="Recommended"
              hint="Proxy injects a stored key as x-api-key; the agent never sees the raw key."
              checked={state.anthropicAuth === "apikey"}
            >
              <Field
                className="mt-3 border-t border-border pt-3"
                label="Stored secret"
                hint="Proxy-injected — the agent never sees the raw key."
              >
                <div className="flex items-center gap-2">
                  <SecretCombobox
                    value={state.llmSecretName}
                    onChange={(name) => patch({ llmSecretName: name })}
                    secrets={secrets}
                    loading={secretsLoading}
                  />
                  <Button type="button" variant="ghost" size="sm" onClick={onAddSecret}>
                    <Plus className="size-4" /> Add secret
                  </Button>
                </div>
                {!state.llmSecretName && (
                  <p className="flex items-start gap-1.5 rounded-md border border-warning/40 bg-warning-subtle px-2.5 py-1.5 text-[11px] leading-snug text-warning">
                    <TriangleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
                    No key selected — this run will launch with no model access and its first model call
                    will 404. Pick a stored key (or Add secret).
                  </p>
                )}
              </Field>
            </AuthOption>

            <AuthOption
              value="subscription"
              id="auth-subscription"
              icon={Fingerprint}
              title="Subscription (OAuth)"
              hint="Mount your host ~/.claude OAuth creds into the sandbox. Reduced isolation."
              checked={state.anthropicAuth === "subscription"}
            >
              <Field
                className="mt-3 border-t border-border pt-3"
                label="Host ~/.claude directory"
                htmlFor="sub-claude-dir"
                hint="Must be an ABSOLUTE host path (e.g. /home/you/.claude). The sibling .claude.json is mounted too."
              >
                <Input
                  id="sub-claude-dir"
                  placeholder="/home/you/.claude"
                  value={state.subscriptionClaudeDir}
                  onChange={(e) => patch({ subscriptionClaudeDir: e.target.value })}
                  className="font-mono"
                />
              </Field>
            </AuthOption>

            <AuthOption
              value="bedrock"
              id="auth-bedrock"
              icon={Cloud}
              title="Bedrock"
              badge="Auto"
              badgeTone="neutral"
              hint="Amazon Bedrock is set up by your operator (Settings → Connect a model). When configured, Claude runs use it automatically — it isn't a per-run choice."
              checked={state.anthropicAuth === "bedrock"}
              disabled
            />
          </RadioGroup>
        </div>
      ) : (
        <div className="rounded-lg border border-border p-3">
          <div className="flex items-center gap-2">
            <KeyRound className="size-4 text-primary" />
            <Label className="text-sm font-semibold text-foreground">OpenAI API key</Label>
          </div>
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            Pick a stored secret by name. Its value is injected proxy-side as{" "}
            <span className="font-mono">Authorization: Bearer …</span> — the agent
            never sees the raw key.
          </p>
          <div className="mt-3 flex items-center gap-2">
            <SecretCombobox
              value={state.llmSecretName}
              onChange={(name) => patch({ llmSecretName: name })}
              secrets={secrets}
              loading={secretsLoading}
            />
            <Button type="button" variant="ghost" size="sm" onClick={onAddSecret}>
              <Plus className="size-4" /> Add secret
            </Button>
          </div>
          {!state.llmSecretName && (
            <p className="mt-2 flex items-start gap-1.5 rounded-md border border-warning/40 bg-warning-subtle px-2.5 py-1.5 text-[11px] leading-snug text-warning">
              <TriangleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
              No key selected — this run will launch with no model access and its first model call will
              404. Pick a stored key (or Add secret).
            </p>
          )}
        </div>
      )}

      {/* --- GitHub token grant --- */}
      <div className="rounded-lg border border-border p-3">
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-start gap-2">
            <Github className="mt-0.5 size-4 shrink-0 text-primary" />
            <div>
              <Label htmlFor="gh-enable" className="text-sm font-semibold text-foreground">
                GitHub token
              </Label>
              <p className="mt-0.5 text-[11px] text-muted-foreground">
                Mint a short-lived, repo-scoped installation token. The broker clamps
                permissions to its ceiling regardless of what's requested.
              </p>
            </div>
          </div>
          <Switch
            id="gh-enable"
            checked={state.githubEnabled}
            onCheckedChange={(c) => patch({ githubEnabled: c })}
          />
        </div>

        {state.githubEnabled && (
          <div className="mt-3 space-y-4 border-t border-border pt-3">
            <Field label="Repositories" htmlFor="gh-repos" hint="One or more org/repo, comma or space separated.">
              <Input
                id="gh-repos"
                placeholder="acme/payments-service, acme/shared-libs"
                value={state.githubRepos}
                onChange={(e) => patch({ githubRepos: e.target.value })}
                className="font-mono"
              />
            </Field>

            <Field label="Permission">
              <Select
                value={state.githubPermission}
                onValueChange={(v) => patch({ githubPermission: v as GitHubPermission })}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="read">Read — contents:read</SelectItem>
                  <SelectItem value="read+write">
                    Read + write — contents:write, pull_requests:write
                  </SelectItem>
                </SelectContent>
              </Select>
            </Field>

            <div className="flex items-center justify-between">
              <Label htmlFor="gh-approval" className="text-sm">
                Requires approval before minting
              </Label>
              <Switch
                id="gh-approval"
                checked={state.githubRequiresApproval}
                onCheckedChange={(c) => patch({ githubRequiresApproval: c })}
              />
            </div>

            <Field label="Token TTL (minutes)" htmlFor="gh-ttl">
              <Input
                id="gh-ttl"
                type="number"
                min={1}
                value={state.githubTtlMinutes}
                onChange={(e) => patch({ githubTtlMinutes: Number(e.target.value) })}
                className="w-32 font-mono"
              />
            </Field>
          </div>
        )}
      </div>

      {/* --- git PAT (git_pat grant) for a non-GitHub host --- */}
      <div className="rounded-lg border border-border p-3">
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-start gap-2 min-w-0">
            <GitBranch className="mt-0.5 size-4 shrink-0 text-primary" />
            <div className="min-w-0">
              <Label htmlFor="pat-enable" className="text-sm font-semibold text-foreground">
                Git PAT (non-GitHub host)
              </Label>
              <p className="mt-0.5 text-[11px] text-muted-foreground">
                Broker a stored Personal Access Token to git for Azure DevOps /
                GitLab. Unlike an LLM key, the PAT value reaches git via the
                credential helper — Wardyn can't expire or down-scope it.
              </p>
            </div>
          </div>
          <Switch
            id="pat-enable"
            checked={state.gitPatEnabled}
            onCheckedChange={(c) => patch({ gitPatEnabled: c })}
          />
        </div>

        {state.gitPatEnabled && (
          <div className="mt-3 space-y-4 border-t border-border pt-3">
            <Field label="Host" htmlFor="pat-host" hint="The git host, e.g. dev.azure.com or gitlab.com. Unioned into allowed egress.">
              <Input
                id="pat-host"
                placeholder="dev.azure.com"
                value={state.gitPatHost}
                onChange={(e) => patch({ gitPatHost: e.target.value })}
                className="font-mono"
              />
            </Field>

            <Field label="Stored PAT secret">
              <div className="flex items-center gap-2">
                <SecretCombobox
                  value={state.gitPatSecretName}
                  onChange={(name) => patch({ gitPatSecretName: name })}
                  secrets={secrets}
                  loading={secretsLoading}
                />
                <Button type="button" variant="ghost" size="sm" onClick={onAddSecret}>
                  <Plus className="size-4" /> Add secret
                </Button>
              </div>
            </Field>

            <Field
              label="Git username (optional)"
              htmlFor="pat-username"
              hint="Defaults by host: Azure DevOps → pat, GitLab → oauth2. Override only if your host needs a different username."
            >
              <Input
                id="pat-username"
                placeholder="(auto)"
                value={state.gitPatUsername}
                onChange={(e) => patch({ gitPatUsername: e.target.value })}
                className="w-48 font-mono"
              />
            </Field>

            <div className="flex items-center justify-between">
              <Label htmlFor="pat-approval" className="text-sm">
                Requires approval before minting
              </Label>
              <Switch
                id="pat-approval"
                checked={state.gitPatRequiresApproval}
                onCheckedChange={(c) => patch({ gitPatRequiresApproval: c })}
              />
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

// A primary-choice card: radio dot + icon + bold title (+ optional badge) up
// top, description below, and — only when selected — the disclosed body. This
// is the "one card, its own details" unit the Anthropic-auth group is built
// from (C9 progressive disclosure).
function AuthOption({
  value,
  id,
  icon: Icon,
  title,
  badge,
  badgeTone = "primary",
  hint,
  checked,
  disabled,
  children,
}: {
  value: string;
  id: string;
  icon: React.ElementType;
  title: React.ReactNode;
  badge?: string;
  badgeTone?: "primary" | "neutral";
  hint: React.ReactNode;
  checked: boolean;
  disabled?: boolean;
  children?: React.ReactNode;
}) {
  return (
    <label
      htmlFor={id}
      className={cn(
        "flex flex-col gap-2.5 rounded-lg border p-3 transition-colors",
        checked ? "border-primary bg-primary/10" : "border-border",
        disabled && "cursor-not-allowed opacity-60",
      )}
    >
      <div className="flex items-center gap-2.5">
        <RadioGroupItem value={value} id={id} disabled={disabled} />
        <span className="text-sm font-semibold text-foreground">{title}</span>
        {badge && (
          <span
            className={cn(
              "rounded-md border px-1.5 py-0.5 text-[10px] font-semibold tracking-wide uppercase",
              badgeTone === "primary"
                ? "border-primary/30 bg-primary/15 text-primary"
                : "border-border bg-muted text-muted-foreground",
            )}
          >
            {badge}
          </span>
        )}
        <Icon className="ml-auto size-4 shrink-0 text-muted-foreground" />
      </div>
      <p className="pl-[26px] text-[11px] leading-snug text-muted-foreground">{hint}</p>
      {checked && children && <div className="pl-[26px]">{children}</div>}
    </label>
  );
}

function SecretCombobox({
  value,
  onChange,
  secrets,
  loading,
}: {
  value: string;
  onChange: (name: string) => void;
  secrets: string[];
  loading: boolean;
}) {
  const [open, setOpen] = React.useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          role="combobox"
          aria-expanded={open}
          className="w-64 justify-between font-mono"
        >
          <span className={cn(!value && "font-sans text-muted-foreground")}>
            {value || (loading ? "Loading secrets…" : "Select a secret…")}
          </span>
          <ChevronsUpDown className="size-4 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-64 p-0" align="start">
        <Command>
          <CommandInput placeholder="Search secrets…" />
          <CommandList>
            <CommandEmpty>{loading ? "Loading…" : "No secrets found."}</CommandEmpty>
            <CommandGroup>
              {value && (
                <CommandItem
                  value="__none__"
                  onSelect={() => {
                    onChange("");
                    setOpen(false);
                  }}
                  className="text-muted-foreground"
                >
                  Clear selection
                </CommandItem>
              )}
              {secrets.map((name) => (
                <CommandItem
                  key={name}
                  value={name}
                  onSelect={(v) => {
                    onChange(v);
                    setOpen(false);
                  }}
                  className="font-mono"
                >
                  <Check
                    className={cn("size-4", value === name ? "opacity-100" : "opacity-0")}
                  />
                  {name}
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
