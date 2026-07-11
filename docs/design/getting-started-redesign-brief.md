# Getting Started — redesign brief (for the Figma Make design agent)

**Mission:** redesign Wardyn's "Getting started" experience (first-boot welcome + 9-step setup
funnel) so it communicates visually — diagrams, ladders, checklists, comparison tables, icons —
instead of paragraphs. Today every screen is sentences; the stepper is so crunched its labels
truncate. Keep the honest content; change how it's shown.

This brief is self-contained: all content, copy, states, tokens, and process are inline. You do
not need repo access to design — code paths are given for the humans who will implement.

---

## 1. Product context

**Wardyn** is a local-first control plane that runs coding agents (Claude Code, Codex) inside
sandboxes ("barriers") with no resident credentials, brokered short-lived tokens, gated egress,
human approvals, and a full audit trail. Tagline used in-product: **"Let agents work while you
keep your keys."**

**Who sees this screen:** a single operator (developer or platform engineer) who just booted
`wardynd` on their own machine or a team box. They may be on a laptop (WSL2/Docker Desktop —
weakest barrier only), a native Linux box (all three barriers possible), or a sealed
docker-compose deployment. They want to launch their first sandboxed agent run in minutes.

**Tone:** honest security console. The product deliberately never overclaims — warnings stay
amber, residual risks are always named, "Optional" is never dressed as required. Friendly but
precise; no marketing fluff.

**When it appears:** auto-opens on a fresh local console (no runs yet, or not ready) until the
operator finishes, launches a run, or clicks "Finish later". It then stays reachable from a
"Getting started" item pinned at the bottom of the left nav (with a live Ready/Needs-setup chip).
It must never trap the operator — all other nav stays clickable.

---

## 2. Where things live

| Artifact | Location |
|---|---|
| Current-state screenshot | `docs/img/getting-started.png` in the repo |
| Code (implementation target) | `ui/src/app/components/screens/onboarding/` (welcome) + `ui/src/app/components/screens/setup/` (funnel) |
| Original Figma Make base file | fileKey `EXAMPLEFILEKEY00000000` (Opus structure — shadcn/ui + Tailwind v4) |
| Colorway reference Make file | fileKey `EXAMPLEFILEKEY11111111` (Gemini neutral monochrome) |
| Design tokens (source of truth) | `ui/src/styles/theme.css` — mirrored in §4 below |

The live console renders the funnel in a content column of **max-width 860px** (welcome hero:
780px) beside a ~226px sidebar. That 860px is where 9 stepper chips currently crunch.

---

## 3. What's wrong today (the problems you are solving)

1. **The 9-chip stepper is unreadable.** Nine equal-width chips in 860px → every label truncates
   ("Env…", "Mo…", "Ho…", "SC…"). Sub-badges truncate too. Users can't see where they are or
   what's left.
2. **Paragraph soup.** Every step opens with a 2–4 line paragraph; cards contain 3 more
   sentences each. The barrier cards alone carry ~5 lines of prose each. Nothing is scannable.
3. **No graphics at all.** A security product whose core mental model is *concentric isolation
   layers* (fence → wall → vault) has zero diagrams. The "how it works" strip is five text
   boxes with 16px icons. The tier "strength" is three 10px dashes.
4. **Banner stacking.** Intro panel + "You're ready" fast-path banner + stepper + step heading
   all stack before content; on first load the actual task starts ~600px down.
5. **Equal visual weight for unequal steps.** Two steps are essential (Barrier, Model), three
   are corporate-only (Proxy, SCM, Artifacts), two are optional (Workspaces, Credentials), two
   are wrap-up (Review, Launch). The stepper renders all nine identically, so a laptop user
   can't tell the happy path is really: pick barrier → connect model → launch.
6. **Repeated chrome.** Nearly every step has its own "Re-check" button + "checked just now"
   label in a slightly different place.
7. **Text-only empty/status states.** Empty workspaces, review rollup, provider rows — all
   text rows with a chip; no illustration, no visual grouping.

---

## 4. Hard constraints (do not violate)

### 4.1 Design system — Wardyn "Gemini colorway" tokens

Neutral monochrome surfaces (never blue-tinted), teal as the only action color. Light **and**
dark themes are both required; dark is the security-console default in screenshots.

| Token | Light | Dark |
|---|---|---|
| background / card | `#ffffff` / `#ffffff` | `#0a0a0a` / `#111111` |
| surface-2 / muted | `#f4f4f5` | `#1f1f1f` |
| foreground | `#171717` | `#ededed` |
| muted-foreground | `#737373` | `#a3a3a3` |
| border / strong | `#e5e5e5` / `#d4d4d4` | (near-black equivalents) |
| **primary (teal)** | `#0d9488` | `#14b8a6` |
| success (emerald) | `#10b981` | `#10b981` |
| warning (amber) | `#f59e0b` | `#f59e0b` |
| danger (red) | `#ef4444` | `#dc2626` |
| info (blue) | `#3b82f6` | `#3b82f6` |
| agent accent (Claude) | `#d97757` | `#d97757` |

Each semantic color has a `-subtle` 13%-alpha wash for fills. Radius: `0.5rem` base (cards use
10–16px). Fonts: **Inter** (UI) + **JetBrains Mono** (commands, paths, secret names — anything
machine-literal). Icons: lucide, stroke style, typically 14–16px.

**Barrier tier metal ramp** — the tiers have their own reserved colorway (teal is *never* a
tier color, tiers are *never* teal):

| Tier | Ramp | fg (light) | fg (dark) | bg wash |
|---|---|---|---|---|
| Fence (CC1) | bronze | `#8f5825` | `#cf9862` | `rgba(176,114,58,.14)` |
| Wall (CC2) | silver | `#5c6a74` | `#b7c0c7` | `rgba(154,163,171,.18)` |
| Vault (CC3) | gold | `#8a6a12` | (gold equiv.) | `rgba(201,162,39,.16)` |

### 4.2 Honesty rules (product law — copy may move, never soften)

- Every tier explanation carries its residual risk prefixed **"Doesn't stop:"** — never dropped,
  never hidden more than one interaction away (tooltip/expander is acceptable; deletion is not).
- **Grants and warnings are amber, never green.** A capability grant is not a success state.
- Unified status vocabulary (use verbatim): `Ready`, `Needs setup`, `Unavailable here`,
  `Incompatible here` (= hardware can never run it, always with the concrete why), `Checking…`,
  `Connected`, `Unverified`. Don't invent synonyms.
- Button labels predict outcomes: "Show setup command", "Re-check", "Install guide →",
  "Re-check login", "Finish later". No vague "Continue"/"Got it" for actions with effects.
- Never render fabricated readiness: the fast-path banner only appears when a model really is
  connected; "done" checkmarks derive from live probes, not visit-history.
- Credentials step is **always Optional** and excluded from readiness. Workspaces is
  "Optional" (recommended) — neutral, never a red nag.
- The one constant note (keep verbatim, one placement near the tier picker): *"Whatever the
  barrier, every run still gets Wardyn's egress filtering, short-lived brokered credentials,
  human approvals, and full audit — those are set by policy, not the barrier. The barrier only
  decides how strongly the sandbox is walled off from your machine."*

### 4.3 Structural constraints

- The 9 step **ids and labels are frozen** (e2e tests target them): Environment,
  Model/Harness Provider, Host Proxy, SCM Provider, Artifact Redirect, Workspaces, Credentials,
  Review, Launch. You may **group, re-layout, and re-chrome** them freely (phases, rail,
  accordion…) but each step keeps its identity and full label somewhere visible.
- Backend contract is frozen: design must work from the existing `SetupStatus` probe data
  (everything listed in §6). No new server data may be assumed. Purely visual derivations are
  fine.
- Redesign the **content area only**. App shell (top bar, left nav, theme toggle) stays.
- Existing overlays are reused as-is (black boxes you just trigger): Add-secret dialog,
  New-run dialog, Import-workspace wizard (its own 6-step flow), Setup-guide dialog, Tier-matrix
  dialog — though you may restyle Tier-matrix and Setup-guide if you have budget (see §8).
- Accessibility: WCAG AA contrast in both themes; async re-check feedback must have a visible
  (and aria-live) state; selected/pressed states must not be color-only.

---

## 5. The flow, end to end

```
first boot ──► WELCOME (one-time hero)
                 │ "Get set up"            │ "Skip to the console"
                 ▼                          ▼ (funnel stays in nav, auto-reopens while not ready)
               SETUP FUNNEL (9 steps, jump-anywhere, Back/Next linear default)
                 1 Environment (pick barrier)      ← lands here
                 2 Model/Harness Provider
                 3 Host Proxy        (corporate, optional)
                 4 SCM Provider      (corporate, optional)
                 5 Artifact Redirect (corporate, optional)
                 6 Workspaces        (recommended, optional)
                 7 Credentials       (optional, never counts)
                 8 Review  (go/no-go rollup)
                 9 Launch  (→ New-run dialog → console)
               exits: "Finish later" (footer, always) · launching a run · fast-path banner → step 9
```

A ready host can shortcut everything: the **fast-path banner** ("You're ready — launch your
first run now… Vault is up and Claude connected") appears above the stepper whenever the
backend says ready **and** a model is genuinely connected.

---

## 6. Screen-by-screen content inventory (what must exist, in some form)

### 6.0 Welcome hero (one-time)

- Shield mark, H1 **"Let agents work. Keep your keys."**
- One-sentence blurb: *"Wardyn runs your coding agents behind a barrier, with **no resident
  credentials by default and no privileged host access**. Every run gets its own identity; you
  gate the risky moments; everything is audited."*
- **How-it-works pipeline, 5 nodes** (currently 5 text cards with chevrons — prime candidate
  for a real diagram): ① Own identity — "Every run, cryptographically scoped" ② **Behind a
  barrier** — "Fence, Wall, or Vault — you choose" (teal/primary node) ③ Keys stay brokered —
  "Short-lived tokens, never your real keys" ④ **You gate the risky bits** — "Egress and writes
  ask first" (amber node — deliberately a warning tone) ⑤ Everything recorded — "Append-only
  audit; session replay where the runner supports it".
- **Live readiness chips** probed from the host: "This host right now:" + Barrier
  (`Barrier: Wall ready`) / Model (`Model: Claude connected`) / Composer (`Composer: ready`),
  each `Ready`(green) / `Needs setup`(amber) / `Checking…`.
- CTAs: primary **"Get set up — about 2 minutes"** (label flips to "Finish setup" when already
  ready) + outline **"Skip to the console"**.
- Fine print: shown once; everything lives on under "Getting started" in the sidebar.

### 6.1 Environment — "Pick your barrier" (the flagship step)

Three tier cards, weakest→strongest. Everything below comes from live probes:

| | **Fence** (CC1, bronze) | **Wall** (CC2, silver) | **Vault** (CC3, gold) |
|---|---|---|---|
| Tagline | Weakest · runs anywhere | **Default** · runs anywhere Docker does | Strongest · needs KVM hardware |
| Pick when | "Trying Wardyn out, or the code is your own — quickest start, works on any host." | "Real work on real repos — closes the Fence's holes so the agent never touches your kernel." | "Untrusted code or secrets nearby — the strongest box Wardyn can build." |
| **Doesn't stop:** | "A kernel exploit or container escape — it shares your machine's kernel (the 'holes' in the fence)." | "A flaw in the sandbox software itself (rare); it's still not a fully separate machine." | "A flaw in the virtualization layer itself — a very rare hypervisor- or CPU-level escape." |
| Metaphor (tooltip-able) | fence has holes | wall closes the holes, but over/under exists | vault covers every side |
| Mechanism (tooltip) | runc + userns/seccomp/AppArmor | gVisor userspace kernel | Kata microVM (/dev/kvm) |

Per-card live state: `Ready` / `Needs setup` (+ **"Show setup command"** → inline `$ wardyn
setup wall` + copy button + honest doc note) / `Incompatible here` (Vault on KVM-less hosts,
with the concrete hardware reason). Ready cards show substrate provenance ("Running here as
oci/runc") and are **selectable** — exactly one ring+check marks the saved default barrier.
One card carries a **"Recommended"** chip (strongest *compatible* tier, even if not yet
installed). After a re-check that still finds nothing: red line, e.g. *"Still not detected —
gVisor's runsc runtime isn't listed in `docker info` runtimes yet."*

Step extras: "Re-check" + "Checked just now/Ns ago" (top right), the constant-note (§4.2),
footnote "Your pick is saved in this browser as the default barrier for new runs", link
**"Compare all three →"** opening the **tier matrix dialog**:

| Protection | Fence | Wall | Vault |
|---|---|---|---|
| Isolated from your files, processes, network | ✓ | ✓ | ✓ |
| Kernel exploit can't reach host kernel | ✗ | ✓ *caveat* | ✓ |
| Full break-in stays sealed inside | ✗ | ✓ *caveat* | ✓ *caveat* |
| **Where it runs** | Any host | Any Docker host | Needs KVM hardware |

(caveat = qualified yes; tooltip reuses the tier's "Doesn't stop:" line. This matrix is begging
to become the step's hero visual rather than a buried dialog.)

Error state (replaces cards): danger card "No sandbox runner — runs can't launch." + fix line.

### 6.2 Model/Harness Provider — "Connect a model or agent harness"

- Lead (state-dependent): needs *"a stored API key the proxy injects, or a resident CLI
  subscription"*; when connected: *"One connected path is enough — you're already covered by
  {Claude connected}."*
- Two **provider families**, each a card with a Connected/Not-configured chip:
  - **Claude / Anthropic**: rows appear only for *detected* mechanisms — Claude subscription
    (Claude Code CLI; detail comes from the authoritative expiry-aware probe sentence, may read
    EXPIRED), Anthropic API key, AWS Bedrock (region/model in mono + access-key/secret-key
    buttons). Undetected mechanisms collapse to one-click chips after "Set up:" / "Add another
    way:" — Log in to Claude CLI · Add Anthropic API key · Set up AWS Bedrock.
  - **OpenAI / Codex**: OpenAI API key row, Codex CLI row; same collapsed pattern.
  - Row actions: Add key/Edit (→ Add-secret dialog), Install guide → / Re-check login
    (→ Setup-guide dialog: one copy-paste command, e.g. `claude login`, + manual steps).
- **Contextual rescue box** (only when: model undetected ∧ sealed compose ∧ local box):
  explains the sandbox can't see the host's `~/.claude` login; fix is `make setup`.
- Sub-section **"AI Run Composer backends"**: advisory-only backends list (mono rows:
  `name · provider/model · transport · auth` + Ready/Needs-setup + add-key); empty state
  points at `-composer-config`.
- "Refresh detection" link.

### 6.3 Host Proxy — "Corporate host proxy" (optional)

Read-only **detection breakdown** of what Wardyn found on the host (HTTP_PROXY / HTTPS_PROXY /
ALL_PROXY / NO_PROXY, git proxies, per-tool configs, PAC url flagged `manual`, env case
mismatches, masked credential warning) — mono values + source chips. One input: upstream proxy
secret name + Save. Re-check.

### 6.4 SCM Provider — "Source control provider" (optional)

GitHub (App recommended, PAT/SSH over 443) + Azure DevOps; all clones go through the credential
broker. Actions: "Add PAT secret" (naming convention `git-pat-<host-slug>` shown in mono),
self-hosted GHES/ADO host input → removable host pills. Re-check.

### 6.5 Artifact Redirect — "Artifact registry redirection" (optional)

Redirect npm/pip/cargo/maven/go/nuget to a corporate mirror. Existing overrides as removable
mono rows (`npm → https://artifactory…/npm-remote · token: artifactory-token`); add-form:
ecosystem select + base URL + optional token secret ("Injected proxy-side at fetch time — the
sandbox never holds it"). Re-check.

### 6.6 Workspaces — "Onboard a workspace" (recommended/optional)

- Lead: a run attaches an onboarded directory or repo — never a raw host path; ephemeral
  scratch still works with none.
- Empty state: dashed card, "No workspaces onboarded yet." + **"Onboard your first workspace"**
  → Import-workspace wizard (black box: Source → Scan → Configure → Record → Verify → Finalize).
- Row per workspace: name, `repo`/`local dir` + source path (mono), scan-profile summary line
  ("2 languages · 3 secrets needed · postgres, redis · 1 suspected leak"), status chip
  (Ready/Scanning…/other), action (Scan / Resume import). + "Add workspace".

### 6.7 Credentials — "Repo & cloud credentials" (Optional — explicitly badge it)

- Lead: only needed for private repos / cloud accounts; skipping never blocks and never counts
  against readiness.
- **GitHub App** card: status chip, "The broker mints short-lived, scoped tokens from this —
  agents never see the real key.", App ID input + Save, "Add private key (PEM)", Verify.
- **Personal access token** card: for ADO/GitLab; "Add PAT".

### 6.8 Review — "Review readiness" (go/no-go rollup)

All probe checks grouped: **Blocking** (fail) → **Worth a look** (warn) → **Ready** (ok/info),
each group with a count; check rows = status icon + label + detail + "Fix:" hint. Separate
reference group **"About this host"** (permanent platform facts — WSL, KVM…, nothing to do).
Re-check + last-checked. Footer note: the Review only summarizes; each item is fixed on its own
step (jump links).

### 6.9 Launch — "Launch your first run"

- Lead: describe a task in plain language; the composer proposes a safe config you review
  before anything starts.
- **Example run card** clearly stamped `EXAMPLE / Not live config — just to show the shape of a
  run`: Task ("Add a health check endpoint and a unit test for it"), Agent (Claude Code),
  Barrier (Fence chip + "ready now — harden to Wall later"), Mode (Interactive — "You drive; it
  asks before it acts.").
- Primary **"Launch your first run"** (→ New-run dialog) + "Open Runs". If a run already
  happened: green "You've already launched a run on this control plane."

### 6.10 Shared chrome

- Header: "Getting started" + subtitle "Let agents work while you keep your keys." +
  Show/Hide-intro toggle; dismissible intro panel (blurb + 5-node strip — same content as
  Welcome).
- Fast-path banner (conditions in §5).
- Stepper: 9 steps, each with live badge — Environment `Ready · 2 of 3 barriers`/`Needs setup`;
  Provider `Ready · Claude connected`/`Needs setup`; Proxy/SCM/Artifacts `Configured`/`Optional`;
  Workspaces `Ready · N onboarded`/`Optional`; Credentials `Optional` always; Review
  `All essentials ready`/`Needs attention`/`Review what's left`; Launch `First run
  launched`/`Ready to launch`/`Set up the essentials first`. Done-state ✓ derives from live
  probes only.
- Footer: **"Finish later"** + "Come back anytime from Getting started." — Back / **Next: {step}**.

---

## 7. Redesign direction (what good looks like)

You own the visual solution; these are the directions the team wants explored — deviate with
reason, not by accident:

1. **Kill the 9-chip horizontal stepper.** Strongest candidate: a **left vertical rail** inside
   the content area, steps grouped into phases —
   **Essentials** (Barrier, Model) · **Corporate network** (Proxy, SCM, Artifacts — collapsed
   into one group row until expanded, "all optional") · **Your work** (Workspaces, Credentials)
   · **Finish** (Review, Launch). Full labels always visible, live badge per step, phase-level
   progress (e.g. "Essentials 2/2"). The rail widens the usable content column by replacing the
   860px center-cap with a two-column grid.
2. **Make the barrier picker graphical.** The fence/wall/vault ladder *is* the product story.
   Candidates: three-column selectable cards with a real strength meter and metal-ramp
   iconography/illustration per tier (bronze fence with visible gaps → silver wall → gold vault),
   the protection matrix (§6.1) promoted to the primary comparison surface, prose demoted to
   tooltips/expanders. "Doesn't stop:" stays visible per §4.2.
3. **Turn the 5-node how-it-works strip into an actual pipeline diagram** — agent → identity →
   barrier → brokered keys → gated egress/writes → audit trail, one visual, node colors per the
   semantic tones (barrier teal, gating amber). Reuse it in Welcome and the intro panel.
4. **One status bar to rule the re-checks:** a single persistent "host status" strip (last
   checked · Re-check) instead of per-step buttons; per-step re-check only where the probe is
   step-specific (login detection).
5. **Checklist visual language for Review** — make it read like a pre-flight: big go/no-go
   verdict, then grouped items; counts as badges; "About this host" as muted reference footer.
6. **Icons + definition lists over sentences** everywhere: provider rows, proxy breakdown,
   credential cards. Every current paragraph should become: one bold claim (≤8 words) + one
   muted qualifier line, with the rest behind an info affordance.
7. **Real empty states** (workspaces, composer backends): small illustration + one line + one
   action.
8. **Density targets:** step content above the fold at 1440×900 with intro hidden; stepper/rail
   never truncates at 1280; graceful at 1024 (rail collapses to icons+numbers with tooltips).

Anti-goals: don't add steps, don't merge/rename the frozen nine, don't soften honesty copy,
don't introduce a new accent color (tiers use the metal ramp; actions stay teal), don't design
marketing-style hero imagery — this is a working console.

---

## 8. Design against these five host fixtures

Design every screen in all applicable fixtures — they exercise every state:

- **A — Fresh laptop (WSL2/Docker Desktop):** Fence Ready, Wall Needs setup (+command), Vault
  **Incompatible here** (no /dev/kvm, full reason), no model, no workspaces, review has
  warnings, launch gated. *The default screenshot state.*
- **B — Ready host:** Fence+Wall Ready (Wall recommended+selected), Claude subscription
  connected, composer ready, fast-path banner visible, 1 workspace onboarded.
- **C — Sealed compose on a personal box:** model undetected though the operator *is* logged in
  on the host → the `make setup` rescue box on step 2.
- **D — Hardened native box:** all 3 barriers ready (Vault selected), Bedrock configured,
  has_runs=true — everything green, welcome CTA reads "Finish setup".
- **E — Broken:** no sandbox runner at all (danger card replaces tier picker); one provider row
  EXPIRED.

Plus: every screen in **light and dark**; re-check in-flight state; "still not detected" state.

---

## 9. Deliverables

1. **Frames (Make prototype pages):** Welcome; Funnel steps 1–9 (each in its primary fixture +
   the states from §8 that apply); fast-path variant; tier-matrix surface; setup-guide dialog
   (restyle optional); loading ("Checking Wardyn's setup…") and error states. Desktop 1440
   primary, 1280 check, 1024 collapsed-rail variant. Light + dark for every final screen.
2. **A working Make prototype** wired for the happy path: Welcome → Get set up → pick Wall →
   step 2 connect → jump to Launch → New-run stub. Use mocked `SetupStatus` fixtures A–E as
   the prototype's data (toggleable if cheap).
3. **Component inventory delta:** which existing primitives you reused (Chip, StatusChip,
   ConfinementChip, strength strip, buttons, Field) and every new component you introduce
   (rail, phase group, tier illustration, pipeline diagram, status bar), with all variants/states
   enumerated.
4. **Handoff notes per screen:** layout grid, spacing, which copy moved where (especially where
   any "Doesn't stop:" line now renders), interaction specs (selection, expand, re-check
   feedback incl. the aria-live announcement).

## 10. Acceptance checklist (self-review before handing back)

- [ ] All 9 step labels fully readable at 1280 — zero truncation anywhere in the navigation.
- [ ] Essentials-vs-optional hierarchy visible at a glance; a laptop user can see the 3-step
      happy path (barrier → model → launch) immediately.
- [ ] Every tier surface shows its "Doesn't stop:" residual (≤1 interaction away, never gone).
- [ ] Constant-note (§4.2) appears exactly once near the tier picker.
- [ ] No green on grants/warnings; status words only from the fixed vocabulary; button labels
      outcome-true.
- [ ] Fast-path, rescue-box, incompatible-Vault, EXPIRED-subscription, no-runner, empty-workspace
      states all designed — in both themes.
- [ ] Step-1 content (3 tiers + compare) above the fold at 1440×900 with intro hidden.
- [ ] Fewer words on screen than today for every step (paragraphs → claim + qualifier + info
      affordance), with zero honesty content deleted.
- [ ] Selection, focus, and done states distinguishable without color; AA contrast both themes.
- [ ] Prototype clicks through the happy path with fixtures, and every frame is named
      `gs/<step>/<fixture>/<theme>`.

## 11. Working process (end to end)

1. **Discover (½ day):** read this brief fully; open the current screenshot; browse the two Make
   source files (`EXAMPLEFILEKEY00000000` structure, `EXAMPLEFILEKEY11111111` colorway) to
   absorb the existing component style; restate the IA + fixture matrix in your own words and
   flag anything ambiguous **before** drawing.
2. **IA + wireframe pass (gate #1):** low-fi frames for the rail/grouping model, the barrier
   step, and one corporate step, in fixtures A and B. Present rationale (one paragraph, not an
   essay). Wait for owner sign-off — this is the only mandatory gate.
3. **Hi-fi pass:** all screens × applicable fixtures × both themes, on the token set in §4.1.
   Build the pipeline diagram and tier illustrations as reusable components.
4. **Prototype + self-review:** wire the happy path; run the §10 checklist; fix what fails.
5. **Handoff:** deliver §9 items + a short changelog of every place the design deviates from
   this brief and why. Engineering ports it into `ui/src/app/components/screens/{onboarding,setup}/`
   against the frozen `SetupStatus` contract; expect follow-up questions on states you invent.

**Budget guidance:** the barrier step and the navigation model are 60% of the value — spend
your effort there. The three corporate steps can share one template. If time-boxed, ship
fixtures A+B fully and the rest as single-state frames.

---

## Appendix A — the workspace onboarding flow (context, not a redesign target)

The Import-workspace wizard stays a **stubbed black box** in this pass (§4.3), but the
Workspaces step you *are* designing triggers it, resumes it, and renders its outcomes — so you
need its shape. This is how it actually works.

### A.1 Why it exists

A run can only attach an **onboarded** workspace — never a raw host path. Onboarding turns a
directory or repo into a reviewed, least-privilege profile: what languages/tools it uses, which
secret *names* it needs (values are never read), which hosts it may reach, and which setup
commands are approved to run. Runs then reuse that profile.

### A.2 Where it lives

Its own large overlay dialog (`ImportWorkspaceDialog`, ~4xl, 88vh) opened *on top of* the
Getting Started Workspaces step (and the Workspaces screen). It never navigates away — even
the live terminal renders inline — and returns to the caller on close, which reloads the
workspace list. It is **resumable**: reopening for a given workspace jumps the rail to the
step its server-side status implies.

### A.3 The six steps (rail allows backward jumps only)

1. **Source** — "Add a new workspace" opens a small form: Name · Kind (Local directory /
   Repo) · Source (absolute path, or `org/repo`) · Ref (repo only, optional) · Default target
   (optional — where it attaches in the sandbox). Below: "Or resume an in-progress import" —
   cards for every workspace whose status isn't `ready`.
2. **Scan** — auto-fires on a fresh workspace. Deterministic scan of *committed files*:
   languages, package managers, tools, declared secret **names only**, egress hosts, services,
   devcontainer/Dockerfile presence, build-memory hints, and suspected committed-secret
   **leak findings** (path:line + detector only — "the secret value itself is never shown or
   stored"). Local dirs scan inline (seconds); a repo scan runs as a *real governed sandboxed
   run* ("watch it in Runs"), so the pane shows an honest in-flight state, then the results
   panel. Error state offers Rescan.
3. **Configure** — three blocks: a persistent **security chip** (your default barrier +
   "Secrets are brokered — never written into the sandbox"); **Setup commands** — the
   scanner's detected commands as editable rows (stage select install/build/test/lint +
   mono command + include-checkbox), labeled *"Detected from committed files (untrusted)…
   nothing runs until you save"*; and the **needs panel** in actionable mode — declared
   secrets each with an "Add" that opens the standard AddSecretDialog prefilled (a brokered
   one shows a stored-chip), an advisory "Also read in code / CI" group (may be plain
   config, not secrets), services, and the egress tiers (A.5).
4. **Record** — *"Recommended · skippable"*. The operator records **named sessions**
   ("build", "run tests", "drive the agent" — no fixed taxonomy): each opens an **OPEN
   (allow-all-egress) interactive sandbox** with the repo cloned and the model provider
   wired, an embedded terminal, live approvals, and the scan-detected commands as copy-paste
   hints. "Done recording" stops the run; capture happens on termination (closing the panel
   mid-recording is an implicit Done, never a lost session). Each session's **observed
   egress** can be promoted into the approved list — always through the untrusted-content
   confirm. "Save session profile" opens the profile-review drawer. Warnings: no model path
   configured (the agent's calls would fail), and a louder one when the default barrier is
   Fence (open egress on a shared-kernel box). Footer offers both "Skip recording" and
   "Continue to Verify".
5. **Verify** — prove it works **confined** before an agent uses it. Two surfaces share the
   step: (a) *confined replay* — pick a recording and re-run its steps in a locked sandbox
   (default-deny egress limited to the approved set); any off-policy host is **blocked live**
   and one click (+confirm) to approve; (b) *automated verify* — the approved setup commands
   run in a governed sandbox, rendered as a live checklist: pending ("waiting") → running
   (gerund label + streamed log tail) → pass (duration, logs collapsed behind "show output")
   → fail (exit code / "timed out" chip, logs expanded), with a slim "step N of M" progress
   bar. Phase vocabulary: **Not verified / Verifying / Success / Partial / Failed** (Partial =
   some steps passed). On failure, three escalating fixes: a server-classified
   **failure hint** (e.g. missing toolchain — so an exit 127 isn't misread as an egress
   problem); **"Suggest a fix from denied egress"** (lists denied hosts → Approve → re-run);
   and **"Diagnose with AI"** (advisory prose from the composer, human-applied, hidden when
   no composer backend — currently force-hidden). Inline notices: 503 no runner ("you can
   still finalize as configured"), 422 no approved commands (→ back to Configure), 409
   already running.
6. **Finalize** — lock in the reviewed profile ("Runs will reuse it"), with an optional
   checkbox **"Emit devcontainer.json / AGENTS.md"**; after finalizing, the emitted files
   render as copyable blocks ("commit these to keep the environment reproducible") and Done
   closes back to the Workspaces step.

### A.4 Status vocabulary (drives the rows you design on the Workspaces step)

| Status | Chip tone | Resumes at |
|---|---|---|
| `pending_scan` | warning "Pending scan" | Scan |
| `scanning` | info (transient — polled) | Scan |
| `scanned` | info "Scanned" | Configure |
| `building` / `verifying` | info (transient — polled) | Verify |
| `build_error` / `verify_failed` | danger | Verify |
| `error` (scan failed) | danger | Scan |
| `ready` | success "Ready" | Finalize / done |

A mid-recording workspace resumes on Record (recording is not a status; its state lives on
the session results). The Getting-started row's action is status-driven: `ready` → "Scan"
(re-scan), anything else → **"Resume import"**.

### A.5 Egress model (three tiers + one probe)

**Allowed automatically** (scan-derived baseline) · **Approved by you** (removable pills) ·
**Suggested** (scan-proposed, needs approval) — a host only ever appears in its strongest
tier. Plus lazily-fetched **observed-but-denied** hosts from run history ("Check run
history"). Every approval of scan-/run-derived hosts passes the same untrusted-content
confirm dialog — those names come from the workspace's own files or a run's traffic, neither
trusted.

### A.6 Invariants the design must never break

- Secret **names only** — no surface ever displays or stores a secret value; leak findings
  show location + detector only.
- Setup commands are **untrusted until operator-saved**; nothing executes before approval.
- Egress promotion is never one-click-silent — always the confirm gate.
- Logs render server-masked text verbatim; never imply the UI can unmask.
- Record = open sandbox (frame it amber/attention, never as a safe default); Verify =
  confined (this is the reassuring one). The pair is the product's learn-then-prove story.
- Recording is recommended, never required; skipping must stay a first-class exit.

### A.7 What this means for your pass

- The **stub** should show the dialog frame, the 6-step rail (Source → Scan → Configure →
  Record → Verify → Finalize), and the step's one-line purpose — enough for reviewers to
  understand what opens.
- The **Workspaces step** rows must carry: name, kind + source (mono), the scan-profile
  summary ("2 languages · 3 secrets needed · postgres, redis · 1 suspected leak"), the status
  chip from A.4, and the status-driven action (Scan / Resume import). Design the empty state,
  a transient state (Scanning…), a danger state (Verify failed → Resume), and ready.
- If you propose visual language for status/progress here (e.g. a mini 6-segment progress
  glyph per row derived from A.4), keep it derivable from `status` alone — that's all the
  list has.
