# Mock prompt — UI sandboxes: the run-detail third lane

Design brief for Workstream D's console affordance (0.6, pillar 4). This is a
**mock-round artifact**: the mock and the strings table below are the source of
truth for the later UI stage (D3), not a description of code that exists. No
`ui/` code lands in this stage.

Companion mock: [`ui-sandboxes-mock/index.html`](ui-sandboxes-mock/index.html)
— static, self-contained, opens in a browser, shows every state named here.

Read first: [docs/SSH.md](../SSH.md) (the lane this one sits next to and copies
its honesty rules from) and the "Attach from your terminal" card it extends,
`ui/src/app/components/screens/run-detail-ssh.tsx`.

## 1. What is being added

A run's policy may declare **UI apps** — a name, a loopback port and a path
inside the sandbox (`RunPolicySpec.ui_apps`). Wardyn can relay one of them to
the operator's browser over the existing exec lane, on a **second listener**
(`WARDYN_UI_SANDBOX_LISTEN`), and the console needs one honest affordance for
"open it".

That affordance is a **third lane inside the existing `ConnectSSHCard`** —
after `Wardyn CLI` and `SSH`, same card, same section-card chrome. It is not a
new widget and not a new run-detail panel: the run-detail layout's widget id
set (`runLayoutWidgetIDs`) is closed server-side, and this lane needs none of
it.

What the lane must convey, in this order: what it does, that it opens on a
**different address** than the console and why, and that **nothing you do in
the app is recorded**.

## 2. Where it lands (exact seams)

| Thing | Seam |
|---|---|
| The card | `ui/src/app/components/screens/run-detail-ssh.tsx` → `ConnectSSHCard` |
| Lane position | after the `SSH` block's `<div className="mt-4 border-t border-border pt-3">`, as a sibling block with the same divider treatment |
| Card-level gate (unchanged) | `owned && running` — the card already returns `null` otherwise |
| Enablement probe | `GET /healthz` → `ui_sandbox` block (mirrors the existing `ssh` block, `internal/api/sshgateway.go` `sshGatewayHealthz`) |
| Declared apps | the run's **effective** policy `ui_apps`, read off the run payload (`GET /runs/{id}`) as a read-only denormalization — same pattern as `workspace_ids`. `AgentRun` carries only `policy_id` today, and an inline or default policy has no id to fetch, so the console must not resolve this through `GET /policies/{id}`. **D1.2 owns exposing it.** |
| Ticket | existing `POST /runs/{id}/attach-ticket` (owner-or-admin, single-use, 30s TTL) |
| Open | `window.open('<ui-origin>/__wardyn/enter?run=<run-id>&app=<name>&ticket=<t>', '_blank', 'noopener')` |
| Policies screen row | `ui/src/app/components/screens/policies.tsx` → `PolicyDetail`, read-only |

The UI origin comes from the `ui_sandbox` healthz block, never from
`window.location` — the whole point is that it is a different origin. Whether
that block advertises a single `base_url` or a per-run host template
(`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`) is **D1.5's call**; the console reads
whatever one field it publishes and never builds the origin itself.

## 3. States to draw

The card is owner-only and RUNNING-only already; the lane inherits that gate
and adds nothing to it. Inside a rendered card, the lane has five states.

| # | Condition | Renders |
|---|---|---|
| S0 | not the run's owner, **or** run not `RUNNING` | nothing — the whole card is absent (existing behavior; the lane must not weaken it) |
| S1 | `healthz.ui_sandbox` absent/disabled | heading + one off line naming `WARDYN_UI_SANDBOX_LISTEN` |
| S2 | enabled, run declares no `ui_apps` | heading + one line naming the policy field |
| S3 | enabled, ≥1 declared app | heading + intro + one row per app (name, `localhost:<port><path>`, **Open** button) + new-tab/origin note + no-recording notice |
| S4 | an **Open** click in flight | that row's button reads `Opening…` and is disabled; other rows stay clickable |
| S5 | the open failed | S3 plus an inline error block under the row that failed (warning tone, mirrors the card's existing `border-warning/30 bg-warning-subtle` block) |

S5's leading case is the **BYOI missing-launcher** one: Wardyn starts an app by
exec'ing `/usr/local/bin/wardyn-ui-<name>` in the sandbox, and an operator's own
image will not have it. The 502 names the path; the console shows a short
lead-in plus the fix, and prints the server's message verbatim underneath —
never a paraphrase, never a swallowed error.

S0 is deliberately *hidden*, not *disabled*: the console does not render a
control the server would refuse (the card's own rule). Note that this is one
notch tighter than the ticket endpoint, which is owner-**or-admin** — an admin
looking at someone else's run sees no lane, exactly as they see no SSH lane
today. Keep it that way; loosening it is a separate decision with a threat-model
row, not a UI tweak.

## 4. Interaction

1. Click **Open {app}** → button → `Opening…`, disabled.
2. `POST /runs/{id}/attach-ticket` → single-use ticket.
3. `window.open` the enter URL on the UI origin, `_blank`, `noopener`.
4. Button returns to **Open {app}**. No polling, no embedded iframe, no
   progress bar — the new tab is the feedback.
5. Any failure in 2 or 3 → S5 under that row; the other rows are untouched.

Never embed the app in this page — an `<iframe>` on the console origin is the
exact attack the second listener exists to prevent.

## 5. Policies screen (D3.2)

One **read-only** row in `PolicyDetail`, next to the egress/lifecycle facts:
label `UI apps`, value either `None declared` or the declared apps rendered as
`{name} → localhost:{port}{path}`, comma-joined. No editor control in 0.6 —
`ui_apps` is operator-authored (API/YAML). Do not add a create/edit form.

## 6. Visual rules

- Reuse `SectionCard`, `CodeBlock`, `Mono`, `Button`, and the card's existing
  type scale (`0.75rem` lane heading, `0.7188rem` body) verbatim. No new sizes.
- Divider between lanes: `mt-4 border-t border-border pt-3`, as SSH's block does.
- Theme tokens only (`--foreground`, `--muted-foreground`, `--primary`,
  `--warning`, `--warning-subtle`); light and dark both legible.
- Env var names, ports, paths and file paths render in `Mono`.
- No icons beyond the card's existing one. No status dot, no badge, no spinner.
- The no-recording notice is body text in the lane, not a tooltip and not a
  collapsed `<details>` — an honesty claim a human has to open is not made.

## 7. Canonical strings (FROZEN)

These are the app strings. The implementation imports/renders them byte for
byte; the mock renders the same bytes; D3's vitest asserts them. `{app}`,
`{port}`, `{path}` are interpolations, everything else is literal.

| ID | Where | String |
|---|---|---|
| `lane.title` | lane heading | `UI apps` |
| `lane.intro` | S3, under the heading | `Wardyn relays a port the sandbox is already listening on to your browser. The sandbox gets no network of its own — the relay rides the same exec lane the terminal does.` |
| `lane.app.sub` | S3, per app | `localhost:{port}{path}` |
| `lane.cta` | S3/S5, per app | `Open {app}` |
| `lane.cta.busy` | S4 | `Opening…` |
| `lane.newtab` | S3, under the app rows | `Opens in a new tab, on a different address than this console. That separation is deliberate: the app is the sandbox's own code, and it must never be able to read your console session.` |
| `lane.norecording` | S3, last line of the lane | `Session recording does not capture this: no keystrokes, no screen, no page content. Wardyn records that you opened and closed the app, never what you did in it.` |
| `lane.off` | S1 | `Off on this deployment. It relays a declared loopback port inside the sandbox — a code editor, a dev server — to your browser through Wardyn. An operator turns it on by setting WARDYN_UI_SANDBOX_LISTEN where wardynd starts.` |
| `lane.noapps` | S2 | `On for this deployment, but this run's policy declares no UI apps. The relay serves only ports named in the policy's ui_apps list — an app is a name, a loopback port and a path.` |
| `lane.error.title` | S5 | `Couldn't start {app}` |
| `lane.error.launcher` | S5, missing-launcher case | `This image has no /usr/local/bin/wardyn-ui-{app}. Use an image that ships the launcher (deploy/images/vscode/), or add one to your own image.` |
| `lane.hidden.notowner` | S0 | — (nothing rendered; the card returns `null` for a non-owner) |
| `lane.hidden.notrunning` | S0 | — (nothing rendered; the card returns `null` unless `run.state === "RUNNING"`) |
| `policy.uiapps.label` | policies detail | `UI apps` |
| `policy.uiapps.none` | policies detail | `None declared` |
| `policy.uiapps.value` | policies detail, per app | `{app} → localhost:{port}{path}` |

Server-side counterpart, frozen with them because the console prints it
verbatim under `lane.error.launcher` (D1.4 owns the 502 body):

```
no UI launcher in this image: /usr/local/bin/wardyn-ui-{app} not found
```

Two strings above are load-bearing beyond copy review and must not be softened
without a threat-model pass: `lane.newtab` (states the second-origin residual)
and `lane.norecording` (states the recording bound `ui.open`/`ui.close` audit
actions are designed around).

## 8. Out of scope for this mock

Terminal/attach behavior, the recording picker, VNC/desktop lanes, an in-page
iframe viewer, `ui_apps` editing, per-app auth, and anything that would put
sandbox-authored content on the console origin.

## 9. What D3 checks against this file

- `lane.title`, `lane.cta`, `lane.off`, `lane.noapps`, `lane.norecording` and
  `lane.error.launcher` byte-match the mock.
- Non-owner → nothing rendered. Non-RUNNING → nothing rendered.
  *(0.6 amendment, after this round: an ADMIN is not a "non-owner" here. The
  server's three lanes are all owner-or-admin — attach_ticket.go's isOperator,
  uigateway.go's role check, sshgateway.go's admin arm — so hiding the card
  from an admin offered less than the API serves. Owner-or-admin → rendered;
  everyone else → nothing rendered.)*
- `ui_sandbox` absent from healthz → S1, and the rendered text contains
  `WARDYN_UI_SANDBOX_LISTEN`.
- `git diff --stat -- ui/` is empty for **this** stage.

`WARDYN_UI_SANDBOX_LISTEN` and `WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE` are named
here as design inputs; their [docs/ENV.md](../ENV.md) rows land with the code
that reads them (D5.3).
