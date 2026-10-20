/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add tools and access: the CLIs, MCP servers and APIs the person's organisation
// grants, and the doors for a secret, a host, an MCP server by URL and a custom
// component. What it adds shows under Added to this run, with the server's own
// state for each. An organisation row the person is not granted is never listed:
// the server says the same thing whether or not it exists.

import * as React from "react";
import { getErrorMessage } from "../../../../../lib/format";
import { Lock, Plus } from "lucide-react";
import { components as api } from "../../../../../lib/api/components";
import { secrets } from "../../../../../lib/api/secrets";
import reasons from "../../../../../lib/access-reasons.golden.json";
import type { CatalogueCLI, CatalogueCLIKind, ComponentRef, MyComponents, OrgComponentView } from "../../../../../lib/types/components";
import type { AccessFacts, AccessRow } from "../../../../../lib/types/new-run-access";
import { Button } from "../../../../ui/button";
import { ADD_ACCESS, CUSTOM_COMPONENT as CC } from "../../../../wardyn/copy/components";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { Chip } from "../../../../wardyn/primitives";
import { Segmented } from "../../../../wardyn/segmented";
import type { RunIntent } from "../../draft/run-intent";
import { addAllowedHost } from "../../lenses/tools-lens";
import { useNewRunDispatch } from "../../new-run-route";
import { useAnnounce } from "../step-shell";
import { AddMcpDialog } from "./add-mcp-dialog";
import { AddSecretDialog, type SecretAddition } from "./add-secret-dialog";
import { AllowHostDialog } from "./allow-host-dialog";
import { addHeaderSecret, attachComponent, detachComponent, removeAllowedHost, removeHeaderSecret } from "./catalogue-edits";
import { addedMark, CARD_ICON, CLI_CARD } from "./catalogue-pending";
import { IconTile } from "./icon-tile";
import { githubAccess, kubectlAccess, kubectlSwitchOn, localMcpAccess, MODE_DELIVERY, mcpAccess, type RunFacts } from "./tool-access";
import { NOTHING, ToolCard, type ToolAccess } from "./tool-card";
import { CustomComponentDialog } from "./custom-component-dialog";
import { isLocalMcp, useLocalMcpOn } from "./local-mcp";

type Category = "all" | "cli" | "mcp" | "api" | "secret";
type Door = "secret" | "host" | "mcp" | "custom";

const SENTENCES = new Map(reasons.map((r) => [r.key, r.text]));
const SECRETS_PATH = "/secrets";
const FROM_KIND: Record<string, string> = {
  custom: NR_TOOLS.FROM_CUSTOM, cli: NR_TOOLS.FROM_CLI, mcp: NR_TOOLS.FROM_MCP, api: NR_TOOLS.FROM_API, secret: NR_TOOLS.FROM_SECRET, host: NR_TOOLS.FROM_HOST,
};

// How a CLI row's own credential reaches the run, as the server's access facts
// give it (internal/api/run_access_facts.go): the GitHub App token and the
// person's Azure token by the proxy, AWS role credentials in the sandbox.
const CLI_DELIVERY: Partial<Record<CatalogueCLIKind, string>> = {
  azure: NR_TOOLS.DELIVERY_PROXY, github: NR_TOOLS.DELIVERY_PROXY, aws: NR_TOOLS.DELIVERY_SHORT_LIVED,
};

/** The server's sentence for a closed reason code; a code this console does not know says nothing. */
const sentence = (code: string | undefined): string | undefined => (code ? SENTENCES.get(code) : undefined);

/** The person's catalogue, or why it could not be read (the read's own sentence, as the other steps show it). */
function useMyComponents(): { mine: MyComponents | null; failed?: string } {
  const [state, setState] = React.useState<{ mine: MyComponents | null; failed?: string }>({ mine: null });
  React.useEffect(() => {
    let live = true;
    api.mine().then(
      (m) => live && setState({ mine: m }),
      (e: unknown) => live && setState({ mine: null, failed: getErrorMessage(e) }),
    );
    return () => { live = false; };
  }, []);
  return state;
}

/** The hosts a card brings, then how its credential reaches the run. */
function Brings({ hosts, deliveries }: { hosts: readonly string[]; deliveries: readonly string[] }) {
  if (hosts.length === 0 && deliveries.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-1.5">
      {hosts.map((h) => <Chip key={h} mono className="whitespace-normal text-foreground [overflow-wrap:anywhere]">{h}</Chip>)}
      {[...new Set(deliveries)].map((d) => <Chip key={d} className="whitespace-normal">{d}</Chip>)}
    </div>
  );
}

function Reason({ text, id }: { text: string; id?: string }) {
  return (
    <p id={id} className="flex items-start gap-1.5 text-xs text-muted-foreground">
      <Lock aria-hidden className="mt-px size-3.5 shrink-0" />
      {text}
    </p>
  );
}

/**
 * Add, or Remove once added. A row that cannot be added shows its reason in
 * place of Add; an added row keeps its reason beside Remove.
 */
function RowAction({ name, attached, reason, status, details, onAdd, onRemove }: { name: string; attached: boolean; reason?: string; status?: React.ReactNode; details?: React.ReactNode; onAdd: () => void; onRemove: () => void }) {
  if (attached) {
    return (
      <>
        <div className="flex min-w-0 flex-col gap-1">
          {(status || details) && <div className="flex items-center gap-2">{status}{details}</div>}
          {reason && <Reason text={reason} />}
        </div>
        <Button type="button" variant="outline" size="sm" aria-label={`${NR_TOOLS.CARD_REMOVE} ${name}`} onClick={onRemove}>{NR_TOOLS.CARD_REMOVE}</Button>
      </>
    );
  }
  if (reason) return <Reason text={reason} />;
  return (
    <>
      {details ?? <span />}
      <Button type="button" variant="outline" size="sm" aria-label={`${NR_TOOLS.CARD_ADD} ${name}`} onClick={onAdd}>{NR_TOOLS.CARD_ADD}</Button>
    </>
  );
}

/** A door card: a dialog that adds something of the person's own. */
function DoorCard({ icon, title, body, button, reason, onOpen }: { icon: string; title: string; body: string; button: string; reason?: string; onOpen: () => void }) {
  const reasonId = React.useId();
  return (
    <ToolCard icon={icon} title={title} body={body}>
      {reason ? <Reason text={reason} id={reasonId} /> : <span />}
      <Button type="button" variant="outline" size="sm" disabled={!!reason} aria-describedby={reason ? reasonId : undefined} onClick={onOpen}>{button}</Button>
    </ToolCard>
  );
}

function Status({ status, answered }: { status: string; answered: boolean }) {
  // Until the server has answered this body, a state is only Requested.
  if (!answered) return <Chip>{NR_TOOLS.NET_REQUESTED}</Chip>;
  if (status === "ready") return <Chip tone="success" dot>{NR_TOOLS.STATUS_READY}</Chip>;
  if (status === "needs_setup") return <Chip tone="warning" dot>{NR_TOOLS.STATUS_NEEDS_SETUP}</Chip>;
  return null;
}

export interface AddToolsProps {
  /** The preview's `access` block: what the added things bring, and the state the server gives each. */
  access: AccessFacts | undefined;
  /** The server has answered exactly this body; until it has, rows are Requested, not Ready. */
  answered: boolean;
  intent: RunIntent;
  /** The draft's components that `access` answered; its "inline:<n>" rows point into this list. */
  answeredFor: RunIntent["components"];
  /** The GitHub repositories the run's clones go through the App for: what the GitHub CLI can act on. */
  githubRepos?: readonly string[];
}

export function AddTools({ access, answered, intent, answeredFor, githubRepos = [] }: AddToolsProps) {
  const headingId = React.useId();
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const { mine, failed } = useMyComponents();
  const localMcp = useLocalMcpOn();
  // Absent reads as off: the kubectl card stays Coming soon until the server says the kind is on.
  const kubectlOn = kubectlSwitchOn(useShellSetupStatus().status);
  const [category, setCategory] = React.useState<Category>("all");
  const [door, setDoor] = React.useState<Door | null>(null);
  // A dialog that chains into another (the custom form into the full editor) unmounts the button that opened it, so
  // focus would fall to the page. The button is remembered and takes focus back once the last dialog is gone.
  const opener = React.useRef<HTMLElement | null>(null);
  const open = (d: Door) => { opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : null; setDoor(d); };
  const [names, setNames] = React.useState<string[] | null>(null);
  const [namesError, setNamesError] = React.useState<string>();
  const [namesTry, setNamesTry] = React.useState(0);

  // The saved secret names a dialog offers are read when one opens. A read that failed is not an empty list:
  // the dialog opens without names (a tokenless path still works) and says why, with a retry.
  React.useEffect(() => {
    if (!door || door === "host") return;
    let live = true;
    secrets.listSecrets().then(
      (n) => { if (live) { setNames(n); setNamesError(undefined); } },
      (e: unknown) => { if (live) { setNames([]); setNamesError(getErrorMessage(e)); } },
    );
    return () => { live = false; };
  }, [door, namesTry]);
  const namesFailure = namesError ? { message: namesError, retry: () => setNamesTry((n) => n + 1) } : undefined;

  React.useEffect(() => {
    if (door) return;
    const back = opener.current;
    opener.current = null;
    if (!back) return;
    const t = setTimeout(() => {
      const active = document.activeElement;
      if ((!active || active === document.body) && back.isConnected) back.focus();
    }, 0);
    return () => clearTimeout(t);
  }, [door]);

  const edit = (apply: (i: RunIntent) => RunIntent) => dispatch({ type: "edit", apply });
  const close = () => { setDoor(null); setNames(null); setNamesError(undefined); };
  const added = (message: string) => { announce(message); close(); };
  const attach = (ref: ComponentRef) => { edit((i) => attachComponent(i, ref)); added(ref.name ? NR_TOOLS.COMPONENT_ADDED_NAMED(ref.name) : NR_TOOLS.COMPONENT_ADDED); };
  const addSecret = (a: SecretAddition) => {
    if (a.kind === "resident") return attach(a.ref);
    const { host } = a.secret;
    edit((i) => {
      const next = addHeaderSecret(i, a.secret);
      return access?.hosts.some((h) => h.host === host) ? next : addAllowedHost(next, host);
    });
    added(NR_TOOLS.COMPONENT_ADDED);
  };

  const mayDefine = !!mine?.may_define;
  const residentAllowed = !!mine?.resident_delivery_allowed;
  const residentOff = !mine ? undefined : !mayDefine ? sentence("component_unavailable") : CC.ERR.RESIDENT_OFF;
  const attached = (id: string) => !!intent.components?.some((c) => c.id === id);
  const stateOf = (id: string) => access?.rows.find((r) => r.id === id);
  const org = (kind: OrgComponentView["category"]) => (mine?.org ?? []).filter((o) => o.category === kind);
  const show = (c: Exclude<Category, "all">) => category === "all" || category === c;
  // Until the server has said, neither door claims a reason; once it says no, both say why.
  const noDefine = mine && !mayDefine ? ADD_ACCESS.NOT_ALLOWED : undefined;
  const noDefineId = React.useId();
  const failedId = React.useId();

  const cliCards = (mine?.clis ?? []).flatMap((raw: CatalogueCLI) => {
    const cli: CatalogueCLI = raw.kind === "kubectl" && !kubectlOn ? { kind: "kubectl", state: "coming_soon", rows: [] } : raw;
    const generic = CLI_CARD[cli.kind];
    if (cli.state !== "available") return [{ key: cli.kind, title: generic.name, cli, row: undefined as OrgComponentView | undefined }];
    return cli.rows.map((id) => ({ key: id, title: mine?.org.find((o) => o.id === id)?.name ?? generic.name, cli, row: mine?.org.find((o) => o.id === id) }));
  });

  const runFacts: RunFacts = { access, answered };
  // A card the person can still add shows what it brings; one that cannot shows nothing, and one already on a run
  // the server has refused brings nothing to it.
  const breakdown = (on: boolean, reason: string | undefined, brings: ToolAccess): ToolAccess | undefined =>
    !reason ? brings : on ? NOTHING : undefined;

  const orgCard = (row: OrgComponentView) => {
    const residentRow = row.secrets.some((s) => s.delivery.mode !== "header");
    const fact = stateOf(row.id);
    const reason = residentRow && !residentAllowed ? CC.ERR.RESIDENT_OFF : answered ? sentence(fact?.reason) : undefined;
    const on = attached(row.id);
    // A remote MCP server shows what it brings; an API row keeps its chips until its Other line has words.
    const kind = row.category === "mcp" ? mcpAccess(row, on ? runFacts : undefined) : undefined;
    return (
      <ToolCard
        key={row.id}
        icon={row.category === "mcp" ? CARD_ICON.mcp : CARD_ICON.api}
        title={row.name}
        org
        body={row.description || undefined}
        access={kind && breakdown(on, reason, kind)}
        brings={!kind && <Brings hosts={row.hosts} deliveries={row.secrets.map((s) => MODE_DELIVERY[s.delivery.mode])} />}
        look={on ? "added" : reason ? "off" : "plain"}
      >
        {(details) => (
          <RowAction
            name={row.name}
            attached={on}
            reason={reason}
            status={on && fact ? <Status status={fact.status} answered={answered} /> : undefined}
            details={details}
            onAdd={() => { edit((i) => attachComponent(i, { id: row.id })); announce(NR_TOOLS.CARD_ADDED(row.name)); }}
            onRemove={() => { edit((i) => detachComponent(i, row.id)); announce(NR_TOOLS.COMPONENT_REMOVED); }}
          />
        )}
      </ToolCard>
    );
  };

  const removeAdded = (row: AccessRow) => {
    if (row.id.startsWith("host:")) edit((i) => removeAllowedHost(i, row.name));
    else if (row.id.startsWith("secret:")) edit((i) => removeHeaderSecret(i, row.id.slice("secret:".length)));
    else {
      // A stale or repeated press removes nothing, so it says nothing either.
      if (detachComponent(intent, row.id, answeredFor) === intent) return;
      edit((i) => detachComponent(i, row.id, answeredFor));
    }
    announce(NR_TOOLS.COMPONENT_REMOVED);
  };
  const addedRows = access?.rows.filter((r) => r.from === "added") ?? [];

  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-3.5 rounded-xl border border-border bg-card p-[18px]">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-[1_1_20rem]">
          <h2 id={headingId} className="font-semibold">{NR_TOOLS.ADD_TITLE}</h2>
          <p className="text-sm text-muted-foreground">{NR_TOOLS.ADD_BODY}</p>
        </div>
        <div className="flex max-w-[16rem] flex-col items-end gap-1">
          <Button type="button" variant="outline" disabled={!mayDefine} aria-describedby={noDefine ? noDefineId : failed ? failedId : undefined} onClick={() => open("custom")}>
            <Plus aria-hidden />
            {NR_TOOLS.CUSTOM_COMPONENT}
          </Button>
          {noDefine && <Reason text={noDefine} id={noDefineId} />}
        </div>
      </div>
      {failed && <p id={failedId} role="alert" className="text-sm text-danger">{failed}</p>}
      <div role="group" aria-label={NR_TOOLS.CATEGORY_ARIA}>
        <Segmented<Category>
          value={category}
          onChange={setCategory}
          options={[
            { value: "all", label: NR_TOOLS.CATEGORY_ALL },
            { value: "cli", label: NR_TOOLS.CATEGORY_CLI },
            { value: "mcp", label: NR_TOOLS.CATEGORY_MCP },
            { value: "api", label: NR_TOOLS.CATEGORY_API },
            { value: "secret", label: NR_TOOLS.CATEGORY_SECRET },
          ]}
        />
      </div>
      <ul className="grid gap-2.5 [grid-template-columns:repeat(auto-fill,minmax(320px,1fr))]">
        {show("cli") && cliCards.map(({ key, title, cli, row }) => {
          const generic = CLI_CARD[cli.kind];
          const on = !!row && attached(row.id);
          const off = cli.state !== "available";
          const reason = cli.state === "not_granted" ? sentence("cli_not_granted") : answered ? sentence(row ? stateOf(row.id)?.reason : undefined) : undefined;
          const delivery = CLI_DELIVERY[cli.kind];
          const facts = on ? runFacts : undefined;
          // The kubectl and GitHub CLI cards show what they bring; the other CLIs keep their chips for now.
          const kind = row && cli.kind === "kubectl" ? kubectlAccess(row, facts) : row && cli.kind === "github" ? githubAccess(row, githubRepos, facts) : undefined;
          return (
            <ToolCard
              key={key}
              icon={generic.icon}
              title={title}
              body={row?.description || generic.body}
              access={kind && breakdown(on, reason, kind)}
              brings={row && !kind && <Brings hosts={row.hosts} deliveries={delivery ? [delivery] : []} />}
              look={on ? "added" : off || reason ? "off" : "plain"}
            >
              {(details) => cli.state === "coming_soon" ? (
                <Chip>{NR_SHARED.COMING_SOON}</Chip>
              ) : (
                <RowAction
                  name={title}
                  attached={on}
                  reason={reason}
                  status={on && row && stateOf(row.id) ? <Status status={stateOf(row.id)?.status ?? ""} answered={answered} /> : undefined}
                  details={details}
                  onAdd={() => { if (row) edit((i) => attachComponent(i, { id: row.id })); announce(NR_TOOLS.CARD_ADDED(title)); }}
                  onRemove={() => { if (row) edit((i) => detachComponent(i, row.id)); announce(NR_TOOLS.COMPONENT_REMOVED); }}
                />
              )}
            </ToolCard>
          );
        })}
        {show("mcp") && org("mcp").map(orgCard)}
        {show("api") && org("api").map(orgCard)}
        {show("secret") && (
          <>
            <DoorCard icon={CARD_ICON.secret} title={NR_TOOLS.SECRET_CARD_TITLE} body={NR_TOOLS.SECRET_CARD_BODY} button={NR_TOOLS.SECRET_CARD_BUTTON} onOpen={() => open("secret")} />
            <DoorCard icon={CARD_ICON.host} title={NR_TOOLS.HOST_CARD_TITLE} body={NR_TOOLS.HOST_CARD_BODY} button={NR_TOOLS.HOST_CARD_BUTTON} onOpen={() => open("host")} />
            <DoorCard icon={CARD_ICON.mcp} title={NR_TOOLS.MCP_CARD_TITLE} body={localMcp ? NR_TOOLS.MCP_CARD_BODY_LOCAL : NR_TOOLS.MCP_CARD_BODY} button={NR_TOOLS.MCP_CARD_BUTTON} reason={noDefine} onOpen={() => open("mcp")} />
          </>
        )}
      </ul>
      {addedRows.length > 0 && (
        <div className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">{NR_TOOLS.ADDED_HEADING}</h3>
          <ul className="flex flex-col gap-2">
            {addedRows.map((row) => isLocalMcp(row) && access ? (
              <ToolCard key={row.id} icon={CARD_ICON.mcp} title={row.name} sub={NR_TOOLS.SUB_RUN_ONLY} access={localMcpAccess(row, access, intent, runFacts)} look="added">
                {(details) => (
                  <RowAction
                    name={row.name}
                    attached
                    reason={answered ? sentence(row.reason) : undefined}
                    status={<Status status={row.status} answered={answered} />}
                    details={details}
                    onAdd={() => undefined}
                    onRemove={() => removeAdded(row)}
                  />
                )}
              </ToolCard>
            ) : (
              <li key={row.id} className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-surface-2 p-3 text-sm">
                <IconTile mark={addedMark(row)} />
                <span className="min-w-0 flex-1 [overflow-wrap:anywhere]">{row.name}</span>
                <Chip>{FROM_KIND[row.kind] ?? NR_TOOLS.FROM_CUSTOM}</Chip>
                <Status status={row.status} answered={answered} />
                {answered && sentence(row.reason) && <span className="basis-full text-muted-foreground">{sentence(row.reason)}</span>}
                <Button type="button" variant="ghost" size="sm" aria-label={`${NR_TOOLS.ROW_REMOVE} ${row.name}`} onClick={() => removeAdded(row)}>{NR_TOOLS.ROW_REMOVE}</Button>
              </li>
            ))}
          </ul>
        </div>
      )}
      {door === "host" && <AllowHostDialog onAdd={(host) => { edit((i) => addAllowedHost(i, host)); added(NR_TOOLS.COMPONENT_ADDED); }} onClose={close} />}
      {door === "secret" && names && (
        <AddSecretDialog
          names={names}
          namesFailure={namesFailure}
          hosts={access?.hosts.map((h) => h.host) ?? []}
          residentAllowed={residentAllowed}
          mayDefine={mayDefine}
          residentOffReason={residentOff ?? ""}
          onAdd={addSecret}
          onClose={close}
        />
      )}
      {door === "mcp" && names && (
        <AddMcpDialog names={names} namesFailure={namesFailure} localServers={localMcp} residentAllowed={residentAllowed} residentOffReason={residentOff} onAdd={attach} onClose={close} />
      )}
      {door === "custom" && names && <CustomComponentDialog names={names} namesFailure={namesFailure} residentAllowed={residentAllowed} secretsPath={SECRETS_PATH} onAttach={attach} onClose={close} />}
    </section>
  );
}
