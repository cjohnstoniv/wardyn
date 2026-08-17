# The demo recording

The Wardyn demo video is a script, not a performance. `make record-demo` wipes
the stack, brings it up on camera, drives the console through the whole story,
and writes an mp4. After a UI change you re-run it — you do not re-choreograph
it.

```sh
make record-demo                          # the works
make record-demo ARGS=--no-reset          # keep the current stack (iterating on the driver)
make record-demo ARGS=--no-record         # drive the UI, capture nothing (preflight)
make record-demo ARGS="--video 03"        # one video of the 0.5 series (see below)
make record-demo ARGS="--video 09 --terminal-script scripts/demo-beats/09-ci-and-headless.sh"
                                          # a terminal-first video (see below)
```

| Piece | Where |
|---|---|
| Orchestration (reset → capture → `make setup` → driver) | [`scripts/record-demo.sh`](../scripts/record-demo.sh) |
| The driver (acts 1–6) | [`ui/e2e/demo/walkthrough.spec.ts`](../ui/e2e/demo/walkthrough.spec.ts) |
| The rig: one browser, one recorded context, one shared page | [`ui/e2e/demo/stage.ts`](../ui/e2e/demo/stage.ts) |
| Walking the funnel, deciding an approval | [`ui/e2e/demo/funnel.ts`](../ui/e2e/demo/funnel.ts) |
| Captions, spotlight ring, chapter cards, terminal typing | [`ui/e2e/demo/overlay.ts`](../ui/e2e/demo/overlay.ts) |
| The same vocabulary for host-shell beats (`--terminal-script`) | [`scripts/demo-typist.sh`](../scripts/demo-typist.sh) |
| Task text, workspace path, the five demo commands | [`ui/e2e/demo/task.ts`](../ui/e2e/demo/task.ts) |
| The workspace the run attaches | [`examples/workspaces/demo-node/`](../examples/workspaces/demo-node/) |
| Playwright `demo` project (headed, 1920×1080, `:8080`) | [`ui/playwright.config.ts`](../ui/playwright.config.ts) |

## The series harness (0.5)

The 0.5 story is not one video, it is ten. Same rig throughout — same driver,
same overlay, same narration, same verifier — with one spec per video:

```sh
scripts/record-demo.sh --video 03    # records ui/e2e/demo/03-*.spec.ts, and only that
scripts/record-demo.sh               # no --video: the end-to-end walkthrough, unchanged
```

`--video <nn>` does four things and nothing else:

| | |
|---|---|
| **Picks the spec** | `ui/e2e/demo/<nn>-*.spec.ts`, handed to Playwright as a positional filter. Resolved in **preflight**, so a number with no spec (or two specs) dies before anything destructive has happened |
| **Names the output** | `wardyn-<nn>-<slug>-<stamp>.mp4`, the slug taken from the spec's own filename — a folder of takes then sorts into viewing order rather than reshoot order |
| **Decides the reset** | see below |
| **Exports `WARDYN_DEMO_VIDEO`** | which is how `verify-demo-take.sh` knows which take it is looking at |

With no `--video` nothing is filtered at all: the `demo` project's own
`testMatch` decides, exactly as it did before the series existed.

The shared code the ten specs sit on is [`stage.ts`](../ui/e2e/demo/stage.ts)
(the browser, the recorded context, the one page — importing it registers a
spec's `beforeAll`/`afterAll`, and each test reads its page out of `stage()`)
and [`funnel.ts`](../ui/e2e/demo/funnel.ts) (`advance()`, `clearWorkspace()`,
`decide()`). Both were lifted out of `walkthrough.spec.ts` unchanged. **A spec
that imports `stage.ts` keeps its own `test.skip(!process.env.WARDYN_DEMO, …)`
guard** — without it, a bare `pnpm exec playwright test --project=demo` points a
headed browser at a developer's live stack and starts clicking Launch.

### The clean slate belongs to video 01

`reset-all` runs for **video 01 and the no-flag walkthrough only**. Every other
`--video` implies `--no-reset`; `--reset` overrides that, `--no-reset` opts 01
out.

This is not a speed optimisation. The series is shot in an order where each
video opens on state an earlier one left behind — a workspace that was
onboarded, a run that was launched, a host that was permanently granted
(**DA14/SV20**). Inside the **V8 → V2 → V3** window in particular, *every* take
runs with `--no-reset`: a wipe between them does not merely cost minutes of dead
air, it films a story whose first half never happened — and the take stays green
the whole time, which is exactly why the rule lives in the script rather than
with whoever is holding the clapperboard.

### The SSH videos need the gateway on before `make setup`

`WARDYN_SSH_LISTEN` empty is the product default and means *off*: no listener,
and the host key is not even generated (`buildOptionalFeatures`,
`cmd/wardynd/boot_deps.go`). A take that films the SSH pane therefore has to
bring the stack up with it set, in the shell that runs the recorder — the
script's `make setup` inherits the environment:

```sh
export WARDYN_SSH_LISTEN=:2222
export WARDYN_SSH_ADVERTISE=127.0.0.1:2222   # advisory copy: it is what the pane's ssh command shows
scripts/record-demo.sh --video 07
```

Then **preflight the fingerprint before rolling**, not on camera:

```sh
curl -s localhost:8080/healthz | jq .ssh   # expect enabled: true, plus host_key_fingerprint
```

`enabled: false` means the variable never reached wardynd (or the image predates
the gateway) — fix that before the take, because the run-detail card simply does
not render and there is nothing to film.

The host key is minted and persisted on first boot with the gateway on, so
**video 01's `reset-all` mints a new one**. Any `known_hosts` entry from an
earlier stack then makes the client refuse with the full REMOTE HOST
IDENTIFICATION HAS CHANGED banner — mid-take, in a governance demo, on camera:

```sh
ssh-keygen -R '[127.0.0.1]:2222'
```

### The verifier dispatches on the video

`verify-demo-take.sh` reads `WARDYN_DEMO_VIDEO` and picks its checks from it.
Unset means the legacy walkthrough, which films act 5 — so it gets act 5's
checks, byte for byte what this script always ran, and that is also `--video 02`.
The other nine are stubs (`video-specific checks TBD by spec`): **each video's
assertions ship with the spec that films them**, because a check written before
the beat exists is a guess, and a guess that passes is worse than no check.

What every take gets regardless is the **shared** half — a narration timeline
with cues and zero overlaps, and an mp4 that is 1920×1080 and actually carries
an audio stream. That alone catches a take that recorded nothing, recorded
silently, or recorded at the wrong size.

### The terminal lane: `--terminal-script`

Two videos of the series have no page to film. **V09 (CI & headless)** is a
policy file, a long env-prefixed `scripts/ci-run.sh` invocation, its exit code
and its artifacts; **V10 (audit & attach)** is three terminals each holding
`ssh <run-uuid>@127.0.0.1 -p 2222` with a different key. Playwright cannot drive
either of them.

```sh
scripts/record-demo.sh --video 09 --terminal-script scripts/demo-beats/09-ci-and-headless.sh
```

runs that beat script under the **same** gdigrab capture Act 0 uses — same
`FIFO`, same `stop_capture`, one ffmpeg — and the script gets its presentation
from [`scripts/demo-typist.sh`](../scripts/demo-typist.sh), which is the
terminal's answer to `overlay.ts`:

| Verb | The browser lane's equivalent |
|---|---|
| `narration_zero` / `narration_end` | `narrationZero()` / `narrationEnd()` — opens and closes the clock |
| `say "<line>"` | `caption()` — prints the line, speaks it through the same Kokoro server, and **holds the terminal until the clip has finished** (+300ms), so speech never runs over the next command |
| `type_cmd "<command>"` | `typeInTerminal()` — echoes the command at 45ms/char, runs it, shows its real output. The status is returned *and* left in `TYPIST_RC`, and `$?` is restored before the command runs so a beat of `echo $?` reports the pipeline's code and not the keystroke loop's |
| `beat <ms>` | `beat()` — a pause, for pacing only |
| `chapter "<title>" "<sub>"` | `chapter()` — cleared screen, title held ~2.6s, cleared again |

A beat script is an ordinary bash script that sources the typist:

```sh
. "$(dirname "${BASH_SOURCE[0]}")/../demo-typist.sh"
narration_zero
chapter "CI & headless" "The same governance, unattended"
say "This is the policy the pipeline runs under."
type_cmd "cat examples/policies/ci.json"
type_cmd 'echo $?'
narration_end
```

**A video may be terminal-only, browser-only, or both.** With `--terminal-script`
and no `ui/e2e/demo/<nn>-*.spec.ts`, the beats *are* the video and the driver
never starts — the slug then comes off the script's filename, so the take still
lands as `wardyn-<nn>-<slug>-<stamp>.mp4` and sorts with its siblings. With both,
the terminal segment is filmed first and joined onto the front of `console.webm`
by the same concat `--with-terminal` has always used (so the joined file keeps
that lane's `…-full.mp4` name, and `…-full-narrated.mp4` once it speaks).

Narration crosses the seam. The typist writes its own
`ui/test-results/demo-video/narration-terminal.json` — narrator.ts rewrites
`narration.json` wholesale after every cue, so a shared file would lose every
terminal cue the moment the driver spoke — and `record-demo.sh` merges the two,
shifting the **browser** cues by the terminal segment's measured duration, then
muxes the merged timeline flat. The terminal cues need no shift because
`start_capture` exports `WARDYN_DEMO_CAPTURE_ZERO` and `narration_zero` times
from the start of the *picture*, not from the moment the beat script happened to
run.

Two things to know before rolling:

- **The capture region is live for the whole beat script, not just Act 0.** Same
  warning as `--with-terminal`, for longer: WSLg cannot raise or z-order windows
  from Linux, so whatever sits in that rectangle is what gets filmed, and you
  cannot tell until you watch it. It has eaten two takes. Without
  `--with-terminal` the camera starts *after* `make setup`, so the video opens on
  the beats rather than on two minutes of bring-up — but the corner still has to
  be clear from that point on.
- **Warm the voice first.** `narrate-prewarm.sh` only reads `ui/e2e/demo/*.ts`,
  so a beat script's lines are not pre-rendered and the first take of a new line
  pauses ~1.5s while Kokoro renders it. Clips are cached by content hash, so a
  `--no-record` dry run of the beat script (or simply the previous take) makes
  the real one warm.

## Before the first take

```sh
winget.exe install Gyan.FFmpeg              # capture. From WSL it needs the .exe
claude setup-token > ~/.wardyn-demo-token   # model access, read from the file, never printed
```

winget's Gyan.FFmpeg is a zip package: it appends to the **Windows** PATH, and a
WSL shell only inherits that at startup — so a freshly installed `ffmpeg.exe`
stays invisible in the shell you installed it from. `record-demo.sh` resolves it
directly (PATH, then the WinGet `Packages` directory) so you do not have to open
a new shell; `WARDYN_DEMO_FFMPEG=/path/to/ffmpeg.exe` overrides. Preflight also
checks the build actually has `gdigrab` and `libx264`, because a stripped ffmpeg
would otherwise fail mid-take.

Then, once, by hand: **put the terminal in the top-left 1920×1080 of the
screen** — not maximized. That rectangle is filmed only during Act 0
(`make setup`); once the driver starts, the browser records itself and you can
use the machine normally.

During Act 0 that corner *is* the frame. `record-demo.sh` captures
`WARDYN_DEMO_CAPTURE=1920x1080+0+0` rather than the whole desktop, for two
measured reasons: this machine's desktop is 5120×1440, which would produce a
3.5:1 video nobody can share, and gdigrab cannot keep up with that pixel rate —
a full-desktop grab measured **16 fps of a requested 30**, where the cropped
region holds **27**. `WARDYN_DEMO_CAPTURE=full` grabs everything;
`WARDYN_DEMO_FRAMERATE` overrides the rate.

Anything inside that rectangle is in the Act 0 segment — other windows,
notifications, wallpaper. Clear it before rolling. After Act 0 it stops
mattering: see "The video is captured from TWO sources" below.

## The video is captured from TWO sources, on purpose

| Segment | Captured by | Default |
|---|---|---|
| Acts 1–6 — the console | **the browser recording itself** (Playwright `recordVideo`) | **always** → `…/Videos/wardyn-demo-<ts>.mp4` |
| Act 0 — `make setup` in the terminal | ffmpeg `gdigrab`, screen region | **opt-in**, `--with-terminal` → joined as `…-full.mp4` |
| Host-shell beats (V09, V10) | the same ffmpeg `gdigrab` | **opt-in**, `--terminal-script <path>` → joined the same way |

**The terminal segment is off by default and that is deliberate.** It is the
only part of the pipeline that films your screen, and on this host that cannot
be made safe (below). It has ruined two takes — one recorded six minutes of a
browser game, one a fantasy football draft — while every act passed and the
script reported success. The console segment cannot be corrupted that way, so
the default output is always the real thing.

The split exists because a screen grab of the browser is **not reliable on this
host**. WSLg presents the browser as a RAIL window, which means:

- `--window-position` is a request the compositor may ignore (the driver now
  moves the window with CDP `Browser.setWindowBounds`, which actually works, and
  fails loudly if the window still lands outside the frame);
- the window cannot be reliably raised from Linux, and **X stacking order is not
  the Windows compositor's z-order**, so occlusion cannot even be *detected*
  from this side;
- gdigrab's `-i title=…` window capture cannot find WSLg windows at all
  (`I/O error` for every exact title) — verified, so it is not an option.

Net effect: a desktop grab films whatever is on top of that screen corner, and
you cannot tell until you watch it. Playwright's capture comes from inside the
page, so neither occlusion nor window position can corrupt it — with the default
(console only) you can use the machine normally while a take runs. Only
`--with-terminal` asks you to leave a corner alone, and only for the ~2 minutes
of `make setup`.

The `DEMO_CDP` lane cannot record this way (the browser is not ours to
configure), so it falls back to the desktop grab and inherits the occlusion risk.

### Framing: the window must be sized to viewport PLUS chrome

The recording is the page, at a fixed 1920x1080 canvas. If the page renders any
smaller, Playwright pads it - and that resampling is what makes small text look
soft. Setting the window to 1920x1080 while the viewport is also 1920x1080 does
exactly that: the tab strip and address bar eat ~90px, the page comes out
1920x985, and it lands upscaled-and-letterboxed in the file. Measured with
`ffmpeg -vf cropdetect`, that was `crop=1920:985`; sizing the window to the
viewport plus its own measured chrome (`outerHeight - innerHeight`) gives
`crop=1920:1064` - 1:1, no resampling, crisp text.

If a take ever looks soft or letterboxed, check it the same way:

```sh
ffmpeg -ss 6 -i console.webm -vf cropdetect=24:2:0 -frames:v 20 -f null -
```

Anything materially below 1080 tall means the page is not filling the canvas.

## Why it is built this way

- **Act 0 needs a real screen grab**, because a terminal is not a web page —
  hence ffmpeg gdigrab on the Windows side. Playwright's *bundled* ffmpeg cannot
  do it: that build is `--disable-everything` with no `x11grab` and no
  `gdigrab`, only an mjpeg-in/webm-out encoder.
- **Acts 1–6 are recorded by the browser itself**, which is the only capture
  here that another window cannot ruin (see the section above).
- **The browser is a headed Chromium on WSLg.** `DEMO_CDP=http://<host>:9222`
  instead attaches to the real Windows Chrome that
  `~/tester/bin/chrome-cdp.sh --launch` brings up — better-looking chrome, same
  driver, but no browser-side recording, so that lane is desktop-grab only.
- **The overlay exists because the mouse pointer does not move.** Playwright
  dispatches synthetic input; Windows' cursor stays put. So the driver paints a
  ring on whatever it is about to click and a caption bar under it.
- **`~/tester` is not the driver.** Its drive-books are prose an agent
  improvises from, so two runs take different paths — the opposite of what a
  re-shoot needs. It contributes its Chrome launcher and its `knowledge/wardyn.md`
  gotchas (the `WARDYN_UP_NO_BROWSER=1` rule, the stale `wardyn-internal`
  network, the WSL2-NAT trap that forces containerized mode).

## Narration — the captions are spoken

Every caption and chapter card the driver renders is also read aloud, so the
video explains what it is doing without a human recording a voice-over. On by
default; `make record-demo ARGS=--silent` turns it off (captions still render).

| Piece | Role |
|---|---|
| `scripts/narrate-server.py` | Long-lived TTS server. **Kokoro-82M** (Apache-2.0, offline) with the installed **piper** as automatic fallback. One JSON line in, one clip out. |
| `ui/e2e/demo/narrator.ts` | Spawns it, records a `{file, tMs, durMs}` timeline to `ui/test-results/demo-video/narration.json`. |
| `ui/e2e/demo/overlay.ts` | `caption()`/`chapter()` speak their own text; `beat()`/`act()` hold for `max(nominal beat, audio + 300ms)`. |
| `scripts/demo-typist.sh` | The terminal lane's half of the same thing — its own coprocess, the same cue shape, `narration-terminal.json`, merged in by the recorder. |
| `scripts/narrate-mux.py` | Lays the timeline onto the finished mp4, video stream-copied. |

One-time setup (already done on this machine, in `~/.cache/wardyn-narrate/`):

```sh
python3 -m venv ~/.cache/wardyn-narrate/venv
~/.cache/wardyn-narrate/venv/bin/pip install kokoro-onnx soundfile
# kokoro-v1.0.onnx (310MB) + voices-v1.0.bin (27MB) from the kokoro-onnx release
~/.cache/wardyn-narrate/venv/bin/python scripts/narrate-server.py --selftest
```

### Why it is built this way

- **Kokoro, not `edge-tts`.** edge-tts rides an undocumented Microsoft endpoint
  that periodically 403s (reported again January 2026). A pipeline whose whole
  point is "re-run it after a UI change" must not have a third party able to
  switch it off. Kokoro is Apache-2.0, offline, and needs no network at render
  time — which also suits a project about egress control.
- **Clips are keyed by `sha1(engine|voice|text)`, not by a caption id.** The
  obvious design is a `narration.ts` of stable keys, and it is wrong here: this
  repo already has two hand-synced caption duplicates that have **drifted**
  (`FUNNEL_DEMOS` vs `demo-catalog.ts`, `TASK.md` vs `DEMO_TASK`), and a keyed
  module would be a third — where a caption edited without its key silently
  narrates the *old* line. Hashing the text means a copy change re-renders
  exactly that clip, and no list has to be kept in sync with anything.
- **`walkthrough.spec.ts` is not touched at all.** Narration hangs entirely off
  `overlay.ts`, so captions added to any act are spoken automatically.
- **The mux runs last, on the mp4.** The raw recording is VP8 and cannot be
  stream-copied into an mp4 container; muxing after the transcode also means the
  picture is encoded exactly once, so narration cannot soften the text.
- **It cannot fail a take.** A missing model, a dead renderer or a bad line
  degrades to a silent caption. `--silent` and an absent renderer are the same
  code path.

### Checking a narrated take

The timeline is a plain JSON file — read it rather than scrubbing the video:

```sh
python3 -c "import json;d=json.load(open('ui/test-results/demo-video/narration.json'));
print(sum(1 for i,c in enumerate(d['cues']) if i and c['tMs']<d['cues'][i-1]['tMs']+d['cues'][i-1]['durMs']),'overlaps')"
```

Any overlap means a line was cut off on screen. `narrate-mux.py` warns about the
same thing. To confirm the audio really landed where the timeline claims, probe
a speech window and a gap — speech reads around −23 dB, silence −91 dB:

```sh
ffmpeg -ss 8 -t 2 -i narrated.mp4 -af volumedetect -f null -   # expect ~-23 dB
ffmpeg -ss 12.6 -t 1 -i narrated.mp4 -af volumedetect -f null - # expect -91 dB
```

## When there is no model quota: `WARDYN_DEMO_SHELL_ACT5=1`

Act 5 normally launches a real Claude Code agent. If the subscription is out of
quota the agent prints `You've hit your weekly limit` and exits 1 — it does no
work, never reaches `example.com`, and the held-approval beat never happens. The
take still completes and still produces a video, and `verify-demo-take.sh`
correctly fails it.

```sh
WARDYN_DEMO_SHELL_ACT5=1 make record-demo
```

records Act 5 as a **Shell command** run instead: the same workspace, the same
two hosts, the same `NOTES.md`, and every governance beat intact — the host held
at the proxy, the decision scoped to `always`, the workspace receipt, and the
proof run that never has to ask. What it does not show is a coding agent writing
code.

This is honest rather than degraded: Wardyn's claim is that it governs **any**
workload — *"a coding agent is the flagship use, not the only one"* — and the
narration says so out loud in that variant rather than implying an agent.

Drop the flag once quota is back and the agent version records again with no
code change.

**Check quota before a long take**, or you find out 25 minutes in:

```sh
wardyn run --agent claude-code --task-mode exec --task 'echo ok' \
  --policy-file examples/policies/sandbox.yaml --wait --timeout 3m
```

## Beat sheet

Every literal string the driver targets is listed here. A copy change in the app
breaks the driver loudly, and this table is where you look to fix it.

### Act 0 — cold start (terminal)

| Beat | Command |
|---|---|
| Clean slate | `WARDYN_FORCE_RESET=1 ./scripts/up.sh reset-all --purge-env` — **without** `--purge-images`; rebuilding agent images is minutes of dead air |
| Bring it up | `WARDYN_SETUP_MODE=container WARDYN_WORKSPACES_ROOT=~/wardyn-demo WARDYN_UP_NO_BROWSER=1 make setup` |
| Model | `wardyn subscription connect --token-stdin < ~/.wardyn-demo-token` — visible command, invisible token |

The workspace is rebuilt from the fixture every take (a previous recording left
the agent's `slugify()` in it) and `git init`ed, so the run's Files surface has a
real diff.

### Act 1 — first light

| Targets | |
|---|---|
| `heading` level 1 | **Run anything. Keep your keys.** |
| `button` | **Get started — a few minutes** |
| then | `Step 1 of …` |

A fresh install lands here on its own: no runs and no dismissed tour means
`firstRunLanding()` (`setup-gate.ts`) redirects `/` → `/setup`.

### Act 2 — essentials (funnel steps 1–3)

| Step | Heading | Notes |
|---|---|---|
| 1 | **Pick your barrier** | Fence / Wall / Vault, gated on what the host really has |
| 2 | **Corporate network** | Mandatory gate. **Test connectivity** must pass before Next unlocks — a blocked step *replaces* Next with its own action button, which `advance()` handles generically |
| 3 | **Connect your model** | Shows the subscription connected in Act 0 |

Footer buttons: `Next: <step>` and, on the last step, **Finish setup**.

### Act 3 — the guardrails (funnel steps 4–8)

Each step's start button is `demo-start-<id>` (**Start demo**), its audit panel
`demo-audit-panel`, and it ends with **End demo**. Approvals render as
`live-approval-row` with **Approve** / **Deny** — always decided BY HOST, never
by position (see below). Note `demo-card-<id>` does NOT exist here; that wrapper
belongs to the `/demos` catalog, and the funnel renders `DemoRunControls` bare.

| Step | Typed into the terminal | What must happen on camera |
|---|---|---|
| **The sealed box** | `curl -sSI https://example.com` | Instant refusal. **No approval appears** — nothing to click |
| **Fail, then approve** | the same curl **twice** | First is refused *and* raises an approval → Approve (This run) → the retry returns `HTTP/2 200` |
| **Held at the door** | `curl -sSI --max-time 60 https://example.com`, then the same for `wikipedia.org` | First **hangs**; Approve → that same request completes, no retry. Second → **Deny** → confirm dialog → instant refusal |
| **Lines that can't be crossed** | `example.com`, `169.254.169.254`, `192.168.1.1` | Public host works; metadata and LAN refused **with egress wide open** |
| **Once, or for good** | the same curl **twice**, then once more | First is refused *and* raises an approval → the split button's caret → **Once** → the retry returns `HTTP/2 200` → the SAME command a third time is refused again and raises a brand-new approval, left undecided |

### Act 4 — your work (funnel steps 9–10)

**Onboard a workspace** → **Add workspace** dialog: source **Local directory**,
**Path on this host**, **Name**, the **Advanced** disclosure (mount path, write
permission), submit **Add workspace**. Then **Review readiness** → **Finish
setup** → lands on Runs.

### Act 5 — a real run

**New run** → `/runs/new`. **Title** (required — Launch is disabled without one),
run type **Agent task**, **Batch** (see the rename note below), **Task**
textarea, workspace combobox, confinement **Confined**, then **Edit hosts…** →
dialog **Network for this run**: allow the `api.anthropic.com` chip, pick **Hold
it for approval**, **Save hosts**. Then **Launch run**.

> **Pending rename:** Track B renames this mode **Batch → Autonomous**. The
> driver matches `getByRole("radio", { name: /^Batch/ })`, so the day that lands
> it fails loudly at exactly one line rather than recording an interactive run
> that never executes the task — but every **Batch** in this document, and in
> `walkthrough.spec.ts`, is stale from that moment. Re-shooting against a build
> that already has the rename? Read **Batch** as **Autonomous** throughout.

> **Order matters:** **Batch** is clicked BEFORE the Task box is filled. An
> interactive run has no Task field at all (the server ignores task for one), so
> the textarea does not exist until the mode changes.

> **Load-bearing:** `applyLLMCredMount` (`internal/api/llmcred.go`) refuses to
> inject the subscription credential unless the resolved policy allows
> `api.anthropic.com`. Ticking that chip is not decoration — drop it and the run
> has no model at all.

Mid-run the held `example.com` request surfaces in `LiveApprovals` under the
output that caused it; the driver approves it **with Always**, not a plain
Approve (the split button's caret → **Always**), and the metadata probe shows up
as a denial no approval could have rescued. The task text lives in
`ui/e2e/demo/task.ts` (`DEMO_TASK`), with the reasoning in
`examples/workspaces/demo-node/TASK.md`.

**The `always` beat's second half, once the run finishes:** Workspaces →
the onboarded workspace → the **Allowed hosts** card, spotlighting
`example.com` now on the permanent list. Then a SECOND, much smaller run
against the same workspace — **New run** → **Title** (`PROOF_RUN_TITLE`) →
**Terminal — a shell in the workspace dir** (interactive stays the wizard's
default; only "Start with" changes) → the same workspace → **Launch run**,
with Confinement and Network left untouched entirely. In the attached
terminal, `curl -sSI https://example.com` succeeds immediately — no hold, no
approval row, just the `live-approvals-idle` hint — because the workspace's
own permanent grant folds into the run automatically. The driver then
navigates back to the main run's URL (captured before this detour) so Act 6
recaps that run's audit trail and recording, not the proof run's.

### Act 6 — the receipts

Tabs (role `tab`): **Audit**, then **Recording**. Closes on the caption
*"Run anything. Keep your keys."*

## Funnel behaviours the driver has to model

Found by actually running it. Each of these silently breaks a driver that
assumes the obvious thing:

- **One step can need more than one Next.** The Corporate network gate answers
  the first Next by swapping its **Host proxy** tab for **Egress redirection**
  and *staying on step 2*. So `advance()` presses Next until the "Step N of M"
  counter changes, rather than pressing once — which also covers the
  blocked-step case where the gate's own action button replaces Next entirely.
- **The connectivity probe launches a real run.** "Test connectivity" proves the
  path from inside a sandbox, so `has_runs` flips true during Act 2. That
  matters because `firstRunLanding()` only redirects `/` → `/setup` while
  `has_runs` is false: the second time you run the driver against the same
  stack, `/` lands on Runs. Act 1 falls through to `/setup` rather than
  requiring a full reset just to iterate.
- **`demo-card-<id>` does not exist in the funnel.** It belongs to
  `demo-screen.tsx`'s `DemoCard`, which stacks all seven demos on `/demos`. The
  funnel step renders the shared `DemoRunControls` bare, one demo per step, so
  on that path the page is the scope. `demo-start-<id>` *is* inside
  `DemoRunControls` and works on both.
- **"Lines that can't be crossed" produces NO audit rows for its two headline
  denials** — and that is the block being *stronger* than the card claims, not
  weaker. `https://example.com` leaves via CONNECT through the egress proxy, so
  the proxy decides it and logs `egress.allow`. The `http://169.254.169.254` and
  `http://192.168.1.1` probes have no proxy in their path and the sandbox
  carries **no default route**, so they die at the network layer in ~0 ms with
  `curl: (7) Failed to connect` — never reaching the proxy that would have
  recorded a decision. The demo card's own step text
  (`demo-catalog.ts:188`) says *"you'll see a 403 (or a curl error), **and a deny
  row in the Audit panel**"*; the deny row does not exist and cannot. The driver
  therefore proves this demo from the terminal, and its narration says "these two
  never even got a connection" rather than claiming the audit trail shows a
  denial. **Worth fixing in the product copy** — see the note in
  `walkthrough.spec.ts`.
- **Two components look identical and carry different roles.** The Add-workspace
  dialog's source/image cards are `OptionCard` (`form-primitives.tsx`) — an
  `aria-pressed` `<button>`. New Run's Confinement and Network cards are a
  different component rendering `role="radio"`, whose accessible name is the
  title *plus* the hint (`"Confined Default-deny. New hosts are held at the
  door for your approval."`). Assuming one from the other breaks the driver in
  whichever direction you guessed. Read the a11y snapshot Playwright writes to
  `test-results/<test>/error-context.md` on failure; it lists every role and
  name on the page and settles it in seconds.
- **New Run's Workspace select has NO accessible name.** The Agent select beside
  it is labelled ("Agent") because it goes through `Field`/`Label htmlFor`; the
  Workspace `<SelectTrigger>` never got the same treatment, so its a11y node is
  a bare `combobox` whose only text is a nested `generic` holding the
  placeholder. `getByRole("combobox", { name: … })` therefore cannot match it,
  and the driver filters on visible text instead. **This is a real
  accessibility defect, not a test inconvenience** — a screen-reader user
  tabbing onto it hears "combobox" and nothing about what it selects. Worth an
  `aria-label` on the trigger.
- **The `demo` Playwright project must not spread `devices["Desktop Chrome"]`.**
  The preset carries `deviceScaleFactor`, which Playwright refuses to combine
  with the `viewport: null` this project needs to let the real window size the
  frame (`"deviceScaleFactor" option is not supported with null "viewport"`).

## Act 5 specifics, all learned the hard way

- **Run mode defaults to `interactive`** (`initialWizardState()`, wizard-types.ts),
  which launches an IDLE sandbox waiting for a human to type. The agent never
  executes the task, so nothing reaches for `example.com` and the held approval
  never appears — the run just sits there. Act 5 selects **Batch** explicitly
  (**Autonomous** once Track B's rename lands — see Act 5 above).
  This is now STRUCTURAL as well as semantic: the Task field only exists in batch
  mode, so filling it before the **Batch** click targets nothing. An interactive
  run instead offers **Start with** (the agent CLI, or a bare terminal).
- **Every run needs a title** (`DEMO_TITLE`, `ui/e2e/demo/task.ts`). Launch stays
  disabled and reads *"Give this run a title."* until one is typed, and the title
  — not the task — is the run's headline on the board and in the run-detail
  command bar, so it is on camera for the rest of the film.
- **"Allow writes to this directory" must be ticked**, or the mount is read-only
  and the agent's edits never reach the host: the run reports success over a
  workspace with an empty `git diff`. (Ticking it was not enough on its own —
  see the `resolvedMountReadOnly` fix below.)
- **Always decide approvals BY HOST, never "the first row".** A real Claude Code
  run reaches for its telemetry endpoint,
  `http-intake.logs.us5.datadoghq.com`, and under a review-gated policy that
  surfaces as a pending approval — often *before* the one the act is about.
  Taking `.first()` meant the driver approved a telemetry host on camera while
  `example.com` sat undecided. Every `decide()` call names its host.
- **The ceiling does NOT clamp this demo — the request is genuinely HELD.**
  `internal/api/inline_policy.go` applies `composer.Clamp` only when
  `!s.isOperator(...)`, and BOTH local mode and the admin token count as
  operator — so a demo run's `wait_for_review` survives verbatim. Act 5's
  `example.com` is held open at the proxy and the on-camera approval completes
  that same in-flight request. Two consequences worth pinning: the UI renders
  "Sandbox is waiting" with a WAITING badge (narration must agree), and an
  approved hold logs **only `egress.allow`** — asserting `egress.pending`
  would fail a correct take. `DEMO_TASK`'s retry instruction stays as
  belt-and-braces for a missed 30s window, not as the primary mechanic.
- `new-run-screen.tsx` never loaded the workspace list. `useWorkspaceList` does
  not fetch on mount (each caller does its own load — `setup-screen.tsx` does);
  New Run's only call was the Add-workspace dialog's `onCreated`. The Workspace
  select therefore offered nothing but "Ephemeral scratch", so a workspace
  onboarded anywhere else could not be attached to a run at all.
- `wizard-types.ts`'s `resolvedMountReadOnly` ignored `sources[].writable`. It
  granted write only from a `write:<path>` requirement row, which nothing
  creates — making the dialog's "Allow writes to this directory" checkbox a
  no-op for every UI-launched run. `internal/api/workspace_run.go` had always
  honoured `src.Writable`; the client mirror now agrees.

## Constraints worth knowing before a manual re-shoot

- **`wait_for_review` holds for exactly 30 seconds** (`defaultHoldTimeout`,
  `internal/egress/proxy/approvals.go`; `proxy.go` passes `0`, meaning "keep the
  default", so there is no per-run override). The driver approves in about two
  seconds. A human re-shooting by hand has half a minute, then it falls back to
  a 403 and needs a retry.
- **Step 2 of the task uses `curl --max-time 90` on purpose** — it has to outlast
  that hold so the *same* request completes on approval.
- **The agent is not deterministic.** The driver waits on observable state
  (an approval row appears, the run reaches a terminal state), never on a clock.
  If a take looks wrong, look at what the agent actually did before assuming the
  driver broke.
- **`make setup` has no documented duration** and varies with what Docker has
  cached. Don't narrate a number; the script timestamps each phase so an editor
  can cut the build.
- **The five funnel demos need a real runner.** They start actual sandboxes, so
  the driver runs against the compose stack on `:8080`, never the hermetic
  `-runner none` e2e backend (which renders every Start disabled).

## Checking a take — one command

```sh
scripts/verify-demo-take.sh /mnt/c/Users/<you>/Videos/wardyn-demo-<ts>.mp4
```

It checks the take against the **audit trail and the filesystem**, not the exit
code, and exits non-zero if any of it fails. The list below is the walkthrough's
— i.e. video 02's, and the default when `WARDYN_DEMO_VIDEO` is unset; the last
two entries are shared by every video (see "The series harness" above):

- the act-5 run exists and `api.anthropic.com` was allowed (the model path worked)
- `example.com` went `egress.pending` → `approval.decide` → `egress.allow`
  (held, decided on camera, and the retry actually landed), with its decision scope printed
- **no telemetry host was approved** — the `.first()` trap that once approved
  Claude Code's Datadog endpoint while the intended host sat pending
- **nothing unexpected is in `approved_egress`** — with decision scopes, that
  same slip would now write a permanent grant, on camera, in a governance demo
- the workspace has a real diff: `slugify()` in `src/slug.js`, plus `NOTES.md`
- the narration timeline is complete with **zero overlapping lines**
- the video is 1920x1080 and actually carries an audio track

## Before publishing a take

1. Scrub for secrets. Capture is whole-desktop: check no frame shows
   `deploy/compose/.env`, `~/.wardyn-demo-token`, an admin token, or a terminal
   scrollback with any of them.
2. Confirm the claims are true, not just photogenic —
   `wardyn audit --run <id> --json` should carry `egress.allow` for
   `api.anthropic.com`, and `approval.decide outcome=approved
   decision_scope=always` → `egress.allow` for `example.com` on the Act 5 run.
   **There is deliberately NO row for `169.254.169.254`** — that probe never
   reaches the proxy (uppercase-only `HTTP_PROXY` vs libcurl's lowercase-only
   `http_proxy` for plain http, then no default route), so nothing decides it
   and nothing logs it. Its absence IS the expected result; a demo that claims
   an audited denial there is overclaiming. `scripts/verify-demo-take.sh`
   encodes all of this.
3. Confirm the two new scope beats, the same way — exit 0 is the least
   reliable signal here too:
   - On the Act 3 once-or-for-good run's audit, a SECOND `egress.pending` for
     `example.com` after the `approval.decide outcome=approved
     decision_scope=once` — the re-raise is the entire point of that demo, so
     its absence means the demo silently taught the wrong lesson.
   - On the onboarded workspace (`wardyn workspace get <id>` or its detail
     page's Allowed hosts card), `example.com` present in `approved_egress` —
     the permanent grant the proof run at the end of Act 5 depends on.
4. Check the captions are legible at the resolution you are publishing at.
