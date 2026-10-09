# Documentation style

Rules for every tracked Markdown file. `make lint` checks form and links; `make diagrams` checks visuals.
[Enforcement](#4-enforcement) lists the docs each gate fails.

## 1. Form

### 1.1 Default forms

| Content | Form | Not |
|---|---|---|
| Options, roles, states, fields, reason codes, env vars | Table | Prose list |
| "If X then Y", 2+ branches | Decision table (condition / outcome / where) | Paragraph |
| Steps a reader performs | Numbered list, one action per step, command in a fence | Running text |
| Facts about one thing | Bullets, one fact each | Paragraph |
| Warning, caveat, residual | GitHub alert: `> [!WARNING]`, `> [!NOTE]`, `> [!IMPORTANT]` | Bold sentence in prose |
| Depth few readers need | `<details>` with the conclusion in the summary | Inline |
| Mechanism with 3+ moving parts | Visual (§3) plus a table | 300-word paragraph |
| Why a design is the way it is | Terse bullets in place; `<details>` over 80 words | Essay, or moved elsewhere |

### 1.2 Sizes

| Unit | Cap | Gate |
|---|---|---|
| Sentence | 35 words | `MAX_SENTENCE_WORDS` |
| Paragraph (one block) | 80 words | `MAX_PARAGRAPH_WORDS` |
| List item with its continuation lines | 60 words | `MAX_ITEM_WORDS` |
| Table cell | 40 words | `MAX_CELL_WORDS` |
| Prose share of a page | 35% target; fails above 50% | `MAX_PARAGRAPH_SHARE`, or `share=` in the doc's [budget line](../scripts/doc-form.d/README) |
| Lines between the H1 and the first H2 | 1–5 | `has_summary` |

- Prose share = paragraph, quote and over-cap item lines, over non-blank lines.
- A line that is only an image does not count toward the 1–5 lines between the H1 and the first H2.
- A blockquote is prose. An alert's marker line is exempt from the word count; its body is not.
- YAML front matter that starts on line 1 (between two lines that are exactly `---`) is metadata: it meets no cap, summary rule or share, but its words still count in the prose budget.
- Aim below the caps: sentences ≤ 25 words, paragraphs ≤ 3 sentences.
- No summary line under an H2 unless the section exceeds 150 words and its first line is not already a summary.
- No lead-in or stub lines ("Apply it:", "Result:"). Put the fact in the bullet.

### 1.3 Page length

| Kind | Target (prose words) | Ceiling |
|---|---|---|
| Reference table page | none | none |
| How-to / task page | 800–2,000 | 3,000 |
| Concept page | 1,000–2,500 | 4,000 |
| Multi-task runbook | ≤ 4,000 per H3 | none per file |
| Threat model | ≤ 250 per lettered sub-residual; an unlettered residual counts as one | none per file |
| Front door / index | ≤ 1,500 | 2,000 |

- A residual over its cap stays verbatim inside `<details>` under a ≤ 40-word summary.
- An H3 over its ceiling becomes a task page under `docs/operations/` only when no guard reads it by the parent's path ([Stable headings](#stable-headings)).

### 1.4 Wording

- Plain words: use, run, refuse, stop. No hedging filler ("it should be noted", "essentially").
- Parallel bullets, one fact each; a contrast or condition pair stays together ("X is masked; Y is not").
- Present tense, active voice; second person for procedures, third person for mechanism.
- Numbers, limits, defaults, ports, timeouts: backticked, exact, with the unit.
- A version qualifier that bounds current behaviour stays ("runs that predate 0.8.6"). Release narration becomes a link to the [changelog](../CHANGELOG.md).
- No model, tool or vendor names for the tooling that produced text or visuals. Wardyn's own identifiers (image names, `--agent` values, harness directories) are code and stay in backticks.

### 1.5 Never compressed away

Keep verbatim, whatever the form:

- Security caveats and residual-risk statements; residual numbers (`#NN`), never renumbered.
- Conditions, exceptions, ordering and negations on a fact ("only when", "unless", "never").
- Upgrade, rollback and recovery steps, in order.
- Refusal sentences and console or server copy quoted in a doc.
- Defaults, limits, units, exit codes, reason codes, audit actions, env vars, flags, routes, migration stems.
- Every string a guard pins, markers and backticks included. Whitespace may re-wrap unless the guard reads raw bytes.
- The pins live in `_test.go` doc guards, most under `cmd/wardynd/` and `internal/api/`, and in shell gates under `scripts/`.
- Every table a parser reads: same column count, header row and first-cell shape.
- Code fences, byte-identical.

A pinned sentence that reads badly stays verbatim in an alert, with a summary above it.

### 1.6 Compression

Only the first two rules below remove content. Nothing else is removed or relocated.

- A fact appears once, at its home; every other doc links there.
- A history note goes when nobody upgrading still needs it. A version note stays when it bounds current behaviour, is an upgrade or rollback fact, or is pinned.
- Rationale stays in place, terse; never move it to an appendix.
- The gate counts prose words: outside code fences, minus table pipes and list markers. `wc -w` never gates.
- A budget line (`path=N share=S`) fails a doc whose prose words exceed `N` or whose prose share exceeds `S` percent. No budget line means no cap.
- A rewrite sets `N` to its before-count minus one, so growth fails.

## 2. Linking

### 2.1 Rules

- Every reference to a repo file is a relative Markdown link once the doc is in the must-link list.
- The link gate checks only a tracked path that has a directory part and ends in `.go` `.md` `.sh` `.ts` `.tsx` `.yaml` `.yml` `.json` `.sql` `.toml` `.py` `.mjs` `.css` `.html` `.example`, or is named `Makefile`, `Dockerfile`, `LICENSE` or `NOTICE`.
- Link other tracked files too; the gate skips them, fenced code and headings.
- Code is cited by symbol, never by line. Shape: `` [`internal/api/runs.go#Server.createRun`](../internal/api/runs.go) ``; methods are `Type.Method`.
- Doc sections are cited by heading anchor: `[Second user, same host](OPERATIONS.md#second-user-same-host)`, never "see §X".
- Anchors follow GitHub's slugger: lowercase; drop every character that is not a letter, digit, space, hyphen or underscore; spaces become hyphens. Nothing is collapsed or trimmed, so " — " becomes `--`. A repeated heading gets `-1`, `-2`.
- Headings carry no emphasis markers and no links: `_x_` or `**x**` changes the slug.
- No `github.com/…/blob/…` URLs for in-repo targets, except text printed at runtime, which pins `main` or a tag.
- No leading `/`. `./` only for a same-directory target that would otherwise read as a bare word.
- Code spans, not references: commands, flags, bare file names (`main.go`), directories (`scripts/`), runtime paths (`/etc/wardyn`) and the doc's own path.
- A link whose text is a backtick span with a directory part that resolves to a tracked path must target that same path.
- A span that names a tracked directory may link to that directory or to the `README.md` directly inside it, and to no other file.
- The backticked path is repo-relative; the target adds one `../` per directory level of the writing file. The gate catches a wrong depth.

### 2.2 Threat-model citations

The citation guard ([`cmd/wardynd/citation_guard_test.go`](../cmd/wardynd/citation_guard_test.go)) needs three things. Every backticked `.go` path exists. An adjacent `` `Symbol` `` + `` `path.go` `` pair resolves. At least one path and one pair exist in the file.

- Write `` `Symbol` in [`internal/x/f.go`](../internal/x/f.go) `` or `` ([`internal/x/f.go`](../internal/x/f.go), `Symbol`) ``.
- Keep the spans adjacent, separated by one of `,` `—` `-` `in` `is in` `lives in` `, see` `:`.
- Never put the symbol and the path in different table cells.
- Never write `` `path.go#Symbol` `` in the threat model: the guard cannot see that span. [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md) is the exception; it uses that span, link-wrapped, in each table row after the action.

## 3. Visuals

- A visual replaces ≥ 150 words of mechanism prose, or shows an order, a boundary or a containment a table cannot. One idea per visual.
- A visual never carries a fact the text does not also state in checkable form.
- Every label is real text in a committed file (SVG `<text>`, or a Mermaid label), never pixels. Labels that name code match the symbol byte for byte; `make diagrams` checks them.
- Alt text on every image: one sentence on what it shows.
- Light and dark: `<picture>` with a `prefers-color-scheme: dark` source and two files, because an SVG shown as an image ignores the page's colours.
- `docs/img/<id>.svg` is the source; a `.webp` render sits beside it only when GitHub cannot render the visual. One row per visual in the [image index](img/README.md).
- A dark-only `.webp` figure is shown as built and its source is kept outside the repository; review checks its labels and alt text, and the diagram gate checks only that its alt text is present.
- An SVG shown through `<img>` or `<picture>` loads no external file: fonts, textures and icons are inline (`data:` URIs) or absent.
- No foreign copyright or metadata line in an SVG. No hostnames, tokens, account or customer data in any visual.
- No new Mermaid fences. An existing fence stays only until its SVG replacement ships.

## 4. Enforcement

| Check | Script | `make` target | Fails on |
|---|---|---|---|
| Form, prose budget and share | [`scripts/doc-form.sh`](../scripts/doc-form.sh) | `lint` | Docs a `.list` names (the strict tier); budget: docs with a `.budget` line |
| Links resolve (file and anchor); file references are links | [`scripts/check-doc-links.sh`](../scripts/check-doc-links.sh) | `doc-links`, also in `lint` | Every tracked `.md` except the frozen files; must-link: docs a `.links` names |
| Diagram syntax and style, label truth, alt text, SVG location | [`scripts/check-diagrams.sh`](../scripts/check-diagrams.sh) | `diagrams` | Docs in its `DOCS` list |
| Citation shape and resolution | [`cmd/wardynd/citation_guard_test.go`](../cmd/wardynd/citation_guard_test.go) | `test` | The threat model docs and [USERS.md](USERS.md) |

- Each script's header comment defines its rule. [doc-form.d](../scripts/doc-form.d/README) holds the lists: asserted docs (`.list`), budgets (`.budget`), must-link docs (`.links`).
- Ten `docs/operations/` pages named in the form script keep their older checks until a `.list` names them. No other doc fails the caps.
- A doc that gains a visual joins `DOCS` in the same change; until then no gate checks its alt text or labels.
- Dead links in [CHANGELOG.md](../CHANGELOG.md), [CHANGELOG-ARCHIVE.md](../CHANGELOG-ARCHIVE.md) and the third-party notices are counted, never failed. `docs/design/` is exempt from must-link.
- A `:NNN` suffix after a `.go`, `.md`, `.ts` or `.sh` name is a line citation: it fails in a must-link doc and warns elsewhere.
- A guard change never weakens its check: state what it protected and how the new text still does.

## Stable headings

Code, scripts and other docs quote these headings and strings. Changing one updates the quoting code in the same change.

[OPERATIONS.md](OPERATIONS.md):

| Heading | Quoted by |
|---|---|
| [Multi-user: who can change what](OPERATIONS.md#multi-user-who-can-change-what) | [`docs/README.md`](README.md), [`scripts/setup.sh`](../scripts/setup.sh), [`internal/api/runs_create_validate.go`](../internal/api/runs_create_validate.go) |
| [Who decides who gets in: chart vs console vs IdP](OPERATIONS.md#who-decides-who-gets-in-chart-vs-console-vs-idp) | [the Kubernetes setup skill](../.claude/skills/wardyn-k8s-setup/SKILL.md), [`deploy/helm/wardyn/README.md`](../deploy/helm/wardyn/README.md) |
| [Three roles, and who sets the walls](OPERATIONS.md#three-roles-and-who-sets-the-walls) | [`docs/README.md`](README.md) |
| `**What admin-only still means**` and its 2-column table | [`internal/api/operations_tier_doc_test.go`](../internal/api/operations_tier_doc_test.go) |
| [Every denial that isn't a 404](OPERATIONS.md#every-denial-that-isnt-a-404), with its rows keyed by `reason` | [`internal/api/authz_denied_doc_test.go`](../internal/api/authz_denied_doc_test.go) |
| [Second user, same host](OPERATIONS.md#second-user-same-host) | [`install.sh`](../install.sh), [`scripts/test-claims-match-code.sh`](../scripts/test-claims-match-code.sh) |
| [Upgrading a one-line install](OPERATIONS.md#upgrading-a-one-line-install) | [`install.sh`](../install.sh), [`scripts/test-install-sh-trust.sh`](../scripts/test-install-sh-trust.sh) |
| [Stopped-writer upgrade](OPERATIONS.md#stopped-writer-upgrade) | [`cmd/wardynd/boot_flags.go`](../cmd/wardynd/boot_flags.go), [`cmd/wardynd/migrate_only.go`](../cmd/wardynd/migrate_only.go) |
| [High availability](OPERATIONS.md#high-availability) | [`cmd/wardynd/single_instance.go`](../cmd/wardynd/single_instance.go), [`cmd/wardynd/boot_posture.go`](../cmd/wardynd/boot_posture.go), [`deploy/helm/wardyn/values.yaml`](../deploy/helm/wardyn/values.yaml), [`deploy/helm/wardyn/README.md`](../deploy/helm/wardyn/README.md) |
| [Leavers and SCIM](OPERATIONS.md#leavers-and-scim) | [`ui/src/app/lib/scim-copy.ts`](../ui/src/app/lib/scim-copy.ts), [`deploy/helm/wardyn/README.md`](../deploy/helm/wardyn/README.md) |
| The page path, no heading | [`ui/src/app/lib/tier-picker-copy.ts`](../ui/src/app/lib/tier-picker-copy.ts) |
| [Network: upstream proxy and egress redirects](OPERATIONS.md#network-upstream-proxy-and-egress-redirects) | [`deploy/helm/wardyn/README.md`](../deploy/helm/wardyn/README.md), [`deploy/helm/wardyn/templates/deployment.yaml`](../deploy/helm/wardyn/templates/deployment.yaml), [`cmd/wardynd/docs_ops_guard_test.go`](../cmd/wardynd/docs_ops_guard_test.go), the env examples in [`deploy/desktop/`](../deploy/desktop/) |
| [Retention, erasure and GDPR — a residual, not a solved problem](OPERATIONS.md#retention-erasure-and-gdpr--a-residual-not-a-solved-problem) | [`docs/README.md`](README.md), [`cmd/wardynd/docs_ops_guard_test.go`](../cmd/wardynd/docs_ops_guard_test.go) |
| [Renamed in 0.8](OPERATIONS.md#renamed-in-08), with its `Go:` rows | [`scripts/test-repo-guards.sh`](../scripts/test-repo-guards.sh) |
| [Restore them](OPERATIONS.md#restore-them), [Recovery set by deployment](OPERATIONS.md#recovery-set-by-deployment) | [`cmd/wardynd/docs_ops_guard_test.go`](../cmd/wardynd/docs_ops_guard_test.go), [`cmd/wardynd/recovery_set_doc_test.go`](../cmd/wardynd/recovery_set_doc_test.go) |
| [Splitting the migrator and app roles](OPERATIONS.md#splitting-the-migrator-and-app-roles-wardyn_pg_migrate_dsn) | [`cmd/wardynd/migrate_dsn_doc_test.go`](../cmd/wardynd/migrate_dsn_doc_test.go) |
| [User drives on Docker](OPERATIONS.md#user-drives-on-docker), [User drives on Kubernetes](OPERATIONS.md#user-drives-on-kubernetes) | [`cmd/wardynd/drive_object_name_doc_test.go`](../cmd/wardynd/drive_object_name_doc_test.go) |
| [Testing AWS SSO without an AWS tenant](OPERATIONS.md#testing-aws-sso-without-an-aws-tenant) | [`CONTRIBUTING.md`](../CONTRIBUTING.md) |
| [Upgrades](OPERATIONS.md#upgrades) | [`cmd/wardynd/docs_ops_guard_test.go`](../cmd/wardynd/docs_ops_guard_test.go) |

Other docs:

| Doc | Heading or string | Quoted by |
|---|---|---|
| [POLICIES.md](POLICIES.md) | [Bound the token itself: a GitHub ruleset](POLICIES.md#bound-the-token-itself-a-github-ruleset) | [`internal/broker/broker.go`](../internal/broker/broker.go), [`internal/api/setup_checks.go`](../internal/api/setup_checks.go) |
| [POLICIES.md](POLICIES.md) | The 11 anchors the console's field help links to: `top-level`, `first_use_approval-modes`, `eligible_grants--grantspec`, `workspace_mounts--workspacemount`, `workspace_repos--workspacerepo`, `llm_inspection--llminspectionspec`, `resources--resourcelimits`, `ui_apps--uiapp`, `tool_rules--toolrule`, `git_push_any_branch-the-per-run-opt-out`, `push_rules--pushrulesspec` | [`ui/src/app/components/wardyn/policy-field-help.ts`](../ui/src/app/components/wardyn/policy-field-help.ts) |
| [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md) | One [Renamed in 0.8](AUDIT-ACTIONS.md#renamed-in-08); [Grammar](AUDIT-ACTIONS.md#grammar) and its `**Verbs:**` line | [`cmd/wardynd/audit_actions_doc_guard_test.go`](../cmd/wardynd/audit_actions_doc_guard_test.go) |
| [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md) | The `Action` and `rule_source` table headers; rows `authz.denied`, `ui.open`, `llm.scan.*`, `builtin:private-ip`, `builtin:dial-failed` | [`cmd/wardynd/audit_actions_doc_guard_test.go`](../cmd/wardynd/audit_actions_doc_guard_test.go), [`internal/api/authz_denied_doc_test.go`](../internal/api/authz_denied_doc_test.go), [`cmd/wardynd/docs_r3_guard_test.go`](../cmd/wardynd/docs_r3_guard_test.go) |
| [sdk.md](sdk.md) | Each `Reason` / `Meaning` table, up to the next `## ` | [`internal/api/reason_docs_guard_test.go`](../internal/api/reason_docs_guard_test.go) |
| [SSH.md](SSH.md) | [Bounds](SSH.md#bounds) | [`ui/src/app/components/screens/ssh-keys.tsx`](../ui/src/app/components/screens/ssh-keys.tsx), [`internal/api/sshgateway.go`](../internal/api/sshgateway.go), [`internal/store/store_apitokens.go`](../internal/store/store_apitokens.go) |
| [VERIFY.md](VERIFY.md) | Numbered headings `## 1.` and `## 6.` | [`install.sh`](../install.sh), [`scripts/up.sh`](../scripts/up.sh) |
| [AZURE-DEVOPS.md](AZURE-DEVOPS.md) | [How pushes work](AZURE-DEVOPS.md#how-pushes-work), [The app registration](AZURE-DEVOPS.md#the-app-registration), [Choosing how people connect](AZURE-DEVOPS.md#choosing-how-people-connect), [Upgrading](AZURE-DEVOPS.md#upgrading), [What bounds a token-creating credential](AZURE-DEVOPS.md#what-bounds-a-token-creating-credential) | [`docs/POLICIES.md`](POLICIES.md), [`docs/ENV.md`](ENV.md), [`deploy/azure-entra-sso/README.md`](../deploy/azure-entra-sso/README.md), the page's own links |
| [Helm README](../deploy/helm/wardyn/README.md) | [Split SSH exposure](../deploy/helm/wardyn/README.md#split-ssh-exposure) | [the Kubernetes setup skill](../.claude/skills/wardyn-k8s-setup/SKILL.md) |
| [THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) | Prefixes `### CC3 —`, `### 4.2 The unconditional IP guard` | [`cmd/wardynd/threatmodel_claims_guard_test.go`](../cmd/wardynd/threatmodel_claims_guard_test.go), [`cmd/wardynd/docs_r3_guard_test.go`](../cmd/wardynd/docs_r3_guard_test.go) |
| [THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) | `Resident-secret exceptions`, up to ``**`ssh_key` and `git_pat` ``; a line starting `Nine closed kinds` (a count word) | [`cmd/wardynd/threatmodel_claims_guard_test.go`](../cmd/wardynd/threatmodel_claims_guard_test.go) |
| [THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) | Section number `5.1a`; [Console auth token storage](../threatmodel/THREAT-MODEL.md#console-auth-token-storage) | [`ARCHITECTURE.md`](../ARCHITECTURE.md), [`docs/DATA-FLOW.md`](DATA-FLOW.md), [`docs/DESKTOP.md`](DESKTOP.md), [`docs/POLICIES.md`](POLICIES.md), [`cmd/wardynd/threatmodel_claims_guard_test.go`](../cmd/wardynd/threatmodel_claims_guard_test.go) |
| [THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) | Residual numbers (`#NN`) | [`scripts/up.sh`](../scripts/up.sh), [`scripts/test-install-sh-trust.sh`](../scripts/test-install-sh-trust.sh), [`Makefile`](../Makefile), [`cmd/wardynd/threatmodel_claims_guard_test.go`](../cmd/wardynd/threatmodel_claims_guard_test.go) |
| [THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) | `` explicit kill** path (`handleKillRun`) runs this fixed order: ``, a blank line, then a list opening `1. **` | [`internal/api/killorder_doc_test.go`](../internal/api/killorder_doc_test.go) |
| [AGENT-THREAT-MODEL.md](../threatmodel/AGENT-THREAT-MODEL.md) | The row whose first cell is `14` | [`cmd/wardynd/docs_086_review_doc_test.go`](../cmd/wardynd/docs_086_review_doc_test.go) |
| [ARCHITECTURE.md](../ARCHITECTURE.md) | `authoritative, complete table`, then any table up to the indented `Secret values are masked` | [`cmd/wardynd/threatmodel_claims_guard_test.go`](../cmd/wardynd/threatmodel_claims_guard_test.go) |
| [CONTRIBUTING.md](../CONTRIBUTING.md) | [Branching, issues and pull requests](../CONTRIBUTING.md#branching-issues-and-pull-requests) | [`docs/README.md`](README.md), [`RELEASING.md`](../RELEASING.md) |
| [RELEASING.md](../RELEASING.md) | [How a release is prepared](../RELEASING.md#how-a-release-is-prepared) | [`docs/README.md`](README.md) |
| [RELEASING.md](../RELEASING.md) | `` ci.yml` job list: `` and the job names up to the first ` — ` | [`cmd/wardynd/releasing_joblist_guard_test.go`](../cmd/wardynd/releasing_joblist_guard_test.go) |
| [RELEASING.md](../RELEASING.md) | The `for img in …; do` line; the `"contexts": [` array | [`cmd/wardynd/published_images_guard_test.go`](../cmd/wardynd/published_images_guard_test.go), [`scripts/test-claims-match-code.sh`](../scripts/test-claims-match-code.sh) |
| [integrations.md](operations/integrations.md) | [Azure Foundry (`azure_foundry`)](operations/integrations.md#azure-foundry-azure_foundry); the row that starts with `azure_foundry` | [`cmd/wardynd/docs_086_review_doc_test.go`](../cmd/wardynd/docs_086_review_doc_test.go) |
