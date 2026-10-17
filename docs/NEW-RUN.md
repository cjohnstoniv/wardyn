# New run

The New run flow turns a few plain choices into one governed run. This page is for members who launch runs and for operators who decide what members see. Every path ends at the same review, and the server checks policy and availability again when you launch.

## Ways to begin

Open **New run** (`/runs/new`) for three cards.

| Card | What it does |
|---|---|
| **AI composer** | Marked **Coming soon**. It cannot be opened. |
| **Guided setup** (**Fastest**) | Opens step 1, Shape. Your organisation's defaults fill what you skip. |
| **YAML / JSON** (**Advanced**) | Opens the [run document](#the-run-document) editor. |

The other starting points sit outside the Start screen's cards.

| Start from | How you get there | Where you land |
|---|---|---|
| **Saved template** | **Use** on the [Templates screen](#templates), or **Start from** on the Shape step | Review, on the revision you picked |
| **Run again** | **Start from** on the Shape step, then **Previous run**; or **Start a run like this one** on an ended run's page | Review, on that run's kept setup |
| **From scratch** | The Shape step default. **New run from scratch** on the Templates screen opens the Start screen | A blank draft |

- A scratch draft cannot skip ahead. Opening a later step or Review sends you back to the first step with a missing answer.
- **Run again** lists your own earlier runs (`GET /api/v1/me/launch-intents`). Only the creator reads a kept setup. Anyone else gets the `404` a missing run gets; the creator of a run that kept none gets `launch_intent_not_found`.
- A kept setup omits the title, description and wait limit. See [Run again](sdk.md#run-again).
- **Start a run like this one** on a run that kept no setup opens Review prefilled from what the run's row and audit trail still hold.
- A template or Run again link that cannot be read shows the server's sentence and **New run from scratch**.

## The steps

Guided setup has five steps, then Review. The progress bar names the five steps, and "Guided setup · N of 5" sits above each step's heading.

| # | Step | What it asks | Required |
|---|---|---|---|
| 1 | **Shape** | What you are running | A shape |
| 2 | **Run** | How and where it runs | A command for an Application sandbox; a pool |
| 3 | **Harnesses** | Which agent tools the sandbox holds | One harness for an Agent task; a task if Autonomous |
| 4 | **Files** | Which repositories, drives and folders it opens | A main drive size, and an item or **No extra files** |
| 5 | **Tools and access** | What else it needs, and setup still open | Nothing blocks here; open setup becomes a Review blocker |

### Step 1: Shape

Pick one of three shapes. The server records it as `shape` ([run documents](sdk.md#run-documents) lists what each requires).

| Shape | Sub-line shown | Runs as |
|---|---|---|
| **Agent task** | Interactive · Autonomous · Repeat / schedule coming soon | A one-time coding task, driven by you or running on its own |
| **Environment sandbox** | Interactive · Your drive is attached when you have one | A reusable workspace for development or analysis |
| **Application sandbox** | Automation / script · Application / server coming soon | A sandboxed command that runs on its own |

- Choosing a shape opens step 2. Choosing the shape a draft already has changes nothing: its starter, prompt and task stay.
- Picking **Saved template** or **Run again** hides the shapes, and **Continue** opens Review.

### Step 2: Run

- **How should it run?** (Agent task only): **Interactive** or **Autonomous**.
  - Interactive means you work in the sandbox, and one harness can start with it.
  - Autonomous means an agent runs on its own, with no SSH or terminal. **When should it run?** offers **Run Now**; **Schedule For Later** is marked **Coming soon**.
- **What should run?** (Application sandbox only): **Automation / script**. **Application / server** is **Coming soon**.
- **Command** is required for an Application sandbox. It runs verbatim, and the run ends when it exits.
- **Pool** is required. A pool that cannot take this run stays listed with its reason, such as **No online runners** or **Background only**.
- The pool panel shows **Total pool capacity**, **Your allowance**, **Waiting**, **Run types** and **Per-run limits**; the strictest limit applies.
- **Barrier** is **Fence**, **Wall** or **Vault**, weakest to strongest. A barrier that cannot be offered is disabled with its reason. An untouched barrier lets the server choose.

### Step 3: Harnesses

A harness is an agent tool the sandbox holds, such as a coding CLI.

| Shape | Harnesses |
|---|---|
| Agent task, Interactive | At least one. At most one starts with the run, with an optional **Starting prompt** |
| Agent task, Autonomous | Exactly one, with a required **Task**. **Add another harness** is disabled and marked **Coming soon** |
| Environment or Application sandbox | Optional. They are available inside the sandbox and start nothing |

- A harness you cannot pick is disabled with its reason, such as not installed in this server's images or **Not enabled by your admin**.
- **Start the run in this harness** marks the one that starts. The last harness of an Agent task cannot be removed.

### Step 4: Files

The step shows the main drive, one list of items, a starting folder and a mount map.

- **Main drive** is the sandbox's own disk, and every run has one. **Size** is in GiB and required. The pool's default fills it, and a size above the cap is clamped with a note naming who set the cap. With no size the step's summary reads "Set a size for the main drive."
- A chip says whether the runner holds the size: **Enforced**, **Enforced by stopping the run** or **Not enforced on this runner**.
- **What happens to it** says whether the drive is deleted when the run ends or kept so you can resume. **Reuse it across runs** is disabled and marked **Coming soon**.

Press **+ Add** to open a dialog with three stages: **Type**, **Choose**, **Mount path**.

| Type | What you choose |
|---|---|
| **Repository** | One of your onboarded repositories and its **Branch**. **+ Add repository** opens Workspaces |
| **User drive** | An existing drive of yours |
| **Shared drive** | An existing shared drive. You get your own folder on it |
| **Folder on this machine** | A folder already onboarded in Workspaces, on the computer running Wardyn; with none it reads "No folders onboarded yet. Add one in Workspaces." Disabled when the server runs its sandboxes on Kubernetes |
| **Your computer** | Marked **Coming soon** |

- A drive that cannot attach is greyed with its reason, such as paused by your admin or not available in this pool's runners.
- **Mount path** sits inside the sandbox user's home directory, and no two items may overlap. A drive's path is fixed. A repository's or folder's path is fixed too, unless it is its workspace's only item.
- A folder that shares its workspace with other items mounts at its source's own target, else `/home/agent/work`. It is read-only unless its source is writable. Onboarding refuses a second folder on that default path.
- **Access** is **Can edit** or **Read-only**. A drive your admin made read-only shows a lock and **Set by your admin**.
- Each card has **Edit** and **Remove**. Open one to see **What happens to it** and, for a drive or folder, **Stored on**.
- **Remove** is disabled for a folder that comes with its workspace's repositories; remove those to remove it.
- The first item you add is where the run starts. Change it under **Starting folder**, with an optional **Subfolder** inside that item. With nothing attached the run starts in the main drive.
- With nothing added, choose **No extra files — only the main drive**. A held **Continue to tools and access** says "Add what it needs, or choose No extra files."
- An Environment sandbox opens with your own drive attached when it can attach.

### Step 5: Tools and access

The step reads the server's policy preview for the draft ([the `access` block](sdk.md#access)).

| Section | Shows |
|---|---|
| **Already included** | What your harness, repositories and drives bring. Open a row for its **Network** and **Credential** details |
| **Harness setup** | Each harness's model provider, default for tools with no rule, **Tool rules** and MCP servers |
| **Add tools and access** | The catalogue of things you can add |
| **Network** and **Credentials** | The total the run can reach |
| **Setup still needed** | Open items, each with a button: **Sign in**, **Choose**, **Add** or **Connect Azure DevOps** |

- A run that carries an Azure DevOps credential, on a connection that creates a token for each run, says so under **Credentials**: "Azure DevOps: a token for this run with …. It's revoked when the run ends."
- If you have not connected, that card also reads "Connect Azure DevOps once before launching; Wardyn creates the run's token from that connection." beside **Connect Azure DevOps**.
- Hosts read **Requested** until the server has answered this exact draft. Then each reads **Allowed**, **Needs approval** or **Not allowed** against the clamped policy.
- A line under the host list says what happens to a host nothing lists: refused, held for review, or allowed. It follows your organisation's `first_use_approval` mode.
- Each harness has a **Model provider** list. The server's default or only candidate stands in until you choose; when it needs your choice the field reads "Required before launch."
- **When a tool has no rule** is **Run it**, **Ask first** or **Block it**. Your policy can set a floor; an option below it is disabled with the reason (`policy_stricter_default`).
- A tool rule names one tool exactly, for example `WebFetch` or `mcp__jira__create_issue`. `codex-cli` cannot pause a tool call (`harness_no_hold`).

**Add tools and access** filters by **All**, **CLIs**, **MCP servers**, **APIs** and **Secrets and hosts**. Each card shows the hosts and credential delivery it brings.

| Item | Notes |
|---|---|
| CLIs | Azure, GitHub and AWS cards, each named for the row your admin granted (`cli_not_granted` otherwise). **Google Cloud CLI** and **kubectl** are **Coming soon** |
| GitHub CLI | Works only with this run's GitHub repositories (`cli_no_github_repo`). The proxy adds its token |
| AWS CLI | Role credentials are issued into the sandbox, short-lived. On a run whose model provider is Bedrock, `AWS_REGION` and `AWS_DEFAULT_REGION` are the provider's region and win over the profile's; pass `--region` for another |
| Organisation components | MCP servers and APIs your admin published, marked **From your organisation** |
| **Add a secret** | One of your saved secrets as **Request header** (preferred), **Environment variable** or **File** |
| **Allow a host** | One more host with no credential. Your organisation's ceiling still applies |
| **Add an MCP server** | A remote server by HTTPS URL with an optional token. Local-command servers are **Coming soon** |
| **Custom component** | Hosts and a secret bundled for reuse. Disabled when your organisation does not let you define components |

- Your admin can turn off environment-variable and file delivery (`resident_off`); use a request header then.
- A repository whose token or key lane the run drops reads **Needs setup**, with "No credential is set up for this repository, so the run starts without one." (`repo_no_credential`) under it.
- A background Claude Code run with no MCP server and no tool-approval hold still loads the repository's own `.mcp.json`. That traffic goes through the run's egress policy like any other.
- Harnesses in one sandbox run as the same user, so each can read the others' MCP addresses. The rules that hold for a server are those of the least restricted harness that has it turned on.

## Review

Review shows what Wardyn will create and where each value came from. The **Pretty**, **YAML** and **JSON** switch changes the view.

**Pretty** has six groups under a **Needs attention** or **Complete** badge: **Run and timing**, **Where it runs**, **What it works on**, **Tools and model**, **Access** and **Limits**.

- Each value carries a source chip such as **Added by you**, **Organisation default** or **Source policy**. A field only a run document can set shows **Run document**.
- **Change** opens an editor in place for the pool, barrier, command, CPU or **Memory MiB**; **Done** closes it. **Edit run and timing**, **Edit files**, **Edit harnesses** and **Edit tools and access** jump to the step.
- **Effective policy** shows "What the sandbox can actually do": barrier, network, credentials, files and tool handling. **Only what I changed** filters it.
  - A policy that grants Azure DevOps access adds one line that starts "Azure DevOps:" and names what it grants.
  - A background run whose policy holds pushes for review adds "Nobody is here to answer, so a push that would be held is refused instead."
- **YAML** and **JSON** show the draft as a read-only `kind: Run` document with **Copy**. **Edit YAML** (or **Edit JSON**) opens the editor.

### Blockers and notices

Notices report a pool at its limit, a CPU or memory request that was narrowed, and the server's own warnings.

- A request narrowed by a named pool or profile is titled "Your request was narrowed by …".
- The server's own limit reads "Not allowed on this server.", and your runners' limit reads "More than your runners can give."

Blockers hold **Launch run**. They appear in this order, each linking to its fix.

1. Missing answers from the steps.
2. Document fields this screen does not support.
3. A template's setup you have not specified, shown as "Needs setup: …".
4. The server's refusal of the draft, in its own sentence.
5. Setup items still missing from step 5.
6. A stale result, shown only when nothing else holds **Launch run**.

**Launch run** opens only with no blockers and a preflight answer for exactly this draft that is under 60 seconds old. **Check again** refreshes it. The create call decides in the end.

## Templates

The **Templates** screen lists saved starting points at `/templates` (User view) and `/admin/templates` (Admin view).

| Action | Who may | Server rule |
|---|---|---|
| **Use** | Anyone who can read the row | Opens New run at Review, on the listed revision |
| **Rename**, **Delete** | The owner of a personal template; an organisation admin for an organisation one; the group's template managers for a group one | `template_scope_forbidden`, `template_group_unverified` |
| **Publish to organisation** | An organisation admin, in the Admin view only | Copies the current revision to the organisation |
| **Save as template**, **Save as policy** (Review) | You, to **Only me** or a group you manage | Never the organisation from Review |

- [`internal/types/template.go#TemplateAuthorize`](../internal/types/template.go) decides authority. A template you cannot read answers `template_not_found`, like a missing one.
- A row you cannot change keeps **Rename** and **Delete** disabled, with the reason beside them.
- A rename sends the revision you saw. If the template moved on, the server answers `template_revision_conflict` and changes nothing.
- A **Policy** row holds only network, credential and access rules; your organisation's defaults supply the rest.

On Review, **Save as template** and **Save as policy** open one dialog.

1. Enter a **Name**. It must be unique among templates for the same people (`template_name_taken`).
2. Choose **Available to**: **Only me**, or a group you manage and are verified in.
3. Choose **Everything in this draft** or **Choose a subset**. A policy subset offers **Network**, **Credentials** and **Access rules**.
4. Save. The draft does not change, and a run title is never stored.

- A template keeps the names of secrets, workspaces and drives, never their values. People it is shared with see the names, and anything they lack shows as **Needs setup**.
- A secret value in the draft is refused (`template_secret_refused`). An audience at its limit is refused (`template_limit_reached`).

## The run document

A `kind: Run` document is the draft written as text. Choose **YAML / JSON** on the Start screen, or **Edit YAML** or **Edit JSON** on Review.

- The format switch changes the document between YAML and JSON. **Import file** reads a `.yaml`, `.yml` or `.json` file of up to 1 MiB. **Load example** writes an interactive agent task for the first enabled harness.
- **Validate** sends the text to the import door, `POST /api/v1/templates/import`, which stores nothing. Each problem is placed on its line.
- Unsupported input is reported, never discarded. "N issues must be fixed" blocks **Continue to review**. **Open in Guided setup** validates, then opens step 1.
- [Run documents](sdk.md#run-documents) lists the fields. A title or description is refused as `template_field_excluded`; you name the run at launch.

From a terminal, `wardyn run --file <doc>` (`-` reads standard input) sends a `Run` document through the same door check, then creates the run. Only `--title`, `--description`, `--wait`, `--timeout`, `--json` and `--dry-run` go with it. A document's `inline_policy` is laid onto your default policy, as `policy_overlay`.

The `wardyn template` commands are listed in [Run templates](sdk.md#run-templates).

## After you launch

**Launch run** opens **Name this run**. **Title** is required; **Description** is optional.

- The dialog checks the draft again (**Checking…**, then **Checked just now**). **Launch** stays off until it ends. If the check fails or is rate-limited, **Launch** stays available and the launch itself refuses what the check would have.
- A refusal shows the server's sentence, and what you typed stays.
- A launch the run's Azure DevOps token refused adds a note. Either connect again with **Connect Azure DevOps**, after which the dialog checks once more, or read that your organisation restricts who can create personal access tokens.

The Launched screen opens at `?launched=<run id>`. It refreshes every 4 seconds while the run is starting or waiting.

| Badge | Run state | Shows |
|---|---|---|
| **Starting** | `PENDING`, `STARTING` | "Your governed sandbox is being prepared." and a progress bar |
| **Waiting** | `QUEUED` | **Position** ("Nth in line"), **Waiting since**, **Gives up** and **Cancel run** |
| The run's state | Any other | The run's state badge |

- A waiting run says why: none of your runners is online (`runner_pool_no_runner_free`), every runner is at its limit (`runner_pool_runners_at_capacity`), or the pool is running its limit.
- A run also waits when recording is required and the organisation keeps no session recordings (`runner_pool_recording_store_off`).
- **Cancel run** asks you to confirm, then calls `POST /api/v1/runs/{id}/kill`. The run leaves the queue and ends as killed. A failure shows under the waiting card.
- The owner, an admin or, for a service identity's run, its sponsor can cancel. A run that already ended otherwise answers `run_kill_already_terminal`.
- **View run** opens the run's page. **Start another run** returns to the Start screen.

## For operators

What you configure changes what members see. Each row links to the section that owns the setting.

| You configure | Members see | Read |
|---|---|---|
| Runner pools, runner limits, run caps | Step 2 pools, reasons and the queue | [Runner pools](operations/runner-pools.md), [Runner limits](runner-registration.md#runner-limits) |
| Governance profile and ceilings | **Set by …** limits and narrowed sizes at Review | [Decision scopes, the clamp, and the first run](operations/member-runs.md#decision-scopes-the-clamp-and-the-first-run) |
| Harness images (`WARDYN_AGENT_IMAGES`) | Which harnesses step 3 can pick | [The image a run's harnesses start from](operations/build-images.md#the-image-a-runs-harnesses-start-from) |
| Model providers | The provider list and sign-in items on step 5 | [Model access comes from a model provider](operations/integrations.md#model-access-comes-from-a-model-provider) |
| Components and CLI grants | Catalogue cards and `cli_not_granted` | [Custom components](operations/custom-components.md) |
| `first_use_approval`, tool rules | The "anything else" line and tool defaults | [`first_use_approval` modes](POLICIES.md#first_use_approval-modes), [`tool_rules`](POLICIES.md#tool_rules--toolrule) |
| User and shared drives | The drive lists on step 4 | [User drives](operations/user-drives.md) |
| Organisation templates, template managers | Who can publish, rename and delete | [Run templates](OPERATIONS.md#run-templates) |

- Template managers are granted with `PUT /api/v1/admin/template-group-admins` or `wardyn template group-admin grant`; no console screen does this in this release.
