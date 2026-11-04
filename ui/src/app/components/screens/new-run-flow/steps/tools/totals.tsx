/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Network and Credentials totals: what the run can reach, read from the
// server's `access` block. A host is Requested until the server has answered
// this exact body; only then does its verdict show.

import { Grid3x3, Shield } from "lucide-react";
import { AdoLaunchNote, AdoOwnTokenNote, AdoRunTokenLine, adoLaunchNoteKind, ownTokenNoteOrgs } from "../../../../wardyn/ado-run-token-line";
import { Switch } from "../../../../wardyn/form-primitives";
import { Chip } from "../../../../wardyn/primitives";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { CC_META } from "../../../../wardyn/cc-meta";
import { NR_WALLS, YOUR_WALLS } from "../../../../wardyn/copy/personal-walls";
import { WALL_PRESETS } from "../../../../wardyn/copy/wall-presets";
import { FORGE_NAME, forgeLimits, readsOnly } from "../../../../../lib/wall-preset-words";
import type { PersonalWallsSpec } from "../../../../../lib/api/walls";
import { appURL } from "../../../../../lib/base-path";
import type { SCMAccessPAT } from "../../../../../lib/types/ado-pat";
import type { AccessCredential, AccessFacts, AccessUnlisted } from "../../../../../lib/types/new-run-access";
import { hostBadge, rowLabel, wallsLeaveOut, type HarnessNames, type HostBadge } from "../../lenses/tools-lens";
import { isLocalMcp } from "./local-mcp";

const DELIVERY: Record<string, string> = {
  proxy: NR_TOOLS.DELIVERY_PROXY,
  git_broker: NR_TOOLS.DELIVERY_GIT_BROKER,
  git_helper: NR_TOOLS.DELIVERY_GIT_HELPER,
  sandbox_env: NR_TOOLS.DELIVERY_ENV,
  sandbox_file: NR_TOOLS.DELIVERY_FILE,
  sandbox_short_lived: NR_TOOLS.DELIVERY_SHORT_LIVED,
};
export const BADGE: Record<HostBadge, { text: string; tone: "neutral" | "success" | "info" | "danger" }> = {
  requested: { text: NR_TOOLS.NET_REQUESTED, tone: "neutral" },
  allowed: { text: NR_TOOLS.NET_ALLOWED, tone: "success" },
  needs_approval: { text: NR_TOOLS.NET_NEEDS_APPROVAL, tone: "info" },
  not_allowed: { text: NR_TOOLS.NET_NOT_ALLOWED, tone: "danger" },
};

const TOTAL_CLASS = "flex min-w-0 flex-col gap-2.5 rounded-lg border border-border bg-surface-2 p-3.5";
const POLICY_CLASS = "border-t border-dashed border-border pt-2.5 text-sm text-muted-foreground";

/** The knobs the person's saved walls name, each as the clause the walls line says it in; none for walls that name nothing. First use of a new site is left out: it is inert on an allow-all run and a stricter ceiling changes it, which the walls alone cannot tell. */
export function wallsApplyClauses(walls: PersonalWallsSpec | undefined): string[] {
  if (!walls) return [];
  const clauses: string[] = [];
  if (walls.allowed_domains) clauses.push(walls.allowed_domains.length > 0 ? NR_WALLS.APPLY_SITES_LISTED(walls.allowed_domains.length) : NR_WALLS.APPLY_SITES_SETUP_ONLY);
  if (walls.denied_domains?.length) clauses.push(NR_WALLS.APPLY_SITES_NEVER(walls.denied_domains.length));
  if (walls.min_confinement_class) clauses.push(NR_WALLS.APPLY_BARRIER(CC_META[walls.min_confinement_class]?.label ?? walls.min_confinement_class));
  const blocked = Object.entries(walls.desktop ?? {}).flatMap(([dir, rule]) => (rule === "blocked" && NR_WALLS.APPLY_TRANSFERS[dir] ? [NR_WALLS.APPLY_TRANSFERS[dir]] : []));
  if (blocked.length > 0) clauses.push(NR_WALLS.APPLY_NO_TRANSFER(blocked.length === 1 ? blocked[0] : `${blocked.slice(0, -1).join(NR_WALLS.APPLY_JOIN)}${NR_WALLS.APPLY_OR}${blocked[blocked.length - 1]}`));
  // Each entry is one restriction: a scoped entry (one host or key) leaves the kind's other credentials allowed, so kinds are not counted.
  const limits = walls.withheld_grants?.length ?? 0;
  if (limits > 0) clauses.push(NR_WALLS.APPLY_CREDENTIAL_LIMITS(limits));
  return clauses;
}

/**
 * One neutral line saying which of the person's own walls apply to the run, from the walls they saved; Review and the
 * Tools step draw it. Nothing for walls that name nothing, and nothing when the walls could not be read (`walls` is then undefined).
 */
export function WallsApplyLine({ walls }: { walls: PersonalWallsSpec | undefined }) {
  const clauses = wallsApplyClauses(walls);
  if (clauses.length === 0) return null;
  return (
    <p data-testid="walls-apply" title={walls?.allowed_domains ? YOUR_WALLS.SITES_ONLY_HINT : undefined} className="flex items-start gap-2 rounded-md border border-border bg-muted px-3 py-2 text-xs text-foreground">
      <Grid3x3 aria-hidden="true" className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
      <span className="min-w-0 [overflow-wrap:anywhere]">
        {NR_WALLS.APPLY_BEFORE}{clauses.join(NR_WALLS.APPLY_JOIN)}{NR_WALLS.APPLY_AFTER}{" "}
        {/* A new tab: the draft lives in the New Run route's state, so leaving this page for Settings would drop it. */}
        <a href={appURL("/account")} target="_blank" rel="noopener noreferrer" className="font-semibold text-primary hover:underline">{NR_WALLS.CHANGE_WALLS}</a>
      </span>
    </p>
  );
}

/**
 * The clauses of the chosen wall preset, in the walls line's words: the sites it keeps and refuses, what an unlisted
 * site meets, the barrier, the desktop transfers and credentials it takes out, read capabilities only, every push
 * refused. None for a preset that names nothing (Open).
 */
export function wallPresetClauses(spec: PersonalWallsSpec): string[] {
  const sites = wallsApplyClauses({ allowed_domains: spec.allowed_domains, denied_domains: spec.denied_domains });
  const rest = wallsApplyClauses({ min_confinement_class: spec.min_confinement_class, desktop: spec.desktop, withheld_grants: spec.withheld_grants });
  return [
    ...sites,
    ...(spec.first_use_approval === "always_deny" ? [WALL_PRESETS.CLAUSE_UNLISTED_REFUSED] : []),
    ...rest,
    ...(readsOnly(spec) ? [WALL_PRESETS.CLAUSE_READ_ONLY] : forgeLimits(spec).filter((f) => f.reads).map((f) => WALL_PRESETS.CLAUSE_FORGE_READ(FORGE_NAME[f.forge]))),
    ...(spec.push_rules?.deny_all === true ? [WALL_PRESETS.CLAUSE_EVERY_PUSH] : []),
  ];
}

/** One neutral line saying what the chosen wall preset narrows, above the person's own walls line (Tools step). Nothing for Open. */
export function WallPresetApplyLine({ name, spec }: { name: string; spec: PersonalWallsSpec }) {
  const clauses = wallPresetClauses(spec);
  if (clauses.length === 0) return null;
  return (
    <p data-testid="wall-preset-apply" className="flex items-start gap-2 rounded-md border border-border bg-muted px-3 py-2 text-xs text-foreground">
      <Shield aria-hidden="true" className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
      <span className="min-w-0 [overflow-wrap:anywhere]">{WALL_PRESETS.PRESET_LINE(name, clauses.join(NR_WALLS.APPLY_JOIN))}</span>
    </p>
  );
}

/** A delivery code in words; a code this console does not know shows no label rather than a raw code. */
export const deliveryLabel = (delivery: string): string | undefined => DELIVERY[delivery];

/**
 * A credential's label: a model credential names its provider, or says Model
 * provider before one is known. The other kinds carry no label on the wire, so
 * they show the row they are for and their delivery.
 */
export const credentialLabel = (c: AccessCredential, harness: string | undefined, providers?: ReadonlyMap<string, string>): string | undefined => {
  if (c.kind !== "model_provider") return c.label || undefined;
  const provider = (harness && providers?.get(harness)) || NR_TOOLS.PROVIDER_LABEL;
  // The only model credential left in the sandbox as a file is the SSO access token (internal/api modelCredentials).
  return c.delivery === "sandbox_file" ? `${provider} ${NR_TOOLS.SSO_TOKEN}` : provider;
};

/** "Anything else" for the clamped, effective spec, then whether the organisation's limits have been checked. */
export function unlistedSentences(u: AccessUnlisted, answered: boolean): string[] {
  if (!answered) return [NR_TOOLS.NET_POLICY_CEILING];
  const admin = u.decider !== "you";
  const out: string[] = [];
  if (u.allow_all) out.push(NR_TOOLS.NET_POLICY_ALLOW_ALL);
  else if (u.mode === "always_deny") out.push(NR_TOOLS.NET_POLICY_ALWAYS_DENY);
  else if (u.mode === "deny_with_review") out.push(admin ? NR_TOOLS.NET_POLICY_DENY_REVIEW_ADMIN : NR_TOOLS.NET_POLICY_DENY_REVIEW_YOU);
  else if (u.mode === "wait_for_review") {
    out.push(admin ? NR_TOOLS.NET_POLICY_HELD_ADMIN : NR_TOOLS.NET_POLICY_HELD_YOU);
    if (u.hold_sec) out.push(NR_TOOLS.NET_POLICY_TIMEOUT(u.hold_sec));
  }
  out.push(NR_TOOLS.NET_POLICY_CHECKED);
  return out;
}

/**
 * The policy line says short-lived credentials are renewed while the run lives.
 * That is untrue once a short-lived credential is not renewed (a Bedrock SSO
 * sign-in with the proxy-inject switch off), so the line is withheld then and
 * each credential row states its own delivery.
 */
export const renewalHolds = (credentials: readonly AccessCredential[]): boolean =>
  !credentials.some((c) => c.delivery === "sandbox_short_lived" && !c.renewed);

/** The credential a repository on the Azure DevOps sign-in lane brings (the server's kind for it). */
const ADO_SIGN_IN = "ado_signin";

/**
 * What the Credentials total says of this run's Azure DevOps credential when the person's row creates a
 * token for each run: what that token carries, and the door when they have yet to connect.
 */
export interface AdoTokenFacts {
  /** The caller's own Azure DevOps answer (the shell's setup status). */
  access: SCMAccessPAT | undefined;
  /** The run's policy capabilities; the row's default profile stands in when it names none. */
  policyCaps: unknown;
  connecting: boolean;
  onConnect: () => void;
  blockedUrl?: string | null;
  onFallbackClick?: () => void;
}

/** The Network total's "Only the sites listed" switch: whether the draft sends allow_all_egress: false, and how to change it. */
export interface NarrowEgress {
  on: boolean;
  onChange: (on: boolean) => void;
}

export function Totals({ access, answered, names: harnessNames, providers, ado, ownRows, narrow, walls }: { access: AccessFacts | undefined; answered: boolean; names?: HarnessNames; providers?: ReadonlyMap<string, string>; ado?: AdoTokenFacts; ownRows?: readonly SCMAccessPAT[]; narrow?: NarrowEgress; walls?: PersonalWallsSpec }) {
  const names = new Map((access?.rows ?? []).map((r) => [r.id, rowLabel(r, harnessNames)]));
  const rowName = new Map((access?.rows ?? []).map((r) => [r.id, r.name]));
  const localRows = new Set((access?.rows ?? []).filter(isLocalMcp).map((r) => r.id));
  // Only a run that carries the Azure DevOps credential gets a token, so only its row says so.
  const leftOut = wallsLeaveOut(walls, access, answered);
  // The switch shows while the run allows all egress, and stays once it is on (the answer then says it does not).
  const onlyListed = narrow && (narrow.on || access?.unlisted.allow_all) ? narrow : undefined;
  const adoToken = ado?.access?.token_mode === "minted_pat" && !!access?.credentials.some((c) => c.kind === ADO_SIGN_IN) ? ado : undefined;
  return (
    <>
    <WallsApplyLine walls={walls} />
    <section aria-label={NR_TOOLS.TOTALS_ARIA} className="grid gap-3 [grid-template-columns:repeat(auto-fit,minmax(280px,1fr))]">
      <div className={TOTAL_CLASS}>
        <h3 className="font-medium">{NR_TOOLS.NETWORK_TITLE}</h3>
        <ul className="flex flex-col gap-1.5">
          {access?.hosts.map((h) => {
            const from = h.rows.flatMap((id) => names.get(id) ?? []);
            const badge = BADGE[hostBadge(h, answered)];
            return (
              <li key={h.host} className="flex flex-wrap items-center justify-between gap-2">
                <span className="min-w-0 [overflow-wrap:anywhere]">
                  <span className="font-mono text-sm">{h.host}</span>
                  {from.length > 0 && <span className="text-sm text-muted-foreground"> {NR_TOOLS.FROM_ROW(from.join(", "))}</span>}
                </span>
                <Chip tone={badge.tone} className="whitespace-normal">{badge.text}</Chip>
              </li>
            );
          })}
          {answered && access?.hosts.length === 0 && <li className="text-sm text-muted-foreground">{NR_TOOLS.NO_HOSTS}</li>}
        </ul>
        {access && <p className={POLICY_CLASS}>{unlistedSentences(access.unlisted, answered).join(" ")}</p>}
        {leftOut.map((host) => (
          <p key={host} data-testid="walls-leave-out" className="flex items-center gap-2 rounded-md border border-border bg-muted px-3 py-2 text-xs text-foreground">
            <Grid3x3 aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
            <span className="min-w-0 [overflow-wrap:anywhere]">
              {NR_WALLS.LEAVE_OUT_BEFORE}
              <span className="font-mono">{host}</span>
              {NR_WALLS.LEAVE_OUT_AFTER}
            </span>
          </p>
        ))}
        {onlyListed && (
          <div className="flex items-start gap-2.5 border-t border-border pt-2.5">
            <Switch checked={onlyListed.on} onChange={onlyListed.onChange} label={NR_WALLS.ONLY_LISTED} className="mt-0.5" />
            <div>
              <p className="text-sm font-medium">{NR_WALLS.ONLY_LISTED}</p>
              <p className="text-xs text-muted-foreground">{NR_WALLS.ONLY_LISTED_HELP}</p>
            </div>
          </div>
        )}
      </div>
      <div className={TOTAL_CLASS}>
        <h3 className="font-medium">{NR_TOOLS.CREDENTIALS_TITLE}</h3>
        <ul className="flex flex-col gap-1.5">
          {access?.credentials.map((c) => {
            const kind = credentialLabel(c, rowName.get(c.row), providers);
            const delivery = deliveryLabel(c.delivery);
            return (
              <li key={c.id} className="flex flex-wrap items-center justify-between gap-2">
                <span className="min-w-0 [overflow-wrap:anywhere]">
                  {kind && <span>{kind} </span>}
                  {names.has(c.row) && <span className="text-sm text-muted-foreground">{NR_TOOLS.FOR_ROW(names.get(c.row) ?? "")}</span>}
                  {localRows.has(c.row) && (c.delivery === "sandbox_env" || c.delivery === "sandbox_file") && (
                    <span className="block text-sm text-muted-foreground">{NR_TOOLS.MCP_SANDBOX_READABLE}</span>
                  )}
                  {c.after_end === "expires" && <span className="block text-sm text-muted-foreground">{NR_TOOLS.EXPIRES_OWN}</span>}
                </span>
                {delivery && <Chip className="whitespace-normal">{delivery}</Chip>}
                {adoToken && c.kind === ADO_SIGN_IN && (
                  <AdoRunTokenLine className="basis-full text-sm text-muted-foreground" policyCaps={adoToken.policyCaps} defaults={adoToken.access?.default_profile} />
                )}
              </li>
            );
          })}
          {adoToken && adoLaunchNoteKind(adoToken.access) && (
            <li>
              <AdoLaunchNote access={adoToken.access} connecting={adoToken.connecting} onConnect={adoToken.onConnect} blockedUrl={adoToken.blockedUrl} onFallbackClick={adoToken.onFallbackClick} />
            </li>
          )}
          {access?.credentials.some((c) => c.kind === ADO_SIGN_IN) && ownTokenNoteOrgs(ownRows).length > 0 && (
            <li>
              <AdoOwnTokenNote rows={ownRows} />
            </li>
          )}
          {answered && access?.credentials.length === 0 && <li className="text-sm text-muted-foreground">{NR_TOOLS.NO_CREDENTIALS}</li>}
        </ul>
        {access && renewalHolds(access.credentials) && <p className={POLICY_CLASS}>{NR_TOOLS.CREDENTIALS_POLICY}</p>}
      </div>
    </section>
    </>
  );
}
