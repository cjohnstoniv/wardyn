# New Run: canon strings

Every string the New Run flow shows lives once in this file and once in the lazy copy modules under `ui/src/app/components/wardyn/copy/new-run/`. `ui/src/app/lib/new-run-copy.test.ts` parses §7 back and compares each row with its module constant, byte for byte, in both directions.

## 1. Sources

| Source | What it is |
|---|---|
| Approved mock | The owner-approved New Run mock: Start, the Guided steps, the run document editor, Review, Save as, Name and Launched. |
| Tools and access prototype | The approved step 5 prototype. |
| Templates prototype | The approved Templates screen prototype. |
| Deviation tables | The owner-approved changes to the mock's wording: the Files wording W1-W5, the placement wording package, the harness and scheduling copy, the run document format, the pool words (organization and user pools; self-hosted and remote-hosted runners). |
| Pending wording | The recommended text for each new wording row, behind one constant until the owner answers. |

## 2. Behaviour summary

| Area | Rule |
|---|---|
| Steps | Shape, Run, Harnesses, Files, Tools and access, then Review. The eyebrow reads "Guided setup · N of 5". |
| Start | Three paths meet at Review: AI composer (Coming soon), Guided setup, and the YAML or JSON run document. |
| Template and Run again | Choosing one loads the saved setup and opens Review. Anything missing is a blocker that links to its step. |
| Review | Blockers come first: local issues, unsupported document fields, template setup items, the server refusal, missing setup items, then a stale check. Launch stays shut until a check under 60 seconds old matches the draft. |
| Name dialog | Opening it runs a fresh check. "Checked just now" shows only from that result; "Checking…" shows while it runs. |
| Launched | Starting shows an indeterminate bar. Waiting shows the place in line, the time waiting and when the run gives up. |
| Disabled options | Every disabled option shows its reason as visible text or an InfoTip. |

## 3. How to read section 7

| Column | Meaning |
|---|---|
| Key | The constant's name in the screen's namespace. A key with arguments is a function; the test calls it with each argument written as `{name}`. |
| Source | `mock` (approved mock or prototype, verbatim), `deviation` (owner-approved change, cites its table), `pending` (recommended text until the owner answers, cites its row), `server` (pointer only). |
| Frozen string | The exact text. Backticks mark mono display and are not part of the string. |

- A `server` row names the Go reason that owns the sentence. The console shows the server's sentence, so no constant exists for it.
- Step lanes import these constants and never add strings. A missing string goes to the lead.
- `REF_NOT_CHECKED` (W-14), `SHARED_DISCLOSURE` (W-25) and `MCP_ASKS` (W-44) are lane-composed: Part B gives no recommended text for them, so they need owner wording.

### Display rules

Some constants show only under a condition. A step lane applies the rule; `new-run-copy.test.ts` checks that each constant named here exists.

- `NR_TOOLS.PROVIDER_PROXY` shows only when the provider credential's delivery is `proxy`: key, token and subscription kinds. A provider whose delivery is `sandbox_short_lived` (Bedrock SSO, whose role credentials sit in the sandbox) shows `NR_TOOLS.DELIVERY_SHORT_LIVED` and `NR_TOOLS.DELIVERY_AWS` instead.
- `NR_SHARED.CONTINUE_TO` writes the step name in lower case ("Continue to harnesses", "Continue to files": Make `App.tsx`:1928, 2144). Every Guided step's primary button, `NR_SHARED.CONTINUE` and the document's `NR_DOCUMENT.CONTINUE_REVIEW` end in an arrow shown to sight only (Make `App.tsx`:1293, 1928, 2144, 2577, 2695); `NR_REVIEW.LAUNCH_RUN` has none.
- `NR_LAUNCH.CANCEL_RUN` opens the console's kill confirm before it cancels the waiting run; the confirm's own strings are the shared kill dialog's. Deviation (owner 2026-10-13: keep the Cancel-run confirm); Make cancels at once (`App.tsx`:3335-3343).
- `NR_TOOLS.EFFECT_ALLOW` is both the default option and a rule effect ("Run it" stays, per the tool-rule ruling): offered when the policy allows it, otherwise disabled with the server's reason.

## 4. Never shown

| Text | Why |
|---|---|
| Scenario names, the scenario switcher, the header and theme strings | Mock scaffolding, not product copy. |
| "NFS", "Project", and any `/mnt/...` path | No server concept stands behind them. |
| A "(team)" suffix on a model provider | Providers show their record names. |
| "Platform team" as an approver | No team routing exists. An approval sentence says "An admin decides each new host." |
| "Custom harness" as a literal row | The harness roster comes from the server. |
| The mock's interactive "kept until its 72-hour maximum lifetime" line and "the main drive is kept until its lifetime ends" | False for runners that keep a drive for a grace period only. |

## 7. Frozen strings

### 7.1 Start

| Key | Source | Frozen string |
|---|---|---|
| `EYEBROW` | mock | Launch a governed workload |
| `TITLE` | mock | New run |
| `LEDE` | mock | Choose how you want to shape this run. Every path creates the same draft and meets again at review. |
| `AI_TITLE` | mock | AI composer |
| `AI_BODY` | mock | Create policies, sandboxes and runs with an agent's help. |
| `AI_FOOT` | mock | Describe what you need |
| `GUIDED_TITLE` | mock | Guided setup |
| `GUIDED_BADGE` | mock | Fastest |
| `GUIDED_BODY` | mock | Answer a few plain questions. Your organisation's defaults take care of the rest. |
| `GUIDED_FOOT` | mock | Start guided setup |
| `DOC_TITLE` | mock | YAML / JSON |
| `DOC_BADGE` | mock | Advanced |
| `DOC_BODY` | mock | Edit or import a schema-aware run document and see what the server will allow. |
| `DOC_YAML` | mock | YAML |
| `DOC_JSON` | mock | JSON |
| `TRUST_LINE` | mock | Policy and availability are checked again when you launch. |

### 7.2 Shape

| Key | Source | Frozen string |
|---|---|---|
| `TITLE` | mock | What are you running? |
| `LEDE` | mock | Pick the closest shape. You can adjust every detail before launch. |
| `START_FROM` | mock | Start from |
| `SOURCE_SCRATCH` | mock | From scratch |
| `SOURCE_TEMPLATE` | mock | Saved template |
| `SOURCE_AGAIN` | mock | Run again |
| `TEMPLATE_LABEL` | mock | Template |
| `TEMPLATE_OPTION(name, scope)` | deviation (DS-2 console touchpoints) | {name} · {scope} |
| `PREVIOUS_LABEL` | mock | Previous run |
| `AGAIN_OPTION(title, when)` | deviation (DS-2 console touchpoints) | {title} · {when} |
| `AGAIN_NOTE` | pending (W-01) | Settings are rechecked now. Short-lived credentials are issued fresh; standing approvals still apply. |
| `EMPTY_TEMPLATES` | pending (W-23) | No saved templates yet. |
| `EMPTY_AGAIN` | pending (W-23) | No previous runs to start again. |
| `AGENT_TITLE` | mock | Agent task |
| `AGENT_BODY` | mock | A one-time coding task, driven by you or running autonomously. |
| `AGENT_SUB` | mock | Interactive · Autonomous · Repeat / schedule coming soon |
| `ENV_TITLE` | mock | Environment sandbox |
| `ENV_BODY` | mock | A reusable workspace for development, analysis or investigation. |
| `ENV_SUB` | pending (A18) | Interactive · Your drive is attached when you have one |
| `APP_TITLE` | mock | Application sandbox |
| `APP_BODY` | mock | Run a sandboxed command, automation or script on its own. |
| `APP_SUB` | mock | Automation / script · Application / server coming soon |

### 7.3 Run

| Key | Source | Frozen string |
|---|---|---|
| `TITLE_AGENT` | mock | Choose how and where it runs |
| `TITLE_ENV` | mock | Choose where the environment runs |
| `TITLE_APP` | mock | Choose where the automation runs |
| `LEDE_ORG` | pending (W-05) | Your organisation's defaults are selected. |
| `LEDE_PERSONAL` | pending (W-05) | Your defaults are selected. |
| `LEDE_NONE` | pending (W-05) | Choose where it runs. |
| `MODE_LABEL` | mock | How should it run? |
| `MODE_INTERACTIVE` | mock | Interactive |
| `MODE_INTERACTIVE_BODY` | pending (W-02) | You work in the sandbox; one harness can start with it. |
| `MODE_AUTONOMOUS` | mock | Autonomous |
| `MODE_AUTONOMOUS_BODY` | deviation (DS-1 truthful copy) | An agent runs on its own, with no SSH or terminal. |
| `TIMING_LABEL` | mock | When should it run? |
| `TIMING_NOW` | mock | Run Now |
| `TIMING_NOW_BODY` | mock | Start as soon as you launch the run. |
| `TIMING_LATER` | mock | Schedule For Later |
| `TIMING_LATER_BODY` | mock | Choose a future date and time for this run to start. |
| `APP_LABEL` | mock | What should run? |
| `APP_AUTOMATION` | mock | Automation / script |
| `APP_AUTOMATION_BODY` | mock | A command or job that runs on its own. |
| `APP_AUTOMATION_META` | mock | Available now |
| `APP_SERVER` | mock | Application / server |
| `APP_SERVER_BODY` | mock | App servers and MCP servers. |
| `COMMAND_LABEL` | mock | Command |
| `COMMAND_HELP` | mock | Run verbatim in the sandbox. The run ends when the command exits. |
| `WHERE_LABEL` | mock | Where should it run? |
| `POOL_LABEL` | mock | Pool |
| `POOL_HELP` | deviation (DS-5 §8) | A pool is required. Pools you can use stay visible, with the reason when one can't take this run. |
| `POOL_OPTION_REASON(pool, reason)` | mock | {pool} · {reason} |
| `REASON_NO_ONLINE` | mock | No online runners |
| `REASON_SWITCHED_OFF` | pending (W-03) | Switched off |
| `REASON_NO_RUNNERS` | pending (W-03) | No runners |
| `REASON_BACKGROUND_ONLY` | pending (W-03) | Background only |
| `REASON_INTERACTIVE_ONLY` | pending (W-03) | Interactive only |
| `REASON_NOT_ALLOWED_PROFILE` | pending (W-03) | Not allowed by your profile |
| `REASON_RUNNERS_OFF` | pending (W-03) | Runners are switched off |
| `PLACEMENT_DENIED(profile, kind)` | pending (W-03) | {profile} doesn't allow runs on {kind} pools. |
| `CAPACITY_LABEL` | mock | Total pool capacity |
| `CAPACITY_ARIA` | mock | Total pool capacity used |
| `USED_OF_MAX(used, max)` | mock | {used} of {max} running |
| `RUNNING_ONLY(used)` | deviation (DS-5 §8) | {used} running |
| `MAX_RUNS(max)` | mock | {max} runs maximum in this pool |
| `POOL_LIMIT_FROM_RUNNERS` | pending (W-50) | Set by its runners |
| `ALLOWANCE_LABEL` | mock | Your allowance |
| `ALLOWANCE_ARIA` | deviation (DS-5 §8) | Your allowance used |
| `ALLOWANCE_USED(active, max)` | deviation (DS-5 §8) | {active} of {max} running (all pools) |
| `ALLOWANCE_INCLUDES(active)` | deviation (DS-5 §8) | Includes your {active} current runs in any pool |
| `ALLOWANCE_INCLUDES_ONE` | deviation (DS-5 §8) | Includes your 1 current run in any pool |
| `NO_PERSONAL_LIMIT` | deviation (DS-5 §8) | No personal limit |
| `WAITING_LABEL` | mock | Waiting |
| `IN_QUEUE(n)` | mock | {n} in queue |
| `NO_RUNS_WAITING` | mock | No runs waiting |
| `JOIN_QUEUE` | mock | New runs join the queue |
| `START_WHEN_LAUNCHED` | mock | A run can start when launched |
| `RUN_TYPES_LABEL` | mock | Run types |
| `RUN_TYPES_BOTH` | mock | Interactive and background |
| `RUN_TYPES_BACKGROUND` | mock | Background only |
| `RUN_TYPES_INTERACTIVE` | mock | Interactive only |
| `SET_BY_POOL` | deviation (DS-5 §8) | Set by this pool |
| `NO_RESTRICTION` | deviation (DS-5 §8) | No restriction |
| `SET_BY(source)` | deviation (DS-5 §8) | Set by {source} |
| `SET_BY_RUNNERS` | pending (W-03) | Set by your runners |
| `SET_BY_ORG` | pending (W-03) | Set by your organisation |
| `LIMITS_LABEL` | mock | Per-run limits |
| `LIMITS_NOTE(profile)` | deviation (DS-5 §8) | Strictest of this pool, {profile} and this server applies |
| `LIMITS_NOTE_UNGOVERNED` | deviation (DS-5 §8) | Strictest of this pool and this server |
| `LIMITS_NOTE_ORG` | pending (W-03) | Strictest of this pool, your organisation and this server applies |
| `LIMIT_CPU` | mock | CPU |
| `LIMIT_MEMORY` | mock | Memory |
| `LIMIT_LIFETIME` | mock | Lifetime |
| `LIMIT_MAIN_DRIVE` | mock | Main drive |
| `LIMIT_MAXIMUM` | mock | Maximum per run |
| `LIMIT_EXTRA_DRIVES` | mock | Extra allowed drives can still be attached |
| `LIMIT_NO_MAXIMUM` | pending (W-03) | No maximum |
| `BARRIER_LABEL` | mock | Barrier |
| `COMPARE_BARRIERS` | mock | Compare barriers |
| `TAGLINE_FENCE` | mock | Weakest |
| `TAGLINE_WALL` | mock | Stronger |
| `TAGLINE_VAULT` | mock | Strongest |
| `STRENGTH_ARIA(n)` | mock | Barrier strength {n} of 3 |
| `ABOUT_ARIA(barrier)` | mock | About {barrier} |
| `PROTECTS_HEAD` | mock | Protects against |
| `NOT_PROTECTS_HEAD` | mock | Doesn't protect against |
| `HOW_HEAD` | mock | How it works |
| `FENCE_PROTECTS` | mock | The agent seeing or touching the rest of your machine — it gets its own files, processes, and network. |
| `FENCE_NOT_PROTECTS` | mock | A kernel exploit or container escape — it shares your machine's kernel (the “holes” in the fence). |
| `FENCE_HOW` | mock | Shared-kernel container (runc) hardened with user namespaces, seccomp, AppArmor, and capability drop. |
| `WALL_PROTECTS` | mock | Everything Fence does, plus most kernel exploits — a software kernel (gVisor) handles the agent's system calls so it never touches your real kernel (the holes are closed). |
| `WALL_NOT_PROTECTS` | mock | A flaw in the sandbox software itself (rare); it's still not a fully separate machine. |
| `WALL_HOW` | mock | gVisor userspace kernel intercepts syscalls — the default; needs a native Docker engine with its runsc runtime registered (a one-time setup step). |
| `VAULT_PROTECTS` | mock | Everything Wall does, plus a full break-in staying trapped — its own hardware-walled VM with its own kernel; even total takeover inside stays inside. |
| `VAULT_NOT_PROTECTS` | mock | A flaw in the virtualization layer itself — a very rare hypervisor- or CPU-level escape. |
| `VAULT_HOW` | mock | Kata microVM — hardware-virtualized, with its own guest kernel (requires /dev/kvm). |
| `ROW_ISOLATION` | mock | Isolated from your files, processes, and network |
| `ROW_KERNEL` | mock | A kernel exploit can't reach your host kernel |
| `ROW_BREAK_IN` | mock | A full break-in stays sealed inside |
| `ROW_WHERE` | mock | Where it runs |
| `WHERE_FENCE` | mock | Any host |
| `WHERE_WALL` | mock | Native Docker engine + setup |
| `WHERE_VAULT` | mock | Needs KVM hardware |
| `STATUS_YES` | mock | Yes |
| `STATUS_NO` | mock | No |
| `STATUS_CAVEAT` | mock | With a caveat |
| `CAVEAT_EXCEPT(text)` | mock | Except: {text} |
| `BADGE_ORG_DEFAULT` | mock | Organisation default |
| `BADGE_DEFAULT` | deviation (DS-5 §8) | Default |
| `BADGE_EXPERIMENTAL` | mock | Experimental |
| `BARRIER_REQUIRED(barrier)` | mock | Your organisation requires at least {barrier}. |
| `BARRIER_NO_KVM` | mock | This pool's runners can't run Vault (no KVM). |
| `BARRIER_POOL_DISALLOWS(pool, barrier)` | deviation (DS-5 §8) | {pool} doesn't allow {barrier}. |
| `BARRIER_NONE_OFFERS(barrier)` | deviation (DS-5 §8) | No runner in this pool offers {barrier}. |
| `BARRIER_KNOWN_WHEN_ONLINE` | deviation (DS-5 §8) | Known when one of your runners is online. |
| `BARRIER_POLICY_REQUIRES(barrier)` | deviation (DS-5 §8) | This run's policy requires at least {barrier}. |
| `BARRIER_ACCESS_VAULT` | deviation (DS-5 §8) | This run's access requires Vault. |
| `BARRIER_UNKNOWN` | pending (W-03) | Not known right now. Check again in a moment. |
| `BARRIER_NO_TERMINAL` | pending (W-07) | This pool's runners can't open a terminal at Vault. |
| `POLICY_NOTE` | mock | Whatever the barrier, every run still gets Wardyn's egress filtering, short-lived brokered credentials, human approvals, and full audit — those are set by policy, not the barrier. The barrier only decides how strongly the sandbox is walled off from your machine. |
| `NO_ONLINE_RUNNER` | server | reason runner_offline (Go owns the sentence) |
| `WAIT_AT_CAPACITY` | server | wait reason at_capacity, runnerpool WaitAtCapacityMsg (Go owns the sentence) |

### 7.4 Harnesses

| Key | Source | Frozen string |
|---|---|---|
| `TITLE` | mock | Choose the harnesses |
| `LEDE_OPTIONAL` | mock | Harnesses are optional for this workload. Add one only when the sandbox should include an agent tool. |
| `LEDE_INTERACTIVE` | mock | Add one or more harnesses. At most one can start with the run and receive an optional starting prompt. |
| `LEDE_AUTONOMOUS` | deviation (DS-1 truthful copy) | An agent runs on its own, with no SSH or terminal. |
| `EMPTY_TITLE` | mock | No harnesses added |
| `EMPTY_BODY` | mock | This workload can continue without one. |
| `EYEBROW(n)` | mock | Harness {n} |
| `HARNESS_LABEL` | mock | Harness |
| `AVAILABLE(name)` | mock | {name} is available in this environment. |
| `CODEX_HINT` | mock | Codex CLI cannot hold tools for approval. |
| `CUSTOM_HINT(name)` | pending (W-08) | {name} cannot hold tools for approval. |
| `NOT_INSTALLED(name)` | pending (W-08) | {name} isn't installed in this server's sandbox images. Ask an admin to add an image that includes it. |
| `NO_COMBINATION(a, b)` | pending (W-08) | No sandbox image here includes {a} and {b} together. |
| `NO_PROVIDER(name)` | pending (W-08) | No model provider you can use serves {name}. |
| `NOT_ENABLED` | deviation (DS-1 truthful copy) | Not enabled by your admin |
| `OPTIONAL_BEHAVIOUR` | mock | Available inside the sandbox without changing how this workload starts. |
| `START_IN_HARNESS` | mock | Start the run in this harness |
| `STARTING_PROMPT` | mock | Starting prompt |
| `STARTING_PROMPT_PLACEHOLDER` | mock | Give this harness a starting point |
| `STARTING_PROMPT_HELP` | mock | Only the harness that starts with the run can receive a starting prompt. |
| `TASK_LABEL` | mock | Task |
| `TASK_PLACEHOLDER` | mock | What should this agent accomplish? |
| `TASK_HELP` | mock | This harness starts automatically when it has a task. |
| `TASK_ERROR` | mock | An autonomous run needs at least one task to perform. |
| `ADD_HARNESS` | mock | Add a harness |
| `ADD_ANOTHER` | mock | Add another harness |
| `ADDED` | mock | Harness added. |
| `REMOVED` | mock | Harness removed. |

### 7.5 Files

| Key | Source | Frozen string |
|---|---|---|
| `TITLE` | mock | What files does it need? |
| `LEDE` | mock | Pick the repositories, drives and folders it can open. It starts in the one you mark. Anything not on a drive is deleted when the run ends. |
| `MAIN_TITLE` | mock | Main drive |
| `MAIN_BODY` | deviation (DS-3 W3) | The sandbox's own disk: system, tools and /home/agent. Every run has one. |
| `SIZE_LABEL` | mock | Size |
| `UP_TO(cap)` | mock | Up to {cap} GiB in this pool |
| `CLAMP_POOL(cap, pool)` | mock | Clamped to {cap} GiB by the {pool} pool. |
| `CLAMP_PROFILE(cap, profile)` | pending (W-11) | Clamped to {cap} GiB by {profile}. |
| `CLAMP_SERVER(cap)` | pending (W-11) | Clamped to {cap} GiB by this server. |
| `CLAMP_POLICY(cap)` | pending (W-11) | Clamped to {cap} GiB by this run's policy. |
| `CLAMP_ORG(cap)` | pending (W-11) | Clamped to {cap} GiB by your organisation's limits. |
| `SIZE_ERROR` | mock | Set a size for the main drive. |
| `ENFORCED` | mock | Enforced |
| `ENFORCED_TIP` | deviation (DS-3 W1) | Writes past this size fail. |
| `ENFORCED_BY_STOP` | deviation (DS-3 W1) | Enforced by stopping the run |
| `ENFORCED_BY_STOP_TIP` | deviation (DS-3 W1) | If /tmp, /home/agent/work and /home/agent/.cache together pass this size, Kubernetes stops the run. Other paths aren't counted. |
| `NOT_ENFORCED` | mock | Not enforced on this runner |
| `NOT_ENFORCED_ARIA` | mock | Disk size is not enforced on this runner |
| `NOT_ENFORCED_TIP` | mock | This runner's storage can't cap a container's disk; the size is a target, not a limit. |
| `REPORTED_BY_RUNNER` | deviation (DS-3 W1) | (reported by the runner) |
| `LIFE_LABEL` | mock | What happens to it |
| `LIFE_DELETED` | mock | Deleted when the run ends. |
| `LIFE_KEPT(max, grace)` | deviation (DS-3 W2) | Kept after its end time so you can extend and resume it, up to its {max} maximum lifetime. Deleted when you end the run, or {grace} after its end time. |
| `LIFE_KEPT_UNLIMITED(grace)` | pending (W-12) | Kept after its end time so you can extend and resume it. Deleted when you end the run, or {grace} after its end time. |
| `LIFE_NO_END` | deviation (DS-3 W2) | Kept until you end the run. |
| `REUSE_LABEL` | mock | Reuse it across runs |
| `REUSE_ARIA` | mock | About reusing this drive |
| `REUSE_TIP` | mock | Save this disk when the run ends and start another run from it. |
| `REPOS_TITLE` | mock | Repositories |
| `REPOS_BODY` | deviation (DS-3 W4) | Copied onto the main drive. Changes leave only as commits you push. |
| `REPOSITORY_BADGE` | mock | Repository |
| `INCLUDE_ARIA(name)` | mock | Include {name} |
| `BRANCH_ARIA(name)` | mock | Branch for {name} |
| `STARTS_HERE` | mock | Starts here |
| `ADD_REPOSITORY` | mock | + Add repository |
| `BRANCH_PLACEHOLDER` | pending (W-13) | default branch |
| `BRANCH_FALLBACK` | pending (W-13) | Branches couldn't be listed. Type a branch, tag or commit; leave it empty for the default branch. |
| `BRANCH_OTHER` | pending (W-13) | Other… |
| `REF_NOT_CHECKED` | pending (W-14) | Not checked until the run starts. |
| `USER_DRIVES_TITLE` | mock | Remote user drives |
| `SHARED_DRIVES_TITLE` | mock | Remote shared drives |
| `DRIVES_BODY` | mock | Live storage. Changes are kept. |
| `SHARED_DRIVES_BODY` | pending (W-16) | Live storage. Changes are kept. Each person gets their own folder on the share. |
| `SOURCE_PERSONAL` | mock | Personal |
| `SOURCE_TEAM` | mock | Team |
| `SOURCE_ORG` | pending (W-15) | Organisation |
| `FAMILY_DOCKER_VOLUME` | deviation (DS-3 W5) | Docker volume |
| `FAMILY_HOST_FOLDER` | deviation (DS-3 W5) | Host folder |
| `FAMILY_CLUSTER_VOLUME` | deviation (DS-3 W5) | Cluster volume |
| `ACCESS_EDIT` | mock | Can edit |
| `ACCESS_READ` | mock | Read-only |
| `ACCESS_ARIA(name)` | mock | Access for {name} |
| `LOCKED_ARIA` | mock | Why this drive is read-only |
| `LOCKED_TIP` | mock | Set by your admin |
| `NOT_IN_POOL` | mock | Not available in this pool's runners. |
| `CREATE_USER_DRIVE` | mock | + Create user drive |
| `CREATE_SHARED_DRIVE` | mock | + Create shared drive |
| `MANAGE_DRIVES` | mock | Manage drives → |
| `CREATE_DISABLED` | pending (W-17) | Your organisation doesn't let people create drives. Ask an admin. |
| `CREATE_DENIED(profile)` | pending (W-17) | Not allowed by {profile}. |
| `CREATE_LIMIT(n, limit)` | pending (W-17) | You have {n} of {limit} drives. |
| `CREATE_UNSUPPORTED` | pending (W-17) | This server can't create drives. |
| `CREATE_NO_DELETE` | pending (W-17) | This server can't delete drives, so it can't create them. |
| `DELETE_DRIVE` | pending (W-17) | Delete drive |
| `DELETE_CONFIRM(name)` | pending (W-17) | Delete {name}? Its files are deleted and can't be recovered. |
| `DRIVE_DELETED(name)` | pending (W-17) | {name} deleted. |
| `CREATE_TITLE` | mock | Create user drive |
| `CREATE_INTRO` | mock | Wardyn assigns the mount path. You can attach the drive to this run after it is created. |
| `DRIVE_NAME` | mock | Drive name |
| `DRIVE_NAME_PLACEHOLDER` | mock | e.g. Investigation files |
| `CREATE_CONFIRM` | mock | Create drive |
| `DRIVE_CREATED(name)` | mock | {name} created and added to this run. |
| `COMPUTER_TITLE` | mock | Your computer |
| `COMPUTER_BODY` | mock | Folders on your laptop, desktop or VDI. |
| `COMPUTER_SOON` | mock | A sandbox can't use folders on your computer yet. |
| `NO_EXTRA` | mock | No extra files — only the main drive |
| `NO_EXTRA_DELETED` | mock | Only the main drive. Nothing from this run is kept. |
| `NO_EXTRA_KEPT(grace)` | pending (W-12) | Only the main drive. It is deleted {grace} after the run's end time. |
| `CHOOSE_FILES` | deviation (owner-approved Files prototype 2026-10-13) | Add what it needs, or choose No extra files. |
| `MOUNT_TITLE` | mock | Mount map |
| `MOUNT_BODY` | mock | Read-only view of where files appear in the sandbox. |
| `MOUNT_MAIN(n)` | mock | Main drive · {n} GiB |
| `MOUNT_ITEM(name)` | mock | ← {name} |
| `MOUNT_STARTS` | mock | (Starts here) |
| `MOUNT_READ_ONLY` | mock | (read-only) |
| `SUMMARY_MAIN(n)` | mock | Main drive {n} GiB |
| `SUMMARY_STARTS(name)` | mock | starts in {name} |
| `SUMMARY_KEPT_ONE(names)` | mock | {names} is kept |
| `SUMMARY_KEPT_MANY(names)` | mock | {names} are kept |
| `SUMMARY_DELETED` | mock | the main drive is deleted when the run ends |
| `SUMMARY_KEPT(grace)` | pending (W-12) | the main drive is kept {grace} after its end time |
| `SUMMARY_NO_END` | pending (W-12) | the main drive is kept until you end the run |
| `SUMMARY_NO_EXTRA(n, life)` | mock | Main drive {n} GiB · no extra files · {life}. |
| `ATTACH_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Repositories, drives and folders |
| `ATTACH_BODY` | deviation (owner-approved Files prototype 2026-10-13) | Each one appears in the sandbox at its mount path. |
| `ADD` | deviation (owner-approved Files prototype 2026-10-13) | + Add |
| `ADD_ARIA` | deviation (owner-approved Files prototype 2026-10-13) | Add a repository, drive or folder |
| `KIND_USER_DRIVE` | deviation (owner-approved Files prototype 2026-10-13) | User drive |
| `KIND_SHARED_DRIVE` | deviation (owner-approved Files prototype 2026-10-13) | Shared drive |
| `KIND_FOLDER` | deviation (owner-approved Files prototype 2026-10-13) | Folder |
| `FOLDER_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Folder on this machine |
| `FOLDER_BODY` | deviation (owner-approved Files prototype 2026-10-13) | A folder on the computer running Wardyn. |
| `SHARED_SUB` | deviation (owner-approved Files prototype 2026-10-13) | Provided by your admin. You get your own folder on it. |
| `FOLDER_REMOTE` | deviation (owner-approved Files prototype 2026-10-13) | Wardyn runs on a remote server, so it can't open folders on this machine. |
| `EDIT` | deviation (owner-approved Files prototype 2026-10-13) | Edit |
| `EDIT_NAMED(name)` | deviation (owner-approved Files prototype 2026-10-13) | Edit {name} |
| `REMOVE_NAMED(name)` | deviation (owner-approved Files prototype 2026-10-13) | Remove {name} |
| `STORED_ON` | deviation (owner-approved Files prototype 2026-10-13) | Stored on |
| `ADDED(name)` | deviation (owner-approved Files prototype 2026-10-13) | {name} added. |
| `UPDATED(name)` | deviation (owner-approved Files prototype 2026-10-13) | {name} updated. |
| `REMOVED(name)` | deviation (owner-approved Files prototype 2026-10-13) | {name} removed. |
| `DIALOG_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Add to this run |
| `ADD_REPO_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Add repository |
| `ADD_USER_DRIVE_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Add user drive |
| `ADD_SHARED_DRIVE_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Add shared drive |
| `ADD_FOLDER_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Add folder |
| `STEPS_ARIA` | deviation (owner-approved Files prototype 2026-10-13) | Steps |
| `STEP_TYPE` | deviation (owner-approved Files prototype 2026-10-13) | Type |
| `STEP_CHOOSE` | deviation (owner-approved Files prototype 2026-10-13) | Choose |
| `STEP_MOUNT` | deviation (owner-approved Files prototype 2026-10-13) | Mount path |
| `ALREADY_ADDED` | deviation (owner-approved Files prototype 2026-10-13) | Already added to this run. |
| `BRANCH_LABEL` | deviation (owner-approved Files prototype 2026-10-13) | Branch |
| `MOUNT_HINT` | deviation (owner-approved Files prototype 2026-10-13) | Where it appears in the sandbox. |
| `PATH_INVALID` | deviation (lead 2026-10-13: the server accepts /home/agent only) | Use a path inside /home/agent. |
| `PATH_TAKEN(name, path)` | deviation (owner-approved Files prototype 2026-10-13) | {name} is already mounted at {path}. |
| `ACCESS_LABEL` | deviation (owner-approved Files prototype 2026-10-13) | Access |
| `SAVE` | deviation (owner-approved Files prototype 2026-10-13) | Save |
| `ADD_CONFIRM` | deviation (owner-approved Files prototype 2026-10-13) | Add |
| `START_TITLE` | deviation (owner-approved Files prototype 2026-10-13) | Starting folder |
| `START_BODY` | deviation (owner-approved Files prototype 2026-10-13) | Where it starts. Choose from what you added above. |
| `START_FOLDER_LABEL` | deviation (owner-approved Files prototype 2026-10-13) | Folder |
| `START_OPTION(name, path)` | deviation (owner-approved Files prototype 2026-10-13) | {name} · {path} |
| `START_MAIN` | deviation (owner-approved Files prototype 2026-10-13) | Main drive · /home/agent |
| `SUBFOLDER_LABEL` | deviation (owner-approved Files prototype 2026-10-13) | Subfolder |
| `FOLDERS_NONE` | pending (W-48) | No folders onboarded yet. Add one in Workspaces. |
| `FOLDER_WITH_REPOS(workspace)` | pending (W-49) | This folder comes with {workspace}'s repositories. Remove them to remove it. |
| `SUBFOLDER_PLACEHOLDER` | deviation (owner-approved Files prototype 2026-10-13) | e.g. src |
| `SUBFOLDER_ERROR(name)` | deviation (owner-approved Files prototype 2026-10-13) | Use a folder inside {name}. |
| `LIST_COMMA(a, b)` | deviation (owner-approved Files prototype 2026-10-13) | {a}, {b} |
| `LIST_AND(a, b)` | deviation (owner-approved Files prototype 2026-10-13) | {a} and {b} |

### 7.6 Tools and access

| Key | Source | Frozen string |
|---|---|---|
| `TITLE` | mock | What else does it need? |
| `LEDE` | mock | Everything your earlier choices bring is listed with the access it adds. Add the CLIs, MCP servers, APIs and secrets the run needs, then finish any setup. Wardyn adds each tool's hosts and credentials for you, and your organisation's policy still applies at review. |
| `INCLUDED_TITLE` | mock | Already included |
| `INCLUDED_BODY` | mock | Added by your harness, repositories and drives. Open one to see the hosts and credentials it brings. |
| `HARNESS_SETUP_TITLE` | mock | Harness setup |
| `HARNESS_SETUP_BODY` | mock | Choose each harness's model and how its tool calls are handled. MCP servers you add below can be turned on per harness. |
| `ADD_TITLE` | mock | Add tools and access |
| `ADD_BODY` | mock | Give the run tools that aren't covered by a harness, repository or drive. Each one brings its own hosts and credentials. |
| `CUSTOM_COMPONENT` | mock | Custom component |
| `CATEGORY_ARIA` | mock | Category |
| `CATEGORY_ALL` | mock | All |
| `CATEGORY_CLI` | mock | CLIs |
| `CATEGORY_MCP` | mock | MCP servers |
| `CATEGORY_API` | mock | APIs |
| `CATEGORY_SECRET` | mock | Secrets and hosts |
| `ADDED_HEADING` | mock | Added to this run |
| `FROM_ORG` | mock | From your organisation |
| `CARD_ADD` | mock | Add |
| `CARD_REMOVE` | mock | Remove |
| `ROW_REMOVE` | mock | Remove |
| `FROM_HARNESS` | mock | Harness |
| `FROM_REPOSITORY` | mock | Repository |
| `FROM_DRIVE` | mock | Drive |
| `FROM_COMMAND` | mock | Command |
| `FROM_CLI` | mock | CLI |
| `FROM_MCP` | mock | MCP server |
| `FROM_API` | mock | API |
| `FROM_SECRET` | mock | Secret |
| `FROM_HOST` | mock | Host |
| `FROM_CUSTOM` | mock | Custom |
| `SUB_STARTS` | mock | Starts with the run |
| `SUB_AVAILABLE` | mock | Available inside the sandbox |
| `SUB_COMMAND` | mock | Runs on its own; no harness |
| `SUB_NO_CREDENTIAL` | mock | No credential |
| `SUB_SAVED` | mock | Saved to your components |
| `SUB_RUN_ONLY` | mock | This run only |
| `NOTE_COMMAND` | mock | Package registry access comes from your organisation's default policy. |
| `NOTE_NONE_NEEDED` | mock | No network access or credentials needed. |
| `REPO_SUB` | pending (W-35) | Via Wardyn's git broker · this run's repositories only |
| `STATUS_NEEDS_SETUP` | mock | Needs setup |
| `STATUS_READY` | mock | Ready |
| `STATUS_HELD` | mock | Held for approval |
| `DETAILS_FOR(name)` | mock | Details for {name} |
| `DETAIL_NETWORK` | mock | Network |
| `DETAIL_CREDENTIAL` | mock | Credential |
| `DETAIL_APPROVAL` | mock | Approval |
| `DETAIL_MCP` | mock | MCP |
| `DETAIL_IMAGE` | mock | Image |
| `DETAIL_NOTE` | mock | Note |
| `DETAIL_NONE` | mock | None |
| `DETAIL_MCP_BODY` | mock | Turn it on for a harness under Harness setup. |
| `IMAGE_NO` | pending (W-34) | Not in this sandbox's image. The access still works if the tool is installed another way. |
| `IMAGE_UNKNOWN` | pending (W-34) | Wardyn can't tell whether this sandbox's image has it. |
| `NO_HARNESSES` | mock | No harnesses in this run. Add one on the Harnesses step if the sandbox should include an agent tool. |
| `PROVIDER_LABEL` | mock | Model provider |
| `PROVIDER_CHOOSE` | mock | Choose a model provider |
| `PROVIDER_REQUIRED` | mock | Required before launch. |
| `PROVIDER_PROXY` | mock | The proxy adds the provider's credential; the sandbox never holds it. |
| `PROVIDER_SET(name)` | mock | Model provider set for {name}. |
| `DEFAULT_LABEL` | mock | When a tool has no rule |
| `EFFECT_ALLOW` | mock | Run it |
| `EFFECT_HOLD` | mock | Ask first |
| `EFFECT_DENY` | mock | Block it |
| `DEFAULT_HELP_GATED` | pending (W-30) | Ask first holds the call until an admin approves it. |
| `DEFAULT_HELP_OPERATOR` | pending (W-30) | Ask first holds the call until you or another admin approves it. |
| `DEFAULT_HELP_ATTENDED` | pending (W-30) | Ask first: Claude Code asks you in the terminal before it runs the tool. |
| `PROFILE_REQUIRES(profile)` | pending (W-30) | {profile} requires Ask first or stricter for tools with no rule. |
| `CODEX_HOLD_TIP` | mock | Codex CLI can't pause a tool call for approval. |
| `CODEX_NOTE` | pending (W-32) | Codex CLI can't pause a tool call for approval, and Wardyn can't block its own tools. Rules for its MCP tools are enforced by the proxy. |
| `RULES_LABEL` | mock | Tool rules |
| `ADD_RULE` | mock | Add a tool rule |
| `MCP_FOR_HARNESS` | mock | MCP servers for this harness |
| `MCP_FOLLOWS` | mock | Calls to an MCP server follow this harness's tool rules, as `mcp__<server>__<tool>`. |
| `MCP_NONE` | mock | None added. Add an MCP server below and it appears here. |
| `MCP_ASKS` | pending (W-44) | The harness asks before an MCP tool runs; Wardyn does not hold the call. |
| `RULE_TITLE(name)` | mock | Add a tool rule for {name} |
| `RULE_INTRO` | pending (W-31) | Name one tool exactly, for example WebFetch or `mcp__jira__create_issue`. Rules for parts of a command are coming soon. |
| `RULE_TOOL` | mock | Tool |
| `RULE_PLACEHOLDER` | pending (W-31) | WebFetch |
| `RULE_EFFECT` | mock | What happens |
| `RULE_CONFIRM` | mock | Add rule |
| `RULE_ADDED` | mock | Rule added. |
| `RULE_ERR_TOOL` | mock | Enter a tool name. |
| `RULE_ERR_DUPLICATE` | mock | That tool already has a rule. |
| `RULE_CODEX_BUILTIN` | server | reason harness_no_block_builtin (Go and its golden own the sentence) |
| `SECRET_CARD_TITLE` | mock | Add a secret |
| `SECRET_CARD_BODY` | mock | Give the run one of your saved secrets as an environment variable, a file, or a header the proxy adds. |
| `SECRET_CARD_BUTTON` | mock | Add secret |
| `HOST_CARD_TITLE` | mock | Allow a host |
| `HOST_CARD_BODY` | mock | Let the run reach one more host with no credential. Your organisation's ceiling still applies. |
| `HOST_CARD_BUTTON` | mock | Allow host |
| `MCP_CARD_TITLE` | mock | Add an MCP server |
| `MCP_CARD_BODY` | mock | Connect a remote MCP server by URL. Its token stays with the proxy. |
| `MCP_CARD_BUTTON` | mock | Add MCP server |
| `CLI_GITHUB_BODY` | deviation (owner 2026-10-13, T9) | Actions and the REST API with gh; issue and pull request lists aren't supported yet. |
| `CARD_ADDED(name)` | mock | {name} added. Its hosts and credential are included. |
| `COMPONENT_ADDED` | mock | Added to this run. |
| `COMPONENT_ADDED_NAMED(name)` | mock | {name} added to this run. |
| `COMPONENT_REMOVED` | mock | Removed. |
| `CATALOGUE_NOT_AVAILABLE` | server | reason component_unavailable (Go owns the sentence) |
| `CATALOGUE_BLOCKED_HOST` | server | reason componentRefused (Go owns the sentence) |
| `CLI_NOT_GRANTED` | server | reason cli_not_granted (Go owns the sentence) |
| `CLI_NO_REPO` | server | reason cli_no_github_repo (Go owns the sentence) |
| `ADD_TO_RUN` | mock | Add to run |
| `SECRET_INTRO` | pending (W-39) | Choose one of your saved secrets and how the run receives it. Prefer a request header: no copy is placed in the sandbox. |
| `SECRET_LABEL` | mock | Secret |
| `SECRET_HELP` | mock | Values are stored encrypted and never shown again. Add new secrets under Your account. |
| `DELIVERY_LABEL` | mock | Delivery |
| `DELIVERY_HEADER_OPTION` | pending (W-39) | Request header |
| `DELIVERY_ENV_OPTION` | pending (W-39) | Environment variable |
| `DELIVERY_FILE_OPTION` | pending (W-39) | File |
| `SECRET_HOST_LABEL` | pending (W-39) | Host |
| `SECRET_VARIABLE_LABEL` | pending (W-39) | Variable name |
| `SECRET_FILE_LABEL` | pending (W-39) | File name |
| `SECRET_FILE_PATH(name)` | pending (W-39) | /run/wardyn/secrets/{name} |
| `SECRET_ERR_HOST` | mock | Enter the host the proxy adds it for. |
| `SECRET_ERR_VARIABLE` | mock | Enter the variable name. |
| `SECRET_ERR_FILE` | mock | Enter the file name. |
| `HOST_INTRO` | mock | The run can reach this host with no credential. If your organisation's ceiling doesn't allow it, review shows why. |
| `HOST_LABEL` | mock | Host |
| `HOST_PLACEHOLDER` | mock | e.g. pypi.org or *.example.com |
| `HOST_ERR` | mock | Enter a host. |
| `MCP_INTRO` | pending (W-41) | A remote MCP server reached over HTTPS. If it needs a token, Wardyn adds it to each request; no copy is placed in the sandbox. Servers that need you to sign in are coming soon. |
| `MCP_NAME` | mock | Name |
| `MCP_NAME_PLACEHOLDER` | mock | e.g. Linear |
| `MCP_URL` | mock | Server URL |
| `MCP_URL_PLACEHOLDER` | pending (W-41) | https://mcp.example.com/mcp |
| `MCP_TOKEN` | mock | Token |
| `MCP_NO_TOKEN` | mock | No token |
| `MCP_LOCAL_SOON` | mock | Servers that run inside the sandbox (a local command) are Coming soon. |
| `MCP_ERR_URL` | mock | Enter the server URL. |
| `CC_INTRO` | mock | Bundle the hosts and secrets one tool needs, so you can reuse it. Saved components appear under Add tools and access next time. |
| `CC_NAME` | mock | Name |
| `CC_NAME_PLACEHOLDER` | mock | e.g. Billing service API |
| `CC_HOSTS` | mock | Hosts |
| `CC_HOSTS_PLACEHOLDER` | mock | billing.acme.internal, api.stripe.com |
| `CC_SECRET` | mock | Secret |
| `CC_SECRET_NONE` | mock | None |
| `CC_SECRET_HELP` | mock | Delivered by the proxy as a header unless you choose otherwise when editing it. |
| `CC_SAVE` | mock | Save to my components |
| `CC_ERR` | mock | Enter a name and at least one host. |
| `CC_HEADER_HOST` | pending (W-40) | Header host |
| `CC_MORE_OPTIONS` | pending (W-40) | More options |
| `TOTALS_ARIA` | mock | What this run can reach |
| `NETWORK_TITLE` | mock | Network |
| `CREDENTIALS_TITLE` | mock | Credentials |
| `NO_HOSTS` | mock | No hosts. |
| `NO_CREDENTIALS` | mock | No credentials. |
| `FROM_ROW(name)` | mock | from {name} |
| `FOR_ROW(name)` | mock | for {name} |
| `NET_ALLOWED` | mock | Allowed |
| `NET_NEEDS_APPROVAL` | mock | Needs approval |
| `NET_REQUESTED` | pending (W-35) | Requested |
| `NET_NOT_ALLOWED` | pending (W-35) | Not allowed |
| `NET_POLICY_DENY_REVIEW_YOU` | pending (W-36) | Anything else is refused the first time the run asks for it and raises a request. Once you approve it, the run can try again. |
| `NET_POLICY_DENY_REVIEW_ADMIN` | pending (W-36) | Anything else is refused the first time the run asks for it and raises a request. Once an admin approves it, the run can try again. |
| `NET_POLICY_ALWAYS_DENY` | pending (W-36) | Anything else is refused and logged. |
| `NET_POLICY_HELD_YOU` | mock | Anything else is held for your review the first time the run asks for it. |
| `NET_POLICY_HELD_ADMIN` | pending (W-36) | Anything else is held for an admin's review the first time the run asks for it. |
| `NET_POLICY_TIMEOUT(n)` | pending (W-36) | If nobody decides within {n} seconds, it's refused. |
| `NET_POLICY_ALLOW_ALL` | pending (W-36) | Any other public host is allowed, except ones your organisation blocks. |
| `NET_POLICY_CEILING` | mock | Your organisation's ceiling still narrows this list at review. |
| `NET_POLICY_CHECKED` | pending (W-36) | Checked against your organisation's limits just now. |
| `NET_POLICY_PROFILE(profile)` | pending (W-36) | {profile}'s ceiling still narrows this list at review. |
| `NET_DECIDER` | pending (W-36) | An admin decides each new host. |
| `CREDENTIALS_POLICY` | pending (W-33) | Short-lived credentials are issued when the run first needs them and renewed while it runs. 'Proxy only' means no copy is placed in the sandbox. |
| `DELIVERY_PROXY` | mock | Proxy only |
| `DELIVERY_ENV` | mock | In the sandbox as an environment variable |
| `DELIVERY_FILE` | mock | In the sandbox as a file |
| `DELIVERY_SHORT_LIVED` | pending (W-33) | In the sandbox, short-lived |
| `DELIVERY_GIT_BROKER` | pending (W-33) | Via Wardyn's git broker |
| `DELIVERY_GIT_HELPER` | pending (W-33) | Given to git when it asks |
| `DELIVERY_AZURE` | pending (W-33) | Proxy only · renewed every hour or so |
| `DELIVERY_GITHUB` | pending (W-33) | Proxy only · acts as your organisation's Wardyn GitHub App, limited to this run's repositories |
| `DELIVERY_AWS(session)` | pending (W-33) | Your AWS role credentials are issued into the sandbox for up to {session} at a time. Wardyn renews them while the run lives and stops when it ends; ones already issued stay valid at AWS until they expire. |
| `SETUP_TITLE` | mock | Setup still needed |
| `SETUP_BODY` | mock | Finish these here, or review now and they appear as blockers before launch. |
| `SETUP_ALL_SET` | mock | Nothing else to set up. |
| `SETUP_DONE` | mock | Done |
| `SETUP_SIGN_IN` | mock | Sign in |
| `SETUP_CHOOSE` | mock | Choose |
| `SETUP_ADD` | mock | Add |
| `SETUP_CHOOSE_PROVIDER(name)` | mock | Choose a model provider for {name} |
| `SETUP_SIGN_IN_TO(provider)` | pending (W-38) | Sign in to {provider} |
| `SETUP_SIGN_IN_AGAIN` | pending (W-54) | Sign in again |
| `SIGN_IN_INTRO` | mock | You'll sign in with your own account in a new window. Wardyn keeps a refresh grant and issues the run a short-lived credential when it starts. The sandbox never sees your sign-in. |
| `SIGN_IN_CONFIRM` | mock | Continue to sign in |
| `SIGNED_IN` | mock | Signed in. The credential is issued when the run starts. |
| `TOKEN_INTRO` | mock | Paste the token once. It's stored encrypted as one of your secrets, and the proxy adds it to requests, so the sandbox never holds it. |
| `TOKEN_LABEL` | mock | Token |
| `TOKEN_CONFIRM` | mock | Save token |
| `TOKEN_SAVED` | mock | Token saved. |
| `STICKY_ONE` | mock | 1 item still needs setup |
| `STICKY_MANY(n)` | mock | {n} items still need setup |
| `STICKY_DONE` | mock | Everything is set up |
| `REVIEW_RUN` | mock | Review run |

### 7.7 Document

| Key | Source | Frozen string |
|---|---|---|
| `EYEBROW` | mock | Advanced setup |
| `TITLE` | mock | Run document |
| `LEDE` | mock | Edit the shared draft directly. Unsupported input is reported, never discarded. |
| `OPEN_IN_GUIDED` | mock | Open in Guided setup |
| `FORMAT_ARIA` | mock | Document format |
| `FORMAT_YAML` | mock | YAML |
| `FORMAT_JSON` | mock | JSON |
| `IMPORT_FILE` | mock | Import file |
| `LOAD_EXAMPLE` | mock | Load example |
| `EDITOR_ARIA(format)` | mock | {format} run document |
| `READY` | pending (W-18) | Ready to validate against Wardyn's run document schema. Policy and availability are checked at review. |
| `VALID` | pending (W-18) | Valid document. Policy and availability are checked at review. |
| `ISSUES(n)` | mock | {n} issues must be fixed |
| `ISSUES_ONE` | pending (W-53) | 1 issue must be fixed |
| `VALIDATE` | mock | Validate |
| `CONTINUE_REVIEW` | mock | Continue to review |
| `ANNOUNCE_ERRORS(n)` | mock | {n} errors found. |
| `ANNOUNCE_ERRORS_ONE` | pending (W-53) | 1 error found. |
| `ANNOUNCE_VALID` | mock | Document valid. |
| `JSON_INVALID` | mock | This JSON document isn't valid. Check the highlighted structure. |
| `YAML_INVALID` | pending (W-19) | This YAML document isn't valid. Check the highlighted structure. |
| `ERR_TITLE_EXCLUDED` | server | reason template_field_excluded, mapped by run-document-canon (it owns the sentence) |
| `ERR_END_CONFLICT` | server | reason run_end_conflicting_fields, mapped by run-document-canon (it owns the sentence) |
| `ERR_VERSION` | server | reason template_version_unsupported, mapped by run-document-canon (it owns the sentence) |

### 7.8 Review

| Key | Source | Frozen string |
|---|---|---|
| `EYEBROW` | mock | Ready for a final check |
| `TITLE` | mock | Review your run |
| `LEDE` | mock | This is what Wardyn will create. Every applied limit names its source. |
| `VIEW_ARIA` | mock | Review view |
| `VIEW_PRETTY` | mock | Pretty |
| `VIEW_YAML` | mock | YAML |
| `VIEW_JSON` | mock | JSON |
| `NARROWED_BY(source)` | pending (W-04) | Your request was narrowed by {source}. |
| `NARROWED_APPLIES(applied)` | mock | {applied} will apply. |
| `NARROWED_TAIL_POOL(pool)` | pending (W-04) | Not allowed in {pool}. |
| `NARROWED_TAIL_CEILING` | mock | Not allowed by your ceiling. |
| `NARROWED_TAIL_SERVER` | pending (W-04) | Not allowed on this server. |
| `NARROWED_TAIL_RUNNERS` | pending (W-04) | More than your runners can give. |
| `BLOCKERS_ONE` | mock | 1 blocker before launch |
| `BLOCKERS_MANY(n)` | mock | {n} blockers before launch |
| `BLOCKER_STALE` | mock | The setup changed. Check the server's current result again. |
| `BLOCKER_UNSUPPORTED(path)` | pending (W-29) | This document field isn't supported here: {path}. |
| `CHECK_AGAIN` | mock | Check again |
| `CHECKED_ANNOUNCE` | mock | Server result checked. Launch is ready. |
| `SETUP_TITLE` | mock | Run setup |
| `SETUP_AGENT_AUTONOMOUS` | mock | Autonomous agent task |
| `SETUP_AGENT_INTERACTIVE` | mock | Interactive agent task |
| `SETUP_APPLICATION` | mock | Background automation |
| `SETUP_ENVIRONMENT` | mock | Interactive environment |
| `BADGE_ATTENTION` | mock | Needs attention |
| `BADGE_COMPLETE` | mock | Complete |
| `GROUP_RUN` | mock | Run and timing |
| `GROUP_WHERE` | mock | Where it runs |
| `GROUP_WORK` | mock | What it works on |
| `GROUP_TOOLS` | mock | Tools and model |
| `GROUP_ACCESS` | mock | Access |
| `GROUP_LIMITS` | mock | Limits |
| `VALUE_AGENT` | mock | Agent task |
| `VALUE_APPLICATION` | mock | Application sandbox |
| `VALUE_ENVIRONMENT` | mock | Environment sandbox |
| `TIMING_APPLICATION` | mock | Background automation · Runs when launched |
| `TIMING_INTERACTIVE` | mock | Interactive · Starts when launched |
| `TIMING_NOW` | mock | Autonomous · Run Now |
| `WHERE_VALUE(pool)` | mock | {pool} pool |
| `WHERE_DETAIL(kind, used, max)` | deviation (pool words) | {kind} · {used} of {max} running |
| `WHERE_ONLINE(n)` | pending (W-03) | {n} of your runners online |
| `PROV_ADDED` | mock | Added by you |
| `PROV_SELECTED` | mock | Selected by you |
| `PROV_RUN_SETUP` | mock | Run setup |
| `PROV_ORG_DEFAULT` | mock | Organisation default |
| `PROV_MODEL_PROVIDER` | mock | Selected model provider |
| `PROV_SOURCE_POLICY` | mock | Source policy |
| `WORK_NONE` | mock | No repositories or drives |
| `WORK_ITEM_ONE` | mock | 1 item attached |
| `WORK_ITEMS(n)` | mock | {n} items attached |
| `MAIN_DRIVE(n)` | mock | Main drive · {n} GiB |
| `NO_HARNESSES` | mock | No harnesses |
| `TOOLS_COMMAND_ONE` | mock | Command · 1 optional harness |
| `TOOLS_COMMAND(n)` | mock | Command · {n} optional harnesses |
| `TOOLS_NONE` | mock | Optional · none added |
| `TOOLS_STARTS(name, provider)` | mock | {name} starts with the run · {provider} |
| `TOOLS_NO_HARNESS` | mock | No harness |
| `TOOLS_AUTO(n, total)` | mock | {n} of {total} harnesses start automatically |
| `HARNESS_STARTS` | mock | Starts with run |
| `HARNESS_AFTER` | mock | Available after start |
| `HARNESS_AUTO` | mock | Starts automatically |
| `HARNESS_NOT_AUTO` | mock | Not auto-started |
| `HARNESS_NO_PROMPT` | mock | No starting prompt |
| `HARNESS_HAS_PROMPT` | mock | Has prompt |
| `HARNESS_STARTS_LONG` | mock | Starts with the run |
| `HARNESS_AFTER_LONG` | mock | Available after start |
| `HARNESS_INCLUDED` | mock | Included, not auto-started |
| `ACCESS_NOTE` | mock | Hosts are layered onto the organisation default, then narrowed by your ceiling. |
| `LIMITS_DETAIL` | mock | Ends at its limit or when the work finishes |
| `LIMITS_VALUE(cpu, memory, lifetime)` | mock | {cpu} CPU · {memory} MiB · {lifetime} |
| `LIMIT_UP_TO(cap, source)` | pending (W-27) | Up to {cap} · Set by {source} |
| `DOC_ONLY_CHIP` | pending (W-29) | Run document |
| `SHARED_MODEL_ACCESS` | pending (W-09) | Each harness's model access is added by the proxy, and any process in this sandbox can use it. |
| `CHANGE` | mock | Change |
| `CLOSE` | mock | Close |
| `DONE` | mock | Done |
| `EDIT_RUN` | mock | Edit run and timing |
| `EDIT_FILES` | mock | Edit files |
| `EDIT_HARNESSES` | mock | Edit harnesses |
| `EDIT_TOOLS_ACCESS` | pending (W-26) | Edit tools and access |
| `POOL_LABEL` | mock | Pool |
| `BARRIER_LABEL` | mock | Barrier |
| `COMMAND_LABEL` | mock | Command |
| `CPU_LABEL` | mock | CPU |
| `MEMORY_LABEL` | mock | Memory MiB |
| `POLICY_TITLE` | mock | Effective policy |
| `POLICY_BODY` | mock | What the sandbox can actually do |
| `ONLY_CHANGED` | mock | Only what I changed |
| `NOTHING_CHANGED` | mock | You haven't changed anything for this run. |
| `SHOW_EVERYTHING` | mock | Show everything |
| `POLICY_BARRIER` | mock | Barrier |
| `POLICY_NETWORK` | mock | Network |
| `POLICY_HOSTS(n)` | mock | {n} hosts allowed |
| `POLICY_HOST_ONE` | deviation (lead 2026-10-13, the singular of POLICY_HOSTS) | 1 host allowed |
| `POLICY_CREDENTIALS` | mock | Credentials |
| `POLICY_NO_CREDENTIALS` | mock | No credentials requested |
| `POLICY_HIDDEN` | mock | Selected model provider · Hidden |
| `POLICY_FILES` | mock | Files and code |
| `POLICY_TOOLS` | mock | Tools and pushes |
| `POLICY_HOLD` | mock | Hold for approval |
| `POLICY_AUTOMATIC` | mock | Run automatically |
| `RAW_TITLE(format)` | mock | {format} run document |
| `RAW_BODY` | mock | Read-only resolved draft |
| `COPY` | mock | Copy |
| `COPIED` | mock | Copied |
| `EDIT_FORMAT(format)` | mock | Edit {format} |
| `SAVE_TEMPLATE` | mock | Save as template |
| `SAVE_POLICY` | mock | Save as policy |
| `LAUNCH_RUN` | mock | Launch run |
| `POOL_FULL` | server | wait reason at_capacity, runnerpool WaitAtCapacityMsg (Go owns the sentence) |
| `OFFLINE_BLOCKER` | server | reason runner_offline (Go owns the sentence) |

### 7.9 Save as

| Key | Source | Frozen string |
|---|---|---|
| `TITLE_TEMPLATE` | mock | Save as template |
| `TITLE_POLICY` | mock | Save as policy |
| `INTRO(kind)` | mock | Saving changes the {kind}, not this run draft. A run title is never stored. |
| `NAME_LABEL` | mock | Name |
| `NAME_PLACEHOLDER_TEMPLATE` | mock | e.g. Frontend investigation |
| `NAME_PLACEHOLDER_POLICY` | mock | e.g. Package access |
| `AVAILABLE_LABEL` | mock | Available to |
| `SCOPE_ME` | mock | Only me |
| `SCOPE_ORG` | mock | Organisation |
| `SCOPE_GROUP(label)` | mock | {label} group |
| `HELP_NO_GRANTS` | pending (W-20) | Only you can use it. Organisation admins and group template managers publish shared templates. |
| `HELP_MANAGER` | pending (W-20) | You can also publish to the groups you manage. |
| `INCLUDE_LEGEND` | mock | Include |
| `INCLUDE_ALL` | mock | Everything in this draft |
| `INCLUDE_SUBSET` | mock | Choose a subset |
| `SUBSET_NETWORK` | pending (W-21) | Network |
| `SUBSET_CREDENTIALS` | pending (W-21) | Credentials |
| `SUBSET_ACCESS_RULES` | pending (W-21) | Access rules |
| `OMITTED` | pending (W-22) | Omitted values inherit defaults or are asked for when it's used. Explicit false, zero and empty values are kept. |
| `SHARED_DISCLOSURE` | pending (W-25) | A template keeps the names of secrets, workspaces and drives, never their values. People it is shared with can see those names. Each person's own access is checked when they use it, and anything they lack shows as Needs setup. |
| `CANCEL_SAVE` | mock | Cancel |
| `CONFIRM_TEMPLATE` | mock | Save template |
| `CONFIRM_POLICY` | mock | Save policy |
| `SAVED(kind)` | mock | {kind} saved without changing the draft. |
| `NAME_TAKEN` | server | reason template_name_taken (Go owns the sentence) |
| `LIMIT_REACHED` | server | reason template_limit_reached (Go owns the sentence) |
| `POLICY_FIELD_REFUSED` | server | reason template_policy_field_refused (Go owns the sentence) |

### 7.10 Name and launch

| Key | Source | Frozen string |
|---|---|---|
| `TITLE` | mock | Name this run |
| `INTRO` | mock | A title and description help you find and understand this run later. |
| `TITLE_LABEL` | mock | Title |
| `TITLE_PLACEHOLDER` | mock | e.g. Fix payment test |
| `DESCRIPTION_LABEL` | mock | Description |
| `DESCRIPTION_PLACEHOLDER` | mock | Add context for your team |
| `CHECKED_JUST_NOW` | mock | Checked just now |
| `CHECKING` | pending (W-28) | Checking… |
| `SUMMARY(pool, cpu, lifetime)` | mock | {pool} · {cpu} CPU · {lifetime} |
| `CANCEL` | mock | Cancel |
| `LAUNCH` | mock | Launch |
| `LAUNCH_REFUSED` | server | the create refusal, shown inside the dialog in the server's own sentence |

### 7.11 Launched

| Key | Source | Frozen string |
|---|---|---|
| `BADGE_STARTING` | mock | Starting |
| `BADGE_WAITING` | mock | Waiting |
| `STARTING_BODY` | mock | Your governed sandbox is being prepared. |
| `WAITING_BODY(pool, n)` | mock | {pool} is running its limit of {n}. This run starts when there's room. |
| `POSITION` | mock | Position |
| `IN_LINE(ordinal)` | mock | {ordinal} in line |
| `WAITING_SINCE` | mock | Waiting since |
| `JUST_NOW` | mock | Just now |
| `GIVES_UP` | mock | Gives up |
| `GIVES_UP_IN(wait)` | deviation (DS-5 §8) | In {wait} |
| `CANCEL_RUN` | mock | Cancel run |
| `START_ANOTHER` | mock | Start another run |
| `VIEW_RUN` | mock | View run |

### 7.12 Templates

| Key | Source | Frozen string |
|---|---|---|
| `EYEBROW` | mock | Saved starting points |
| `TITLE` | mock | Templates |
| `LEDE` | mock | Start a new run from a saved setup. Using a template opens New run at review, where your organisation's current policy and availability are checked again. A template never stores a run's title. |
| `NEW_FROM_SCRATCH` | mock | New run from scratch |
| `SCOPE_FILTER_ARIA` | mock | Who it's available to |
| `KIND_FILTER_ARIA` | mock | Kind |
| `SEARCH_LABEL` | mock | Search templates |
| `SEARCH_PLACEHOLDER` | mock | Search by name |
| `LIST_ARIA` | mock | Templates |
| `SCOPE_ALL` | mock | All |
| `SCOPE_ME` | mock | Only me |
| `SCOPE_GROUPS` | mock | Groups |
| `SCOPE_ORG` | mock | Organisation |
| `KIND_ALL` | mock | All kinds |
| `KIND_TEMPLATES` | mock | Templates |
| `KIND_POLICIES` | mock | Policies |
| `NOTE` | mock | Save a template from a run's review page with Save as template or Save as policy. A policy holds only network, credential and access rules, and anything it leaves out comes from your organisation's defaults. |
| `COL_NAME` | mock | Name |
| `COL_AVAILABLE` | mock | Available to |
| `COL_INCLUDES` | mock | Includes |
| `COL_UPDATED` | mock | Updated |
| `COL_ACTIONS` | mock | Actions |
| `POLICY_BADGE` | mock | Policy |
| `NEEDS_SETUP(label)` | mock | Needs setup: {label} |
| `GROUP_LABEL(group)` | mock | {group} group |
| `ORG_ADMINS` | mock | Organisation admins |
| `TEMPLATE_MANAGERS` | pending (W-24) | Template managers |
| `PART_RUN` | mock | Run |
| `PART_WHERE` | mock | Where it runs |
| `PART_TOOLS` | mock | Tools |
| `PART_FILES` | mock | Files |
| `PART_ACCESS` | mock | Access |
| `PART_LIMITS` | mock | Limits |
| `REVISION(n)` | mock | Revision {n} |
| `USE` | mock | Use |
| `RENAME` | mock | Rename |
| `DELETE` | mock | Delete |
| `PUBLISH` | pending (W-24) | Publish to organisation |
| `WHY_ORG` | mock | Published by your organisation. Only organisation admins can change it. |
| `WHY_GROUP(scope)` | mock | Published to {scope}. Only its template managers can change it. |
| `EMPTY_TITLE` | mock | No templates yet |
| `EMPTY_BODY` | mock | When you review a run, choose Save as template to start from the same setup next time. |
| `EMPTY_ACTION` | mock | Start a new run |
| `NO_MATCH_TITLE` | mock | Nothing matches |
| `NO_MATCH_BODY` | mock | Try another filter or search term. |
| `CLEAR_FILTERS` | mock | Clear filters |
| `RENAME_TITLE` | mock | Rename template |
| `RENAME_BODY` | mock | The new name applies everywhere this template is offered. Runs already started from it keep their own titles. |
| `NAME_LABEL` | mock | Name |
| `NAME_EMPTY` | mock | Enter a name. |
| `RENAME_CONFIRM` | mock | Rename |
| `RENAMED(name)` | mock | Renamed to {name}. |
| `DELETE_TITLE(name)` | mock | Delete {name}? |
| `DELETE_BODY_ME` | mock | Only you can use it, so nobody else is affected. Runs already started from it keep running and keep their settings. |
| `DELETE_BODY_SCOPE(scope)` | mock | People in {scope} can no longer start from it. Runs already started from it keep running and keep their settings. |
| `DELETE_CONFIRM` | mock | Delete template |
| `DELETED(name)` | mock | {name} deleted. |
| `CANCEL` | mock | Cancel |
| `NAME_TAKEN` | server | reason template_name_taken (Go owns the sentence) |

### 7.13 Shared

| Key | Source | Frozen string |
|---|---|---|
| `COMING_SOON` | mock | Coming soon |
| `REQUIRED` | mock | Required |
| `OPTIONAL` | mock | Optional |
| `BACK` | mock | Back |
| `CONTINUE` | mock | Continue |
| `CANCEL` | mock | Cancel |
| `REMOVE` | mock | Remove |
| `CLOSE_DIALOG` | mock | Close dialog |
| `EYEBROW(n)` | deviation (DS-6 §8) | Guided setup · {n} of 5 |
| `PROGRESS_ARIA` | mock | Setup progress |
| `STEP_SHAPE` | mock | Shape |
| `STEP_RUN` | mock | Run |
| `STEP_HARNESSES` | mock | Harnesses |
| `STEP_FILES` | mock | Files |
| `STEP_TOOLS` | mock | Tools and access |
| `CONTINUE_TO(step)` | mock | Continue to {step} |
| `REVIEW_RUN` | mock | Review run |
| `BARRIER_FENCE` | mock | Fence |
| `BARRIER_WALL` | mock | Wall |
| `BARRIER_VAULT` | mock | Vault |
| `GIB(n)` | mock | {n} GiB |
| `MIB(n)` | mock | {n} MiB |
| `CPU(n)` | mock | {n} CPU |
| `HOURS(n)` | mock | {n} hours |
| `HOUR_ONE` | deviation (DS-5 §8) | 1 hour |
| `JOIN(a, b)` | mock | {a} · {b} |
