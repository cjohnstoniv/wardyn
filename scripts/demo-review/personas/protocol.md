# Demo-review viewing protocol (shared core)

The reusable harness for reviewing the Wardyn video series through first-time
viewers' eyes. A review round pairs ONE persona file from this directory with
ONE video's corpus (transcript + timestamped frames) and this protocol. The
persona is the audience, not an auditor.

## Fidelity rules (binding on whoever assembles the prompt)

- The viewer gets ONLY what a viewer gets: the video's frames in timestamp
  order, interleaved with the narrator's lines. Never the specs, never the
  source, never plan files. The prompt must FORBID reading the repository.
- Sequential knowledge: watching video N includes the transcripts of videos
  1..N-1 as "you watched these earlier." Video 01 is fully cold.
- An unshot video (transcript-only) is said plainly: "the visuals are not
  filmed yet — you are hearing the narration; judge the words and the flow of
  steps, and say what you EXPECT to need to see."
- Series context, titles only: the viewer also gets the numbered TITLES of
  every episode after the one being watched — the way a course syllabus would
  show them — and NEVER their content. Feedback that amounts to "I wanted X"
  when a later title plainly owns X is out of scope for this episode; judge
  instead whether THIS episode hands off to it properly (does it say where X
  lives, and does the promise match the title?).
- A persona who did not watch an episode reports NOT-WATCHED for its lane
  rather than nothing — silence is indistinguishable from a pass.
- Optional sub-episodes (lettered — 03b, 03c, 03d today; a syllabus title
  marks them "(optional)", "(desktop)", "(cloud)", "(members)" or "(admin)")
  are watched right after their parent episode, with the core path through the
  parent as prior knowledge. A later CORE episode's prior knowledge is the
  core path only — the core must stand without its detours, and a core line
  that depends on a detour is a finding.

## Think aloud as you watch, with timestamps

- **Follow the UI**: at each moment, say what you think you are looking at and
  what just happened. Flag any frame where you cannot tell, where the
  spotlight/ring sits on something other than what is being talked about,
  where text is too small or dense to read, or where the screen changed and
  you don't know why.
- **Follow the steps**: could you repeat what was just done? Name every step
  where you'd fumble — a click you didn't see coming, a field that appeared
  from nowhere, a precondition that was never shown.
- **Comprehension friction**: every term you don't know at the moment it is
  used; every claim you don't see proven; every stretch where you are bored or
  lost (long stillness, or watching something meaningless to you).
- **Skepticism**: claims you don't buy yet, and what would convince you.
- **Say it aloud (script reviews)**: the captions ARE the narration — read
  every line as the TTS will speak it and flag pronunciation traps:
  initialisms that must be spelled (CI, CLI, API…), heteronyms whose reading
  depends on sense (live/lives, record noun-vs-verb, read present-vs-past),
  digits, versions, and hostnames. A trap is a finding even when the wording
  is fine on the page; the fix lands in narrate-server.py's speakable(), not
  by mangling the on-screen caption.

## Close with

1. In your own words: what is Wardyn, and what did THIS video teach you it can
   do? (Honest — say what actually landed, not what was probably intended.)
2. The video's quiz (below), answered ONLY from what you saw — "I don't know"
   is a valid and valuable answer.
3. The three changes that would most improve this video for someone like you.
4. Your verdict, in your persona's own terms (each persona file defines it),
   ending with your lane's PASS / FAIL (see "Verdict lanes").

## Per-video quiz

| Video | Questions (answer from the viewing only) |
|---|---|
| 01 (primer) | Why would a team without AI agents still want a sandbox? What are a sandbox, egress, and a proxy, in your own words? What was the real caught-on-camera example, and what happened to it? What does "observe, then decide" replace? |
| 02 | What is a "barrier"? What happens when a run reaches a host that isn't allowed? Where does the model key live? What did the setup actually require of you? |
| 03a | Name the four things that can happen to a host a run dials, and which policy line decides each. What happens when you try to read a secret back? Where did the key live while the run used it? |
| 03b (optional) | Which host did the agent reach, and how did you know it was the only one? What did the recording propose, and from what? What happened on the third `curl` after an approval scoped Once? |
| 03c (optional) | What did the second mint return, and why is that the point? Why is a PAT minted into a pipe and not a header? What did the audit panel show for the approval-gated grant, in order? |
| 03d (optional) | Which credential kind touches disk, and for how long? Which two demos never started a sandbox, and why is that the lesson rather than a failure? |
| 04 | What is a workspace? What can a run touch outside it? |
| 05 | What does a policy carry that a single run's settings don't? What does the "floor" do? What did the safety meter read, and why? |
| 06 | What ran, and where? How did you know it finished? How do you know which policy the run ACTUALLY ran under? |
| 07 | Who was driving the terminal? What credential did the agent hold, and why didn't it matter? What's the difference from the first run in 06? |
| 08 | What is the ONLY prompt an autonomous agent gets? What can interrupt it mid-run? Who approved example.com and with what scope? |
| 09 | Where did the policy come from? What does "replay confined" mean? What happened to the host the recording never saw? |
| 10 | Name the four approval scopes. Which one outlives the run, and where is its receipt? |
| 11 | What replaces the human approver in CI? What does the pipeline's exit code mean? What receipts does a pipeline run leave? |
| 12 | Who may SSH into a run? What does the second person see? What proof exists afterward of who did what? |
| 13 (cloud) | What is different about where the sandbox runs? What did `exit 37` prove? Which audit rows name the person? |

## Verdict lanes

The six personas are six INDEPENDENT acceptance conditions, not six votes.
Each report ends with its lane's verdict — PASS, FAIL, or NOT-WATCHED — and the
round's ADJUDICATION records all six per episode. Lanes are never averaged: an
episode can pass comprehension and fail security, and it is reported that way.
A FAIL in any lane blocks the take unless the owner rules it standing (the
ruling, with its reason, goes in the ADJUDICATION so later rounds can cite it).
The adjudicator discounts each lane's known blind spot explicitly; the persona
is never told its flaw.

| Lane | Persona | Passes when | Known blind spot |
|---|---|---|---|
| Understandable | Morgan (`morgan-citizen-dev.md`) | could retell the story from the screen, and knows why what was stopped was bad | underweights technical correctness — a convincing analogy can carry a weak mechanism |
| Learnable / adoptable | Priya (`priya-junior-dev.md`) | could repeat the steps, and knows where she'd need help | equates understandable with correct |
| Technically credible | Sam (`sam-senior-dev.md`) | mechanism, failure case and evidence shown, not staged away | over-indexes on implementation detail; a viewer, not a code reviewer |
| Operable | Riley (`riley-platform-engineer.md`) | the setup path is complete and failure behaviour visible; the team could run, upgrade and debug it | may demand operational depth a demo episode cannot carry — route to the desktop/cloud/admin episodes |
| Security-defensible | Alex (`alex-security-reviewer.md`) | the security claim is proved at the enforcement point and the system's limits are stated | may fail an episode for a claim a later episode owns — the titles-only syllabus rule applies |
| Organizationally fundable | Dana (`dana-eng-leader.md`) | receipts — who decided what, when — that would survive security review, procurement and audit | over-credits a polished audit UI; does not validate the evidence herself |

## Series review (after the final video of a round)

Per-video viewing judges each episode alone; a full review round ALSO ends
with one series-level pass per persona, judging the whole as a course:

- **Order**: is this the sequence YOU needed? Name any video you'd move, and
  what confused you at the moment you watched it in the shipped order (a
  concept used before it was taught, a payoff that landed before its setup).
- **Coverage**: what's missing — the video you kept waiting for that never
  came, the question the series never answered that you needed answered
  before your verdict.
- **Duplication**: what did you watch twice? Name the beats that re-taught
  something you already had, and which occurrence to keep.
- **Arc & momentum**: where did the series sag, where did it peak, and where
  would you have stopped watching if nobody was making you continue. Does
  each outro's promise match what the next video actually opens with?
- **Length & split**: any video that should be two, or two that should be one?
- **The one restructure** you'd make if you could only change one thing.

Inputs for this pass: every core transcript in order (01–12), the optional
sub-episodes this persona watched, plus your OWN viewing reports as your
notes. Answer as the persona, in their terms.

## Special passes

- **Red-team pass** — not a persona. An adversarial reviewer is handed ONE
  episode (transcript or frames) plus the list of security claims it makes, and
  tries to break each claim: what would an attacker do, what does the episode not
  show, what test would settle it. Output: the claims that fail and the test for
  each. Run it when a security claim changes or before a security-lane episode
  ships; Alex's lane is the standing review, the red-team pass is the drill.
- **Accessibility, jargon, pacing, pronunciation** are every viewer's job (see
  "Say it aloud" and the think-aloud rules above), not a persona's — a dedicated
  persona would make them one lane's responsibility instead of everyone's.
- **Parked personas.** Jordan (staff architect, evaluating the system boundary):
  not created — revisit only if Dana, Alex and Riley leave architecture questions
  unowned across a whole round. Procurement/legal, AI-platform lead, OSS
  maintainer and PM were considered and rejected as viewing lanes.

## Report format

Timestamped think-aloud (bullet per moment worth a note), then the four
closing sections. Cite frames by filename. Severity is implicit in your
reaction — say it like a person, not like a linter.

## Revisions

- 2026-08-20: initial protocol (persona library round 1).
- 2026-08-20 (b): added the series-level review pass (order / coverage /
  duplication / arc / length / one-restructure) — round 1 reviewed videos only
  individually and shipped no whole-course judgment; owner directive.
- 2026-08-20 (c): viewers now receive the TITLES (only) of later episodes —
  round-1 feedback repeatedly demanded content a scheduled later episode
  already owned, which reads as a gap when it is actually a handoff; owner
  directive.
- 2026-08-24: quiz re-keyed to the restructured series (01–12 + 13); episode
  03 split into 03a + optional 03b/03c/03d; optional-sub-episode rule added
  (owner directive).
- 2026-08-24 (b): six lanes — Alex (security) and Riley (platform) added; verdict
  lanes are independent acceptance conditions (PASS / FAIL / NOT-WATCHED, never
  averaged); red-team as a pass, not a persona; Jordan parked — external
  persona-library review, owner directive.
