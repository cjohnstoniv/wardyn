<!--
Copyright 2025 The Wardyn Authors
SPDX-License-Identifier: Apache-2.0
-->

# UI batch 3 — mock round (F236/ADV3-05, F159/F190, R6 F001)

Mock round for three console strings the 0.7 release-candidate reviews found to be
**untrue as written** — each one tells an operator to run something that either does not
work, or works by doing damage. No behavior change in any of the three: the surfaces,
their trigger conditions and their affordances are byte-identical, only the words move.
Same owner law as `docs/design/ui-batch2-mock.md` — the mock is the source of truth, and
the canon strings below become app strings verbatim.

Read against: R5 `F236` (+ its deduped twin `ADV3-05`), R5 `F159`/`F190`, R6 `F001`;
`docs/OPERATIONS.md` §"`helm upgrade`, and why `--wait` is not optional" (the documented,
runnable upgrade recipe); `deploy/helm/wardyn/values.yaml`'s `secrets:` block and
`deploy/helm/wardyn/templates/secret.yaml`'s age-identity refusals (the Secret-backed
options the chart actually offers); `docs/design/CONSOLE-RULES.md` §10 ("never overclaim",
"canonical strings are the app's strings").

**Where the canon lives for these three.** Two of them are *server*-authored console
strings: `SetupCheck.Fix` is composed in `internal/api/setup_checks.go` and rendered
verbatim by the console (`CheckRow` in `step-bodies.tsx`, and `corp-network-proxy.tsx`),
by `wardyn setup status` in the terminal, and by nothing else — there is no `copy.ts`
table between the two, so this document is their canon, the same way
`ui-batch2-mock.md`'s D9 row is canon for an inline `run-detail-summary-header.tsx`
string. The third is an inline `ui/src` string with the same shape. None of the three
belongs to a `<screen>-prompt.md` §7 table: the setup checklist and the Runs first-run
screen have never had a prompt round. Adding one is out of this batch's remit; a future
setup-screen prompt should fold these rows in rather than re-freeze them.

**No screenshot in this batch is re-shot.** `docs/img/runs-board.png` shows the R6 `F001`
banner with its old command and needs the demo stack to re-shoot — carried as a release
item (H6), not done here.

---

## F236 / ADV3-05 — the Helm "Fix" hints print a runnable command

**Placement.** Three sites, one defect, all rendering a `helm ... --set` invocation an
operator is meant to copy:

1. `runnerCheck`'s k8s-driver arm (`internal/api/setup_checks.go`) — the CC1-only
   "Sandbox runner" row's `Fix`.
2. `confinementFloorCheck`'s k8s addendum (same file) — appended to the "Confinement
   floor" row's `Fix`.
3. `K8S_TIER_GUIDES.CC2`/`.CC3`'s `command`
   (`ui/src/app/components/screens/setup/setup-guide.tsx`) — rendered as `$ {command}`
   in `EnvironmentStep`'s revealed command panel, **beside a copy-to-clipboard button**,
   which is what makes an unrunnable string here worse than a merely-terse one.

All three printed `helm upgrade --set k8s.runtimeClasses.CC2=<name>`. Two things are
wrong with it, and they compound:

- **It does not run.** `helm upgrade` takes two positional arguments (release and chart).
  The string as printed exits `Error: "helm upgrade" requires 2 arguments`.
- **The shape it teaches is the one `OPERATIONS.md` documents as destructive.** A bare
  `--set` with no `-f` makes Helm reset every other value to chart defaults — dropping
  exactly the values a Wardyn install cannot run without (`auth.adminToken.*`,
  `k8s.proxyImage`, `serviceAccount.automount`, `secrets.ageKeyFromSecret`). The chart
  refuses that render rather than shipping a crippled install, so the operator who fixes
  the missing arguments by hand walks straight into a second, more confusing failure.

The corrected form is the one `OPERATIONS.md` already documents and ran against the
`make kind-quickstart` cluster: namespace, release, chart, **and the values file
re-passed**. `--reset-then-reuse-values` (Helm ≥ 3.14) is the accepted alternative for an
operator with no values file, and the guard below takes either.

### ASCII mock — Review step check row, k8s driver (before → after)

```
BEFORE                                                  AFTER
┌ ⓘ Sandbox runner ─────────────────────┐               ┌ ⓘ Sandbox runner ─────────────────────┐
│ Only the Fence tier (weakest — a       │               │ Only the Fence tier (weakest — a       │
│ shared-kernel container) is available  │               │ shared-kernel container) is available  │
│ on this host; runs work but with the   │       →       │ on this host; runs work but with the   │
│ lowest isolation.                       │               │ lowest isolation.                       │
│ Fix: … pin it with `helm upgrade       │               │ Fix: … pin it with `helm -n <namespace>│
│ --set k8s.runtimeClasses.CC2=<name>`   │               │ upgrade <release> ./deploy/helm/wardyn │
│ (or `.CC3=<name>`).                     │               │ -f your-values.yaml --set              │
│         ^ exits: requires 2 arguments   │               │ k8s.runtimeClasses.CC2=<name>` (or     │
└─────────────────────────────────────────┘               │ `.CC3=<name>`). Re-pass your values    │
                                                          │ file — a bare `--set` resets every     │
                                                          │ other value to the chart's defaults.   │
                                                          └─────────────────────────────────────────┘
```

### ASCII mock — EnvironmentStep revealed command panel, k8s driver (before → after)

```
BEFORE                                                  AFTER
┌ Enable the Wall tier ─────────────────┐               ┌ Enable the Wall tier ─────────────────┐
│ $ helm upgrade --set                   │               │ $ helm -n <namespace> upgrade <release>│
│   k8s.runtimeClasses.CC2=…  ...   [⧉]  │       →       │   ./deploy/helm/wardyn -f your-values… │
│                                         │               │                              [⧉]       │
│ Register a gVisor RuntimeClass …        │               │ Register a gVisor RuntimeClass … Pass  │
└─────────────────────────────────────────┘               │ your values file: a bare `--set` …     │
      ^ the [⧉] copies a command that                     └─────────────────────────────────────────┘
        cannot run                                              ^ the [⧉] now copies a whole command
```

The trailing `` ... `` is dropped from both `command` strings: it read as "and whatever
else you need", but this control exists to be copied, and an ellipsis pasted into a shell
is a syntax error rather than a hint.

### Canon strings — `internal/api/setup_checks.go` (server-composed `SetupCheck.Fix`; no `copy.ts` table — the console renders the wire value verbatim)

| id | current string | new string |
|---|---|---|
| `f236:runner-cc1-k8s-fix` | ``Unlock the Wall or Vault tier: register a gVisor (or Kata) RuntimeClass in the cluster, then pin it with `helm upgrade --set k8s.runtimeClasses.CC2=<name>` (or `.CC3=<name>`).`` | ``Unlock the Wall or Vault tier: register a gVisor (or Kata) RuntimeClass in the cluster, then pin it with `helm -n <namespace> upgrade <release> ./deploy/helm/wardyn -f your-values.yaml --set k8s.runtimeClasses.CC2=<name>` (or `.CC3=<name>`). Re-pass your values file — a bare `--set` resets every other value to the chart's defaults.`` |
| `f236:confinement-floor-k8s-suffix` | `` Or register the floor's RuntimeClass in the cluster and pin it: helm upgrade --set k8s.runtimeClasses.%s=<name>.`` | `` Or register the floor's RuntimeClass in the cluster and pin it: helm -n <namespace> upgrade <release> ./deploy/helm/wardyn -f your-values.yaml --set k8s.runtimeClasses.%s=<name> (pass your values file — a bare --set resets everything else to chart defaults).`` |

`%s` is the floor class (`CC2`/`CC3`), interpolated by `confinementFloorCheck`; the
sentence is appended to the docker-shaped advice that precedes it, hence the leading
space and lower-case "Or".

### Canon strings — `ui/src/app/components/screens/setup/setup-guide.tsx` (`K8S_TIER_GUIDES`)

| id | current string | new string |
|---|---|---|
| `f236:k8s-guide-cc2-command` | `helm upgrade --set k8s.runtimeClasses.CC2=<runtimeclass-name> ...` | `helm -n <namespace> upgrade <release> ./deploy/helm/wardyn -f your-values.yaml --set k8s.runtimeClasses.CC2=<runtimeclass-name>` |
| `f236:k8s-guide-cc3-command` | `helm upgrade --set k8s.runtimeClasses.CC3=<runtimeclass-name> ...` | `helm -n <namespace> upgrade <release> ./deploy/helm/wardyn -f your-values.yaml --set k8s.runtimeClasses.CC3=<runtimeclass-name>` |
| `f236:k8s-guide-cc2-docnote` | `Register a gVisor RuntimeClass in the cluster (its object name is operator-chosen — Wardyn can't guess it), then pin it here. Then Re-check.` | `Register a gVisor RuntimeClass in the cluster (its object name is operator-chosen — Wardyn can't guess it), then pin it here. Pass your values file: a bare `--set` resets every other value to the chart's defaults. Then Re-check.` |
| `f236:k8s-guide-cc3-docnote` | `Register a Kata RuntimeClass on a KVM-capable node pool, then pin it here. Then Re-check.` | `Register a Kata RuntimeClass on a KVM-capable node pool, then pin it here. Pass your values file: a bare `--set` resets every other value to the chart's defaults. Then Re-check.` |

### States covered

- **docker driver:** unchanged in all three sites — `runnerCheck`'s docker arm keeps
  `wardyn setup wall`, and `TIER_GUIDES` (not `K8S_TIER_GUIDES`) is what
  `EnvironmentStep` hands down. Only the k8s branches move.
- **k8s driver, CC1-only:** the Sandbox-runner row and both tier guides carry the new
  command.
- **k8s driver, floor not advertised:** the Confinement-floor row's addendum carries it.
- **Member session:** the setup checklist is operator-only (`/setup/status` redacts
  `checks` for a member entirely), so no member-visible string moves.
- **Regression guard:** every `SetupCheck.Fix` naming Helm with a `--set` must also
  carry `-f ` or `--reset-then-reuse-values`, and none may contain the bare
  `helm upgrade --set` shape — so a future check cannot reintroduce it quietly.

---

## F159 / F190 — the age-key "Fix" steers the master key into a plaintext literal

**Placement.** `ageKeyCheck` (`internal/api/setup_checks.go`) — the "Secret store
durability" row's `Fix`, shown whenever the secret store is running on an ephemeral,
boot-generated age identity.

The row's diagnosis is right and stays: an ephemeral key means every stored secret (API
keys, GitHub App credentials) is unreadable after a restart. The **remedy** was wrong for
the one deployment shape that most needs it. It said:

> Generate a durable key with `wardynd -gen-age-key` and set it as WARDYN_AGE_KEY (helm:
> env.WARDYN_AGE_KEY; or -age-key).

`env.WARDYN_AGE_KEY` renders the secret store's **master key** as a plaintext literal in
the Deployment object — readable by anything with `get deploy`, captured in every
`helm get manifest`, and in whatever the cluster's manifests are stored in. The chart
offers two Secret-backed doors instead, and the console's own advice must name them:

- `secrets.ageKeyFromSecret: true` — an `age-key` entry inside the Secret
  `postgres.dsn.secretRef` already names (the chart's own refusal text for a persistent
  external DSN with no identity wired points here: *"add an `age-key` entry to that Secret
  (`wardynd -gen-age-key`) and set secrets.ageKeyFromSecret=true"*).
- `secrets.ageKeySecretRef.name` (+ `.key`, default `age-key`) — a separate,
  chart-unmanaged Secret, for an operator-owned or Postgres-operator-managed Secret that
  would reject an extra key.

They are mutually exclusive, and the chart refuses a render naming two — so the Fix names
them as an either/or, in the chart's own order, and never both at once. The host-side
answer (`-age-key`, or the env var on a bare binary where there is no Deployment object to
leak into) is kept: it is correct there, and it is the shape the compose stack and
`install.sh` already write into `~/.wardyn/.env`.

### ASCII mock — Review step check row (before → after)

```
BEFORE                                                  AFTER
┌ ⚠ Secret store durability ────────────┐               ┌ ⚠ Secret store durability ────────────┐
│ The secret store uses an EPHEMERAL age │               │ The secret store uses an EPHEMERAL age │
│ key generated at boot; stored secrets  │               │ key generated at boot; stored secrets  │
│ … become unreadable after a restart.   │       →       │ … become unreadable after a restart.   │
│ Fix: Generate a durable key with       │               │ Fix: Generate a durable key with       │
│ `wardynd -gen-age-key` and set it as   │               │ `wardynd -gen-age-key`, then wire it   │
│ WARDYN_AGE_KEY (helm:                   │               │ as WARDYN_AGE_KEY: on a host, -age-key │
│ env.WARDYN_AGE_KEY; or -age-key).       │               │ or the env var; on Helm, keep it in a  │
│              ^ a plaintext master key   │               │ Secret — secrets.ageKeyFromSecret=true │
│                in the Deployment        │               │ (an `age-key` entry in the Secret      │
└─────────────────────────────────────────┘               │ postgres.dsn.secretRef names) or       │
                                                          │ secrets.ageKeySecretRef.name for a     │
                                                          │ separate one. Not env.WARDYN_AGE_KEY — │
                                                          │ that renders the master key as a       │
                                                          │ plaintext literal in the Deployment.   │
                                                          └─────────────────────────────────────────┘
```

Longer than the string it replaces, deliberately: this row is a `warn` an operator acts on
once, the two Helm keys are not guessable from the env-var name, and "not X" without
naming X is how the old advice survived three reviews. It stays one paragraph in the same
`text-xs` block — no new component, no disclosure.

### Canon strings — `internal/api/setup_checks.go` (`ageKeyCheck`)

| id | current string | new string |
|---|---|---|
| `f159:age-key-fix` | ``Generate a durable key with `wardynd -gen-age-key` and set it as WARDYN_AGE_KEY (helm: env.WARDYN_AGE_KEY; or -age-key).`` | ``Generate a durable key with `wardynd -gen-age-key`, then wire it as WARDYN_AGE_KEY: on a host, -age-key or the env var; on Helm, keep it in a Secret — secrets.ageKeyFromSecret=true (an `age-key` entry in the Secret postgres.dsn.secretRef names) or secrets.ageKeySecretRef.name for a separate one. Not env.WARDYN_AGE_KEY — that renders the master key as a plaintext literal in the Deployment.`` |

### States covered

- **Durable key (the `ok` arm):** unchanged, and carries no `Fix` at all — `CheckRow`
  renders a `Fix` only on a non-`ok` row.
- **Ephemeral key, host/compose install:** the host half of the sentence answers it
  (`-age-key` or the env var), same advice as before.
- **Ephemeral key, Helm install:** now steered to a Secret, either door, never the
  Deployment literal.
- **Member session:** operator-only row, as above.

---

## R6 F001 — the no-barrier banner names a subcommand that does not exist

**Placement.** `NoBarrierBanner` (`ui/src/app/components/screens/runs-first-run.tsx`) —
the one hard, non-dismissible blocker in the product, shown on `/runs` when
`confinement_classes` comes back empty on a reachable daemon.

It offered `sudo wardyn setup fence` as the fix, in a `Mono` chip with a
copy-to-clipboard button beside it. Two independent reasons that string can never be
true, which is why no alias fixes it:

- **There is no `fence` subcommand and there cannot be one.** `wardyn setup` has
  `status`, `detect-proxy`, `proxy-relay`, `wall` and `vault`. Wall and Vault exist
  because there is a runtime to install (gVisor's `runsc`, Kata). CC1 "Fence" is the
  **baseline** tier — an ordinary container on the shared kernel. Nothing installs it;
  it is what you already have the moment a container runtime answers.
- **The banner's actual trigger is not a missing tier.** It fires on
  `confinement_classes.length === 0`, which on a reachable daemon means the runner
  reported no classes at all — Docker unreachable, the daemon built without
  `-tags docker`, or started with `-runner none`. "Install Fence" is not the diagnosis
  for any of those.

`sudo` is wrong twice over: the CLI talks to the daemon over its API with the operator's
token, so root does nothing, and running it as root reads the *root* user's config rather
than the operator's.

The honest command is `wardyn setup status` — which exists, needs no root, and prints
exactly this host's checklist with the next command beside each unmet item, including the
Sandbox-runner row whose `Fix` (`F236` above, or the docker-driver arm) is the real next
step. The banner keeps its shape: same `Mono` chip, same `CopyButton`, same Re-check
button, same non-dismissible `role="alert"`.

### ASCII mock — `/runs`, no-barrier blocker (before → after)

```
BEFORE
┌ 🛡✗ No sandbox barrier on this host. Runs cannot start. ──────────────────────┐
│   [ sudo wardyn setup fence  ⧉ ]   [ ↻ Re-check ]                             │
└───────────────────────────────────────────────────────────────────────────────┘
        ^ `Error: unknown command "fence" for "wardyn setup"` — and sudo would
          not have helped if it had existed

AFTER
┌ 🛡✗ No sandbox barrier on this host. Runs cannot start. ──────────────────────┐
│   [ wardyn setup status  ⧉ ]       [ ↻ Re-check ]                             │
└───────────────────────────────────────────────────────────────────────────────┘
        ^ prints this host's checklist, each unmet row with its own next command
```

The headline is unchanged: "No sandbox barrier on this host. Runs cannot start." is true
of every trigger of this banner — it says what is missing and what that costs, and makes
no claim about the cause. Only the command moves.

### Canon strings — `runs-first-run.tsx` (inline, no `copy.ts` table needed: one render site, one string)

| id | current string | new string |
|---|---|---|
| `r6f001:no-barrier-command` | `sudo wardyn setup fence` | `wardyn setup status` |
| `r6f001:no-barrier-headline` | `No sandbox barrier on this host. Runs cannot start.` | *(unchanged)* |
| `r6f001:no-barrier-copy-label` | `Copy setup command` | *(unchanged — still a setup command, still copied)* |

### States covered

- **Reachable daemon, zero confinement classes:** banner renders with the new command;
  everything else on the screen is unchanged (the board still renders underneath — the
  banner sits above it, it does not replace the screen).
- **Unreachable daemon (`READY_FALLBACK`):** banner does not render at all, unchanged —
  a connectivity fact must never read as a host-config one.
- **Any live class:** banner absent, unchanged.
- **Re-check / poll:** unchanged. `/setup/status` is polled only while this blocker is
  up, so the banner still clears on its own once a runner comes back; Re-check just
  fires the poll early.
- **Screenshot:** `docs/img/runs-board.png` still shows the old command. Re-shooting it
  needs the demo stack — carried as release item **H6**, not part of this batch.
