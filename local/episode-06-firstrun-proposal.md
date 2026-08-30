# Episode 06 — first-run proposal: captions retired from the pre-split policies episode

Source: `ui/e2e/demo/retiring-policies-and-confinement.spec.ts` (deleted in the 0.7 cleanup pass; unreachable since the
renumber — `scripts/record-demo.sh` globs `<NN>-*.spec.ts`). Of its 44 captions, 23 already live verbatim in
`05-your-first-policy.spec.ts`; these 24 did not. `06-your-first-run.spec.ts` cites this file as the proposal it
is held in lockstep with — treat every line below as **[OWNER SLOT — drafted]**, never edited into a spec without a
rehearse (`scripts/record-demo.sh --no-record --video 06`, live :8080 stack, never `--reset`).

## Cold open — the board keeps failures (candidate for 06's opening beat, or 12's)

- That red card is a run that failed earlier in this series.
- The board keeps it.
- Nothing here quietly disappears because it was inconvenient.

## Why a policy — the by-hand intro 05 already covers in its own words (keep for reference; do not re-add to 05)

- Up to now, we've been configuring every run by hand.
- That works.
- But it doesn't scale.
- A policy lets us take those decisions and make them reusable.
- A policy is that same kind of spec, saved once with a name so runs can reuse it.
- Here's the same set of rules we built by hand in episode seven — saved once, named, and reusable.
- Now a new run just points at that policy.

## Post-launch payoff — 05 stops before launch; this is 06's material (launch by reference against `first-policy`, then read the record)

- This time it runs.
- And now the record tells us two things:
- which barrier actually ran...
- and which policy governed it.
- That's the important part.
- Not what we happened to select on the form.
- What the control plane actually enforced.
- One policy.
- Every run that uses it gets those same rules.
- We've gone from manually configuring every run...
- to writing the rules once.
- But there's still a problem.
- How do you know what the right rules should be in the first place?
- That's what episode nine is about.

The same episode's post-launch checks — `check_video_08_policies` in `scripts/verify-demo-take.sh` (114 lines, undispatched) —
assert the `allowed_domains`/`CC2` pair 05 authors and the run/audit half 06 launches; they are the content arm for
`check_video_floor_05`/06, kept in the tree for that reason.
