# First contact — clickable prototype

The first ten minutes a new admin spends in the Wardyn console: sign in, walk setup,
hit the host that cannot confine anything, and find the way out of it.

Settles three approved issues together, because they are one walk:
[#212](https://github.com/cjohnstoniv/wardyn/issues/212),
[#213](https://github.com/cjohnstoniv/wardyn/issues/213),
[#214](https://github.com/cjohnstoniv/wardyn/issues/214).

**Open `index.html`.** No build, no dependencies, no network — the page makes exactly one
request, for itself.

## How to walk it

Three switches in the grey scaffold bar. Everything in that bar, and the dashed strip
under the frame, is review scaffolding and ships nowhere. Everything inside the framed
app is the product.

| Switch | What it changes |
|---|---|
| **Theme** | Light / dark. The console is dark-first; both are drawn from `ui/src/styles/theme.css`. |
| **Host** | **A** — a Linux host with `/dev/kvm` present and only Fence registered. **B** — the same host with no container runtime answering, so the runner reports no confinement class at all. |
| **Proposal** | **Before** — what the console does today. **After** — what these three issues propose. **After is the thing being approved.** |

Sign in with any token. The dashed strip under the sign-in card walks each refusal in
place.

The walk worth taking: **Host B · Before** → sign in → New run → press Launch → it is
allowed, and fails afterwards, with nothing anywhere linking to the step that fixes it.
Then flip to **After** and take the same walk.

## States covered

**Sign in (#212)** — empty token field; token rejected; Wardyn unreachable; email domain
refused; SSO unconfigured. Each refusal renders in the real alert slot, in both modes.

**Setup (#213)** — the Environment step on a host with one barrier installed and on a host
with none; the step counter and rail grouping in both modes; a needs-setup column
revealing its setup command; an optional step entered from the rail and from Review; the
Review step; the forward gate that stands in for Next when no barrier exists.

**New run (#214)** — barrier picker with every class disabled and a reason per class;
Launch disabled with its reason stated beside it; Launch enabled and failing afterwards
(the Before dead end); a launch failure with and without `role="alert"`.

**Board** — the shipped first-run readiness rows, with the `Sandbox barrier` row red and,
in After, carrying a route out.

## Decisions to approve

1. **The counter counts the four blocking steps, not seventeen.** Environment, People,
   Network, Review are numbered `Step 1 of 4`. The other thirteen move into an
   `Optional · 13` rail group **after** them, with a count instead of numbers.
2. **Review stays a required step, in fourth place** — not the seventeenth item after ten
   demos. The alternative (Review last overall, after the optional group) is the main
   thing to push back on if you disagree.
3. **An optional step shows no number at all** — an `Optional` eyebrow and chip instead,
   and its footer returns to the required walk rather than stepping deeper.
4. **`Recommended` is the strongest class the host reports as INSTALLED.** Never inferred
   from the operating system. On a host reporting none, nothing is recommended and a line
   says so. Today's `recommendedTier` recommends Vault — experimental, and not installed —
   on any Linux host, including Host B, which cannot run anything at all.
5. **The `Recommended` chip becomes `tone="neutral"`.** `CONSOLE-RULES.md` §2 already lists
   the teal chip as a known violation: a recommendation is not an action.
6. **The chip row is fixed-height (22px).** Flip to Before on Host A and watch the Vault
   column's row grow to 52px and shove the matrix down — that is the misalignment in
   `docs/img/getting-started.png`.
7. **New run stays reachable on a barrier-less host; Launch is what is disabled**, with its
   reason beside it, not in a tooltip. Disabling New run instead would hide the
   explanation behind the control that carries it.
8. **The top bar's route is an info link beside New run**, not a disabled New run and not a
   relabelled one. Three routes to the Environment step in total: this link, the shell
   banner, and the reason beside Launch. The board's `Sandbox barrier` row carries a
   fourth.
9. **The Barrier control leaves the Policy card** and becomes its own section above it. It
   is the choice that decides whether a run is confined; it was the hardest one to find.
10. **Setup cannot be finished on a host with no barrier.** `Finish setup` is disabled with
    a gate naming the reason. This is the one decision that could trap someone — see the
    open questions.

## Copy changes

Every string below is a proposal for canon. Anything not listed is reused unchanged from
the source named beside it.

| Where | Today | Proposed |
|---|---|---|
| Token field placeholder (`sign-in.tsx`) | `demo-admin-token` | *(empty)* |
| Token helper (`sign-in.tsx`) | "Paste the token this control plane was started with (WARDYN_ADMIN_TOKEN; the compose demo uses demo-admin-token)." | "The token this Wardyn daemon was started with. Your install printed it when it finished." |
| Unreachable (`sign-in.tsx#submitToken`) | "Could not reach the control plane." | "Wardyn isn't answering at this address. Check that the wardynd daemon is running, then try again." |
| `email_domain` refusal (`sign-in.tsx#authErrorMessage`) | "…Ask an operator to add it to WARDYN_OIDC_EMAIL_DOMAINS." | "This email's domain isn't allowed to sign in to this console. Ask your Wardyn admin to allow it." |
| Generic error (`states.tsx#ErrorState`) | "We couldn't reach the Wardyn control plane. Please try again." | "Wardyn isn't answering. Check that the daemon is running, then retry." |
| Step counter (`setup-layout.tsx`) | "Step 1 of 17" | "Step 1 of 4" + "Required before a run can launch. 13 optional steps follow." |
| Optional step eyebrow | *(none — just the Optional chip)* | "Optional" + "Not required before a run can launch." |
| Rail groups (`steps.ts#PHASES`) | Essentials · Egress demos · Secrets demos · Your work · Finish | Required · 4 / Optional · 13 |

New strings, with no string they replace:

| Where | Proposed |
|---|---|
| Under the matrix, barrier installed | "Recommended is the strongest barrier installed on this host. Wall and Vault are stronger and each needs a one-time setup step." |
| Under the matrix, none installed | "Nothing is recommended while the host reports no barrier. Wardyn recommends what it can see, not what the operating system suggests." |
| Shell / board banner | "No barrier can be built on this host — runs can't launch." + "Wardyn confines every run. Until a container runtime answers, there is nothing to confine it with." |
| Top bar, board row, launch reason | "Set up a barrier" |
| Beside a disabled Launch | "No barrier can be built on this host, so no run can be confined. Set up a barrier." |
| New run barrier options | "Not available — no container runtime is answering." · "Not available — this host doesn't expose /dev/kvm." · "Not available — not set up on this host." |
| Environment forward gate | "No barrier is available on this host." + "Runs can't launch until one can be built. Re-check once a container runtime is answering." |
| Review gate | "Setup can't finish without a barrier." + "Wardyn confines every run. Set up a barrier." |
| Review's optional section | "Optional, whenever you want them" + "13 steps that teach Wardyn's guarantees or connect your own work. None of them blocks a run." |
| Optional step footer | "Back to required steps" · "Done with this one" |

**Reused verbatim, not re-proposed:** the CC ladder and matrix (`cc-meta.ts` — labels,
taglines, `PICK_WHEN`, `CC_MATRIX_ROWS`, `CC_MATRIX_WHERE`, `CONFINEMENT_CONSTANT_NOTE`,
`RESIDUAL_PREFIX`); `STATUS_LABEL` and `BTN` (`copy.ts`); the no-runner danger card and the
"tier matrix is shown for reference" line (`environment-step.tsx`); the Getting started
header and sub (`setup-layout.tsx`); the first-run hero and readiness rows
(`runs-first-run.tsx`); the rejected-token refusal, "This console governs the agents on this
host.", the Remember checkbox and the SSO fallback line (`sign-in.tsx`).

**Written for this prototype, NOT proposals.** The People, Network and Review step *bodies*
and the demo step body are scenery — those screens are not in scope for these three issues
and their real bodies live in `setup/step-bodies.tsx` and the demo catalogue. They exist so
the walk is continuous. Judge the counter, the rail and the gates around them; ignore their
contents.

## Open questions

1. **"control plane" elsewhere.** #212 renames the product in the generic error and
   unreachable states. The SSO fallback line still reads "SSO sign-in isn't configured on
   this **control plane** — use an admin token." Rename every occurrence, or only the two
   #212 names? Left unchanged in the prototype.
2. **What the unreachable state names to check.** #212 asks it to name one thing. I chose
   the daemon ("Check that the wardynd daemon is running"). The alternative is the address
   or the port. Naming a binary may be the wrong register for someone who did not install it.
3. **The "13" is not a constant.** `stepOrder(status)` drops demos needing a connected model
   or a stored secret, so a host with a model connected walks more than thirteen optional
   steps. Should the rail print a live count, or a fixed one that goes stale?
4. **Does a disabled `Finish setup` trap the admin?** Today's gate keeps the operator in
   setup until the end and there is no skip. On a host that genuinely cannot build a
   barrier, `Finish setup` disabled means setup can never be completed. Correct — or does
   it need an "I'll fix this later" exit to the board?
5. **Whether `Recommended` should appear at all when only one class is installed.** On Host
   A it sits on the only selectable column, which may be noise.
6. **`recommendedTier`'s KVM inference has a second reader.** The prototype only changes the
   recommendation. Whether the same inference should stop driving the *incompatible* verdict
   on the Vault column is a separate call not made here.

## Known residual

On the setup screen at phone width the page can be dragged sideways into empty background.
No content is clipped or unreachable — `body.scrollWidth` equals the viewport and nothing
paints out there; it is the root's scroll width aggregating the barrier matrix's own
horizontal scroller. `overflow-x: clip` on `html` and `body` does not suppress it in
Chromium. Cosmetic, scaffold-only, and not a property of the design being approved.

## What was checked

Walked in Chromium at 1280px, 390px and 360px, in both themes: zero console errors, zero
page errors, one network request (the page). Every path listed under **States covered** was
clicked through in all four Host × Proposal combinations.
